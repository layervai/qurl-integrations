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
	"sync"
	"syscall"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackinstall"
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
	// Keep reinstall propagation short while avoiding DDB/KMS on every modal
	// open. Negative caching is deliberately shorter because it can extend the
	// legacy SLACK_BOT_TOKEN fallback window after a customer reinstalls.
	slackWorkspaceTokenCacheTTL         = 30 * time.Second
	slackWorkspaceTokenNegativeCacheTTL = 10 * time.Second
	slackWorkspaceTokenCacheSweepEvery  = time.Minute
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
	tunnelImage := strings.TrimSpace(os.Getenv("QURL_CONNECTOR_IMAGE"))
	if err := internal.ValidateTunnelImageRef(tunnelImage); err != nil {
		return fmt.Errorf("QURL_CONNECTOR_IMAGE: %w", err)
	}
	slackBotToken := strings.TrimSpace(os.Getenv("SLACK_BOT_TOKEN"))
	if err := auth.ValidateSlackBotTokenShape(slackBotToken); err != nil {
		return fmt.Errorf("invalid SLACK_BOT_TOKEN: %w", err)
	}
	workspaceTokenLookup, invalidateWorkspaceSlackToken := newWorkspaceSlackTokenLookupWithInvalidation(ddbProvider, slackBotToken, slackWorkspaceTokenCacheTTL, time.Now)
	openView := newSlackOpenViewFuncWithTokenLookup(workspaceTokenLookup, userAgent, slackViewsOpenURL, nil)
	slog.Info("Slack views.open wired with per-workspace token lookup", "legacy_fallback_enabled", slackBotToken != "") // #nosec G706 -- only a boolean derived from token presence is logged; the token value is never logged.

	postFeedback := buildPostFeedback(userAgent)

	// Conversation-mode seams. All default DARK: the read-only agent only answers
	// when AgentLLM, AgentStore and PostMessage are non-nil AND the kill switch is
	// off (Handler.agentEnabled); the confirm/mutation flow additionally needs
	// PostMessageBlocks wired AND AgentConfirmEnabled true (Handler.agentConfirmEnabled).
	// Both PostMessage seams share the per-workspace token lookup + Grid fallback with
	// the slash-command modals.
	postMessage := newSlackPostMessageFuncWithTokenLookup(workspaceTokenLookup, userAgent, slackChatPostMessageURL, nil)
	postMessageBlocks := newSlackPostMessageBlocksFuncWithTokenLookup(workspaceTokenLookup, userAgent, slackChatPostMessageURL, nil)
	agentDisabled := readAgentKillSwitch()
	agentConfirmEnabled := readAgentConfirmEnabled()
	// Skip building the LLM + state store under a kill switch: it's read once at
	// boot and forces the surface dark regardless (agentEnabled), so the store/LLM
	// would never be used, and un-killing requires a restart anyway. This also
	// avoids the AWS config load + DDB client construction under a kill. Trade-off:
	// a misconfigured QURL_AGENT_STATE_TABLE isn't validated while killed — but the
	// un-kill restart rebuilds and surfaces any construction error then, which is
	// the moment it matters.
	var agentLLM agent.LLM
	var agentStore *slackdata.AgentStore
	if !agentDisabled {
		agentLLM = buildAgentLLM()
		agentStore = buildAgentStore(signalCtx)
	}
	logAgentSurfaceState(agentSurfaceState{
		llmWired:    agentLLM != nil,
		storeWired:  agentStore != nil,
		postWired:   postMessage != nil,
		blocksWired: postMessageBlocks != nil,
		confirmFlag: agentConfirmEnabled,
		killed:      agentDisabled,
	})

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
		PostFeedback:       postFeedback,
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
		AgentLLM:            agentLLM,
		AgentStore:          agentStore,
		PostMessage:         postMessage,
		AgentDisabled:       agentDisabled,
		PostMessageBlocks:   postMessageBlocks,
		AgentConfirmEnabled: agentConfirmEnabled,
	})

	// Alias reads and writes must go through the same slackdata facade so
	// `/qurl-admin protect-connector` can create the resource, bind `$slug`, and
	// let users immediately `/qurl get $slug` against the same table shape.
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
		// Operator reminder: /qurl-admin carries the admin verbs (tunnel
		// install, set-alias, admin add/remove/list/revoke). It must be
		// registered in the Slack app config pointing at the same request
		// URL as /qurl — or those verbs never arrive. Admin enforcement is
		// in-code: every admin verb runs requireAdminSync against the qURL
		// admin set (admin_slack_user_ids), so the AdminStore must be
		// wired. The "admins only" restriction on the /qurl-admin
		// registration is a cosmetic Slack-picker hint, NOT the
		// enforcement boundary — Slack does not gate slash-command
		// invocation on workspace-admin role. /qurl setup is NOT on
		// /qurl-admin and is intentionally open to any workspace member so
		// the first claimant of an unbound workspace can reach it
		// (first-come-claims). Setup re-runs are still owner-only, enforced
		// in code in handleSetup via AdminStore, with the OAuth-callback
		// BindWorkspace check as the structural backstop. The remaining
		// exposure is the first-install claim: restrict who can reach the
		// command at install time (Slack app manifest / onboarding) if
		// first-claim ownership matters for this deployment.
		slog.Info("CONFIGURATION REMINDER: register /qurl-admin in the Slack app config at the same request URL as /qurl (or admin verbs never arrive) and wire the AdminStore — admin enforcement is the in-code requireAdminSync gate, not the manifest 'admin-only' label, which Slack does not enforce for slash commands. Do NOT restrict /qurl to admins: /qurl setup is intentionally open (first-come-claims) so the first claimant can reach it — though setup re-runs are owner-only (enforced in code) — and an admin-only /qurl would lock out the first claimant of an unbound workspace")
	}
	// Else: buildOAuthConfig already logged the specific missing-var
	// list; nothing more to say here.
	slackInstallCfg, ok, err := buildSlackInstallConfig(ddbProvider)
	if err != nil {
		return fmt.Errorf("slack install config: %w", err)
	}
	if ok {
		handler.SetSlackInstallURL(strings.TrimRight(slackInstallCfg.SlackBaseURL, "/") + slackinstall.InstallPath)
		slackInstallCfg.OnTokenStored = invalidateWorkspaceSlackToken
		if err := slackinstall.RegisterRoutes(rootMux, &slackInstallCfg); err != nil {
			return fmt.Errorf("slack install routes: %w", err)
		}
		slog.Info("registered /oauth/slack/{install,callback} routes")
	}

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

	slog.Info("starting Secure Access Agent HTTP server", "addr", listenAddr)
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

