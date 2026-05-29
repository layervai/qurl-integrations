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
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	authFailureMessage       = "Failed to authenticate. Please check your qURL API key configuration."
	workspaceNotSetupMessage = "qURL isn't connected to this workspace yet. Run `/qurl setup` to connect it."
)

// ErrSlackTriggerExpired lets Config.OpenView report Slack's short-lived
// trigger_id expiry distinctly from auth, network, and Slack API failures.
var ErrSlackTriggerExpired = errors.New("slack trigger_id expired")

// ErrSlackRateLimited lets Config.OpenView surface Slack views.open rate
// limiting distinctly so the slash-command follow-up can give the operator a
// retry-shaped action instead of a generic setup failure.
var ErrSlackRateLimited = errors.New("slack views.open rate limited")

// SlackRateLimitError preserves Slack's Retry-After hint while still matching
// [ErrSlackRateLimited] through errors.Is. OpenView implementations return it
// when Slack includes a concrete retry delay.
type SlackRateLimitError struct {
	RetryAfter string
}

func (e *SlackRateLimitError) Error() string {
	if e == nil || e.RetryAfter == "" {
		return ErrSlackRateLimited.Error()
	}
	return fmt.Sprintf("%s: retry_after=%s", ErrSlackRateLimited, e.RetryAfter)
}

func (e *SlackRateLimitError) Unwrap() error {
	return ErrSlackRateLimited
}

// NewSlackRateLimitError wraps a non-empty Retry-After header; empty headers
// fall back to the sentinel so callers do not need separate branching.
func NewSlackRateLimitError(retryAfter string) error {
	retryAfter = strings.TrimSpace(retryAfter)
	if retryAfter == "" {
		return ErrSlackRateLimited
	}
	return &SlackRateLimitError{RetryAfter: retryAfter}
}

// OpenViewFunc posts a Slack modal through `views.open`.
type OpenViewFunc func(ctx context.Context, teamID, triggerID string, viewJSON []byte) error

// SlackRateLimitRetryAfter returns Slack's Retry-After hint from err when the
// OpenView implementation preserved one with [NewSlackRateLimitError].
func SlackRateLimitRetryAfter(err error) string {
	var rateLimitErr *SlackRateLimitError
	if errors.As(err, &rateLimitErr) && rateLimitErr != nil {
		return rateLimitErr.RetryAfter
	}
	return ""
}

// authErrorMessage maps an APIKey-lookup error to the right user-facing
// reply. The ErrWorkspaceNotConfigured sentinel is the "admin hasn't run
// /qurl setup yet" path — surface a useful next-action instead of the
// generic auth-failure string.
func authErrorMessage(err error) string {
	if errors.Is(err, auth.ErrWorkspaceNotConfigured) {
		return workspaceNotSetupMessage
	}
	return authFailureMessage
}

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

// Slash-command names. Both POST to pathSlackCommands with the same HMAC
// signature gate; Slack stamps which command the user invoked in the
// `command` form field, and handleSlashCommand dispatches on it. The
// admin command is a deploy prerequisite — it must be registered in the
// Slack app config pointing at the same request URL as commandUser, or
// these literals never arrive (see the package README / PR notes).
const (
	commandUser  = "/qurl"
	commandAdmin = "/qurl-admin"
)

// adminCommandSuffix is how every env names its admin slash command: the
// user command plus this suffix (`/qurl`→`/qurl-admin`,
// `/qurl-sandbox`→`/qurl-sandbox-admin`; see qurl-integrations-infra
// slack-manifests/envs.json). handleSlashCommand classifies on the suffix
// rather than the literal commandAdmin so a non-prod env whose commands
// carry an env infix still reaches the admin surface instead of falling
// through to the user one.
//
// SCOPE OF THE REWRITE: only the help text is rewritten to the invoked
// command name (userHelpMessage / adminHelpMessage, via ReplaceAll). Static
// error / usage / retry copy elsewhere (tunnelInstallUsage, the alias usage
// strings, modal-error and rate-limit messages, empty-state hints) bakes in
// the prod `/qurl-admin` / `/qurl` literal and is deliberately NOT rewritten.
// That drift is accepted: non-prod (env-infix) installs are operator-only
// internal sandboxes, where a retry hint naming the prod command is a
// non-issue, and customer installs are always prod where the literals are
// already correct. Threading the invoked command through every error site
// isn't worth the churn for that audience.
const adminCommandSuffix = "-admin"

// isAdminCommand reports whether the invoked slash command is the admin
// surface — any `/qurl-…-admin` command, not just the prod commandAdmin.
// Scoped to the `/qurl-` command family (so `/qurl-admin`,
// `/qurl-sandbox-admin` match but a stray `/qurlfoo-admin` or
// `/some-other-admin` doesn't) — a foreign command can't route to admin
// dispatch and then have userCommandName/help name a sibling that doesn't
// exist.
//
// Classification is best-effort and assumes Slack only dispatches the
// commands registered for this app (the `/qurl[-env][-admin]` family). An
// unregistered shape like `/qurl-admin-extra` (suffix `-extra`, not `-admin`)
// would fall through to the user surface and render its own name in help —
// harmless, because Slack never sends a command that wasn't registered.
func isAdminCommand(command string) bool {
	return strings.HasPrefix(command, commandUser+"-") && strings.HasSuffix(command, adminCommandSuffix)
}

// userCommandName / adminCommandName return the sibling command names for
// the invoked command, so wrong-surface redirects and help name the
// command that actually exists in this workspace (e.g.
// `/qurl-sandbox-admin`, not the prod `/qurl-admin`). Both are idempotent
// on their own surface: userCommandName(commandUser)==commandUser and
// adminCommandName(commandAdmin)==commandAdmin.
//
// The returned names are Slack-stamped command literals (a registered slash
// command, never free-form user input), so the redirect/unknown-subcommand
// replies interpolate them into backtick fences raw — only the user-typed
// verb text is run through echoText. That keeps the no-op hygiene off trusted
// literals; if Slack's command tokens ever stopped being backtick-free this
// assumption (documented on isAdminCommand) would need revisiting.
func userCommandName(command string) string {
	return strings.TrimSuffix(command, adminCommandSuffix)
}

