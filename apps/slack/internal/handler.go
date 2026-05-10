// Package internal contains Slack-specific handler logic.
package internal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const authFailureMessage = "Failed to authenticate. Please check your qURL API key configuration."

// Path constants (keep in lockstep with cmd/main.go's mux registration).
// Defined here so the handler dispatch switch + the log-classifier helper
// + any future test assertions all reference the same literal once.
const (
	pathHealth           = "/health"
	pathSlackCommands    = "/slack/commands"
	pathSlackEvents      = "/slack/events"
	pathSlackInteraction = "/slack/interactions"
	// logLabelOther is the log-only sentinel emitted by classifyPath
	// when the request URL doesn't match any registered route. It is
	// NOT a routable path — anything that lands here ultimately 404s.
	logLabelOther = "/other"
)

// sigFailureResponse is the terse body we return for every 401 from the
// signature-verify path. Distinguishing which check failed is already
// captured in the structured slog line; the wire body stays uniform.
const sigFailureResponse = "signature verification failed"

// maxHTTPBodyBytes caps the request body the HTTP adapter will buffer.
// Slack documents a 30 KiB ceiling for slash-command payloads and a 4 KiB
// ceiling for events; observed bodies sit well under 64 KiB in practice.
// 1 MiB is the round-number cap with generous headroom for future Slack
// payload-shape changes (block-kit interactions can grow under modal
// flows). The cap exists so a malicious or stuck client can't stream an
// unbounded body and tie up a goroutine before signature verification
// has any input to reject.
const maxHTTPBodyBytes = 1 << 20 // 1 MiB

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
//
// Retained for backward compatibility with the test surface (which speaks
// APIGatewayProxyRequest directly) and for any future migration back to a
// Lambda runtime. The HTTP entry point (ServeHTTP) adapts incoming
// net/http requests into this shape and reuses the same dispatch logic.
func (h *Handler) Handle(ctx context.Context, req *events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// req.Path is user-controlled but slog's JSONHandler escapes
	// structured field values, so a CRLF in the path can't break out
	// of the log line. classifyPath in ServeHTTP exists to satisfy
	// gosec G706 (which can't see through slog's escaping) and to
	// constrain log labels to a fixed set for dashboarding — both
	// goals, not a real injection-vector difference.
	slog.Info("received request", "path", req.Path, "method", req.HTTPMethod)

	switch {
	case req.Path == pathSlackCommands && req.HTTPMethod == methodPost:
		if err := h.prepareAndVerifySlackRequest(req); err != nil {
			return respond(http.StatusUnauthorized, map[string]string{"error": sigFailureResponse})
		}
		return h.handleSlashCommand(ctx, req)
	case req.Path == pathSlackEvents && req.HTTPMethod == methodPost:
		if err := h.prepareAndVerifySlackRequest(req); err != nil {
			return respond(http.StatusUnauthorized, map[string]string{"error": sigFailureResponse})
		}
		return h.handleEvent(req)
	case req.Path == pathSlackInteraction && req.HTTPMethod == methodPost:
		if err := h.prepareAndVerifySlackRequest(req); err != nil {
			return respond(http.StatusUnauthorized, map[string]string{"error": sigFailureResponse})
		}
		return h.handleInteraction(req)
	case req.Path == pathHealth:
		return respond(http.StatusOK, map[string]string{"status": "ok"})
	default:
		return respond(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// ServeHTTP is the long-running ECS Fargate entry point. It adapts an
// incoming net/http request into the API-Gateway-shaped value the existing
// dispatch logic understands, then writes the resulting envelope back to
// the response writer. The signature-verification, base64-decoding, and
// per-route dispatch all live in Handle so we keep one source of truth.
//
// Body size is capped at maxHTTPBodyBytes — Slack payloads are tiny in
// practice, but the long-running process needs an explicit ceiling so a
// stuck or hostile client can't tie up a goroutine before the HMAC check
// has any input to reject.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Resolve which Slack path this request is bound to from a
	// fixed allow-list. We mux only on /health + /slack/{commands,events,
	// interactions} (cmd/main.go), so any other inbound path is either a
	// probe or a misconfig — we log it as logLabelOther so a hostile
	// client can't plant CR/LF or attacker-chosen content into our log
	// lines (gosec G706, log forgery via taint).
	logPath := classifyPath(r.URL.Path)

	// http.MaxBytesReader is the idiomatic primitive for net/http body
	// caps: it surfaces a typed *http.MaxBytesError on overflow and
	// signals the server to close the connection cleanly so a chunked
	// or `Transfer-Encoding: chunked` client can't keep the goroutine
	// alive after we've decided to reject. The cap fires BEFORE
	// signature verification (see prepareAndVerifySlackRequest in
	// Handle) — that's load-bearing and is fenced by
	// TestServeHTTP_RejectsOversizedBody_BeforeSigVerify.
	r.Body = http.MaxBytesReader(w, r.Body, maxHTTPBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			//nolint:gosec // G706: logPath is one of the path-constant set returned by classifyPath, never user content
			slog.Warn("http body exceeds cap", "path", logPath, "limit_bytes", maxHTTPBodyBytes)
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		//nolint:gosec // G706: logPath is one of the path-constant set returned by classifyPath, never user content
		slog.Warn("http body read failed", "path", logPath, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	headers := make(map[string]string, len(r.Header))
	multiHeaders := make(map[string][]string, len(r.Header))
	for k, v := range r.Header {
		multiHeaders[k] = v
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	req := &events.APIGatewayProxyRequest{
		Path:              r.URL.Path,
		HTTPMethod:        r.Method,
		Headers:           headers,
		MultiValueHeaders: multiHeaders,
		Body:              string(body),
		// IsBase64Encoded stays false: net/http delivers the raw decoded
		// body, and the prepare/verify step's base64-unwrap branch is an
		// API-Gateway-only quirk.
		IsBase64Encoded: false,
	}

	resp, err := h.Handle(r.Context(), req)
	if err != nil {
		//nolint:gosec // G706: logPath is one of the path-constant set returned by classifyPath, never user content
		slog.Error("handler returned error", "path", logPath, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	// resp.Body is the JSON envelope produced by `respond()` — every
	// path through Handle goes through `json.Marshal`, so the byte
	// slice is never user-supplied content. gosec G705 still flags
	// the write because its taint analysis can't see through
	// `json.Marshal`; the nolint is narrow and cites the specific rule.
	if _, werr := w.Write([]byte(resp.Body)); werr != nil { //nolint:gosec // G705: resp.Body is the marshaled JSON envelope from respond(), never user content
		//nolint:gosec // G706: logPath is one of the path-constant set returned by classifyPath, never user content
		slog.Warn("response write failed", "path", logPath, "error", werr)
	}
}

// classifyPath maps a request URL path to a fixed-set log label so
// caller-controlled bytes never reach the log line. The allow-list
// matches the mux wiring in cmd/main.go — a request that hits any
// other path 404s and gets logged as logLabelOther for traffic
// visibility without leaking the attacker-chosen string.
func classifyPath(p string) string {
	switch p {
	case pathHealth:
		return pathHealth
	case pathSlackCommands:
		return pathSlackCommands
	case pathSlackEvents:
		return pathSlackEvents
	case pathSlackInteraction:
		return pathSlackInteraction
	default:
		return logLabelOther
	}
}

// writeJSON is the failure-path response helper for adapter-level errors
// (body too big, body unreadable, dispatch returned an error). The Handle
// code path already encodes its own JSON envelope and goes through the
// header-copy branch in ServeHTTP.
func writeJSON(w http.ResponseWriter, status int, body any) {
	// Inputs are always map[string]string literals from this file's
	// own callers — Marshal can't fail on those. Explicit `_ =`
	// (vs the `_, _ :=` shape) signals to readers and to errcheck
	// that the swallow is deliberate.
	b, err := json.Marshal(body)
	if err != nil {
		// Defensive: if a future caller passes something that DOES
		// fail to marshal, fall back to a hand-built literal so the
		// client still gets a usable error envelope and we log the
		// regression for incident triage.
		slog.Error("writeJSON marshal failed", "error", err)
		b = []byte(`{"error":"internal error"}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, werr := w.Write(b); werr != nil {
		slog.Warn("writeJSON write failed", "error", werr)
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
// valid request would 401 silently — so we decode first. Under the ECS
// HTTP entry point (ServeHTTP) IsBase64Encoded is always false, so this
// branch is a no-op in that path; we keep it because the same Handle
// function still serves both shapes.
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
		return "sig_malformed"
	case errors.Is(err, errSlackTimestampMalformed):
		return "ts_malformed"
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
		slog.Error("failed to create qURL", "error", err, "target_url", targetURL)
		return respondSlack("Failed to create qURL: " + err.Error())
	}

	return respondSlack(fmt.Sprintf("qURL created!\n*Link:* %s\n*Target:* %s", result.QURLLink, targetURL))
}

func (h *Handler) handleList(ctx context.Context, values url.Values) (events.APIGatewayProxyResponse, error) {
	c, err := h.authenticatedClient(ctx, values.Get("team_id"))
	if err != nil {
		slog.Error("failed to get API key", "error", err)
		return respondSlack(authFailureMessage)
	}

	result, err := c.List(ctx, client.ListInput{Limit: 5})
	if err != nil {
		slog.Error("failed to list qURLs", "error", err)
		return respondSlack("Failed to list qURLs: " + err.Error())
	}

	if len(result.QURLs) == 0 {
		return respondSlack("No qURLs found.")
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

	return respondSlack("*Recent qURLs:*\n" + strings.Join(lines, "\n"))
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
	return `*/qurl* — Create and manage qURLs from Slack

*Commands:*
• ` + "`/qurl create <url>`" + ` — Create a qURL for the given URL
• ` + "`/qurl list`" + ` — Show your 5 most recent qURLs
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