type slackBotTokenProvider interface {
	SlackBotToken(ctx context.Context, workspaceID string) (string, error)
}

type cachedSlackBotToken struct {
	token     string
	expiresAt time.Time
}

type workspaceSlackTokenLookupResult struct {
	token string
	err   error
}

type workspaceSlackTokenLookupCall struct {
	done chan struct{}
	workspaceSlackTokenLookupResult
}

type workspaceSlackTokenLookupStart struct {
	token       string
	positiveHit bool
	negativeHit bool
	call        *workspaceSlackTokenLookupCall
	owner       bool
	generation  uint64
}

type workspaceSlackTokenLookupCache struct {
	mu sync.Mutex
	// Cache keys are Slack token owners: workspace team_id values for
	// workspace installs, and enterprise_id values for Enterprise Grid org
	// installs. The ID spaces are disjoint, so one cache can hold both.
	positive       map[string]cachedSlackBotToken
	negative       map[string]time.Time
	inFlight       map[string]*workspaceSlackTokenLookupCall
	generation     map[string]uint64
	fallbackWarned map[string]struct{}
	lastSweep      time.Time
}

func newWorkspaceSlackTokenLookupWithInvalidation(provider slackBotTokenProvider, fallbackToken string, ttl time.Duration, now func() time.Time) (lookup slackBotTokenLookup, purge func(string)) {
	if now == nil {
		now = time.Now
	}
	cache := &workspaceSlackTokenLookupCache{
		positive:       map[string]cachedSlackBotToken{},
		negative:       map[string]time.Time{},
		inFlight:       map[string]*workspaceSlackTokenLookupCall{},
		generation:     map[string]uint64{},
		fallbackWarned: map[string]struct{}{},
	}
	return func(ctx context.Context, teamID string) (string, error) {
		teamID = strings.TrimSpace(teamID)
		if teamID != "" {
			start := cache.getOrStart(teamID, ttl, now())
			switch {
			case start.positiveHit:
				return start.token, nil
			case start.negativeHit && fallbackToken != "":
				cache.warnLegacySlackBotTokenFallback(teamID)
				return fallbackToken, nil
			case start.negativeHit:
				return "", auth.ErrSlackBotTokenNotConfigured
			case !start.owner:
				select {
				case <-start.call.done:
					return start.call.token, start.call.err
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			return fetchAndFinishWorkspaceSlackToken(ctx, provider, cache, start.call, teamID, fallbackToken, ttl, now, start.generation)
		}

		token, _, _, err := fetchWorkspaceSlackToken(ctx, provider, teamID, fallbackToken)
		return token, err
	}, cache.purge
}

func fetchWorkspaceSlackToken(ctx context.Context, provider slackBotTokenProvider, teamID, fallbackToken string) (token string, cachePositive, cacheNegative bool, err error) {
	token, err = provider.SlackBotToken(ctx, teamID)
	if err == nil {
		token = strings.TrimSpace(token)
		if token == "" {
			return "", false, false, errors.New("workspace Slack bot token is empty")
		}
		if err := auth.ValidateSlackBotTokenShape(token); err != nil {
			// Do not cache malformed-token errors; once the workspace row is
			// repaired, the next lookup should observe the fix immediately.
			return "", false, false, fmt.Errorf("workspace Slack bot token: %w", err)
		}
		return token, true, false, nil
	}
	// The legacy fallback is only for workspaces that have not installed the
	// Slack app yet. Other DDB/KMS failures should stay visible to operators.
	if errors.Is(err, auth.ErrSlackBotTokenNotConfigured) {
		if fallbackToken != "" {
			return fallbackToken, false, true, nil
		}
		return "", false, true, err
	}
	return "", false, false, err
}

func fetchAndFinishWorkspaceSlackToken(
	ctx context.Context,
	provider slackBotTokenProvider,
	cache *workspaceSlackTokenLookupCache,
	call *workspaceSlackTokenLookupCall,
	teamID string,
	fallbackToken string,
	ttl time.Duration,
	now func() time.Time,
	generation uint64,
) (token string, err error) {
	var cachePositive bool
	var cacheNegative bool
	result := workspaceSlackTokenLookupResult{}
	finished := false
	defer func() {
		if rec := recover(); rec != nil {
			if !finished {
				result = workspaceSlackTokenLookupResult{err: errors.New("workspace Slack bot token lookup panicked")}
				cache.finish(teamID, call, result, false, false, ttl, 0, now(), generation)
			}
			panic(rec)
		}
	}()
	token, cachePositive, cacheNegative, err = fetchWorkspaceSlackToken(ctx, provider, teamID, fallbackToken)
	result = workspaceSlackTokenLookupResult{token: token, err: err}
	if cacheNegative && err == nil && fallbackToken != "" {
		cache.warnLegacySlackBotTokenFallback(teamID)
	}
	cache.finish(teamID, call, result, cachePositive, cacheNegative, ttl, slackWorkspaceTokenNegativeCacheTTL, now(), generation)
	finished = true
	return token, err
}

func (c *workspaceSlackTokenLookupCache) getOrStart(teamID string, ttl time.Duration, at time.Time) workspaceSlackTokenLookupStart {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepExpiredLocked(at)
	generation := c.generation[teamID]
	if ttl > 0 {
		cached, ok := c.positive[teamID]
		if ok && at.Before(cached.expiresAt) {
			return workspaceSlackTokenLookupStart{token: cached.token, positiveHit: true}
		}
		if ok {
			delete(c.positive, teamID)
		}
	}

	expiresAt, ok := c.negative[teamID]
	if ok && at.Before(expiresAt) {
		return workspaceSlackTokenLookupStart{negativeHit: true}
	}
	if ok {
		delete(c.negative, teamID)
	}

	if call, ok := c.inFlight[teamID]; ok {
		return workspaceSlackTokenLookupStart{call: call}
	}
	call := &workspaceSlackTokenLookupCall{done: make(chan struct{})}
	c.inFlight[teamID] = call
	return workspaceSlackTokenLookupStart{call: call, owner: true, generation: generation}
}

func (c *workspaceSlackTokenLookupCache) finish(
	teamID string,
	call *workspaceSlackTokenLookupCall,
	result workspaceSlackTokenLookupResult,
	cachePositive bool,
	cacheNegative bool,
	positiveTTL time.Duration,
	negativeTTL time.Duration,
	at time.Time,
	generation uint64,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	canCache := generation == c.generation[teamID]
	if cachePositive && positiveTTL > 0 && canCache {
		c.positive[teamID] = cachedSlackBotToken{
			token:     result.token,
			expiresAt: at.Add(positiveTTL),
		}
		delete(c.negative, teamID)
		delete(c.fallbackWarned, teamID)
	}
	if cacheNegative && negativeTTL > 0 && canCache {
		c.negative[teamID] = at.Add(negativeTTL)
	}
	call.workspaceSlackTokenLookupResult = result
	if c.inFlight[teamID] == call {
		delete(c.inFlight, teamID)
	}
	close(call.done)
}

func (c *workspaceSlackTokenLookupCache) purge(teamID string) {
	teamID = strings.TrimSpace(teamID)
	if teamID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.positive, teamID)
	delete(c.negative, teamID)
	delete(c.inFlight, teamID)
	delete(c.fallbackWarned, teamID)
	c.generation[teamID]++
}

func (c *workspaceSlackTokenLookupCache) warnLegacySlackBotTokenFallback(teamID string) {
	teamID = strings.TrimSpace(teamID)
	if teamID == "" || !c.markLegacySlackBotTokenFallbackWarned(teamID) {
		return
	}
	slog.Warn("legacy SLACK_BOT_TOKEN fallback is serving workspace without Slack install token", "team_id", teamID) // #nosec G706 -- Slack team IDs are structured slog attributes; JSON handlers escape control bytes.
}

func (c *workspaceSlackTokenLookupCache) markLegacySlackBotTokenFallbackWarned(teamID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fallbackWarned == nil {
		c.fallbackWarned = map[string]struct{}{}
	}
	if _, ok := c.fallbackWarned[teamID]; ok {
		return false
	}
	c.fallbackWarned[teamID] = struct{}{}
	return true
}

func (c *workspaceSlackTokenLookupCache) sweepExpiredLocked(at time.Time) {
	if !c.lastSweep.IsZero() && at.Sub(c.lastSweep) < slackWorkspaceTokenCacheSweepEvery {
		return
	}
	// The sweep is intentionally minute-gated: it is O(workspaces seen by this
	// process), but the current customer cardinality keeps that below the Slack
	// trigger budget while avoiding a background janitor goroutine.
	for teamID, cached := range c.positive {
		if !at.Before(cached.expiresAt) {
			delete(c.positive, teamID)
		}
	}
	for teamID, expiresAt := range c.negative {
		if !at.Before(expiresAt) {
			delete(c.negative, teamID)
			delete(c.fallbackWarned, teamID)
		}
	}
	c.lastSweep = at
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
	emailConnection := strings.TrimSpace(os.Getenv("AUTH0_EMAIL_CONNECTION"))
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
		Auth0Domain:          domain,
		Auth0ClientID:        clientID,
		Auth0ClientSecret:    clientSecret,
		Auth0Audience:        audience,
		Auth0EmailConnection: emailConnection,
		SlackBaseURL:         baseURL,
		OAuthStateSecret:     []byte(stateSecret),
		Provider:             provider,
		IDTokenVerifier:      verifier,
		Minter:               &oauth.HTTPAPIKeyMinter{BaseURL: qurlEndpoint},
		AsyncTracker:         tracker,
		AdminStore:           adminStore,
		BindClassifyError:    classifyBindError,
		// SlackClient left nil for now — DM-after-success Slack-API
		// wiring is a follow-up; the success-page HTML still renders.
	}, true, nil
}

