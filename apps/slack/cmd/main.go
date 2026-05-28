// Package main is the HTTP entrypoint for the Slack integration.
//
// Runs as a long-lived process behind an ALB on Fargate. Listens on
// :8080, terminates gracefully on SIGTERM (Fargate's task-stop signal,
// sent 30s before SIGKILL).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	listenAddr = ":8080"
	// shutdownTimeout sits inside Fargate's 30s SIGTERM→SIGKILL window with
	// 5s of headroom for the container runtime to actually deliver SIGKILL
	// and reap the process. This is the cap on the drain *as a whole*, not
	// per request — http.Server.WriteTimeout (75s) still bounds individual
	// in-flight handlers; bumping shutdownTimeout above 25s won't extend
	// long-running handlers, only the wait for short ones to drain.
	//
	// Interaction with oauthHandlerTimeout (60s): a SIGTERM landing
	// mid-OAuth-callback means the request handler's TimeoutHandler
	// deadline could exceed our 25s drain budget. srv.Shutdown returns
	// when handlers ack-and-return, which for an OAuth callback is
	// "when the success page is written". A callback in-flight at
	// SIGTERM is allowed to complete its (potentially 60s) work via
	// the WriteTimeout=75s connection budget, but the *drain wait* is
	// capped at 25s. If the callback exceeds 25s, srv.Shutdown
	// returns early, the OS later SIGKILLs, and the OAuth response
	// the user sees is the partial mid-write state. Trade-off:
	// raising shutdownTimeout past Fargate's 30s window doesn't help;
	// the operator-facing fix is to drain the listener and rely on
	// the ALB to stop sending new requests before SIGTERM.
	//
	// Budget contract: shutdownTimeout is sized against the EXPECTED
	// drain, not the theoretical max. The expected drain is
	// dominated by:
	//   srv.Shutdown (≈0ms — handlers ack and return instantly)
	//   + qURL call returns ctx.Canceled almost instantly on signal
	//   + responseURLTimeout (5s) for the failure follow-up POST
	//
	// The theoretical max is asyncWorkTimeout (25s) + 5s = 30s,
	// which would wedge if a worker ignored ctx — that's why
	// handler.WaitTimeout caps the wait at the remaining budget
	// rather than calling Wait. The 25s default leaves comfortable
	// headroom for the expected case while staying well inside
	// Fargate's 30s SIGTERM→SIGKILL window.
	shutdownTimeout = 25 * time.Second
	// maxHeaderBytes is well above Slack's realistic header size (sig +
	// timestamp + standard headers fit comfortably in 2 KiB) but bounds
	// the per-connection memory an attacker can force pre-handler.
	maxHeaderBytes = 8 << 10 // 8 KiB
	// Slack remains the token-validity authority; these bounds are only a local
	// boot-time typo guard for obviously truncated or pasted-wrong values.
	slackBotTokenMinLen = 50
	slackBotTokenMaxLen = 320
)

// version is set at build time via `-ldflags "-X main.version=<sha>"`.
// Used in the qURL client User-Agent so server-side traces can pin a
// failure to a specific bot release.
var version = "dev"