func adminCommandName(command string) string {
	return userCommandName(command) + adminCommandSuffix
}

const (
	// defaultMaxConcurrentAsync caps in-flight goroutines. A Slack-side
	// flood (replay storm, runaway integration) drops with ackBusy past
	// this threshold rather than unbounded-spawning until the task OOMs.
	// Guided tunnel setup acks before its async admin check to preserve
	// Slack's trigger_id window, so keep this high enough that admin-check
	// retries cannot starve normal async replies. 50 is generous for
	// steady-state — the target customer (50 active users) won't sustain
	// >1 click/sec across the whole workspace.
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

// Config carries the runtime wiring for [NewHandler]. Every field is
// captured by value into [Handler.cfg] once and then read on the
// request hot path without synchronization — callers MUST NOT mutate
// the originating Config after the call. Wiring that has to be
// (re)set after NewHandler returns goes through a SetX setter with an
// explicit double-wire panic (see [Handler.SetAliasStore] /
// [Handler.SetOAuthSetup]); do not add a "swap PostDM / OpenView at
// runtime" path without that same posture.
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

	// AdminStore is the DDB-direct facade for workspace_mappings +
	// channel_policies. When nil, the admin verbs short-circuit to a
	// graceful "admin features are not configured" reply — fine for
	// sandbox / no-DDB tests. Production wires one in cmd/main.go
	// from the QURL_*_TABLE env vars (see slackdata.NewStore).
	AdminStore *slackdata.Store

	// OpenView posts a `views.open` Slack web API call to display a
	// modal in response to a slash command. The token owner parameter is
	// usually the workspace team_id; Enterprise Grid org installs can pass
	// enterprise_id instead while the modal metadata remains workspace-scoped.
	// Legacy single-workspace deploys can still fall back to one
	// SLACK_BOT_TOKEN. Tests inject a stub that records the call. Tunnel
	// install uses this for guided setup; setalias-rebind can use the same seam
	// for confirmation modals.
	OpenView OpenViewFunc

	// SlackInstallURL starts the Slack app install/reauthorization flow that
	// stores the per-workspace bot token used by OpenView. When set, guided
	// setup can give sandbox admins a direct recovery link instead of an
	// operator-only reinstall prompt.
	SlackInstallURL string

	// PostDM is the `chat.postMessage` web API for the `dm:true` flag
	// on `/qurl get`. Production wires this in cmd/main.go; tests
	// inject a stub. Empty (nil) on the production path until
	// cmd/main.go ships the bot-token plumbing; `/qurl get dm:true`
	// surfaces a friendly fallback in that case.
	PostDM func(ctx context.Context, slackUserID, text string) error

	// TunnelImage is the Docker image shown by `/qurl-admin tunnel install`.
	// Empty falls back to the public client image with the `latest` tag for
	// dev/sandbox installs; production deploys should set an immutable tag or
	// digest so Slack never instructs customers to run a floating image.
	TunnelImage string
}