const (
	envSlackClientID                    = "SLACK_CLIENT_ID"
	envSlackClientSecret                = "SLACK_CLIENT_SECRET"
	envSlackInstallStateSecret          = "SLACK_INSTALL_STATE_SECRET"
	envSlackBotScopes                   = "SLACK_BOT_SCOPES"
	displayKeySlackInstallStateFallback = "SLACK_INSTALL_STATE"
)

func buildSlackInstallConfig(provider *auth.DDBProvider) (slackinstall.Config, bool, error) {
	clientID := strings.TrimSpace(os.Getenv(envSlackClientID))
	clientSecret := strings.TrimSpace(os.Getenv(envSlackClientSecret))
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("SLACK_BASE_URL")), "/")
	stateSecret := os.Getenv(envSlackInstallStateSecret)
	if stateSecret == "" {
		stateSecret = os.Getenv("OAUTH_STATE_SECRET")
	}

	missing := missingSlackInstallEnvVars(map[string]string{
		envSlackClientID:                    clientID,
		envSlackClientSecret:                clientSecret,
		"SLACK_BASE_URL":                    baseURL,
		displayKeySlackInstallStateFallback: stateSecret,
	})
	if len(missing) > 0 {
		slog.Warn("Slack install routes NOT registered — required env vars unset", "missing", missing)
		return slackinstall.Config{}, false, nil
	}

	scopes := slackinstall.DefaultBotScopes()
	if raw := strings.TrimSpace(os.Getenv(envSlackBotScopes)); raw != "" {
		scopes = slackinstall.NormalizeScopes([]string{raw})
		// Strip unsupported scopes (see slackinstall.DropUnsupportedScopes) from
		// operator overrides before Validate: a stale SLACK_BOT_SCOPES with
		// surviving valid scopes then warns instead of aborting startup. (An
		// override of only unsupported scopes strips to empty and Validate still
		// rejects it.)
		if kept, dropped := slackinstall.DropUnsupportedScopes(scopes); len(dropped) > 0 {
			scopes = kept
			slog.Warn("SLACK_BOT_SCOPES included views:write, which is not a real Slack scope; dropped it. SLACK_BOT_SCOPES must still include commands.")
		}
	}
	cfg := slackinstall.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		SlackBaseURL: baseURL,
		StateSecret:  []byte(stateSecret),
		BotScopes:    scopes,
		TokenStore:   provider,
	}
	if err := cfg.Validate(); err != nil {
		return slackinstall.Config{}, false, err
	}
	return cfg, true, nil
}