func main() {
	// JSON handler is load-bearing for log-injection safety: the G706
	// gosec suppressions in apps/slack/internal/handler.go assume slog's
	// JSON output escapes control characters in tainted attribute
	// values. Don't swap to TextHandler without revisiting those sites.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// run holds the full server lifecycle so `defer stop()` releases the
// signal handler before main reaches os.Exit on the error path.
func run() error {
	// Required env vars are explicit by design: a missing QURL_ENDPOINT
	// previously fell back to the sandbox URL, which is the kind of silent
	// misconfiguration that ships a prod deploy at sandbox.
	qurlEndpoint := os.Getenv("QURL_ENDPOINT")
	if qurlEndpoint == "" {
		return errors.New("QURL_ENDPOINT is required")
	}

	slackSigningSecret := os.Getenv("SLACK_SIGNING_SECRET")
	if slackSigningSecret == "" {
		return errors.New("SLACK_SIGNING_SECRET is required")
	}

	// DDBProvider reads the workspace_state table populated by
	// /oauth/qurl/callback. Missing WORKSPACE_STATE_TABLE fails
	// startup distinctly from an empty table.
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	ddbProvider, err := auth.NewDDBProvider(signalCtx,
		auth.WithTableName(os.Getenv("WORKSPACE_STATE_TABLE")),
		auth.WithKMSKeyARN(os.Getenv("WORKSPACE_STATE_KMS_KEY_ARN")),
	)
	if err != nil {
		return fmt.Errorf("DDBProvider init: %w", err)
	}
	var authProvider auth.Provider = ddbProvider
	userAgent := "qurl-slack/" + version

	maxConcurrentAsync := readMaxConcurrentAsync()
	adminStore := buildAdminStore(signalCtx)
	tunnelImage := strings.TrimSpace(os.Getenv("QURL_TUNNEL_IMAGE"))
	if err := internal.ValidateTunnelImageRef(tunnelImage); err != nil {
		return fmt.Errorf("QURL_TUNNEL_IMAGE: %w", err)
	}
	slackBotToken := strings.TrimSpace(os.Getenv("SLACK_BOT_TOKEN"))
	var openView internal.OpenViewFunc
	if err := validateSlackBotToken(slackBotToken); err != nil {
		return err
	}
	if slackBotToken != "" {
		openView = slackOpenViewFunc(slackBotToken, userAgent)
	} else {
		slog.Info("Slack views.open disabled", "reason", "slack_bot_token_unset")
	}

	// signalCtx is hoisted above so the DDB-provider constructor can
	// observe shutdown during AWS config load. It feeds two seams: the
	// main goroutine (to detect SIGTERM and trigger srv.Shutdown) and
	// Handler.BaseContext (so async slash-command workers observe
	// cancellation through the same signal). Threading the same ctx
	// into both keeps the shutdown story coherent — a worker mid-POST
	// to response_url receives ctx.Canceled at the same instant
	// Shutdown starts refusing new connections.

	handler := internal.NewHandler(internal.Config{
		AuthProvider:       authProvider,
		SlackSigningSecret: slackSigningSecret,
		BaseContext:        signalCtx,
		MaxConcurrentAsync: maxConcurrentAsync,
		AdminStore:         adminStore,
		OpenView:           openView,
		TunnelImage:        tunnelImage,
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlEndpoint, apiKey,
				// The async worker has up to 25s before its context fires;
				// the qURL client may retry transient 429/5xx within that
				// budget. WithRetry(2) gives two retries with the existing
				// exponential-backoff schedule before bubbling to the user.
				client.WithUserAgent(userAgent),
				client.WithRetry(2),
			)
		},
	})

	// Alias reads and writes must go through the same slackdata facade so
	// `/qurl tunnel install` can create the resource, bind `$slug`, and let
	// users immediately `/qurl get $slug` against the same table shape.
	if adminStore != nil {
		handler.SetAliasStore(adminStore)
		slog.Info("alias storage wired via slackdata")
	} else {
		slog.Info("alias storage disabled", "reason", "admin_store_unconfigured")
	}

	// Compose the top-level mux: existing Slack-bot routes (handled
	// by the internal.Handler.ServeHTTP fall-through) + new OAuth
	// routes wired into a sibling ServeMux. The mux is what serves;
	// the internal handler stays unchanged.
	rootMux := http.NewServeMux()
	rootMux.Handle("/", handler)
	// Route the callback's fire-and-forget goroutines through handler.wg
	// so they fall inside the same shutdown drain budget as the
	// slash-command async workers.
	var oauthAdminStore oauth.AdminStore
	if adminStore != nil {
		oauthAdminStore = &adminStoreAdapter{store: adminStore}
	}
	oauthCfg, ok, err := buildOAuthConfig(signalCtx, ddbProvider, handler, oauthAdminStore)
	if err != nil {
		return fmt.Errorf("OAuth config: %w", err)
	}
	if ok {
		oauth.RegisterRoutes(rootMux, oauthCfg)
		slog.Info("registered /oauth/qurl/{start,callback} routes")
		handler.SetOAuthSetup(oauth.SetupConfig{
			StateSecret:  oauthCfg.OAuthStateSecret,
			SlackBaseURL: oauthCfg.SlackBaseURL,
		})
		// Operator reminder: /qurl setup runs whichever Slack user
		// invokes it. Workspace identity comes from the signature-
		// verified payload, but ROLE is not enforced in this binary.
		// The Slack app manifest must restrict /qurl setup to
		// workspace admins; without that gate, any user could
		// initiate a flow that overwrites the workspace's qURL key
		// against their own Auth0 account.
		slog.Info("CONFIGURATION REMINDER: /qurl setup is workspace-admin-only by manifest, not by code — verify the Slack app manifest restricts the command")
	}
	// Else: buildOAuthConfig already logged the specific missing-var
	// list; nothing more to say here.

	srv := &http.Server{
		// Addr intentionally omitted: srv.Serve(ln) ignores it, and we
		// bind via net.ListenConfig below. Setting it would mislead a
		// future reader into thinking it controls the bind.
		Handler:           rootMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// WriteTimeout is the *connection* deadline; http.TimeoutHandler
		// only cancels the request context, it doesn't lift this. The
		// /oauth/qurl/callback handler's worst-case sum (Auth0 token
		// exchange + qurl-service mint + KMS GenerateDataKey + DDB
		// PutItem) approaches 30s, and oauthHandlerTimeout caps the
		// per-handler budget at 60s. WriteTimeout must exceed that cap
		// so the per-handler deadline reliably fires first and produces
		// the friendly "oauth/callback timed out" body rather than a
		// torn connection. 75s = 60 + 15s headroom for response write +
		// keep-alive close. /slack/* and /health respond in
		// milliseconds, so the bump doesn't change their posture.
		WriteTimeout:   75 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: maxHeaderBytes,
	}

	// Bind first so a port-already-in-use failure returns before the
	// drain goroutine spawns — keeps the "received shutdown signal"
	// log line off the bind-failure path. Use a fresh background ctx
	// for the bind so a SIGTERM arriving in the gap between
	// signal.NotifyContext and Listen doesn't surface as
	// "listen: context canceled".
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", listenAddr, err)
	}

	shutdownDone := make(chan struct{})
	go func() {
		<-signalCtx.Done()
		slog.Info("received shutdown signal — draining HTTP server")
		shutdownStart := time.Now()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "error", err)
		}
		// Shutdown returns once HTTP handlers have responded. Slash-
		// command handlers ack and return nearly instantly, so
		// in-flight async workers (the goroutines runAsync spawned)
		// outlive Shutdown. WaitTimeout drains them within whatever
		// of shutdownTimeout remains — a misbehaving worker that
		// ignored its ctx can't wedge the process past Fargate's
		// hard kill.
		drainBudget := shutdownTimeout - time.Since(shutdownStart)
		if drainBudget < 0 {
			drainBudget = 0
		}
		if !handler.WaitTimeout(drainBudget) {
			slog.Warn("async drain timed out — exiting with workers still in flight", "budget", drainBudget)
		}
		close(shutdownDone)
	}()

	slog.Info("starting Slack bot HTTP server", "addr", listenAddr)
	serveErr := srv.Serve(ln)

	// Always release the signal handler and wait for the drain goroutine
	// regardless of how Serve returned — keeps the cleanup deterministic
	// even if Serve fails with a non-ErrServerClosed error.
	stop()
	<-shutdownDone

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", serveErr)
	}
	slog.Info("server stopped cleanly")
	return nil
}

