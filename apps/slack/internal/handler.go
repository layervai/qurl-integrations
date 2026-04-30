// Package internal contains Slack-specific handler logic.
package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const authFailureMessage = "Failed to authenticate. Please check your qURL API key configuration."

const (
	headerSlackSignature = "X-Slack-Signature"
	headerSlackTimestamp = "X-Slack-Request-Timestamp"
)

const (
	pathHealth            = "/health"
	pathSlackCommands     = "/slack/commands"
	pathSlackEvents       = "/slack/events"
	pathSlackInteractions = "/slack/interactions"
)

// maxRequestBodyBytes caps the request body the handler will read. Slack
// slash-command and event payloads are well under 8 KiB; 1 MiB gives
// generous headroom while keeping a single bad client from forcing the
// task to allocate unbounded memory.
//
// HMAC verification needs the raw body, so we can't authenticate before
// reading — a flood of cap-sized junk to /slack/* with bad signatures
// will allocate up to 1 MiB per request. ALB-level rate-limiting / WAF
// is the real defense; this cap bounds the per-request damage.
const maxRequestBodyBytes = 1 << 20

// internalErrorEnvelope is the fallback 500 body used when JSON marshal
// of a richer payload fails (unreachable for current callers).
const internalErrorEnvelope = `{"error":"internal"}`

// Config holds the Slack handler configuration.
type Config struct {
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

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health checks are silent: ALB target-group probes hit this every
	// 15-30s per task and would otherwise dominate log volume.
	if r.URL.Path == pathHealth {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		default:
			respondMethodNotAllowed(w, "GET, HEAD")
		}
		return
	}

	switch r.URL.Path {
	case pathSlackCommands, pathSlackEvents, pathSlackInteractions:
		if r.Method != http.MethodPost {
			respondMethodNotAllowed(w, "POST")
			return
		}
	default:
		// Silent on 404: ALB target groups are reachable to internet
		// probes (/wp-login.php, /.env, etc.); logging each one would
		// be noise. The slack and health paths are the only legitimate
		// surface and they get their own log lines.
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	slog.Info("received request", "path", r.URL.Path, "method", r.Method) //nolint:gosec // G706: slog's JSON handler escapes control chars in attribute values, so tainted paths can't inject log lines.

	// Honest oversize declarations get rejected before allocation.
	// MaxBytesReader still catches dishonest senders during the read.
	if r.ContentLength > maxRequestBodyBytes {
		slog.Info("oversize body rejected", "path", r.URL.Path, "reason", "content_length_pre_check", "declared", r.ContentLength) //nolint:gosec // G706: see ServeHTTP — slog escapes tainted attribute values.
		respondPayloadTooLarge(w)
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		// Same operational condition as the Content-Length pre-check
		// above; bucket them together so dashboards see one 413 stream.
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			slog.Info("oversize body rejected", "path", r.URL.Path, "reason", "max_bytes_during_read") //nolint:gosec // G706: see ServeHTTP — slog escapes tainted attribute values.
			respondPayloadTooLarge(w)
			return
		}
		slog.Warn("failed to read request body", "error", err, "path", r.URL.Path) //nolint:gosec // G706: see ServeHTTP — slog escapes tainted attribute values.
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	if err := h.verifySlackRequest(r, body); err != nil {
		respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "signature verification failed"})
		return
	}

	switch r.URL.Path {
	case pathSlackCommands:
		h.handleSlashCommand(r.Context(), w, body)
	case pathSlackEvents:
		h.handleEvent(w, body)
	case pathSlackInteractions:
		h.handleInteraction(w, body)
	}
}

// readBody reads the full request body up to maxRequestBodyBytes. Slack
// signature verification needs the exact bytes, and the parsed body is
// everything the downstream handlers need — the body is consumed here.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
}

// verifySlackRequest authenticates a request against the configured
// signing secret. Side-effect-free aside from a slog line on failure.
func (h *Handler) verifySlackRequest(r *http.Request, body []byte) error {
	sig := r.Header.Get(headerSlackSignature)
	ts := r.Header.Get(headerSlackTimestamp)
	err := verifySlackSignature(h.cfg.SlackSigningSecret, body, sig, ts, h.now())
	if err != nil {
		attrs := []any{
			"path", r.URL.Path,
			"reason", classifySlackErr(err),
			"has_signature", sig != "",
			"has_timestamp", ts != "",
		}
		// Empty secret means the deployment is effectively open — page on
		// it distinctly from ordinary 401 noise.
		if errors.Is(err, errSlackSigningSecretEmpty) {
			slog.Error("slack signature verification failed — signing secret is empty (deployment is open)", attrs...) //nolint:gosec // G706: attrs carries r.URL.Path which slog escapes.
		} else {
			slog.Warn("slack signature verification failed", attrs...) //nolint:gosec // G706: attrs carries r.URL.Path which slog escapes.
		}
	}
	return err
}