func missingSlackInstallEnvVars(values map[string]string) []string {
	keys := []string{envSlackClientID, envSlackClientSecret, "SLACK_BASE_URL", displayKeySlackInstallStateFallback}
	var missing []string
	for _, k := range keys {
		if values[k] == "" {
			if k == displayKeySlackInstallStateFallback {
				missing = append(missing, envSlackInstallStateSecret+" (or OAUTH_STATE_SECRET)")
				continue
			}
			missing = append(missing, k)
		}
	}
	return missing
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

// buildAgentLLM constructs the conversation-mode language model from
// ANTHROPIC_API_KEY. Returns nil (feature DARK) when the key is unset — the
// agent surface only goes live when AgentLLM, AgentStore and PostMessage are all
// non-nil and the kill switch is off (see Handler.agentEnabled).
func buildAgentLLM() agent.LLM {
	key := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if key == "" {
		return nil
	}
	return agent.NewAnthropicLLM(key)
}

// buildAgentStore constructs the DDB-backed conversation-mode state store from
// QURL_AGENT_STATE_TABLE. Returns nil (feature DARK) when the table is unset.
// A construction failure when the table IS set is logged and also yields nil:
// conversation mode degrades to dark rather than failing the whole boot, exactly
// like buildAdminStore degrades the admin surface.
func buildAgentStore(ctx context.Context) *slackdata.AgentStore {
	if strings.TrimSpace(os.Getenv(slackdata.EnvAgentStateTable)) == "" {
		return nil
	}
	store, err := slackdata.NewAgentStoreFromEnv(ctx)
	if err != nil {
		slog.Error("agent state store construction failed; conversation mode stays DARK", "error", err)
		return nil
	}
	return store
}

// readBoolEnvFailSafe reads a boolean env flag. Absent → emptyDefault. A
// set-but-unparseable value FAILS SAFE to parseErrDefault and logs loudly, so an
// operator typo can never silently flip the flag to its unsafe pole. The two agent
// flags differ only in which pole is safe — see the named wrappers below.
func readBoolEnvFailSafe(name string, emptyDefault, parseErrDefault bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return emptyDefault
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		slog.Warn("agent env flag set to an unparseable value; using the fail-safe default", //nolint:gosec // G706: operator-set flag value, not a secret; slog's JSON handler escapes control bytes like the other env-logging sites.
			"env", name, "value", raw, "fail_safe_default", parseErrDefault)
		return parseErrDefault
	}
	return v
}