// minStateSecretBytes is the operator floor for OAUTH_STATE_SECRET.
// Sourced from oauth.StateMinSecret so the constant is single-sourced
// and a future bump on the verify side propagates here automatically.
const minStateSecretBytes = oauth.StateMinSecret

// newJWKSVerifier is overridable in tests so the env-var-table tests
// don't hit the real internet trying to fetch example.auth0.com's JWKS
// (and don't burn the 5s prime budget in airgapped CI). Production
// calls oauth.NewJWKSVerifier directly via this seam.
var newJWKSVerifier = func(ctx context.Context, issuer, audience string) (oauth.IDTokenVerifier, error) {
	return oauth.NewJWKSVerifier(ctx, issuer, audience)
}

func validateSlackBotToken(token string) error {
	if token == "" {
		return nil
	}
	// Keep this as a light local shape check. Slack token formats have changed
	// over time, and the Slack API remains the authority on token validity. The
	// upper bound is intentionally generous; when set, this only catches obvious
	// config mistakes such as truncated tokens or bytes outside visible ASCII.
	// Keep the lower bound loose: this boot-time check is only a local typo
	// guard, while Slack's auth response remains the validity oracle.
	// TODO(slack-token-rotation): revisit the prefix check if Slack recommends
	// xoxe.xoxb-style rotation tokens for bot-authenticated Web API calls.
	if len(token) < slackBotTokenMinLen {
		return fmt.Errorf("SLACK_BOT_TOKEN is shorter than %d characters", slackBotTokenMinLen)
	}
	if len(token) > slackBotTokenMaxLen {
		return fmt.Errorf("SLACK_BOT_TOKEN is longer than %d characters", slackBotTokenMaxLen)
	}
	if !strings.HasPrefix(token, "xoxb-") {
		return errors.New("SLACK_BOT_TOKEN must be a Slack bot token starting with xoxb-")
	}
	for i, r := range token {
		if r >= '!' && r <= '~' {
			continue
		}
		return fmt.Errorf("SLACK_BOT_TOKEN contains invalid characters near byte %d", i)
	}
	return nil
}