// classifySlackErr maps the sentinel verification errors to stable metric
// labels so operator dashboards can group by cause without string-matching
// error messages. "secret_empty" is unreachable under normal startup —
// cmd/main.go refuses to boot without SLACK_SIGNING_SECRET — so seeing
// it in telemetry implies a code path that bypassed the main entry point
// (tests, custom runtime, etc.).
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

func (h *Handler) handleSlashCommand(ctx context.Context, w http.ResponseWriter, body []byte) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form body"})
		return
	}

	command := values.Get("command")
	text := strings.TrimSpace(values.Get("text"))

	slog.Info("slash command", "command", command, "text", text)

	switch {
	case text == "" || text == "help":
		respondSlack(w, helpMessage())
	case strings.HasPrefix(text, "create "):
		h.handleCreate(ctx, w, values)
	case strings.HasPrefix(text, "list"):
		h.handleList(ctx, w, values)
	default:
		// Surfaced to telemetry so a workspace using a stale slash-command
		// spec is visible in dashboards (rather than only via user reports).
		slog.Info("unknown slash subcommand", "command", command, "text", text)
		respondSlack(w, fmt.Sprintf("Unknown subcommand: `%s`. Try `/qurl help`.", text))
	}
}

func (h *Handler) handleCreate(ctx context.Context, w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get("text"))
	targetURL := strings.TrimPrefix(text, "create ")
	targetURL = strings.TrimSpace(targetURL)

	if targetURL == "" {
		respondSlack(w, "Usage: `/qurl create <url>`")
		return
	}

	c, err := h.authenticatedClient(ctx, values.Get("team_id"))
	if err != nil {
		slog.Error("failed to get API key", "error", err)
		respondSlack(w, authFailureMessage)
		return
	}

	result, err := c.Create(ctx, client.CreateInput{TargetURL: targetURL})
	if err != nil {
		slog.Error("failed to create qURL", "error", err, "target_url", targetURL)
		respondSlack(w, "Failed to create qURL: "+err.Error())
		return
	}

	respondSlack(w, fmt.Sprintf("qURL created!\n*Link:* %s\n*Target:* %s", result.QURLLink, targetURL))
}

func (h *Handler) handleList(ctx context.Context, w http.ResponseWriter, values url.Values) {
	c, err := h.authenticatedClient(ctx, values.Get("team_id"))
	if err != nil {
		slog.Error("failed to get API key", "error", err)
		respondSlack(w, authFailureMessage)
		return
	}

	result, err := c.List(ctx, client.ListInput{Limit: 5})
	if err != nil {
		slog.Error("failed to list qURLs", "error", err)
		respondSlack(w, "Failed to list qURLs: "+err.Error())
		return
	}

	if len(result.QURLs) == 0 {
		respondSlack(w, "No qURLs found.")
		return
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

	respondSlack(w, "*Recent qURLs:*\n"+strings.Join(lines, "\n"))
}

// authenticatedClient resolves an API key for the team and returns a configured client.
func (h *Handler) authenticatedClient(ctx context.Context, teamID string) (*client.Client, error) {
	apiKey, err := h.cfg.AuthProvider.APIKey(ctx, teamID)
	if err != nil {
		return nil, err
	}
	return h.cfg.NewClient(apiKey), nil
}

func (h *Handler) handleEvent(w http.ResponseWriter, body []byte) {
	var v struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(body, &v); err == nil && v.Type == "url_verification" {
		respondJSON(w, http.StatusOK, map[string]string{"challenge": v.Challenge})
		return
	}

	// TODO: Handle link_shared events for unfurling.
	slog.Info("event received", "body_length", len(body))
	respondJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (h *Handler) handleInteraction(w http.ResponseWriter, body []byte) {
	// TODO: Handle interactive components (buttons, modals).
	slog.Info("interaction received", "body_length", len(body))
	respondJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func helpMessage() string {
	return `*/qurl* — Create and manage qURLs from Slack

*Commands:*
• ` + "`/qurl create <url>`" + ` — Create a qURL for the given URL
• ` + "`/qurl list`" + ` — Show your 5 most recent qURLs
• ` + "`/qurl help`" + ` — Show this help message`
}

// respondMethodNotAllowed writes 405 with an RFC 7231 §6.5.5 Allow header.
// The header is the discriminator that lets ops separate "wrong method"
// from "missing path" (404) and "auth-gated" (401).
func respondMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}

// respondPayloadTooLarge writes 413 for both the Content-Length pre-check
// and the MaxBytesReader-during-read paths. Centralizing keeps the wire
// envelope identical so operator dashboards bucket them together.
func respondPayloadTooLarge(w http.ResponseWriter) {
	respondJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
}

func respondJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		// Marshaling a map[string]string / map[string]any can't fail in
		// practice; log and fall back to a fixed JSON envelope so the
		// Content-Type header doesn't disagree with the body.
		slog.Error("response marshal failed", "error", err)
		b = []byte(internalErrorEnvelope)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(b); err != nil {
		slog.Warn("response write failed", "error", err)
	}
}

func respondSlack(w http.ResponseWriter, text string) {
	respondJSON(w, http.StatusOK, map[string]string{
		"response_type": "ephemeral",
		"text":          text,
	})
}