// readAgentKillSwitch reads the QURL_AGENT_DISABLED org kill switch. Absent → not
// disabled (the agent may run if otherwise wired). FAILS SAFE to DISABLED, so an
// operator typo ("QURL_AGENT_DISABLED=disable") can never leave the agent live.
func readAgentKillSwitch() bool {
	return readBoolEnvFailSafe("QURL_AGENT_DISABLED", false, true)
}

// readAgentConfirmEnabled reads QURL_AGENT_CONFIRM_ENABLED — the flag that, on top
// of the read-only surface, lets the agent EXECUTE mutations via the Approve/Reject
// confirm card. Absent → off. FAILS SAFE like the kill switch but in the opposite
// direction: a set-but-unparseable value is treated as OFF (not enabled), so a typo
// can never turn mutation execution on. The flag only takes effect once the read-only
// surface is live and PostMessageBlocks is wired (Handler.agentConfirmEnabled).
func readAgentConfirmEnabled() bool {
	return readBoolEnvFailSafe("QURL_AGENT_CONFIRM_ENABLED", false, false)
}

// agentSurfaceState groups the boot-time facts that decide what conversation mode
// does — a struct so logAgentSurfaceState's growing set of seam booleans can't be
// transposed at the call site.
type agentSurfaceState struct {
	llmWired    bool
	storeWired  bool
	postWired   bool
	blocksWired bool
	confirmFlag bool // QURL_AGENT_CONFIRM_ENABLED
	killed      bool
}