// Handler processes Slack events and commands.
type Handler struct {
	cfg Config
	// now is injected so tests can pin the clock for timestamp-skew checks
	// without touching a package global. Defaults to time.Now.
	now func() time.Time
	// oauthSetup carries the runtime configuration the /qurl setup
	// slash-command needs to mint a state token and build the /start
	// URL. nil when the OAuth surface is not configured (sandbox /
	// missing env vars) — /qurl setup returns a "not configured"
	// ephemeral in that case rather than minting a useless link.
	oauthSetup *oauth.SetupConfig
	// aliasStore persists per-channel alias bindings for the
	// `/qurl-admin set-alias` / `/qurl-admin unset-alias` verbs. nil when not
	// configured (sandbox / pre-#231/#233 deploys) — handlers fail
	// fast with an operator-visible ephemeral rather than silently
	// dropping the write. See handler_alias.go for the interface
	// shape and the schema-gap rationale.
	aliasStore AliasStore
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

// SetAliasStore wires the per-channel alias persistence surface into
// the /qurl-admin set-alias / /qurl-admin unset-alias verbs. Must be called before
// `srv.Serve` — the field is read on the request hot path without
// synchronization, and the only safe write window is before any
// goroutine can observe it. The panic-on-double-wiring below catches
// accidental double-`SetAliasStore(realStore)` in init code; it is
// NOT a synchronization primitive, and calling this from a running
// handler is undefined regardless of the panic.
//
// Calling with nil is a no-op for the field (the verbs will reply
// with a "not configured" ephemeral) so cmd/main.go can omit the
// call on sandbox deploys that haven't onboarded the slackdata
// package yet. Both directions of the nil/non-nil sequence are
// allowed: a defensive `SetAliasStore(nil)` followed by a real
// wiring later is fine, and a real wiring followed by a defensive
// `SetAliasStore(nil)` is also a no-op (the real store stays wired).
// Calling with a non-nil store after the field is already non-nil
// panics, so the real store can't be silently swapped under a
// running handler.
func (h *Handler) SetAliasStore(store AliasStore) {
	if store == nil {
		return
	}
	if h.aliasStore != nil {
		panic("SetAliasStore called twice with a non-nil store — must be wired once before Serve")
	}
	h.aliasStore = store
}

// SetOAuthSetup wires the per-workspace OAuth configuration into the
// /qurl setup slash command. Must be called exactly once, before
// srv.Serve. Empty/short secret or empty base URL is a no-op
// (/qurl setup will reply that OAuth is not configured). A second call
// panics — the field is read without synchronization on the request
// hot path, and the only safe write window is before any goroutine can
// observe it.
func (h *Handler) SetOAuthSetup(cfg oauth.SetupConfig) {
	if h.oauthSetup != nil {
		panic("SetOAuthSetup called twice — must be called once before Serve")
	}
	if len(cfg.StateSecret) == 0 || cfg.SlackBaseURL == "" {
		return
	}
	if len(cfg.StateSecret) < oauth.StateMinSecret {
		// Fail-fast at startup: MintState would reject this later, but
		// the operator-facing failure is more discoverable here.
		panic("SetOAuthSetup: StateSecret shorter than oauth.StateMinSecret")
	}
	// Defensive copy: the field is read on the request hot path without
	// a lock. A caller mutating the original byte slice would silently
	// poison every subsequent MintState call.
	cfg.StateSecret = append([]byte(nil), cfg.StateSecret...)
	h.oauthSetup = &cfg
}

// SetSlackInstallURL wires the customer Slack install URL used to recover
// guided tunnel setup when a workspace has no stored bot token yet. Must be
// called before Serve. Empty is a no-op so deployments without Slack install
// OAuth keep the operator-directed fallback copy.
func (h *Handler) SetSlackInstallURL(installURL string) {
	installURL = strings.TrimSpace(installURL)
	if installURL == "" {
		return
	}
	h.cfg.SlackInstallURL = installURL
}

// NewHandler creates a new Slack handler. Config is intentionally
// passed by value rather than pointer despite gocritic's hugeParam
// warning: the call site is once at process startup (cmd/main.go)
// or once per t.Run in tests, so the copy is amortized to zero
// against the bot's lifetime. Pass-by-value keeps callers from
// mutating fields out from under the handler.
//
//nolint:gocritic // hugeParam: Config copied once per Handler at startup; pass-by-value is intentional.
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

// Compile-time check that *Handler still satisfies oauth.AsyncTracker
// after any future rename of Handler.Go — would break here rather
// than nil-tracker the OAuth callback's fire-and-forget revoke path
// at runtime.
var _ oauth.AsyncTracker = (*Handler)(nil)

// Go runs fn in a goroutine tracked by h.wg so the cmd shutdown drain
// covers it. Implements oauth.AsyncTracker — the OAuth callback's
// fire-and-forget DM and orphan-key revoke goroutines flow through
// here, putting them inside the same WaitTimeout budget as the
// slash-command async workers.
//
// Panics in fn are recovered with a stack-trace log so a buggy Slack
// client or qurl-service stub can't crash the bot. Mirrors the
// recover discipline in runAsync.
func (h *Handler) Go(fn func()) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in OAuth async goroutine",
					"recover", r,
					"stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}

// WaitTimeout drains in-flight async workers, returning early after d.
// Returns true on clean drain; false on timeout (workers still in
// flight). cmd/main.go uses this so a misbehaving worker can't block
// graceful shutdown past the SIGTERM→SIGKILL window.
//
// Note: on the timeout path the inner h.wg.Wait goroutine outlives
// this call until the underlying workers actually finish. This is fine
// in the cmd shutdown path (the process is exiting) but means
// WaitTimeout is NOT appropriate as a hot-path drain primitive — only
// use at end-of-life.
func (h *Handler) WaitTimeout(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
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
		// dispatch: handleGet/handleListResources spawn goroutines that outlive
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

func slashSubcommand(text, command string) bool {
	matched, _ := slashVerb(text, command)
	return matched
}

// adminVerbs are the leading verb words that belong to `/qurl-admin`.
// Used to redirect a user who typed an admin verb on `/qurl` and to
// classify the wrong-surface case. `set-alias`/`unset-alias` carry both
// spellings because slashVerb accepts the dash-free historical form too.
// `setup` is deliberately NOT here — it lives on `/qurl` (see handleSetup)
// so the first claimant of an unbound workspace can reach it.
//
// Adding an admin verb touches three places that must stay in sync: this
// list (wrong-surface classification), a dispatch case in
// dispatchAdminCommand, and — if it's user-facing — adminHelpMessage.
//
// Immutable: read-only on the request hot path (slashVerb ranges it); a
// var only because Go has no const slice. Do not mutate at runtime.
var adminVerbs = []string{"admin", "tunnel", "set-alias", "setalias", "unset-alias", "unsetalias", "set-display-name", "unset-display-name"}

// userVerbs are the leading verb words that belong to `/qurl`. Used to
// redirect a user who typed a user verb on `/qurl-admin`. `setup` is a
// user verb (first-come-claims; see handleSetup), so `/qurl-admin setup`
// redirects here to `/qurl setup`. Immutable like adminVerbs (see above).
var userVerbs = []string{"get", "list", "aliases", "create", "setup"}

// isAdminVerb reports whether text's leading verb is an admin verb.
func isAdminVerb(text string) bool {
	matched, _ := slashVerb(text, adminVerbs...)
	return matched
}

// isUserVerb reports whether text's leading verb is a user verb.
func isUserVerb(text string) bool {
	matched, _ := slashVerb(text, userVerbs...)
	return matched
}

// firstWord returns the first whitespace-delimited token of text, or ""
// when text has none. The wrong-surface redirects echo it as the verb
// word — `admin <action>` collapses to `admin` so the redirect reads
// `/qurl-admin admin list`, matching the retained sub-word grammar. It's
// reached only on already-classified verb text, so the token is a
// known-literal keyword; the redirects echoText-wrap it anyway to keep the
// inline-code-fence safety local rather than by chained invariant.
func firstWord(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// stripBackticks removes backticks from user-controlled text echoed into a
// Slack inline-code span (the wrong-surface and unknown-subcommand replies).
// A stray backtick in the echoed text would otherwise unbalance the `…` fence
// and render the ephemeral reply garbled. Rendering hygiene, not a security
// boundary — ephemerals are plain text, not markup-trusted.
func stripBackticks(s string) string {
	return strings.ReplaceAll(s, "`", "")
}

// maxEchoRunes caps how much user-typed command text the wrong-surface and
// unknown-subcommand replies echo back. The echo is a copy-paste convenience,
// not data; an unbounded paste would render an ungainly ephemeral (Slack also
// truncates server-side, but at a less predictable point). 200 runes
// comfortably fits any real command invocation.
const maxEchoRunes = 200

// echoText prepares user-controlled command text for echoing into a Slack
// inline-code span: it strips backticks (so a stray one can't unbalance the
// `…` fence) and caps the length (so an oversized paste renders predictably).
func echoText(s string) string {
	s = stripBackticks(s)
	// Byte length is an upper bound on rune count, so a string within the cap
	// by bytes is within it by runes too — skip the []rune conversion in the
	// common short case.
	if len(s) <= maxEchoRunes {
		return s
	}
	if r := []rune(s); len(r) > maxEchoRunes {
		return string(r[:maxEchoRunes]) + "…"
	}
	// Byte length exceeded the cap but rune count didn't (multi-byte text):
	// within budget by runes, so return as-is rather than truncate.
	return s
}

func slashVerb(text string, verbs ...string) (matched bool, rest string) {
	for _, verb := range verbs {
		if text == verb {
			return true, ""
		}
		if strings.HasPrefix(text, verb+" ") {
			return true, strings.TrimSpace(strings.TrimPrefix(text, verb))
		}
	}
	return false, text
}

func setAliasSubcommand(text string) bool {
	matched, _ := slashVerb(text, "setalias", "set-alias")
	return matched
}

func stripSetAliasPrefix(text string) string {
	_, rest := slashVerb(text, "setalias", "set-alias")
	return rest
}

func unsetAliasSubcommand(text string) bool {
	matched, _ := slashVerb(text, "unsetalias", "unset-alias")
	return matched
}

func stripUnsetAliasPrefix(text string) string {
	_, rest := slashVerb(text, "unsetalias", "unset-alias")
	return rest
}

func stripSetDisplayNamePrefix(text string) string {
	_, rest := slashVerb(text, "set-display-name")
	return rest
}

func stripUnsetDisplayNamePrefix(text string) string {
	_, rest := slashVerb(text, "unset-display-name")
	return rest
}

func (h *Handler) handleSlashCommand(w http.ResponseWriter, body []byte) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form body"})
		return
	}

	command := values.Get(fieldCommand)
	text := strings.TrimSpace(values.Get(fieldText))

	slog.Info("slash command", "command", command, "text", text)

	// Normalize an empty command (malformed/synthetic payload) to the prod
	// user literal once, here at the HTTP entry, so dispatch, help, and the
	// wrong-surface redirect copy downstream never have to guard for it.
	// Empty already routes to the user surface below (isAdminCommand("") is
	// false), so this only fixes the rendered command name, not the routing.
	if command == "" {
		command = commandUser
	}

	// Both the user command and the admin command POST to the same request
	// endpoint with the same HMAC gate; Slack stamps which one was invoked
	// in the `command` field. Branch on it FIRST so the user surface and
	// the admin surface stay cleanly separated: a verb typed on the wrong
	// command gets a "that's a user command" / "that's an admin command"
	// redirect rather than the bare "unknown subcommand" reply.
	//
	// Classification is by the `-admin` suffix (isAdminCommand), not the
	// literal commandAdmin, so a non-prod env whose commands carry an infix
	// (`/qurl-sandbox`, `/qurl-sandbox-admin`) routes admin verbs to the
	// admin surface too. Matching the literal `/qurl-admin` would send
	// `/qurl-sandbox-admin` down the user path, making every admin verb
	// unreachable in that env.
	if isAdminCommand(command) {
		h.dispatchAdminCommand(w, command, text, values)
	} else {
		// The user command and any unrecognized command land here.
		// Unrecognized is defensive — Slack only sends the commands the app
		// registers — and the user surface is the safe default (it never
		// mutates admin state). Cosmetic caveat: help/redirect copy echoes
		// the invoked command name, so an unrecognized `command` (e.g.
		// `/qurl-bogus`) would render help advertising `/qurl-bogus list`
		// etc. — names that don't exist. We can't distinguish a bogus
		// command from a valid non-prod env command (`/qurl-sandbox`)
		// without a registry, so this is left as-is; it's unreachable given
		// Slack only dispatches registered commands.
		h.dispatchUserCommand(w, command, text, values)
	}
}

// dispatchUserCommand routes the user-facing `/qurl` verbs: setup, get,
// list, aliases, help. Admin verbs typed on `/qurl` get a redirect to
// `/qurl-admin` instead of the generic unknown-subcommand reply, so an
// admin who fat-fingers the command gets a direct correction.
func (h *Handler) dispatchUserCommand(w http.ResponseWriter, command, text string, values url.Values) {
	switch {
	case text == "" || text == "help":
		respondSlack(w, h.userHelpMessage(command))
	case text == "setup":
		// setup is a `/qurl` verb, not admin-gated — first-come-claims;
		// see handleSetup for why it lives on the open user surface.
		h.handleSetup(w, values)
	case slashSubcommand(text, "create"):
		// `/qurl create` is deprecated. It minted for an arbitrary URL,
		// which Slack no longer does — `/qurl get` mints for a tunnel
		// `$slug` or a channel `$alias`. Surface a deprecation hint
		// instead of an "unknown subcommand" so existing users hitting
		// muscle memory get a direct redirect to the new shape.
		respondSlack(w, "`/qurl create` is no longer supported. Use `/qurl get <$slug|$alias>` instead — run `/qurl list` to see your tunnels.")
	case text == "list":
		// Exact match only: the looser `HasPrefix(text, "list")` form
		// matched `listing`, `lists`, `list-foo` (silently routing
		// them to the list handler) AND `list extra args` (which
		// processListResources ignores). Anything other than the
		// bare token falls through to the unknown-subcommand branch
		// and gets a help nudge.
		h.handleListResources(w, values)
	case slashSubcommand(text, "get"):
		// Exact-token boundary so `getter`, `get-foo` fall through
		// to the unknown-subcommand branch instead of silently
		// routing here. The parser then produces ErrEmptyResource
		// for a bare `get`.
		h.handleGet(w, values)
	case text == "aliases":
		h.handleAliases(w, values)
	case isAdminVerb(text):
		// An admin verb typed on `/qurl` — redirect to `/qurl-admin` rather
		// than the generic unknown reply. firstWord(text) is the classified
		// verb word; the full text is echoed (backticks stripped) so the
		// correction is copy-pasteable without a stray backtick unbalancing
		// the inline-code span in the ephemeral reply.
		adminCmd := adminCommandName(command)
		respondSlack(w, fmt.Sprintf("`%s` is an admin command. Use `%s %s` instead, or run `%s help`.", echoText(firstWord(text)), adminCmd, echoText(text), adminCmd))
	default:
		// Surfaced to telemetry so a workspace using a stale slash-command
		// spec is visible in dashboards (rather than only via user reports).
		slog.Info("unknown slash subcommand", "command", command, "text", text)
		respondSlack(w, fmt.Sprintf("Unknown subcommand: `%s`. Try `%s help`.", echoText(text), command))
	}
}

// dispatchAdminCommand routes the admin-facing `/qurl-admin` verbs:
// tunnel install, set-alias, unset-alias, set-display-name,
// unset-display-name, admin add/remove/list/revoke, and help. User verbs
// typed on `/qurl-admin` — including `setup`, which
// is a `/qurl` verb (first-come-claims; see handleSetup) — get a redirect
// to `/qurl` so a user who fat-fingers the command gets a direct
// correction.
//
// The whole command is admin-scoped, so the historical `admin` sub-word
// is retained on the membership verbs (`/qurl-admin admin list` etc.):
// `list` already means "list the channel's tunnels" on `/qurl list`, and
// keeping `admin list` distinct from that avoids two different "list"
// surfaces reading the same. The verb-specific handlers and parser are
// unchanged — they still see `admin <action>` text.
func (h *Handler) dispatchAdminCommand(w http.ResponseWriter, command, text string, values url.Values) {
	// Verb-match order is defensive, not load-bearing today: the
	// admin/tunnel/alias sub-word matches come before the isUserVerb
	// fall-through. slashVerb requires an exact token or a `verb ` prefix,
	// so `admin list` doesn't match the user verb `list` regardless of
	// order. Keeping admin matches first guards against a FUTURE user verb
	// that would collide as the leading token of an admin sub-word grammar.
	switch {
	case text == "" || text == "help":
		// help is intentionally NOT admin-gated, unlike every verb below it:
		// it's discovery, not a privileged action. Gating it would obscure
		// (a non-admin couldn't learn what exists) rather than protect — the
		// roster it renders is the same public grammar carried in the user
		// surface's `/qurl-admin help` pointer. The actual admin verbs each
		// gate in their own handler (requireAdminSync).
		respondSlack(w, h.adminHelpMessage(command))
	case slashSubcommand(text, "admin"):
		// All admin membership verbs (revoke / add / remove / list)
		// route through the parse-then-dispatch handler. The retired
		// `admin claim` verb surfaces as ErrUnknownAdminAction from the
		// parser, so the user gets a helpful "unknown admin action"
		// reply rather than a stale modal opener. Bare `admin` lands
		// here so the parser emits the `missing admin action` error.
		h.handleAdmin(w, values)
	case slashSubcommand(text, "tunnel"):
		h.handleTunnel(w, values)
	case setAliasSubcommand(text):
		// Bare `set-alias` falls through too — parseAliasArgs renders
		// the usage hint, so the user gets the right grammar without
		// a separate "missing args" branch here.
		h.handleSetAlias(w, values)
	case unsetAliasSubcommand(text):
		h.handleUnsetAlias(w, values)
	// Use slashSubcommand directly here (unlike set-alias's dedicated
	// helper): the verb has a single canonical spelling, and the
	// cross-repo dispatcher-drift check (qurl-integrations-infra) only
	// extracts the slashSubcommand and …AliasSubcommand case shapes — a
	// …DisplayNameSubcommand helper would be invisible to it and keep the
	// infra manifest drift check red even after this merges.
	case slashSubcommand(text, "set-display-name"):
		// Bare `set-display-name` falls through too — the handler renders
		// the usage hint, so the user gets the right grammar without a
		// separate "missing args" branch here.
		h.handleSetDisplayName(w, values)
	case slashSubcommand(text, "unset-display-name"):
		h.handleUnsetDisplayName(w, values)
	case isUserVerb(text):
		// A user verb typed on the admin command — redirect to the user one.
		// Echoed text has backticks stripped (see the /qurl-side redirect).
		userCmd := userCommandName(command)
		respondSlack(w, fmt.Sprintf("`%s` belongs on `%s`. Use `%s %s` instead, or run `%s help`.", echoText(firstWord(text)), userCmd, userCmd, echoText(text), userCmd))
	default:
		slog.Info("unknown admin slash subcommand", "command", command, "text", text)
		respondSlack(w, fmt.Sprintf("Unknown admin subcommand: `%s`. Try `%s help`.", echoText(text), command))
	}
}

// handleSetup mints a workspace-bound state token and replies with the
// /oauth/qurl/start URL. team_id + user_id come from the Slack form
// payload, which has already passed signing-secret verification — that
// chain is what binds workspace identity to the resulting state token
// (the alternative, taking team_id from an unsigned query param at
// /start, was the workspace-rebind primitive flagged in PR review).
//
// Surface: setup is a `/qurl` (user) verb, NOT on the admin `/qurl-admin`
// command. qURL is first-come-claims — on an unbound workspace the first
// user to complete setup becomes its owner — so the command must be
// reachable by any workspace member. Putting it on the admin-restricted
// `/qurl-admin` command would lock out the very first claimant, who is by
// definition not yet an admin of anything.
//
// It is still owner-gated against *rebind*: on fresh install (no
// workspace_mappings row) any workspace user may run /setup and becomes
// the workspace owner. First install is first-user-wins: if two members
// race an unbound workspace, BindWorkspace's consistent read picks the
// single winner (the loser gets the rebind-refused page). On subsequent
// runs only the owner is permitted; other workspace members (including
// admins added via `/qurl-admin admin add`) get an "owner-only" reply.
// Without this gate, any added admin could complete OAuth against their
// own Auth0 account and silently rotate the workspace's qURL credential —
// the OAuth callback's BindWorkspace pre-flight (see oauth.checkBindAllowed)
// also rejects that case as a defense in depth, but gating here means
// non-owners don't get a setup URL minted in their name at all (cleaner
// audit, no half-completed OAuth flows).
//
// AdminStore=nil (sandbox / no-DDB) skips the owner gate — same posture
// as every other admin verb; the caller falls through to mint as on a
// fresh install. That is a separate short-circuit from the oauthSetup==nil
// check below, which is the branch that returns "qURL OAuth is not
// configured" (and which fires first, before AdminStore is consulted).
func (h *Handler) handleSetup(w http.ResponseWriter, values url.Values) {
	if h.oauthSetup == nil {
		respondSlack(w, "qURL OAuth is not configured on this Slack bot deployment. Contact the operator.")
		return
	}
	teamID := strings.TrimSpace(values.Get(fieldTeamID))
	userID := strings.TrimSpace(values.Get(fieldUserID))
	if teamID == "" || userID == "" {
		respondSlack(w, "Could not read your Slack workspace or user ID from the command payload.")
		return
	}
	// Owner gate. AdminStore==nil skips entirely (sandbox/no-DDB);
	// otherwise check whether the workspace has an owner and whether
	// it's the invoking user. CheckAdmin returns (isAdmin, ownerID,
	// err); we only consume ownerID here — the admin-set membership
	// is irrelevant for /setup specifically (added admins can't rerun
	// /setup, only the owner can). Times the read off h.baseCtx (not the
	// request ctx) so a Slack-side connection-close can't truncate the
	// gate read mid-flight; adminGateBudget is the only bound — same
	// posture as requireAdminSync.
	if h.cfg.AdminStore != nil {
		gateCtx, gateCancel := context.WithTimeout(h.baseCtx, adminGateBudget)
		defer gateCancel()
		_, ownerID, err := h.cfg.AdminStore.CheckAdmin(gateCtx, teamID, userID)
		if err != nil {
			slog.Error("/qurl setup: owner check failed", "error", err, "team_id", teamID, "caller_user_id", userID)
			respondSlack(w, ":warning: could not verify who connected qURL to this workspace (upstream error; see logs). Try again in a moment.")
			return
		}
		// ownerID=="" → workspace not yet bound → fresh install, allow.
		// (CheckAdmin reads eventually-consistent, and BindWorkspace
		// validates OwnerID != "" before PutItem, so an empty ownerID
		// almost always means "no row yet" rather than a half-written
		// one.) The one exception is a manually-edited row left with a
		// blank owner_id: it also reads as "" here and slips past this
		// gate, but BindWorkspace's consistent check refuses it (the
		// caller lands on the rebind-refused page after the OAuth round-
		// trip, and the empty-owner Warn there flags the bad row). That
		// requires DDB tampering, so it isn't worth a second read to
		// distinguish at the gate.
		// ownerID==userID → idempotent rerun by owner (rotates the
		// API key on OAuth-callback success), allow.
		// otherwise → non-owner rebind attempt, refuse here so we
		// don't even mint the state token / setup URL.
		//
		// This gate is best-effort: in the brief eventual-read window
		// after a fresh bind a fast second-mover could still see "" and
		// get a setup URL, but BindWorkspace's consistent owner check is
		// the structural backstop — that caller just lands on the
		// generic rebind-refused page instead of the friendly copy here.
		// The eventual read is deliberate, not an oversight: CheckAdmin
		// is the shared admin-gate read (same call the admin verbs make),
		// so the race only ever costs the loser a less-friendly error
		// page — never security, since the consistent backstop is
		// authoritative. Upgrading just this caller to a consistent read
		// would spend 2x RCU on every /setup to improve one racer's copy.
		if ownerID != "" && ownerID != userID {
			// Shape-guard the stored owner_id before interpolating it
			// into a `<@%s>` mention. BindWorkspace writes owner_id
			// from the OAuth callback (a different code path than the
			// parser), and a pre-pivot row holds an Auth0 sub, not a
			// Slack ID. Mirrors the looksLikeSlackUserID guard in
			// handleAdminList so a malformed value can't break out of
			// the mention surface.
			if looksLikeSlackUserID(ownerID) {
				slog.Warn("/qurl setup: rebind refused at slash-command gate — caller is not the workspace owner", "team_id", teamID, "caller_user_id", userID, "owner_user_id", ownerID)
				respondSlack(w, fmt.Sprintf("`/qurl setup` can only be re-run by the person who first connected qURL to this workspace (<@%s>). This stops anyone else from re-pointing it at a different qURL account, so ask them to re-run it. For admin tasks that don't need re-connecting, use the `/qurl-admin` commands.", ownerID))
				return
			}
			// Shape-bad owner_id → a pre-pivot Auth0 sub left behind by
			// the #510 owner-model migration. No Slack user can ever
			// match it, so this workspace is locked for everyone unless
			// we let setup recover it. DON'T dead-end here — fall through
			// to mint the setup URL; BindWorkspace self-heals on the
			// callback by reclaiming the orphaned row for this caller
			// (first-come-claims, the same posture as an unbound
			// workspace). Log loudly so the legacy reclaim is grep-able.
			slog.Warn("/qurl setup: stored owner_id is shape-bad (likely a pre-pivot Auth0 sub) — allowing setup to reclaim the legacy row", "team_id", teamID, "caller_user_id", userID, "legacy_owner_prefix", slackdata.LegacyOwnerPrefix(ownerID), "owner_id_len", len(ownerID))
		}
	}
	state, err := oauth.MintState(h.oauthSetup.StateSecret, teamID, userID, h.now())
	if err != nil {
		slog.Error("/qurl setup: MintState failed", "error", err)
		respondSlack(w, "Could not generate setup link. Please try again or contact support.")
		return
	}
	setupURL := h.oauthSetup.SetupURL(state)
	respondSlack(w, "Click to connect qURL to your Slack workspace: <"+setupURL+"|Connect qURL>\n\nThis link is valid for 5 minutes and only works for you.")
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

// userHelpMessage renders the `/qurl help` text — the user-facing verbs
// only. Verbs that depend on optional Config wiring are omitted when that
// wiring is nil — a workspace without PostDM won't see the dm:true
// variant. The verbs still dispatch if a user types them directly; the
// omission is just so help text doesn't advertise a path that will reply
// with ":warning: not configured". The admin verbs live on `/qurl-admin`
// (see [Handler.adminHelpMessage]); a pointer line routes admins there.
func (h *Handler) userHelpMessage(command string) string {
	// command is non-empty here — handleSlashCommand normalizes an empty
	// payload to commandUser before dispatch — so the ReplaceAll below
	// always has a non-empty base (an empty base would strip every `/qurl`).
	lines := []string{
		"*/qurl* — Create and manage qURLs from Slack",
		"",
		"*Commands:*",
	}
	// setup is a user verb (first-come-claims), so it leads the user
	// surface. The owner semantics only exist when AdminStore is wired; on
	// the sandbox/no-DDB path the owner gate in handleSetup is skipped, so
	// re-running /setup just mints again. Append the owner parenthetical
	// only there so the help text matches the deployment's actual behavior.
	setupLine := "• `/qurl setup` — Connect qURL to your Slack workspace"
	if h.cfg.AdminStore != nil {
		setupLine += " (whoever first runs it is the only one who can re-run it — this keeps the workspace's qURL account from being switched to someone else)"
	}
	lines = append(lines, setupLine)
	if h.cfg.AdminStore != nil {
		// Glossary so the `$slug` / `$alias` tokens in the verbs and in
		// `/qurl list` aren't unexplained. Only shown when AdminStore is
		// wired — that's the only deploy where aliases exist.
		//
		// get resolves its $slug/$alias token through resolveTokenForGet,
		// which fails closed (":warning: not configured") when AdminStore is
		// nil — the URL form that once let get work without DDB is gone
		// post-tunnels-only. Gate the get verbs on AdminStore so help never
		// advertises a verb whose only reply would be the not-configured
		// error (same rule as `/qurl aliases` below).
		lines = append(lines,
			"_A tunnel's `$slug` is its name. A `$alias` is an alternate name for a tunnel in a channel — several aliases can point to one slug. Use either with `/qurl get`._",
			"",
			"• `/qurl get <$slug|$alias>` — Mint a one-time qURL for a tunnel `$slug` or a `$alias` configured in this channel",
		)
		if h.cfg.PostDM != nil {
			lines = append(lines, "• `/qurl get <$slug|$alias> dm:true` — DM the link to you instead of posting it in-channel")
		}
		lines = append(lines,
			"• `/qurl get <$slug|$alias> reason:\"…\"` — Mint a one-time qURL, recording a reason in the audit log",
		)
	}
	lines = append(lines,
		"• `/qurl list` — List the tunnels available to you",
	)
	if h.cfg.AdminStore != nil {
		// aliases reads channel_policies through the AdminStore (NOT the
		// aliasStore that set-alias/unset-alias write through), so it
		// gates on AdminStore to match processAliases's own nil-check —
		// otherwise help could advertise `/qurl aliases` on a deploy where
		// it replies ":warning: not configured".
		lines = append(lines,
			"• `/qurl aliases` — List this channel's aliases and the tunnel each one points to",
		)
	}
	lines = append(lines,
		"• `/qurl help` — Show this help message",
		"",
		"Admins: run `/qurl-admin help` for tunnel install, alias, and admin commands.",
	)
	// The lines are authored with the prod command names (`/qurl`,
	// `/qurl-admin`). Rewrite the `/qurl` prefix to the invoked user
	// command so a non-prod env renders its own names — and because every
	// admin literal here is `/qurl-admin` == `/qurl` + adminCommandSuffix,
	// the same replace also fixes the admin pointer line
	// (`/qurl-sandbox` → `/qurl-sandbox-admin help`). command is the user
	// command on this surface, so the replacement is a no-op in prod.
	//
	// MAINTAINER INVARIANT: ReplaceAll is blind, so every `/qurl` substring
	// in `lines` must be a command literal — keep non-command prose (URLs
	// like `qurl.link`, `/qurl-foo` examples) free of the lowercase `/qurl`
	// token, or a non-prod env rewrites them too.
	// TestHelpMessagesContainOnlyCommandTokens guards this: a stray
	// non-command slash token fails there.
	return strings.ReplaceAll(strings.Join(lines, "\n"), commandUser, command)
}

// adminHelpMessage renders the `/qurl-admin help` text — the admin-gated
// verbs only (setup is a user verb and lives in [Handler.userHelpMessage]).
// The conditional gating mirrors what each verb actually does at runtime —
// a verb whose only reply would be ":warning: not configured" (aliasStore,
// AdminStore, OpenView all nil on sandbox deploys) is omitted so help never
// advertises a path the user can't take. These commands are admin-only,
// enforced in code: every admin verb runs requireAdminSync against the
// qURL admin set (see handleSetAlias). The `/qurl-admin` registration
// should also be marked admin-only in the Slack app config, but that is a
// cosmetic picker hint — Slack does not gate slash-command invocation on
// workspace-admin role — not the enforcement boundary.
func (h *Handler) adminHelpMessage(command string) string {
	// command is non-empty here (normalized in handleSlashCommand); see
	// userHelpMessage for why the ReplaceAll below needs a non-empty base.
	lines := []string{
		"*/qurl-admin* — Admin commands for qURL in Slack",
		"",
		"*Admin commands:*",
	}
	if h.aliasStore != nil && h.cfg.AdminStore != nil {
		if h.cfg.OpenView != nil {
			lines = append(lines,
				"• `/qurl-admin tunnel install` — Guided tunnel setup for Docker, Docker Compose, ECS Fargate, or Kubernetes (admin only)",
				"  Guided setup is enabled in this workspace; use bare `/qurl-admin tunnel install` to choose a target environment.",
				"• `/qurl-admin tunnel install <slug> [env:...] [port:8080] [alias:$alias]` — Typed tunnel setup; creates a bootstrap key and binds `$<slug>` in this channel",
				"• Typed tunnel options: `env:docker|docker-compose|ecs-fargate|kubernetes`; Docker accepts `container:<name>` or `web_container:<name>`; Compose accepts `service:<name>`; `env:compose` also works",
			)
		} else {
			lines = append(lines,
				"• `/qurl-admin tunnel install <slug>` — Create a Docker sidecar bootstrap key and bind `$<slug>` in this channel (admin only)",
				"  Guided setup is not enabled in this deployment; use the typed installer form.",
			)
		}
	}
	if h.aliasStore != nil {
		// set-alias/unset-alias reply ":warning: not configured" on a
		// sandbox deploy without an aliasStore; mirror the PostDM gate
		// above so help doesn't advertise verbs whose reply tells the
		// user they can't be used. User-facing copy calls these
		// "aliases" (not "shortcuts") even though the admin verbs retain
		// their historical set-alias/unset-alias names.
		//
		// Gates on aliasStore (the store set-alias/unset-alias WRITE
		// through). At runtime these verbs ALSO need AdminStore for the
		// in-code requireAdminSync gate (see handleSetAlias), so aliasStore
		// isn't the verb's only dependency — but aliasStore and AdminStore
		// are wired in lockstep (both come from the same QURL_*_TABLE env
		// vars; see cmd/main.go), so an aliasStore-wired-but-AdminStore-nil
		// deploy doesn't arise and gating here on aliasStore is equivalent
		// to gating on both. `/qurl aliases` above gates on AdminStore
		// because it READS channel_policies through it.
		lines = append(lines,
			"• `/qurl-admin set-alias $<alias> $<slug>` — Point an alias at a tunnel slug in this channel (admin only)",
			"• `/qurl-admin unset-alias $<alias>` — Remove an alias from this channel (admin only)",
		)
	}
	if h.cfg.AdminStore != nil {
		// Every verb below gates on the in-code requireAdminSync (CheckAdmin
		// against AdminStore), so they're listed only when AdminStore is
		// wired — the same condition the verbs use at runtime. On sandbox
		// deploys without the three QURL_*_TABLE env vars (see cmd/main.go),
		// AdminStore is nil and these verbs render "Admin features are not
		// configured", so gating the help lines on the same condition keeps
		// the listing consistent with what the verbs actually do.
		//
		// set-display-name / unset-display-name set a friendly Display Name
		// on a tunnel id (the `$<slug>` shown by `/qurl list`).
		lines = append(lines,
			"• `/qurl-admin set-display-name <id> <display name>` — Set a tunnel's friendly Display Name shown in `/qurl list` (admin only)",
			"• `/qurl-admin unset-display-name <id>` — Reset a tunnel's Display Name to the default (admin only)",
			"• `/qurl-admin admin add @user` — Promote a Slack user to bot admin (admin only)",
			"• `/qurl-admin admin remove @user` — Demote a Slack user from bot admin (admin only)",
			"• `/qurl-admin admin list` — List who connected qURL (the owner) and the current bot admins (admin only)",
			"• `/qurl-admin admin revoke <qurl_id>` — Revoke a single qURL (admin only)",
		)
	}
	// Always-present anchor: the optional blocks above are all gated on
	// sandbox wiring, so without this line a no-store deploy would render
	// just the header with no verbs. Mirrors the `/qurl help` line on the
	// user surface.
	lines = append(lines, "• `/qurl-admin help` — Show this help message")
	// Authored with the prod admin command name; rewrite to the invoked
	// admin command so a non-prod env renders its own (`/qurl-sandbox-admin`
	// …). Every admin literal here is the full `/qurl-admin`, so a single
	// replace covers them all; command is the admin command on this
	// surface, so the replacement is a no-op in prod.
	return strings.ReplaceAll(strings.Join(lines, "\n"), commandAdmin, command)
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

// Slack slash-command response keys + the ephemeral response-type value.
// Centralized so respondSlack and the parallel writer in postResponse
// can't drift, and so the goconst/keyword consistency stays linter-clean.
const (
	respFieldResponseType = "response_type"
	respFieldText         = "text"
	respTypeEphemeral     = "ephemeral"
)

func respondSlack(w http.ResponseWriter, text string) {
	respondJSON(w, http.StatusOK, map[string]string{
		respFieldResponseType: respTypeEphemeral,
		respFieldText:         text,
	})
}