// missingOAuthEnvVars returns the env-var names with empty values, in
// stable order, so the warn log says exactly which one(s) the operator
// needs to set. Previously the all-or-nothing return just logged the
// whole list, which is harder to act on.
func missingOAuthEnvVars(vals map[string]string) []string {
	// Stable order so the slog attribute is diff-friendly across runs.
	keys := []string{
		"AUTH0_DOMAIN", "AUTH0_CLIENT_ID", "AUTH0_CLIENT_SECRET",
		"AUTH0_AUDIENCE", "SLACK_BASE_URL", "OAUTH_STATE_SECRET", "QURL_ENDPOINT",
	}
	var missing []string
	for _, k := range keys {
		if vals[k] == "" {
			missing = append(missing, k)
		}
	}
	return missing
}

// errOAuthStateSecretTooShort is returned by buildOAuthConfig when the
// operator's OAUTH_STATE_SECRET is set but below oauth.StateMinSecret.
// Bubbling it up to run() turns a misconfiguration that previously
// degraded silently into a fail-fast startup error.
var errOAuthStateSecretTooShort = errors.New("OAUTH_STATE_SECRET shorter than required minimum")

// buildOAuthConfig assembles the oauth.Config from env. Returns
// (cfg, false, nil) when any required env var is missing — the caller
// logs and skips route registration so a sandbox boot with no Auth0
// configured still serves the existing Slack surface. Returns
// (_, false, err) when a required env var is set but malformed
// (short secret, non-HTTPS SlackBaseURL) — the caller fails-fast.
//
// Required env vars:
//
//	AUTH0_DOMAIN, AUTH0_CLIENT_ID, AUTH0_CLIENT_SECRET, AUTH0_AUDIENCE
//	SLACK_BASE_URL, OAUTH_STATE_SECRET, QURL_ENDPOINT
//
// ctx is the parent context for the JWKS refresh goroutine spawned
// inside NewJWKSVerifier — pass the signal-canceled context so the
// goroutine tears down on SIGTERM.
func buildOAuthConfig(ctx context.Context, provider *auth.DDBProvider, tracker oauth.AsyncTracker, adminStore oauth.AdminStore) (oauth.Config, bool, error) {
	// Strip trailing slashes from URL-shaped env vars at one chokepoint so
	// downstream concatenations (redirect_uri, /oauth/token URL composition)
	// can't produce //-path artifacts. Auth0 rejects redirect_uri mismatches
	// strictly, so a single stray slash is a real failure surface.
	//
	// AUTH0_DOMAIN is also stripped of any accidental scheme prefix —
	// jwks.go composes "https://" + domain, so an operator setting
	// "https://example.auth0.com" would silently yield
	// "https://https://example.auth0.com/.well-known/...".
	domain := strings.TrimPrefix(os.Getenv("AUTH0_DOMAIN"), "https://")
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimRight(domain, "/")
	// AUTH0_DOMAIN must be a bare host — embedded paths (or any "/"
	// after stripping the trailing slash) compose into garbage URLs at
	// jwks.go and exchangeAuth0Code. Fail-fast at config-load rather
	// than letting it surface as a 502 from "/.well-known/jwks.json"
	// or a 404 from "/path/oauth/token".
	if strings.ContainsRune(domain, '/') {
		return oauth.Config{}, false, fmt.Errorf("AUTH0_DOMAIN must be a bare host, no path (got %q)", domain)
	}
	clientID := os.Getenv("AUTH0_CLIENT_ID")
	clientSecret := os.Getenv("AUTH0_CLIENT_SECRET")
	audience := os.Getenv("AUTH0_AUDIENCE")
	baseURL := strings.TrimRight(os.Getenv("SLACK_BASE_URL"), "/")
	stateSecret := os.Getenv("OAUTH_STATE_SECRET")
	qurlEndpoint := strings.TrimRight(os.Getenv("QURL_ENDPOINT"), "/")

	// QURL_ENDPOINT is already required at run() startup; including it
	// here is belt-and-suspenders so a refactor that drops the earlier
	// check still fails-soft at the OAuth seam rather than constructing
	// a Minter pointed at an empty URL.
	missing := missingOAuthEnvVars(map[string]string{
		"AUTH0_DOMAIN":        domain,
		"AUTH0_CLIENT_ID":     clientID,
		"AUTH0_CLIENT_SECRET": clientSecret,
		"AUTH0_AUDIENCE":      audience,
		"SLACK_BASE_URL":      baseURL,
		"OAUTH_STATE_SECRET":  stateSecret,
		"QURL_ENDPOINT":       qurlEndpoint,
	})
	if len(missing) > 0 {
		slog.Warn("OAuth routes NOT registered — required env vars unset", "missing", missing)
		return oauth.Config{}, false, nil
	}
	// SLACK_BASE_URL must be HTTPS: the state cookie is Secure, and a
	// browser silently drops Set-Cookie: Secure on an http:// response,
	// which would break the double-submit check with a misleading
	// "setup must be completed in the same browser" error. Reject
	// fail-fast at config load.
	if !strings.HasPrefix(baseURL, "https://") {
		return oauth.Config{}, false, fmt.Errorf("SLACK_BASE_URL must be https:// (got %q)", baseURL)
	}
	// SLACK_BASE_URL must be a bare origin — embedded paths (e.g.
	// https://bot.example/prefix) compose to redirect_uri /authorize
	// hits like https://bot.example/prefix/oauth/qurl/callback, but
	// the mux only registers /oauth/qurl/callback. Auth0's redirect
	// would silently miss the route. Parse + check Path is "".
	if u, err := url.Parse(baseURL); err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return oauth.Config{}, false, fmt.Errorf("SLACK_BASE_URL must be a bare https:// origin with no path/query/userinfo (got %q)", baseURL)
	}
	if len(stateSecret) < minStateSecretBytes {
		// Fail-fast: the bot would silently disable OAuth and /qurl
		// setup would reply "not configured" forever otherwise.
		return oauth.Config{}, false, fmt.Errorf("%w: got %d bytes, want >= %d",
			errOAuthStateSecretTooShort, len(stateSecret), minStateSecretBytes)
	}

	// JWKS verifier opens the network for the initial JWKS fetch
	// (bounded inside NewJWKSVerifier). The callback uses the verifier
	// to extract the id_token `sub` claim, which becomes the
	// workspace_mappings OwnerID at BindWorkspace time. Without a
	// usable verifier, every callback in production would refuse the
	// install (no OwnerID → no bind → 500). Fail-fast at boot when
	// adminStore is wired so the operator sees the configuration error
	// immediately instead of after the first user tries /qurl setup.
	// On sandbox / no-DDB deploys (adminStore==nil) the bind is
	// skipped anyway, so the verifier is downgraded to "email line
	// missing on the success page" — non-fatal, log and continue.
	issuer := "https://" + domain + "/"
	// id_tokens carry the application's client_id as their `aud`
	// claim, distinct from AUTH0_AUDIENCE (the API resource server
	// identifier used at /authorize for access-token scope). Passing
	// clientID here matches what Auth0 actually stamps into id_tokens.
	verifier, err := newJWKSVerifier(ctx, issuer, clientID)
	if err != nil {
		if adminStore != nil {
			return oauth.Config{}, false, fmt.Errorf("JWKS verifier init failed and AdminStore is wired — every callback would refuse the install: %w", err)
		}
		slog.Warn("JWKS verifier init failed — id_token email will not be displayed for the lifetime of this task (AdminStore=nil so bind is skipped anyway)", "error", err)
		verifier = nil
	}

	return oauth.Config{
		Auth0Domain:       domain,
		Auth0ClientID:     clientID,
		Auth0ClientSecret: clientSecret,
		Auth0Audience:     audience,
		SlackBaseURL:      baseURL,
		OAuthStateSecret:  []byte(stateSecret),
		Provider:          provider,
		IDTokenVerifier:   verifier,
		Minter:            &oauth.HTTPAPIKeyMinter{BaseURL: qurlEndpoint},
		AsyncTracker:      tracker,
		AdminStore:        adminStore,
		BindClassifyError: classifyBindError,
		// SlackClient left nil for now — DM-after-success Slack-API
		// wiring is a follow-up; the success-page HTML still renders.
	}, true, nil
}