// logAgentSurfaceState emits startup lines describing what conversation mode will
// do. The read-only LIVE claim is gated on the SAME seams as Handler.agentEnabled
// (LLM + Store + PostMessage, kill switch off); the confirm/mutation line is gated
// on the SAME predicate as Handler.agentConfirmEnabled (read-only live AND the flag
// AND PostMessageBlocks). Both key on EFFECTIVE state, never a raw env bool, so a
// line can't claim LIVE/ENABLED while the handler is actually dark.
func logAgentSurfaceState(s agentSurfaceState) {
	readOnlyLive := !s.killed && s.llmWired && s.storeWired && s.postWired
	switch {
	case s.killed:
		slog.Warn("conversation mode is DISABLED by kill switch (QURL_AGENT_DISABLED); the read-only agent will not respond even if other seams are wired")
	case readOnlyLive:
		slog.Info("conversation mode (read-only) is LIVE: @mentions and DMs will be answered")
	case !s.llmWired && !s.storeWired:
		// LLM + Store are the operator-set agent seams; PostMessage is wired
		// unconditionally, so "neither agent seam set" is the friendly no-agent
		// deploy, not a partial misconfiguration.
		slog.Info("conversation mode is DARK: no agent seams configured",
			"hint", "set ANTHROPIC_API_KEY and "+slackdata.EnvAgentStateTable+" to enable")
	default:
		// Partial config — including the today-unreachable case where PostMessage
		// is nil, so the line stays honest if PostMessage ever becomes conditional.
		var missing []string
		if !s.llmWired {
			missing = append(missing, "ANTHROPIC_API_KEY (AgentLLM)")
		}
		if !s.storeWired {
			// AgentStore is nil for two reasons — the table env is unset, OR it is
			// set but construction failed (buildAgentStore logs the real cause as an
			// error). Don't assert "unset" here, which would contradict that error.
			missing = append(missing, "AgentStore ("+slackdata.EnvAgentStateTable+" unset, or construction failed — see the logged error)")
		}
		if !s.postWired {
			missing = append(missing, "PostMessage")
		}
		slog.Warn("conversation mode is DARK: partially configured; the agent stays off until every seam is set",
			"missing", strings.Join(missing, ", "))
	}

	// Confirm/mutation mode sits ON TOP of the read-only surface. Report its EFFECTIVE
	// state, never the raw flag — a flag set while the surface is dark must NOT read
	// as enabled (the #670 LIVE-gate-consistency lesson, applied to the riskier flip).
	switch {
	case readOnlyLive && s.confirmFlag && s.blocksWired:
		slog.Warn("conversation mode CONFIRM (mutation execution) is LIVE: an admin Approving a card EXECUTES the change. Confirm the hard pre-enablement gates (get-link authorization; R2 public-card replace_original; C1 connector key-privacy; C2 connector trigger-window) AND the DPA/data-handling review have cleared before relying on this.")
	case !s.killed && s.confirmFlag:
		// When killed, the kill-switch line above is the accurate cause; a confirm-DARK
		// line here would name the seams as the blocker, not the kill switch — so let
		// the kill-switch line stand alone (the un-kill restart re-reports confirm state).
		slog.Warn("QURL_AGENT_CONFIRM_ENABLED is set but confirm mode is DARK; mutations will NOT execute until the read-only surface is live and PostMessageBlocks is wired",
			"read_only_live", readOnlyLive, "blocks_wired", s.blocksWired)
	}
}

