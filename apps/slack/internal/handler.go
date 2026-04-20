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

// sigFailureResponse is the terse body we return for every 401 from the
// signature-verify path. Distinguishing which check failed is already
// captured in the structured slog line; the wire body stays uniform.
const sigFailureResponse = "signature verification failed"

const (
	// Matched case-insensitively — API Gateway preserves or lowercases depending on version.
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
		if err := h.prepareAndVerifySlackRequest(req); err != nil {
			return respond(http.StatusUnauthorized, map[string]string{"error": sigFailureResponse})
		}
		return h.handleSlashCommand(ctx, req)
	case req.Path == "/slack/events" && req.HTTPMethod == methodPost:
		if err := h.prepareAndVerifySlackRequest(req); err != nil {
			return respond(http.StatusUnauthorized, map[string]string{"error": sigFailureResponse})
		}
		return h.handleEvent(req)
	case req.Path == "/slack/interactions" && req.HTTPMethod == methodPost:
		if err := h.prepareAndVerifySlackRequest(req); err != nil {
			return respond(http.StatusUnauthorized, map[string]string{"error": sigFailureResponse})
		}
		return h.handleInteraction(req)
	case req.Path == "/health":
		return respond(http.StatusOK, map[string]string{"status": "ok"})
	default:
		return respond(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// prepareAndVerifySlackRequest authenticates a Slack request and mutates
// req.Body + IsBase64Encoded so downstream handlers see the decoded bytes
// Slack signed. The name reflects the side effect — it's not a pure
// predicate.
//
// API Gateway hands the Lambda a base64-wrapped body for any content type
// in the binary-media-types list. If that config ever matches Slack's
// types, HMAC-over-base64 wouldn't match Slack's HMAC-over-raw and every
// valid request would 401 silently — so we decode first.
func (h *Handler) prepareAndVerifySlackRequest(req *events.APIGatewayProxyRequest) error {
	if req.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			slog.Warn("slack signature verification failed — base64 decode error",
				"path", req.Path, "error", err)
			return errSlackSignatureMalformed
		}
		req.Body = string(decoded)
		req.IsBase64Encoded = false
	}

	sig := headerValue(req.Headers, req.MultiValueHeaders, headerSlackSignature)
	ts := headerValue(req.Headers, req.MultiValueHeaders, headerSlackTimestamp)
	err := verifySlackSignature(h.cfg.SlackSigningSecret, req.Body, sig, ts, h.now())
	if err != nil {
		attrs := []any{
			"path", req.Path,
			"reason", classifySlackErr(err),
			"has_signature", sig != "",
			"has_timestamp", ts != "",
		}
		// Empty secret means the deployment is effectively open — page on
		// it distinctly from ordinary 401 noise.
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
// error messages. "secret_empty" is unreachable under normal startup —
// cmd/main.go refuses to boot without SLACK_SIGNING_SECRET — so seeing
// it in telemetry implies a code path that bypassed the main entry point
// (tests, lambda custom runtime, etc.).
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

// headerValue does a case-insensitive lookup against both Headers and
// MultiValueHeaders so v1 and v2 API Gateway both work. Assumes only one
// casing of a given header name is present per request — if both
// "X-Slack-Signature" and "x-slack-signature" appear in the same map,
// Go's randomized map iteration means the returned value is
// non-deterministic. API Gateway doesn't emit that shape in practice.
func headerValue(headers map[string]string, multi map[string][]string, name string) string {
	if v, ok := headers[name]; ok {
		return v
	}
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	if v, ok := multi[name]; ok && len(v) > 0 {
		return v[0]
	}
	for k, v := range multi {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
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