// adminStoreAdapter bridges *slackdata.Store to the oauth.AdminStore
// interface. The two declare WorkspaceMapping in their own packages
// so the callback doesn't import slackdata directly; the adapter
// translates the field-for-field equivalent shape and forwards the
// call.
//
// `store` is typed as the slackdataBinder interface (not concrete
// *slackdata.Store) so the adapter's translation logic can be
// exercised end-to-end in tests against a captor without standing
// up a real Store. *slackdata.Store satisfies the interface by
// declaring BindWorkspace with the matching signature.
type adminStoreAdapter struct {
	store slackdataBinder
}

// slackdataBinder is the slice of slackdata.Store that the adapter
// depends on. Defined here (rather than imported from slackdata)
// so cmd/main_test.go can inject a captor that fences the
// translation without dragging in the full Store surface.
type slackdataBinder interface {
	BindWorkspace(ctx context.Context, m *slackdata.WorkspaceMapping, seedAdmin string) error
}

func (a *adminStoreAdapter) BindWorkspace(ctx context.Context, m *oauth.WorkspaceMapping, seedAdmin string) error {
	return a.store.BindWorkspace(ctx, &slackdata.WorkspaceMapping{
		TeamID:    m.TeamID,
		OwnerID:   m.OwnerID,
		CreatedAt: m.CreatedAt,
	}, seedAdmin)
}