// buildAdminStore constructs the DDB-direct facade for
// workspace_mappings + channel_policies. When both QURL_*_TABLE env
// vars are set, we construct it; otherwise the /qurl-admin admin verbs
// reply "Admin features are not configured" rather than crashing.
// Failure during construction (AWS config load, etc.) degrades the
// bot to no-admin mode rather than failing startup, so the OAuth +
// create/list surface stays available.
func buildAdminStore(ctx context.Context) *slackdata.Store {
	if os.Getenv(slackdata.EnvWorkspaceMappingsTable) == "" ||
		os.Getenv(slackdata.EnvChannelPoliciesTable) == "" {
		slog.Warn("admin store NOT configured — /qurl-admin admin will reply 'not configured'",
			"missing_env", missingAdminStoreEnvVars())
		return nil
	}
	s, err := slackdata.NewStore(ctx)
	if err != nil {
		slog.Error("slackdata.NewStore failed; /qurl-admin admin will be disabled", "error", err)
		return nil
	}
	slog.Info("admin store wired", //nolint:gosec // G706: env-var values are operator-controlled; slog's JSON handler escapes any control bytes the same way as the request-path slog sites.
		"workspace_mappings_table", os.Getenv(slackdata.EnvWorkspaceMappingsTable),
		"channel_policies_table", os.Getenv(slackdata.EnvChannelPoliciesTable))
	return s
}

// buildPostFeedback wires the /qurl feedback delivery seam from
// FEEDBACK_SLACK_WEBHOOK_URL. Feedback posts submissions to an internal Slack
// channel via an incoming webhook. This is an OPTIONAL feature secret whose SSM
// parameter ships as "PLACEHOLDER" until an operator seeds it, so an unset OR
// invalid value returns nil (feedback disabled, logged as a warn) rather than
// failing startup — otherwise a not-yet-seeded deploy would crash-loop. While
// disabled, /qurl feedback replies "not enabled" and help omits it.
// validateFeedbackWebhookURL("") returns an error, but the empty case is
// matched first so it logs the calmer "not set" line.
func buildPostFeedback(userAgent string) internal.PostFeedbackFunc {
	feedbackWebhookURL := strings.TrimSpace(os.Getenv("FEEDBACK_SLACK_WEBHOOK_URL"))
	switch host, err := validateFeedbackWebhookURL(feedbackWebhookURL); {
	case feedbackWebhookURL == "":
		slog.Info("feedback disabled — FEEDBACK_SLACK_WEBHOOK_URL not set; /qurl feedback will reply 'not enabled'")
		return nil
	case err != nil:
		slog.Warn("feedback disabled — FEEDBACK_SLACK_WEBHOOK_URL is set but not a usable URL", "error", err)
		return nil
	default:
		if !strings.EqualFold(host, slackIncomingWebhookHost) {
			slog.Warn("FEEDBACK_SLACK_WEBHOOK_URL host is not Slack's incoming-webhook host; delivering feedback there anyway", "host", host, "expected", slackIncomingWebhookHost) // #nosec G706 -- host is operator-set; slog's JSON handler escapes control bytes in attribute values.
		}
		slog.Info("feedback delivery wired via Slack incoming webhook")
		return newFeedbackWebhookPoster(feedbackWebhookURL, userAgent, nil)
	}
}
