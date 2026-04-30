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
	"sync"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const authFailureMessage = "Failed to authenticate. Please check your qURL API key configuration."

// ackWorkingOnIt is the user-visible ephemeral text returned synchronously
// while async work runs. The hourglass keeps the user oriented that a
// follow-up via response_url is on its way.
const ackWorkingOnIt = ":hourglass: Working on it…"

// ackBusy is returned when the bounded async pool is saturated. Surfacing
// this to the user (rather than silently dropping) makes back-pressure
// visible and gives them an actionable next step.
const ackBusy = ":warning: Slack bot is busy — please retry in a moment."

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

const (
	// defaultMaxConcurrentAsync caps in-flight goroutines. A Slack-side
	// flood (replay storm, runaway integration) drops with ackBusy past
	// this threshold rather than unbounded-spawning until the task OOMs.
	// 50 is generous for steady-state — the target customer (50 active
	// users) won't sustain >1 click/sec across the whole workspace.
	defaultMaxConcurrentAsync = 50

	// asyncWorkTimeout caps how long a single async job may run. Slack's
	// response_url is valid for 30 minutes, but in practice qURL API calls
	// resolve in <1s; 25s is the deadline beyond which the user is better
	// served by a "failed" follow-up than an indefinite "Working on it…".
	//
	// Interaction with WithRetry(2): the qURL client uses exponential
	// backoff with a 30s cap (shared/client.defaultMaxDelay). Under a
	// 5xx storm, retry backoff alone could in principle exceed the
	// remaining ctx budget — `c.waitForRetry` honors ctx and returns
	// ctx.Err() in that case, which surfaces as a non-*APIError and
	// hits sanitizeAPIError's prefix-only fallback. Trade-off is
	// intentional: cap the user's wait at this deadline rather than
	// let retries dominate.
	asyncWorkTimeout = 25 * time.Second

	// responseURLTimeout bounds the POST to Slack's response_url. Slack's
	// hooks endpoint typically responds in <500ms; 5s catches transient
	// blips without holding a goroutine slot for the full asyncWorkTimeout.
	responseURLTimeout = 5 * time.Second
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

	// BaseContext is the server-lifetime parent of every async work
	// goroutine's context. SIGTERM cancels it, which propagates to
	// in-flight qURL API calls and response_url POSTs so they release
	// the worker slot promptly during shutdown. Defaults to
	// context.Background() if nil — fine for tests, not for production
	// (cmd/main.go threads the signal-canceled context).
	BaseContext context.Context

	// MaxConcurrentAsync caps in-flight async goroutines. Zero or
	// negative falls back to defaultMaxConcurrentAsync.
	MaxConcurrentAsync int

	// ResponseURLClient is the HTTP client used to POST follow-up
	// messages to Slack's response_url. Nil means "use a default *http.Client
	// with responseURLTimeout"; tests inject one to assert payloads.
	ResponseURLClient *http.Client
}

// Handler processes Slack events and commands.
type Handler struct {
	cfg Config
	// now is injected so tests can pin the clock for timestamp-skew checks
	// without touching a package global. Defaults to time.Now.
	now func() time.Time
	// baseCtx is captured at NewHandler time from cfg.BaseContext (or
	// context.Background()). Each async goroutine derives a
	// context.WithTimeout(baseCtx, asyncWorkTimeout) — canceling baseCtx
	// (via SIGTERM in main.go) signals every in-flight worker.
	baseCtx context.Context
	// wg tracks live async workers so cmd/main.go's Wait() can drain
	// them after http.Server.Shutdown returns. wg.Add MUST happen on
	// the request goroutine (before the `go` keyword) — adding inside
	// the spawned goroutine races Wait().
	wg sync.WaitGroup
	// sem is a buffered-channel semaphore bounding concurrent async
	// workers to len(sem) capacity. Send-with-default-drop gives back-
	// pressure feedback to the user as ackBusy rather than queueing.
	sem chan struct{}
	// responseURLClient is owned per-Handler so tests can inject a
	// transport and so the lifetime is tied to the handler (not the
	// per-request goroutine).
	responseURLClient *http.Client
	// validateResponseURLFn defaults to validateResponseURL — pinned to
	// https://hooks.slack.com/* in production. Tests override it to
	// permit httptest server URLs (which are http://127.0.0.1:NNNNN).
	// Field rather than parameter so the production default needs no
	// per-deploy wiring.
	//
	// Returns a *url.URL on success rather than just an error so the
	// caller dials the validated/reconstructed URL — the production
	// validator pins Scheme and Host to literal constants on the
	// returned value, which is the SSRF-sanitization pattern CodeQL's
	// taint analysis recognizes.
	validateResponseURLFn func(string) (*url.URL, error)
}