// classifyBindError errors.As's the slackdata.Error and returns the
// matching oauth.BindConflictCode for 409 paths so the callback can
// branch idempotent vs. rebind-refused vs. generic-failure. Non-409
// or non-*slackdata.Error returns "" so the callback treats it as a
// generic failure (500).
func classifyBindError(err error) oauth.BindConflictCode {
	var ae *slackdata.Error
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusConflict {
		return ""
	}
	switch ae.Code {
	case slackdata.ErrCodeWorkspaceAlreadyBoundToCaller:
		return oauth.BindConflictAlreadyBoundToCaller
	case slackdata.ErrCodeWorkspaceAlreadyBound:
		return oauth.BindConflictAlreadyBound
	case slackdata.ErrCodeWorkspaceBindUnverified:
		return oauth.BindConflictUnverified
	default:
		// A 409 from slackdata with an unmapped Code means a new
		// conflict variant was added on the producer side without
		// the classifier here being updated. Surface a warn so
		// on-call sees the drift on CloudWatch before users start
		// reporting "every rebind 500s."
		slog.Warn("classifyBindError: slackdata returned 409 with unmapped Code — defaulting to generic 500 (classifier and slackdata.ErrCodeWorkspace* have drifted)",
			"code", ae.Code, "title", ae.Title)
		return ""
	}
}

