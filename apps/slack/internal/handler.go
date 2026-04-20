// Package internal contains Slack-specific handler logic.
package internal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const methodPost = "POST"

const authFailureMessage = "Failed to authenticate. Please check your QURL API key configuration."

// Header names API Gateway passes through for Slack's signing scheme. Matched
// case-insensitively per RFC 7230 — API Gateway may normalize to lowercase or
// preserve the caller's casing.
const (
	headerSlackSignature = "X-Slack-Signature"
	headerSlackTimestamp = "X-Slack-Request-Timestamp"
)

// Config holds the Slack handler configuration.
type Config struct {
	QURLEndpoint       string
	AuthProvider       auth.Provider
	SlackSigningSecret string
	NewClient          func(apiKey string) *client.Client
}

// Handler processes Slack events and commands.
type Handler struct {
	cfg Config
	// now is injected so tests can pin the clock for timestamp-skew checks
	// without touching a package global. Defaults to time.Now.
	now func() time.Time
}

// NewHandler creates a new Slack handler.
func NewHandler(cfg Config) *Handler {
	return &Handler{cfg: cfg, now: time.Now}
}

// Handle routes incoming API Gateway requests to the appropriate handler.
func (h *Handler) Handle(ctx context.Context, req *events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	slog.Info("received request", "path", req.Path, "method", req.HTTPMethod)

	switch {
	case req.Path == "/slack/commands" && req.HTTPMethod == methodPost:
		if err := h.verifySlackRequest(req); err != nil {
			return unauthorized()
		}
		return h.handleSlashCommand(ctx, req)
	case req.Path == "/slack/events" && req.HTTPMethod == methodPost:
		if err := h.verifySlackRequest(req); err != nil {
			return unauthorized()
		}
		return h.handleEvent(req)
	case req.Path == "/slack/interactions" && req.HTTPMethod == methodPost:
		if err := h.verifySlackRequest(req); err != nil {
			return unauthorized()
		}
		return h.handleInteraction(req)
	case req.Path == "/health":
		return respond(http.StatusOK, map[string]string{"status": "ok"})
	default:
		return respond(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// verifySlackRequest authenticates an incoming Slack HTTP request. On any
// verification error, the caller rejects the request with 401 and logs the
// failure class so operator metrics can separate "not Slack" noise from
// tampering attempts.
//
// API Gateway sets IsBase64Encoded=true for any content type in the Lambda's
// binary-media-types list. Slack slash commands arrive as
// application/x-www-form-urlencoded and events as application/json; both are
// normally treated as text. If the API Gateway config ever flips (or if a
// new binary-media-type matches by accident), the body handed to the Lambda
// is base64, and the HMAC over that base64 will not match Slack's HMAC over
// the raw body — every valid request would start 401ing silently. Decode
// here so the signature check runs against the same bytes Slack signed,
// turning the infra misconfig into a decode-error 401 with a distinct
// log line instead of a silent outage.
func (h *Handler) verifySlackRequest(req *events.APIGatewayProxyRequest) error {
	if req.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			slog.Warn("slack signature verification failed — base64 body decode error",
				"path", req.Path,
				"error", err.Error())
			return errSlackSignatureMalformed
		}
		// Rewrite req.Body so every downstream handler (handleSlashCommand,
		// handleEvent, handleInteraction) sees the same bytes Slack signed,
		// not the API-Gateway-wrapped base64. Without this, a valid signed
		// slash command would verify then silently fall through to the help
		// path because url.ParseQuery would parse a base64 blob.
		req.Body = string(decoded)
		req.IsBase64Encoded = false
	}

	sig := headerValue(req.Headers, headerSlackSignature)
	ts := headerValue(req.Headers, headerSlackTimestamp)
	err := verifySlackSignature(h.cfg.SlackSigningSecret, req.Body, sig, ts, h.now())
	if err != nil {
		// Escalate empty-signing-secret to Error — it means the deployment
		// is effectively open; the fail-closed 401s are defense-in-depth,
		// not the primary guard. Pages should route differently for this.
		// Other failure classes are noise from unauthenticated scans and
		// stay at Warn.
		attrs := []any{
			"path", req.Path,
			"reason", classifySlackErr(err),
			"has_signature", sig != "",
			"has_timestamp", ts != "",
		}
		if errors.Is(err, errSlackSigningSecretEmpty) {
			slog.Error("slack signature verification failed — signing secret is empty (deployment is open)", attrs...)
		} else {
			slog.Warn("slack signature verification failed", attrs...)
		}
	}
	return err
}

// classifySlackErr maps the sentinel verification errors to stable metric
// labels so operator dashboards can group by cause without string-matching
// error messages (which may be reworded over time).
func classifySlackErr(err error) string {
	switch {
	case errors.Is(err, errSlackSigningSecretEmpty):
		return "secret_empty"
	case errors.Is(err, errSlackSignatureMissing):
		return "headers_missing"
	case errors.Is(err, errSlackSignatureMalformed):
		return "malformed"
	case errors.Is(err, errSlackTimestampStale):
		return "stale"
	case errors.Is(err, errSlackSignatureMismatch):
		return "mismatch"
	default:
		return "unknown"
	}
}

// headerValue fetches a header case-insensitively — API Gateway v1 preserves
// caller casing, v2 lowercases, and strings.EqualFold handles both.
func headerValue(headers map[string]string, name string) string {
	if v, ok := headers[name]; ok {
		return v
	}
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// unauthorized returns a 401 response for a Slack-signature failure. Body is
// intentionally terse — a caller able to read it can only learn that signing
// is required, not why the particular attempt failed. Misconfig vs. attacker
// activity is already distinguished in the structured slog.Warn above.
func unauthorized() (events.APIGatewayProxyResponse, error) {
	return respond(http.StatusUnauthorized, map[string]string{"error": "signature verification failed"})
}

func (h *Handler) handleSlashCommand(ctx context.Context, req *events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	values, err := url.ParseQuery(req.Body)
	if err != nil {
		return respond(http.StatusBadRequest, map[string]string{"error": "invalid form body"})
	}

	command := values.Get("command")
	text := strings.TrimSpace(values.Get("text"))

	slog.Info("slash command", "command", command, "text", text)

	switch {
	case text == "" || text == "help":
		return respondSlack(helpMessage())
	case strings.HasPrefix(text, "create "):
		return h.handleCreate(ctx, values)
	case strings.HasPrefix(text, "list"):
		return h.handleList(ctx, values)
	default:
		return respondSlack(fmt.Sprintf("Unknown subcommand: `%s`. Try `/qurl help`.", text))
	}
}

func (h *Handler) handleCreate(ctx context.Context, values url.Values) (events.APIGatewayProxyResponse, error) {
	text := strings.TrimSpace(values.Get("text"))
	targetURL := strings.TrimPrefix(text, "create ")
	targetURL = strings.TrimSpace(targetURL)

	if targetURL == "" {
		return respondSlack("Usage: `/qurl create <url>`")
	}

	c, err := h.authenticatedClient(ctx, values.Get("team_id"))
	if err != nil {
		slog.Error("failed to get API key", "error", err)
		return respondSlack(authFailureMessage)
	}

	result, err := c.Create(ctx, client.CreateInput{TargetURL: targetURL})
	if err != nil {
		slog.Error("failed to create QURL", "error", err, "target_url", targetURL)
		return respondSlack("Failed to create QURL: " + err.Error())
	}

	return respondSlack(fmt.Sprintf("QURL created!\n*Link:* %s\n*Target:* %s", result.QURLLink, targetURL))
}

func (h *Handler) handleList(ctx context.Context, values url.Values) (events.APIGatewayProxyResponse, error) {
	c, err := h.authenticatedClient(ctx, values.Get("team_id"))
	if err != nil {
		slog.Error("failed to get API key", "error", err)
		return respondSlack(authFailureMessage)
	}

	result, err := c.List(ctx, client.ListInput{Limit: 5})
	if err != nil {
		slog.Error("failed to list QURLs", "error", err)
		return respondSlack("Failed to list QURLs: " + err.Error())
	}

	if len(result.QURLs) == 0 {
		return respondSlack("No QURLs found.")
	}

	lines := make([]string, 0, len(result.QURLs))
	for i := range result.QURLs {
		q := &result.QURLs[i]
		line := fmt.Sprintf("• `%s` → %s [%s]", q.ResourceID, q.TargetURL, q.Status)
		if q.Description != "" {
			line = fmt.Sprintf("• *%s* — `%s` → %s [%s]", q.Description, q.ResourceID, q.TargetURL, q.Status)
		}
		lines = append(lines, line)
	}

	return respondSlack("*Recent QURLs:*\n" + strings.Join(lines, "\n"))
}

// authenticatedClient resolves an API key for the team and returns a configured client.
func (h *Handler) authenticatedClient(ctx context.Context, teamID string) (*client.Client, error) {
	apiKey, err := h.cfg.AuthProvider.APIKey(ctx, teamID)
	if err != nil {
		return nil, err
	}
	return h.cfg.NewClient(apiKey), nil
}

func (h *Handler) handleEvent(req *events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// Handle Slack URL verification challenge.
	var body struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal([]byte(req.Body), &body); err == nil && body.Type == "url_verification" {
		return respond(http.StatusOK, map[string]string{"challenge": body.Challenge})
	}

	// TODO: Handle link_shared events for unfurling.
	slog.Info("event received", "body_length", len(req.Body))
	return respond(http.StatusOK, map[string]string{"ok": "true"})
}

func (h *Handler) handleInteraction(req *events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// TODO: Handle interactive components (buttons, modals).
	slog.Info("interaction received", "body_length", len(req.Body))
	return respond(http.StatusOK, map[string]string{"ok": "true"})
}

func helpMessage() string {
	return `*/qurl* — Create and manage QURLs from Slack

*Commands:*
• ` + "`/qurl create <url>`" + ` — Create a QURL for the given URL
• ` + "`/qurl list`" + ` — Show your 5 most recent QURLs
• ` + "`/qurl help`" + ` — Show this help message`
}

func respond(status int, body any) (events.APIGatewayProxyResponse, error) {
	b, _ := json.Marshal(body)
	return events.APIGatewayProxyResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(b),
	}, nil
}

func respondSlack(text string) (events.APIGatewayProxyResponse, error) {
	return respond(http.StatusOK, map[string]string{
		"response_type": "ephemeral",
		"text":          text,
	})
}