// NewHandler creates a new Slack handler.
func NewHandler(cfg Config) *Handler {
	baseCtx := cfg.BaseContext
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	maxAsync := cfg.MaxConcurrentAsync
	if maxAsync <= 0 {
		maxAsync = defaultMaxConcurrentAsync
	}
	respClient := cfg.ResponseURLClient
	if respClient == nil {
		respClient = defaultResponseURLClient()
	}
	return &Handler{
		cfg:                   cfg,
		now:                   time.Now,
		baseCtx:               baseCtx,
		sem:                   make(chan struct{}, maxAsync),
		responseURLClient:     respClient,
		validateResponseURLFn: validateResponseURL,
	}
}

// Wait blocks until every async worker spawned by this handler has
// returned. Call it after http.Server.Shutdown so the process doesn't
// exit while a goroutine is still mid-POST to Slack's response_url.
//
// Wait is a no-op if no async work is in flight, so the cmd path can
// always call it on every shutdown without conditionals.
//
// In production, prefer WaitTimeout — an unbounded Wait() leaves the
// process exposed to a future regression where a worker ignores its
// ctx, wedging shutdown past the platform's hard-kill window.
func (h *Handler) Wait() {
	h.wg.Wait()
}

// WaitTimeout drains in-flight async workers, returning early after d.
// Returns true on clean drain; false on timeout (workers still in
// flight). cmd/main.go uses this so a misbehaving worker can't block
// graceful shutdown past the SIGTERM→SIGKILL window.
func (h *Handler) WaitTimeout(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// defaultResponseURLClient is the http.Client used to POST follow-up
// messages to Slack's response_url unless the caller injects one.
//
// CheckRedirect refusing redirects is load-bearing for the
// host-pinning posture: validateResponseURL only validates the URL the
// caller provided, so a 30x bounce from hooks.slack.com to any host
// would otherwise be silently followed (Go's default cap is 10 hops).
// Returning ErrUseLastResponse surfaces the 30x to the caller without
// dialing the redirected target.
func defaultResponseURLClient() *http.Client {
	return &http.Client{
		Timeout: responseURLTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
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

	// Exact-path match by design: Slack sends the canonical paths without
	// trailing slashes, and a strict match means a path-rewriting proxy
	// can't accidentally normalize "/slack/commands/" into a 404 silently
	// further upstream — it dies here in our routing instead. If we ever
	// front this with such a proxy, switch to strings.TrimRight or move
	// to http.ServeMux.
	switch r.URL.Path {
	case pathSlackCommands, pathSlackEvents, pathSlackInteractions:
		if r.Method != http.MethodPost {
			respondMethodNotAllowed(w, "POST")
			return
		}
	default:
		// Silent on 404 — and 405 on /slack/* takes the same path
		// (method gate above returns before the request log fires).
		// ALB target groups are reachable to internet probes
		// (/wp-login.php, /.env, credentialed scrapers GET-ing
		// /slack/commands); logging each would be noise. Slack and
		// health paths are the only legitimate surface and they get
		// their own log lines.
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
		// r.Context() is intentionally NOT threaded into the slash-command
		// dispatch: handleCreate/handleList spawn goroutines that outlive
		// the HTTP response, and r.Context() cancels as soon as ServeHTTP
		// returns. Async work uses h.baseCtx instead.
		h.handleSlashCommand(w, body)
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

func (h *Handler) handleSlashCommand(w http.ResponseWriter, body []byte) {
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
		h.handleCreate(w, values)
	case text == "list" || strings.HasPrefix(text, "list "):
		// Prefix-match `"list"` alone matched `listing`, `lists`,
		// `list-foo` — silently routing them to the list handler
		// where they'd ignore the trailing tokens. The exact-match
		// + trailing-space fork shows the user a "Unknown subcommand"
		// help nudge instead.
		h.handleList(w, values)
	default:
		// Surfaced to telemetry so a workspace using a stale slash-command
		// spec is visible in dashboards (rather than only via user reports).
		slog.Info("unknown slash subcommand", "command", command, "text", text)
		respondSlack(w, fmt.Sprintf("Unknown subcommand: `%s`. Try `/qurl help`.", text))
	}
}

func (h *Handler) handleCreate(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get("text"))
	targetURL := strings.TrimSpace(strings.TrimPrefix(text, "create "))

	if targetURL == "" {
		respondSlack(w, "Usage: `/qurl create <url>`")
		return
	}

	h.runAsync(w, "create", values, func(ctx context.Context, log *slog.Logger) {
		h.processCreate(ctx, log, values, targetURL)
	})
}

func (h *Handler) handleList(w http.ResponseWriter, values url.Values) {
	h.runAsync(w, "list", values, func(ctx context.Context, log *slog.Logger) {
		h.processList(ctx, log, values)
	})
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
	switch err := json.Unmarshal(body, &v); {
	case err != nil:
		// Bad JSON shouldn't 4xx (Slack retries on non-2xx). Surface
		// the parse error at Debug so spec drift / corrupt payloads
		// are visible to operators without breaking the contract.
		slog.Debug("event JSON parse failed", "error", err, "body_length", len(body))
	case v.Type == "url_verification":
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