// missingAdminStoreEnvVars returns the slackdata table env-var names
// that are empty so the warn log surfaces exactly what's missing.
// Mirrors missingOAuthEnvVars's stable-order shape so the slog
// attribute is diff-friendly across runs.
func missingAdminStoreEnvVars() []string {
	keys := []string{
		slackdata.EnvWorkspaceMappingsTable,
		slackdata.EnvChannelPoliciesTable,
	}
	var missing []string
	for _, k := range keys {
		if os.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	return missing
}

// readMaxConcurrentAsync parses QURL_SLACK_MAX_CONCURRENT_ASYNC. Empty
// env is "use default" silently; non-empty-but-malformed env is a
// misconfiguration surfaced at startup so it doesn't get discovered
// during a saturation incident. Either way the value returned to
// NewHandler may be 0, which Handler interprets as "use the built-in
// default (50)".
func readMaxConcurrentAsync() int {
	raw := os.Getenv("QURL_SLACK_MAX_CONCURRENT_ASYNC")
	if raw == "" {
		return 0
	}
	parsed, err := strconv.Atoi(raw)
	switch {
	case err != nil:
		slog.Warn("ignoring malformed QURL_SLACK_MAX_CONCURRENT_ASYNC; falling back to default", //nolint:gosec // G706: raw is env-var input; slog's JSON handler escapes control bytes in attribute values, same posture as the request-path slog sites.
			"raw", raw, "error", err)
		return 0
	case parsed <= 0:
		// NewHandler treats 0/negative as "use default", but a
		// negative value is more likely a typo or env-substitution
		// mishap than an intentional choice — surface it the same
		// way as malformed input so it doesn't silently swallow.
		slog.Warn("ignoring non-positive QURL_SLACK_MAX_CONCURRENT_ASYNC; falling back to default", //nolint:gosec // G706: raw is env-var input; slog's JSON handler escapes control bytes in attribute values, same posture as the request-path slog sites.
			"raw", raw)
		return 0
	default:
		return parsed
	}
}

// buildAdminStore constructs the DDB-direct facade for
// workspace_mappings + channel_policies. When both QURL_*_TABLE env
// vars are set, we construct it; otherwise the /qurl admin verbs
// reply "Admin features are not configured" rather than crashing.
// Failure during construction (AWS config load, etc.) degrades the
// bot to no-admin mode rather than failing startup, so the OAuth +
// create/list surface stays available.
func buildAdminStore(ctx context.Context) *slackdata.Store {
	if os.Getenv(slackdata.EnvWorkspaceMappingsTable) == "" ||
		os.Getenv(slackdata.EnvChannelPoliciesTable) == "" {
		slog.Warn("admin store NOT configured — /qurl admin will reply 'not configured'",
			"missing_env", missingAdminStoreEnvVars())
		return nil
	}
	s, err := slackdata.NewStore(ctx)
	if err != nil {
		slog.Error("slackdata.NewStore failed; /qurl admin will be disabled", "error", err)
		return nil
	}
	slog.Info("admin store wired", //nolint:gosec // G706: env-var values are operator-controlled; slog's JSON handler escapes any control bytes the same way as the request-path slog sites.
		"workspace_mappings_table", os.Getenv(slackdata.EnvWorkspaceMappingsTable),
		"channel_policies_table", os.Getenv(slackdata.EnvChannelPoliciesTable))
	return s
}
