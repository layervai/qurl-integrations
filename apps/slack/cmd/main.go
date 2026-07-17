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
	"io"
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

	"github.com/layervai/qurl-integrations/internal/ttlcache"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/connectorimage"
	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackinstall"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
	"github.com/layervai/qurl-integrations/shared/observability"
)

const (
	listenAddr                    = ":8080"
	envQURLConnectorImage         = "QURL_CONNECTOR_IMAGE"
	envQURLConnectorImageFallback = "QURL_CONNECTOR_IMAGE_FALLBACK"
	envQURLS3OriginImage          = "QURL_S3_ORIGIN_IMAGE"
	envQURLBindingTTLContract     = "QURL_BINDING_IDEMPOTENCY_TTL_CONTRACT"
	envQURLAPIKeyMintTTLContract  = "QURL_API_KEY_MINT_IDEMPOTENCY_TTL_CONTRACT"
	envSlackRateLimitEnabled      = "QURL_SLACK_RATE_LIMIT_ENABLED"
	envSlackBotTokenRotation      = "QURL_SLACK_BOT_TOKEN_ROTATION_ENABLED"
	connectorImageFallbackSandbox = "dev-sandbox"
	connectorImageFallbackOptIn   = envQURLConnectorImageFallback + "=" + connectorImageFallbackSandbox
	connectorImageFallbackHint    = "dev/sandbox fallback requires leaving " + envQURLConnectorImage + " empty and setting " + connectorImageFallbackOptIn

	connectorImageErrFloating        = "missing or latest tag; use a specific non-latest tag or image@sha256:<64 lowercase hex>"
	connectorImageErrLatestDigest    = "latest tag is not allowed with digest pins; drop :latest or use a specific non-latest tag before the digest"
	connectorImageErrDigestLowercase = "digest must be sha256:<64 lowercase hex>"
	connectorImageErrMalformedRef    = "invalid image reference; use lowercase image:tag or lowercase image@sha256:<64 lowercase hex>"
	connectorImageErrAmbiguousRef    = "ambiguous slashless registry ref; include a repository path such as gcr.io/<org>/<image>:v1"
	connectorImageErrMalformedDigest = "invalid digest ref; use image@sha256:<64 lowercase hex> with a full image name, not bare sha256:<digest>"

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

	// lameduckDuration is the ALB deregistration head start inside
	// shutdownTimeout. On SIGTERM, /health flips to 503, we keep the
	// listener open briefly so ALB can remove the target, then
	// srv.Shutdown starts the actual HTTP drain with the remaining
	// budget. Keep this greater than the ALB target group's unhealthy
	// detection window; with qurl-webhook-runtime's 5s interval x
	// 2-unhealthy-threshold target, 13s gives a small ALB propagation
	// margin while leaving roughly 12s for in-flight requests and async
	// workers before Fargate's 30s SIGTERM→SIGKILL window closes.
	// TODO(upstream-contract): Keep this in lockstep with
	// qurl-integrations-infra's qurl-webhook-runtime health check cadence.
	lameduckDuration = 13 * time.Second
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

func newAppLogger(w io.Writer) *slog.Logger {
	// JSON handler is load-bearing for log-injection safety and operational log
	// metrics: the G706 gosec suppressions in apps/slack/internal/handler.go
	// assume slog's JSON output escapes control characters in tainted attribute
	// values, and alert filters match the JSON "msg" field. Don't swap to
	// TextHandler without revisiting those sites.
	// Redaction mirrors Discord: matched keys blank string/byte values, while
	// containers under matched keys are walked by their inner field names.
	return slog.New(observability.NewRedactingJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func main() {
	if handled, err := runOperatorSubcommand(os.Args, os.Stdout, os.Stderr); handled {
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	slog.SetDefault(newAppLogger(os.Stdout))

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func runOperatorSubcommand(args []string, stdout, stderr io.Writer) (bool, error) {
	if len(args) <= 1 || args[1] != slackMarkdownValidationSubcommand {
		return false, nil
	}
	// Operator-only validation mode shares the bot binary so it exercises the
	// same Slack wire helpers as production while keeping the JSON evidence on stdout.
	slog.SetDefault(newAppLogger(stderr))
	return true, runSlackMarkdownRendererValidationCLI(args[2:], stdout)
}

// run holds the full server lifecycle so `defer stop()` releases the
// signal handler before main reaches os.Exit on the error path.
func run() error {
	// Required env vars are explicit by design: a missing QURL_ENDPOINT
	// previously fell back to the sandbox URL, which is the kind of silent
	// misconfiguration that ships a prod deploy at sandbox.
	rawQURLEndpoint := os.Getenv("QURL_ENDPOINT")
	qurlEndpoint, connectorAPIURL, err := connectorAPIURLFromEndpoint(rawQURLEndpoint)
	if err != nil {
		return err
	}

	slackSigningSecret := os.Getenv("SLACK_SIGNING_SECRET")
	if slackSigningSecret == "" {
		return errors.New("SLACK_SIGNING_SECRET is required")
	}
	// Validate the customer-rendered connector image before infra clients so
	// manifest mistakes fail with the image-specific startup error.
	tunnelImage, err := readTunnelImageConfig()
	if err != nil {
		return err
	}
	s3OriginImage, err := readS3OriginImageConfig()
	if err != nil {
		return err
	}

	// DDBProvider reads the workspace_state table populated by
	// /oauth/qurl/callback. Missing WORKSPACE_STATE_TABLE fails
	// startup distinctly from an empty table. The signal watcher is
	// armed before infra setup so SIGTERM/SIGINT during AWS config
	// load cancels those calls promptly.
	shutdownSignals := newShutdownSignalSource(syscall.SIGTERM, syscall.SIGINT)
	defer shutdownSignals.stop()

	ddbProvider, err := auth.NewDDBProvider(shutdownSignals.ctx,
		auth.WithTableName(os.Getenv("WORKSPACE_STATE_TABLE")),
		auth.WithKMSKeyARN(os.Getenv("WORKSPACE_STATE_KMS_KEY_ARN")),
	)
	if err != nil {
		return fmt.Errorf("DDBProvider init: %w", err)
	}
	var authProvider auth.Provider = ddbProvider
	userAgent := "qurl-slack/" + version

	maxConcurrentAsync := readMaxConcurrentAsync()
	maxConcurrentFollowupAsync := readMaxConcurrentFollowupAsync()
	maxConcurrentFollowupGateAsync := readMaxConcurrentFollowupGateAsync()
	adminStore := buildAdminStore(shutdownSignals.ctx)
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
	// DM seam for secret-bearing user deliveries (`/qurl get dm:true` and qURL
	// Connector bootstrap keys). Same token lookup + Grid fallback as channel posts.
	postDM := newSlackPostDMFuncWithTokenLookup(workspaceTokenLookup, userAgent, slackConversationsOpenURL, slackChatPostMessageURL, nil)
	// chat.postEphemeral seam: delivers a get's one-time link privately in a channel as a
	// standalone ephemeral (the response_url ephemeral collides with the card-replace).
	postEphemeral := newSlackPostEphemeralFuncWithTokenLookup(workspaceTokenLookup, userAgent, slackChatPostEphemeralURL, nil)
	// standard-Markdown seam for the agent's free-text answer, so a channel reply
	// renders like the streaming pane while still carrying a fallback.
	postMarkdownMessage := newSlackPostMarkdownMessageFuncWithTokenLookup(workspaceTokenLookup, userAgent, slackChatPostMessageURL, nil)
	postMessageBlocks := newSlackPostMessageBlocksFuncWithTokenLookup(workspaceTokenLookup, userAgent, slackChatPostMessageURL, nil)
	// Block Kit DM + ephemeral seams: deliver a minted `/qurl get` (dm:true) or
	// agent-confirm channel link as an "Enter Portal" URL button rather than a raw
	// hyperlink. Same token lookup + Grid fallback as their text siblings above.
	// PostEphemeralBlocks is effectively REQUIRED when agent-confirm is on: a confirmed
	// channel get commits to it with no text fallback (deliverConfirmEphemeral), so an
	// unwired seam fails channel get approvals AFTER the mint is burned — logConfirmModeState
	// warns loudly at boot. PostDMBlocks is only for `/qurl get dm:true`, which is refused
	// pre-mint when it's nil (getWork), so that path degrades gracefully.
	postDMBlocks := newSlackPostDMBlocksFuncWithTokenLookup(workspaceTokenLookup, userAgent, slackConversationsOpenURL, slackChatPostMessageURL, nil)
	postEphemeralBlocks := newSlackPostEphemeralBlocksFuncWithTokenLookup(workspaceTokenLookup, userAgent, slackChatPostEphemeralURL, nil)
	// reactions.add/remove seam for the agent's best-effort "working on it" ack. Always
	// wired (same token lookup as the post seams); inert until the agent surface is live
	// and needs the reactions:write scope in the Slack manifest to actually land.
	agentReactions := newSlackReactionPortWithTokenLookup(workspaceTokenLookup, userAgent, slackReactionsAddURL, slackReactionsRemoveURL, nil)
	// conversations.info metadata seam for surface-specific confirm decisions (notably
	// refusing group-DM get links before minting until mpim delivery is proven safe).
	agentResolveConversationInfo := newSlackResolveConversationInfoFuncWithTokenLookup(workspaceTokenLookup, userAgent, slackConversationsInfoURL, nil)
	// Channel-name projection so the agent's system prompt can render "#general
	// (C123)". Shares the same conversations.info closure as the confirm surface
	// classifier; degrades to the bare channel id until the relevant
	// conversations.info scope is in the manifest.
	agentResolveChannelName := slackResolveChannelNameFromConversationInfo(agentResolveConversationInfo)
	// conversations.members seam: gates whether an assistant-pane turn may scope its reads
	// to the channel the user opened the pane from (only a confirmed member's pane is
	// scoped). Always wired (same token lookup + channels:read / groups:read scopes as the
	// name resolver); fail-closed, so until the scopes are in the manifest it answers
	// missing_scope and the pane stays on the un-scoped DM.
	agentChannelMembership := newSlackChannelMembershipFuncWithTokenLookup(workspaceTokenLookup, userAgent, slackConversationsMembersURL, nil)
	// assistant.threads.* seam for the Assistants-container UX (first-run title +
	// suggested prompts, and the per-turn "thinking…" status). Always wired (same token
	// lookup); inert until the "Agents & AI Apps" manifest toggle + assistant:write scope
	// are set (the events don't arrive, and the calls would no-op, until then).
	agentAssistantThreads := newSlackAssistantThreadsPortWithTokenLookup(workspaceTokenLookup, userAgent, slackAssistantSetTitleURL, slackAssistantSetSuggestedPromptsURL, slackAssistantSetStatusURL, nil)
	// views.publish seam for the App Home review surface (a user's own recent confirmed
	// actions). Always wired (same token lookup); inert until the manifest's App Home tab
	// feature + app_home_opened subscription are set — the event doesn't arrive, and the
	// view would no-op, until then.
	agentAppHomePublish := newSlackAppHomePublishFuncWithTokenLookup(workspaceTokenLookup, userAgent, slackViewsPublishURL, nil)
	// chat.startStream/appendStream/stopStream seam for native AI-app reply streaming in
	// the assistant pane. Always wired (same token lookup + Grid fallback); inert until the
	// assistant:write scope + the pane (Agents & AI Apps) are enabled, and only engaged for
	// a pane turn — otherwise the agent posts the reply normally.
	agentStream := newSlackAgentStreamPortWithTokenLookup(workspaceTokenLookup, userAgent, slackChatStartStreamURL, slackChatAppendStreamURL, slackChatStopStreamURL, nil)
	agentDisabled := readAgentKillSwitch()
	agentConfirmEnabled := readAgentConfirmEnabled()
	agentChannelFollowups := readAgentChannelFollowups()
	agentSurfaceExclusiveAcks := readAgentSurfaceExclusiveAcks()
	slackBotTokenRotationEnabled, err := readSlackBotTokenRotationEnabled()
	if err != nil {
		return err
	}
	slog.Info("Slack bot-token revoke handling configured",
		"slack_bot_token_rotation_enabled", slackBotTokenRotationEnabled,
		"tokens_revoked_bot_token_triggers_workspace_purge", !slackBotTokenRotationEnabled,
	)
	// Per-workspace toggle default: false during the staged opt-in rollout, flipped
	// true at GA (every workspace on unless it explicitly opted out). Fail-safe to
	// false. The per-workspace flag itself lives in workspace_mappings (AdminStore).
	agentDefaultEnabled := readAgentDefaultEnabled()
	// Per-user / per-workspace turn caps (turns per rolling hour) — a cost backstop on
	// the LLM spend once conversation mode is live. Conservative non-zero defaults so
	// the backstop holds even if the operator never sets the env; 0 = unlimited.
	agentMaxTurnsPerUser := readAgentMaxTurnsPerUser()
	agentMaxTurnsPerTeam := readAgentMaxTurnsPerTeam()
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
		agentStore = buildAgentStore(shutdownSignals.ctx)
	}
	logAgentSurfaceState(agentSurfaceState{
		llmWired:              agentLLM != nil,
		storeWired:            agentStore != nil,
		postWired:             postMessage != nil,
		blocksWired:           postMessageBlocks != nil,
		ephemeralBlocksWired:  postEphemeralBlocks != nil,
		assistantThreadsWired: agentAssistantThreads != nil,
		confirmFlag:           agentConfirmEnabled,
		exclusiveAcksFlag:     agentSurfaceExclusiveAcks,
		killed:                agentDisabled,
	})

	// shutdownSignals.ctx is hoisted above so the DDB-provider constructor
	// can observe shutdown during AWS config load and the main goroutine can
	// detect the concrete signal. Handler.BaseContext is deliberately decoupled:
	// during lameduck the listener is still open, so requests accepted in
	// that window must not inherit an already-canceled async context. The
	// shutdown sequence cancels handlerCtx immediately before
	// srv.Shutdown starts refusing new connections.
	handlerCtx, cancelHandler := context.WithCancel(context.Background())
	defer cancelHandler()

	handler := internal.NewHandler(internal.Config{
		AuthProvider:                   authProvider,
		SlackSigningSecret:             slackSigningSecret,
		SlackBotTokenRotationEnabled:   slackBotTokenRotationEnabled,
		BaseContext:                    handlerCtx,
		MaxConcurrentAsync:             maxConcurrentAsync,
		MaxConcurrentFollowupAsync:     maxConcurrentFollowupAsync,
		MaxConcurrentFollowupGateAsync: maxConcurrentFollowupGateAsync,
		AdminStore:                     adminStore,
		OpenView:                       openView,
		TunnelImage:                    tunnelImage,
		S3OriginImage:                  s3OriginImage,
		ConnectorAPIURL:                connectorAPIURL,
		PostFeedback:                   postFeedback,
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
		AgentLLM:                    agentLLM,
		AgentStore:                  agentStore,
		PostDM:                      postDM,
		PostMessage:                 postMessage,
		PostEphemeral:               postEphemeral,
		PostMarkdownMessage:         postMarkdownMessage,
		AgentDisabled:               agentDisabled,
		PostMessageBlocks:           postMessageBlocks,
		PostDMBlocks:                postDMBlocks,
		PostEphemeralBlocks:         postEphemeralBlocks,
		AgentConfirmEnabled:         agentConfirmEnabled,
		AgentChannelFollowups:       agentChannelFollowups,
		AgentSurfaceExclusiveAcks:   agentSurfaceExclusiveAcks,
		AgentDefaultEnabled:         agentDefaultEnabled,
		AgentMaxTurnsPerUserPerHour: agentMaxTurnsPerUser,
		AgentMaxTurnsPerTeamPerHour: agentMaxTurnsPerTeam,
		Reactions:                   agentReactions,
		ResolveChannelName:          agentResolveChannelName,
		ResolveConversationInfo:     agentResolveConversationInfo,
		ChannelMembership:           agentChannelMembership,
		AssistantThreads:            agentAssistantThreads,
		AppHomePublish:              agentAppHomePublish,
		AgentStream:                 agentStream,
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
	if err := registerOptionalSetupRoutes(shutdownSignals.ctx, rootMux, ddbProvider, handler, adminStore, invalidateWorkspaceSlackToken); err != nil {
		return err
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
		// Task-stop shutdown is stricter than WriteTimeout: a slow OAuth
		// callback caught during deploy may still be cut so slash-command
		// deploy traffic keeps the ALB lameduck head start.
		WriteTimeout:   75 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: maxHeaderBytes,
	}

	// Bind first so a port-already-in-use failure returns before the
	// drain goroutine spawns — keeps the "received shutdown signal"
	// log line off the bind-failure path. Use a fresh background ctx
	// for the bind so a SIGTERM arriving in the gap between signal setup
	// and Listen doesn't surface as "listen: context canceled".
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", listenAddr, err)
	}

	shutdownDone := make(chan struct{})
	var shutdownOnce sync.Once
	runShutdown := func(duck time.Duration) {
		shutdownOnce.Do(func() {
			runShutdownSequence(srv, handler, cancelHandler, duck, shutdownTimeout, sleepContext)
			close(shutdownDone)
		})
	}
	signalWatcherDone := make(chan struct{})
	go func() {
		defer close(signalWatcherDone)
		select {
		case sig := <-shutdownSignals.first:
			runShutdown(lameduckForSignal(sig))
		case <-shutdownSignals.stopped:
		}
	}()

	slog.Info("starting Secure Access Agent HTTP server", "addr", listenAddr)
	serveErr := srv.Serve(ln)

	// Ensure exactly one shutdown sequence runs before return. The
	// signal goroutine wins on real SIGTERM/SIGINT; if Serve returns
	// first (for example a listener error), this path performs an
	// immediate drain without the ALB lameduck delay.
	runShutdown(0)
	shutdownSignals.stop()
	<-signalWatcherDone
	<-shutdownDone

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", serveErr)
	}
	slog.Info("server stopped cleanly")
	return nil
}

func registerOptionalSetupRoutes(ctx context.Context, rootMux *http.ServeMux, provider *auth.DDBProvider, handler *internal.Handler, adminStore *slackdata.Store, invalidateWorkspaceSlackToken func(string)) error {
	var oauthAdminStore oauth.AdminStore
	if adminStore != nil {
		oauthAdminStore = internal.NewOAuthAdminStoreAdapter(adminStore)
	}
	oauthCfg, ok, err := buildOAuthConfig(ctx, provider, handler, oauthAdminStore)
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
	slackInstallCfg, ok, err := buildSlackInstallConfig(provider)
	if err != nil {
		return fmt.Errorf("slack install config: %w", err)
	}
	if !ok {
		return nil
	}
	handler.SetSlackInstallURL(strings.TrimRight(slackInstallCfg.SlackBaseURL, "/") + slackinstall.InstallPath)
	slackInstallCfg.OnTokenStored = invalidateWorkspaceSlackToken
	if err := slackinstall.RegisterRoutes(rootMux, &slackInstallCfg); err != nil {
		return fmt.Errorf("slack install routes: %w", err)
	}
	slog.Info("registered /oauth/slack/{install,callback} routes")
	return nil
}

func connectorAPIURLFromEndpoint(raw string) (endpoint, apiURL string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", errors.New("QURL_ENDPOINT is required")
	}
	endpoint = strings.TrimRight(trimmed, "/")
	if endpoint == "" {
		return "", "", errors.New("QURL_ENDPOINT must be an absolute URL origin")
	}
	// These pre-checks add operator-specific messages. ValidateConnectorAPIURL
	// remains the canonical security and endpoint-shape validator.
	if parsed, parseErr := url.Parse(endpoint); parseErr == nil && parsed.IsAbs() && parsed.Host != "" {
		if parsed.User != nil {
			return "", "", errors.New("QURL_ENDPOINT must not include credentials")
		}
		if parsed.RawQuery != "" {
			return "", "", errors.New("QURL_ENDPOINT must not include a query")
		}
		if parsed.Fragment != "" {
			return "", "", errors.New("QURL_ENDPOINT must not include a fragment")
		}
		// Give the common already-versioned value a migration-specific error;
		// every other non-empty path is rejected by the general shape check.
		if strings.EqualFold(strings.Trim(parsed.Path, "/"), "v1") {
			return "", "", errors.New("QURL_ENDPOINT must omit the /v1 API suffix")
		}
		if parsed.Path != "" {
			return "", "", errors.New("QURL_ENDPOINT must not include a path")
		}
	}
	apiURL = endpoint + "/v1"
	if validateErr := internal.ValidateConnectorAPIURL(apiURL); validateErr != nil {
		return "", "", fmt.Errorf("QURL_ENDPOINT is invalid: %w", validateErr)
	}
	return endpoint, apiURL, nil
}

func lameduckForSignal(sig os.Signal) time.Duration {
	if sig == syscall.SIGTERM {
		return lameduckDuration
	}
	return 0
}

type shutdownSignalSource struct {
	ctx     context.Context
	first   <-chan os.Signal
	stopped <-chan struct{}
	stop    func()
}

func newShutdownSignalSource(signals ...os.Signal) shutdownSignalSource {
	signalInput := make(chan os.Signal, 1)
	signal.Notify(signalInput, signals...)
	return newShutdownSignalSourceFromInput(signalInput, func() {
		signal.Stop(signalInput)
	})
}

func newShutdownSignalSourceFromInput(signalInput <-chan os.Signal, stopInput func()) shutdownSignalSource {
	ctx, cancel := context.WithCancel(context.Background())
	firstSignal := make(chan os.Signal, 1)
	stopSignalInput := make(chan struct{})
	signalInputDone := make(chan struct{})
	// Only the first signal drives shutdown timing. Later signals do not
	// shorten SIGTERM lameduck; Fargate's 30s SIGKILL remains the hard stop,
	// and local SIGINT maps to immediate drain.
	go func() {
		defer close(signalInputDone)
		select {
		case sig := <-signalInput:
			cancel()
			firstSignal <- sig
		case <-stopSignalInput:
		}
	}()

	var stopOnce sync.Once
	return shutdownSignalSource{
		ctx:     ctx,
		first:   firstSignal,
		stopped: stopSignalInput,
		stop: func() {
			stopOnce.Do(func() {
				if stopInput != nil {
					stopInput()
				}
				cancel()
				close(stopSignalInput)
				<-signalInputDone
			})
		},
	}
}

type shutdownHTTPServer interface {
	Shutdown(context.Context) error
}

type shutdownHandler interface {
	SetHealthy(bool)
	WaitTimeout(time.Duration) bool
}

type shutdownSleeper func(context.Context, time.Duration) bool

func runShutdownSequence(srv shutdownHTTPServer, handler shutdownHandler, cancelHandler context.CancelFunc, duck, timeout time.Duration, sleep shutdownSleeper) {
	if duck < 0 {
		duck = 0
	}
	if timeout < 0 {
		timeout = 0
	}
	if sleep == nil {
		sleep = sleepContext
	}

	shutdownStart := time.Now()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	lameduckBudgetExhausted := false
	if duck > 0 {
		slog.Info("received shutdown signal — entering lameduck", "duration", duck, "shutdown_timeout", timeout)
		handler.SetHealthy(false)
		if !sleep(shutdownCtx, duck) {
			slog.Warn("lameduck ended early — shutdown budget exhausted", "duration", duck, "shutdown_timeout", timeout)
			lameduckBudgetExhausted = true
		} else {
			slog.Info("lameduck complete — draining HTTP server", "remaining_budget", remainingShutdownBudget(shutdownStart, timeout))
		}
	} else {
		slog.Info("draining HTTP server", "shutdown_timeout", timeout)
	}

	cancelHandler()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		if lameduckBudgetExhausted && errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("HTTP drain skipped — shutdown budget exhausted before drain", "error", err)
		} else {
			slog.Error("graceful shutdown failed", "error", err)
		}
	}
	// Shutdown returns once HTTP handlers have responded. Slash-
	// command handlers ack and return nearly instantly, so in-flight
	// async workers (the goroutines runAsync spawned) can outlive
	// Shutdown. Lameduck and Shutdown share the same timeout, so a
	// slow HTTP drain deliberately squeezes this async budget down to
	// zero rather than wedging the process past Fargate's hard kill.
	drainBudget := remainingShutdownBudget(shutdownStart, timeout)
	if lameduckBudgetExhausted {
		drainBudget = 0
	}
	if !handler.WaitTimeout(drainBudget) {
		slog.Warn("async drain timed out — exiting with workers still in flight", "budget", drainBudget)
	}
}

func remainingShutdownBudget(start time.Time, budget time.Duration) time.Duration {
	remaining := budget - time.Since(start)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type slackBotTokenProvider interface {
	SlackBotToken(ctx context.Context, workspaceID string) (string, error)
}

type workspaceSlackTokenCacheValue struct {
	token    string
	negative bool
}

type workspaceSlackTokenLookupCache struct {
	// Cache keys are Slack token owners: workspace team_id values for
	// workspace installs, and enterprise_id values for Enterprise Grid org
	// installs. The ID spaces are disjoint, so one cache can hold both.
	tokens         *ttlcache.Cache[workspaceSlackTokenCacheValue]
	fallbackWarned map[string]struct{}
}

func newWorkspaceSlackTokenLookupWithInvalidation(provider slackBotTokenProvider, fallbackToken string, ttl time.Duration, now func() time.Time) (lookup slackBotTokenLookup, purge func(string)) {
	if now == nil {
		now = time.Now
	}
	cache := newWorkspaceSlackTokenLookupCache()
	return func(ctx context.Context, teamID string) (string, error) {
		teamID = strings.TrimSpace(teamID)
		if teamID != "" {
			start := cache.getOrStart(teamID, now())
			switch {
			case start.Hit:
				value := start.Result.Value
				if value.negative && fallbackToken != "" {
					cache.warnLegacySlackBotTokenFallback(ctx, teamID)
				}
				return value.token, start.Result.Err
			case !start.Owner:
				select {
				case <-start.Call.Done():
					result := start.Call.Result()
					return result.Value.token, result.Err
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			return fetchAndFinishWorkspaceSlackToken(ctx, provider, cache, start.Call, teamID, fallbackToken, ttl, now, start.Generation)
		}

		token, _, _, err := fetchWorkspaceSlackToken(ctx, provider, teamID, fallbackToken)
		return token, err
	}, cache.purge
}

func newWorkspaceSlackTokenLookupCache() *workspaceSlackTokenLookupCache {
	cache := &workspaceSlackTokenLookupCache{
		fallbackWarned: map[string]struct{}{},
	}
	cache.tokens = ttlcache.New[workspaceSlackTokenCacheValue](ttlcache.Options[workspaceSlackTokenCacheValue]{
		SweepEvery: slackWorkspaceTokenCacheSweepEvery,
		OnEvict: func(key string, result ttlcache.Result[workspaceSlackTokenCacheValue]) {
			// OnEvict runs under the ttlcache lock; keep this hook
			// non-reentrant and limited to fallback warning sidecar cleanup.
			if result.Value.negative {
				delete(cache.fallbackWarned, key)
			}
		},
	})
	return cache
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
	call *ttlcache.Call[workspaceSlackTokenCacheValue],
	teamID string,
	fallbackToken string,
	ttl time.Duration,
	now func() time.Time,
	generation uint64,
) (token string, err error) {
	var cachePositive bool
	var cacheNegative bool
	result := ttlcache.Result[workspaceSlackTokenCacheValue]{}
	finished := false
	defer func() {
		if rec := recover(); rec != nil {
			if !finished {
				result = ttlcache.Result[workspaceSlackTokenCacheValue]{Err: errors.New("workspace Slack bot token lookup panicked")}
				cache.tokens.Finish(teamID, call, result, 0, now(), generation)
			}
			panic(rec)
		}
	}()
	token, cachePositive, cacheNegative, err = fetchWorkspaceSlackToken(ctx, provider, teamID, fallbackToken)
	result = ttlcache.Result[workspaceSlackTokenCacheValue]{
		Value: workspaceSlackTokenCacheValue{
			token:    token,
			negative: cacheNegative,
		},
		Err: err,
	}
	cacheTTL := time.Duration(0)
	if cachePositive {
		cacheTTL = ttl
	}
	if cacheNegative {
		cacheTTL = slackWorkspaceTokenNegativeCacheTTL
	}
	cached := cache.tokens.Finish(teamID, call, result, cacheTTL, now(), generation)
	// Warn only when the negative fill was actually cached. If a concurrent
	// purge detached this fill, marking fallbackWarned would leave sidecar
	// state with no cache entry to evict and clear it later.
	finished = true
	if cached && cacheNegative && err == nil && fallbackToken != "" {
		cache.warnLegacySlackBotTokenFallback(ctx, teamID)
	}
	return token, err
}

func (c *workspaceSlackTokenLookupCache) getOrStart(teamID string, at time.Time) ttlcache.Start[workspaceSlackTokenCacheValue] {
	return c.tokens.GetOrStart(teamID, at)
}

func (c *workspaceSlackTokenLookupCache) purge(teamID string) {
	teamID = strings.TrimSpace(teamID)
	if teamID == "" {
		return
	}
	c.tokens.InvalidateWith(teamID, func() {
		// InvalidateWith runs under the ttlcache lock; keep this hook
		// non-reentrant and limited to fallback warning sidecar cleanup.
		delete(c.fallbackWarned, teamID)
	})
}

func (c *workspaceSlackTokenLookupCache) warnLegacySlackBotTokenFallback(ctx context.Context, teamID string) {
	teamID = strings.TrimSpace(teamID)
	if teamID == "" || !c.markLegacySlackBotTokenFallbackWarned(teamID) {
		return
	}
	slog.LogAttrs(
		ctx,
		slog.LevelWarn,
		"legacy SLACK_BOT_TOKEN fallback is serving workspace without Slack install token",
		slog.String("team_id", slackOwnerLogID(teamID)),
	)
}

func (c *workspaceSlackTokenLookupCache) markLegacySlackBotTokenFallbackWarned(teamID string) bool {
	warn := false
	c.tokens.WithLock(func() {
		// WithLock holds the ttlcache mutex; keep this hook non-reentrant and
		// limited to fallback warning sidecar mutation.
		if c.fallbackWarned == nil {
			c.fallbackWarned = map[string]struct{}{}
		}
		if _, ok := c.fallbackWarned[teamID]; ok {
			return
		}
		c.fallbackWarned[teamID] = struct{}{}
		warn = true
	})
	return warn
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
	setupBindingReplayWindowHours, err := readSetupBindingReplayWindowHours()
	if err != nil {
		return oauth.Config{}, false, err
	}
	apiKeyMintReplayWindowHours, err := readAPIKeyMintReplayWindowHours()
	if err != nil {
		return oauth.Config{}, false, err
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
		Auth0Domain:                   domain,
		Auth0ClientID:                 clientID,
		Auth0ClientSecret:             clientSecret,
		Auth0Audience:                 audience,
		Auth0EmailConnection:          emailConnection,
		SlackBaseURL:                  baseURL,
		SetupBindingReplayWindowHours: setupBindingReplayWindowHours,
		APIKeyMintReplayWindowHours:   apiKeyMintReplayWindowHours,
		OAuthStateSecret:              []byte(stateSecret),
		Provider:                      provider,
		IDTokenVerifier:               verifier,
		Minter:                        &oauth.HTTPAPIKeyMinter{BaseURL: qurlEndpoint},
		AsyncTracker:                  tracker,
		AdminStore:                    adminStore,
		BindClassifyError:             internal.ClassifyOAuthBindError,
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
		extraScopes := slackinstall.NormalizeScopes([]string{raw})
		// Strip unsupported scopes (see slackinstall.DropUnsupportedScopes) from
		// operator overrides before Validate: a stale SLACK_BOT_SCOPES with
		// surviving valid scopes then warns instead of aborting startup. Required
		// defaults are always unioned back in, so legacy overrides such as
		// SLACK_BOT_SCOPES=commands stay deployable after new required scopes land.
		if kept, dropped := slackinstall.DropUnsupportedScopes(extraScopes); len(dropped) > 0 {
			extraScopes = kept
			slog.Warn("SLACK_BOT_SCOPES included views:write, which is not a real Slack scope; dropped it. Required qURL Slack scopes are still included automatically.")
		}
		scopes = slackinstall.NormalizeScopes(append(scopes, extraScopes...))
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

// readMaxConcurrentAsync sizes the main async pool from QURL_SLACK_MAX_CONCURRENT_ASYNC.
func readMaxConcurrentAsync() int { return readPoolSizeEnv("QURL_SLACK_MAX_CONCURRENT_ASYNC") }

// readMaxConcurrentFollowupAsync sizes the separate channel-follow-up pool (#712) from
// QURL_SLACK_MAX_CONCURRENT_FOLLOWUP_ASYNC.
func readMaxConcurrentFollowupAsync() int {
	return readPoolSizeEnv("QURL_SLACK_MAX_CONCURRENT_FOLLOWUP_ASYNC")
}

// readMaxConcurrentFollowupGateAsync sizes the short channel-follow-up gate pool (#719)
// from QURL_SLACK_MAX_CONCURRENT_FOLLOWUP_GATE_ASYNC.
func readMaxConcurrentFollowupGateAsync() int {
	return readPoolSizeEnv("QURL_SLACK_MAX_CONCURRENT_FOLLOWUP_GATE_ASYNC")
}

// readSetupBindingReplayWindowHours mirrors qurl-service's binding
// idempotency TTL for operator-facing setup retry logs. Empty preserves the
// upstream default; an explicit value must use the canonical Nh form because
// the emitted event fields are named *_hours.
func readSetupBindingReplayWindowHours() (int, error) {
	return readWholeHourDurationEnv(envQURLBindingTTLContract, oauth.DefaultSetupBindingReplayWindowHours)
}

// readAPIKeyMintReplayWindowHours mirrors qurl-service's API-key mint
// idempotency TTL for operator-facing rotation retry logs. Empty preserves the
// upstream default; an explicit value must use the canonical Nh form because
// the emitted event fields are named *_hours.
func readAPIKeyMintReplayWindowHours() (int, error) {
	return readWholeHourDurationEnv(envQURLAPIKeyMintTTLContract, oauth.DefaultAPIKeyMintReplayWindowHours)
}

func readWholeHourDurationEnv(envName string, defaultHours int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return defaultHours, nil
	}
	invalidReplayWindowErr := fmt.Errorf("%s=%q must be a positive whole-hour duration in canonical Nh form such as 24h", envName, raw)
	hoursText, ok := strings.CutSuffix(raw, "h")
	if !ok || hoursText == "" || strings.HasPrefix(hoursText, "0") {
		return 0, invalidReplayWindowErr
	}
	for i := range hoursText {
		if hoursText[i] < '0' || hoursText[i] > '9' {
			return 0, invalidReplayWindowErr
		}
	}
	// No upper cap: qurl-service owns the replay TTL contract, and this
	// value only feeds operator-facing log fields, so a smaller
	// Slack-only limit could become another drift source.
	hours, err := strconv.Atoi(hoursText)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is too large to fit in an hour value", envName, raw)
	}
	return hours, nil
}

// readTunnelImageConfig makes the fallback policy explicit at startup. The
// handler still treats an empty TunnelImage as "render the dev/sandbox fallback"
// so focused tests can exercise that branch, but production cmd/main.go only
// passes empty after an operator opt-in. The pinning checks stay here because
// they are deploy-time policy; internal renderers keep only snippet-safety
// validation for direct Config construction in tests.
func readTunnelImageConfig() (string, error) {
	image := strings.TrimSpace(os.Getenv(envQURLConnectorImage))
	if err := internal.ValidateTunnelImageRef(image); err != nil {
		return "", fmt.Errorf("%s: %w", envQURLConnectorImage, err)
	}
	if image != "" {
		// An explicit image wins over QURL_CONNECTOR_IMAGE_FALLBACK so stale
		// dev/sandbox env cannot break a correctly pinned production image.
		// Keep startup errors explicit: they land in operator logs, and each
		// branch carries the remediation so bad image config cannot be masked.
		return readPinnedImageConfig(envQURLConnectorImage, image, connectorImageFallbackHint)
	}

	rawFallback := strings.TrimSpace(os.Getenv(envQURLConnectorImageFallback))
	fallback := strings.ToLower(rawFallback)
	switch fallback {
	case connectorImageFallbackSandbox:
		return "", nil
	case "":
		return "", fmt.Errorf("%s is required unless %s explicitly opts into the dev/sandbox fallback", envQURLConnectorImage, connectorImageFallbackOptIn)
	default:
		return "", fmt.Errorf("%s=%q is unsupported; set %s only for dev/sandbox, or set %s to a specific non-latest tag or digest", envQURLConnectorImageFallback, rawFallback, connectorImageFallbackOptIn, envQURLConnectorImage)
	}
}

func readS3OriginImageConfig() (string, error) {
	image := strings.TrimSpace(os.Getenv(envQURLS3OriginImage))
	if image == "" {
		return "", nil
	}
	if err := internal.ValidateTunnelImageRef(image); err != nil {
		return "", fmt.Errorf("%s: %w", envQURLS3OriginImage, err)
	}
	pinned, err := readPinnedImageConfig(envQURLS3OriginImage, image, "")
	if err != nil {
		return "", err
	}
	// readPinnedImageConfig owns startup's detailed operator diagnostics;
	// RequireS3OriginImageDigest deliberately classifies again because it is
	// also the shared render-boundary guard and narrows accepted pins to sha256.
	if err := internal.RequireS3OriginImageDigest(pinned); err != nil {
		return "", fmt.Errorf("%s: %w", envQURLS3OriginImage, err)
	}
	return pinned, nil
}

func readPinnedImageConfig(envName, image, floatingHint string) (string, error) {
	switch connectorimage.ClassifyPin(image) {
	case connectorimage.Accepted:
		return image, nil
	case connectorimage.LatestDigest:
		return "", fmt.Errorf(
			"%s: %s",
			envName, connectorImageErrLatestDigest,
		)
	case connectorimage.UppercaseDigest:
		return "", fmt.Errorf(
			"%s: %s",
			envName, connectorImageErrDigestLowercase,
		)
	case connectorimage.MalformedReference:
		return "", fmt.Errorf(
			"%s: %s",
			envName, connectorImageErrMalformedRef,
		)
	case connectorimage.AmbiguousReference:
		return "", fmt.Errorf(
			"%s: %s",
			envName, connectorImageErrAmbiguousRef,
		)
	case connectorimage.MalformedDigest:
		return "", fmt.Errorf(
			"%s: %s",
			envName, connectorImageErrMalformedDigest,
		)
	case connectorimage.Floating:
		msg := connectorImageErrFloating
		if floatingHint != "" {
			msg += "; " + floatingHint
		}
		return "", fmt.Errorf("%s: %s", envName, msg)
	}
	// Future connectorimage.PinStatus values must fail closed.
	return "", fmt.Errorf("%s could not validate image pinning", envName)
}

// readPoolSizeEnv parses a pool-size env var. Empty is "use default" silently;
// non-empty-but-malformed env is a misconfiguration surfaced at startup so it doesn't get
// discovered during a saturation incident. The returned 0 tells NewHandler to use that
// pool's built-in default. A non-positive value is more likely a typo or env-substitution
// mishap than an intentional choice, so it's surfaced like malformed input rather than
// silently swallowed.
func readPoolSizeEnv(name string) int {
	raw := os.Getenv(name)
	if raw == "" {
		return 0
	}
	parsed, err := strconv.Atoi(raw)
	switch {
	case err != nil:
		slog.Warn("ignoring malformed pool-size env var; falling back to default", //nolint:gosec // G706: raw is env-var input; slog's JSON handler escapes control bytes in attribute values, same posture as the request-path slog sites.
			"env", name, "raw", raw, "error", err)
		return 0
	case parsed <= 0:
		slog.Warn("ignoring non-positive pool-size env var; falling back to default", //nolint:gosec // G706: raw is env-var input; slog's JSON handler escapes control bytes in attribute values, same posture as the request-path slog sites.
			"env", name, "raw", raw)
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
		slog.Warn("env flag set to an unparseable value; using the fail-safe default", //nolint:gosec // G706: operator-set flag value, not a secret; slog's JSON handler escapes control bytes like the other env-logging sites.
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

// readAgentChannelFollowups reads QURL_AGENT_CHANNEL_FOLLOWUPS — the flag that lets
// the agent answer non-@mention thread replies in channel threads it already joined
// (Handler.agentChannelFollowupsEnabled). Absent → off. FAILS SAFE to off: enabling
// it means the manifest subscribes message.channels/groups, so the bot then receives
// every message in channels it's a member of — a data-handling expansion that a typo
// must never turn on. Only takes effect once the read-only surface is live.
func readAgentChannelFollowups() bool {
	return readBoolEnvFailSafe("QURL_AGENT_CHANNEL_FOLLOWUPS", false, false)
}

// readAgentSurfaceExclusiveAcks reads QURL_AGENT_SURFACE_EXCLUSIVE_ACKS — the flag
// that swaps pane (message.im) turns from the staged additive fallback (reaction +
// Debug-level setStatus attempt) to the post-pane exclusive ack path (native status
// only, Warn on setStatus failure). Absent → off until the #1004 manifest/smoke gate
// confirms the assistant pane is live. FAILS SAFE to off: a typo must never remove
// the pre-enable reaction cue from ordinary DMs or create Warn-per-turn noise.
func readAgentSurfaceExclusiveAcks() bool {
	return readBoolEnvFailSafe("QURL_AGENT_SURFACE_EXCLUSIVE_ACKS", false, false)
}

// readAgentDefaultEnabled reads QURL_AGENT_DEFAULT_ENABLED — the per-workspace
// conversation-mode default for a workspace that hasn't set its own toggle. Absent →
// off (the staged opt-in rollout; each workspace opts in via `/qurl-admin agent on`).
// GA flips it true (every workspace on unless it explicitly opted out). FAILS SAFE to
// off, so a typo can never silently turn the surface on for every workspace at once.
func readAgentDefaultEnabled() bool {
	return readBoolEnvFailSafe("QURL_AGENT_DEFAULT_ENABLED", false, false)
}

// readSlackRateLimitEnabled reads QURL_SLACK_RATE_LIMIT_ENABLED, the staged
// opt-in for the DDB-backed in-bot `/qurl get` + `/qurl aliases` gate. Absent
// and malformed both fail safe to OFF so sandbox/open-gate deployments stay
// unchanged until production explicitly enables the write path.
func readSlackRateLimitEnabled() bool {
	return readBoolEnvFailSafe(envSlackRateLimitEnabled, false, false)
}

// readSlackBotTokenRotationEnabled reads QURL_SLACK_BOT_TOKEN_ROTATION_ENABLED.
// Absent → false, preserving today's Marketplace cleanup behavior where a bot
// tokens_revoked callback means local teardown. A set-but-unparseable value
// fails startup: silently choosing either pole could suppress a Marketplace
// cleanup signal or make a routine Slack token rotation destructive.
func readSlackBotTokenRotationEnabled() (bool, error) {
	raw := strings.TrimSpace(os.Getenv(envSlackBotTokenRotation))
	if raw == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", envSlackBotTokenRotation, err)
	}
	return v, nil
}

// Conservative per-hour turn caps applied when the operator doesn't set the env, so
// a GA-live agent always has a cost backstop. Tunable via QURL_AGENT_MAX_TURNS_PER_*;
// an explicit 0 disables the cap.
const (
	defaultAgentMaxTurnsPerUser = 30
	defaultAgentMaxTurnsPerTeam = 300
)

// readAgentMaxTurnsPerUser / readAgentMaxTurnsPerTeam read the per-user and
// per-workspace agent-turn caps (turns per rolling hour) used as an LLM-cost
// backstop. Absent → the conservative default; an explicit "0" disables the cap.
func readAgentMaxTurnsPerUser() int {
	return readIntEnvFailSafe("QURL_AGENT_MAX_TURNS_PER_USER_PER_HOUR", defaultAgentMaxTurnsPerUser)
}

func readAgentMaxTurnsPerTeam() int {
	return readIntEnvFailSafe("QURL_AGENT_MAX_TURNS_PER_TEAM_PER_HOUR", defaultAgentMaxTurnsPerTeam)
}

// readIntEnvFailSafe reads a non-negative integer env var. Absent → def. A
// set-but-malformed or negative value FAILS SAFE to def and logs loudly (a negative
// cap is a typo, not a choice). An explicit "0" is honored and distinct from absent —
// callers use it as a sentinel (e.g. "unlimited" for the turn caps).
func readIntEnvFailSafe(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		slog.Warn("agent env int flag set to an invalid value; using the fail-safe default", //nolint:gosec // G706: operator-set flag value, not a secret; slog's JSON handler escapes control bytes like the other env-logging sites.
			"env", name, "value", raw, "fail_safe_default", def)
		return def
	}
	return v
}

// agentSurfaceState groups the boot-time facts that decide what conversation mode
// does — a struct so logAgentSurfaceState's growing set of seam booleans can't be
// transposed at the call site.
type agentSurfaceState struct {
	llmWired              bool
	storeWired            bool
	postWired             bool
	blocksWired           bool
	ephemeralBlocksWired  bool // PostEphemeralBlocks — confirm-flow channel get-link delivery
	assistantThreadsWired bool
	confirmFlag           bool // QURL_AGENT_CONFIRM_ENABLED
	exclusiveAcksFlag     bool // QURL_AGENT_SURFACE_EXCLUSIVE_ACKS
	killed                bool
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

	// Confirm/mutation mode sits ON TOP of the read-only surface. Split into its own
	// helper so each stays under the cyclomatic-complexity budget.
	logConfirmModeState(s, readOnlyLive)

	if !s.killed && s.exclusiveAcksFlag && !s.assistantThreadsWired {
		slog.Warn("QURL_AGENT_SURFACE_EXCLUSIVE_ACKS is set but AssistantThreads is not wired; pane turns will not have a working-on-it indicator")
	}
}

// logConfirmModeState emits the confirm/mutation startup line(s), keyed on the
// EFFECTIVE predicate (Handler.agentConfirmEnabled), never the raw flag — a flag
// set while the surface is dark must NOT read as enabled (the #670 LIVE-gate
// consistency lesson, applied to the riskier flip). readOnlyLive is the read-only
// surface verdict computed by logAgentSurfaceState.
func logConfirmModeState(s agentSurfaceState, readOnlyLive bool) {
	switch {
	case readOnlyLive && s.confirmFlag && s.blocksWired:
		slog.Warn("conversation mode CONFIRM (mutation execution) is LIVE: an admin Approving a card EXECUTES the change. Confirm the hard pre-enablement gates (get-link authorization; R2 public-card replace_original; C1 connector key-privacy; C2 connector trigger-window) AND the DPA/data-handling review have cleared before relying on this.")
		// A confirmed CHANNEL get delivers the minted link via PostEphemeralBlocks with
		// NO text fallback (deliverConfirmEphemeral), so if it's unwired every channel
		// get approval fails AFTER the mint is burned. (The confirm DM leg rides on
		// PostMessageBlocks — already required by the gate above — and PostDMBlocks is a
		// separate `/qurl get dm:true` concern, refused pre-mint, so neither belongs
		// here.) WARN, not gate: unlike PostMessageBlocks (which renders the confirm CARD
		// for EVERY action, so its absence correctly forces confirm DARK), PostEphemeralBlocks
		// is needed only for GET delivery — folding it into the agentConfirmEnabled gate
		// would also disable revoke/alias/protect confirms that don't touch it. So the
		// targeted choice is to keep confirm live and surface the risk loudly at boot,
		// rather than as a silent per-request post-mint failure in production.
		if !s.ephemeralBlocksWired {
			slog.Warn("CONFIRM is LIVE but PostEphemeralBlocks is unwired; agent channel get approvals will FAIL after minting (no text fallback). Wire PostEphemeralBlocks.",
				"ephemeral_blocks_wired", s.ephemeralBlocksWired)
		}
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
	rateLimitEnabled := readSlackRateLimitEnabled()
	s, err := slackdata.NewStore(ctx, slackdata.WithRateLimitEnabled(rateLimitEnabled))
	if err != nil {
		slog.Error("slackdata.NewStore failed; /qurl-admin admin will be disabled", "error", err)
		return nil
	}
	slog.Info("admin store wired", //nolint:gosec // G706: env-var values are operator-controlled; slog's JSON handler escapes any control bytes the same way as the request-path slog sites.
		"workspace_mappings_table", os.Getenv(slackdata.EnvWorkspaceMappingsTable),
		"channel_policies_table", os.Getenv(slackdata.EnvChannelPoliciesTable),
		"slack_rate_limit_enabled", rateLimitEnabled)
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
