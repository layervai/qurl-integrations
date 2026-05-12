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
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	listenAddr = ":8080"
	// shutdownTimeout sits inside Fargate's 30s SIGTERM→SIGKILL window with
	// 5s of headroom for the container runtime to actually deliver SIGKILL
	// and reap the process. This is the cap on the drain *as a whole*, not
	// per request — http.Server.WriteTimeout (15s) still bounds individual
	// in-flight handlers; bumping shutdownTimeout above 25s won't extend
	// long-running handlers, only the wait for short ones to drain.
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

	// Optional pool-cap override. Empty env is "use default" silently;
	// non-empty-but-malformed env is a misconfiguration we surface at
	// startup so it doesn't get discovered during a saturation
	// incident. Either way the value reaching NewHandler may be 0,
	// which Handler interprets as "use the built-in default (50)".
	maxConcurrentAsync := 0
	if raw := os.Getenv("QURL_SLACK_MAX_CONCURRENT_ASYNC"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			slog.Warn("ignoring malformed QURL_SLACK_MAX_CONCURRENT_ASYNC; falling back to default", //nolint:gosec // G706: raw is env-var input; slog's JSON handler escapes control bytes in attribute values, same posture as the request-path slog sites.
				"raw", raw, "error", err)
		case parsed <= 0:
			// NewHandler treats 0/negative as "use default", but a
			// negative value is more likely a typo or env-substitution
			// mishap than an intentional choice — surface it the same
			// way as malformed input so it doesn't silently swallow.
			slog.Warn("ignoring non-positive QURL_SLACK_MAX_CONCURRENT_ASYNC; falling back to default", //nolint:gosec // G706: raw is env-var input; slog's JSON handler escapes control bytes in attribute values, same posture as the request-path slog sites.
				"raw", raw)
		default:
			maxConcurrentAsync = parsed
		}
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

	// Compose the top-level mux: existing Slack-bot routes (handled
	// by the internal.Handler.ServeHTTP fall-through) + new OAuth
	// routes wired into a sibling ServeMux. The mux is what serves;
	// the internal handler stays unchanged.
	rootMux := http.NewServeMux()
	rootMux.Handle("/", handler)
	if oauthCfg, ok := buildOAuthConfig(signalCtx, ddbProvider); ok {
		oauth.RegisterRoutes(rootMux, oauthCfg)
		slog.Info("registered /oauth/qurl/{start,callback} routes")
		handler.SetOAuthSetup(oauth.SetupConfig{
			StateSecret:  oauthCfg.OAuthStateSecret,
			SlackBaseURL: oauthCfg.SlackBaseURL,
		})
	} else {
		slog.Warn("OAuth routes NOT registered — required env vars unset (AUTH0_DOMAIN, AUTH0_CLIENT_ID, AUTH0_CLIENT_SECRET, AUTH0_AUDIENCE, SLACK_BASE_URL, OAUTH_STATE_SECRET)")
	}

	srv := &http.Server{
		// Addr intentionally omitted: srv.Serve(ln) ignores it, and we
		// bind via net.ListenConfig below. Setting it would mislead a
		// future reader into thinking it controls the bind.
		Handler:           rootMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
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

// minStateSecretBytes is the operator floor for OAUTH_STATE_SECRET. HMAC-
// SHA256 will compute over any length, but a short operator-typed value
// is the kind of weak-CSRF posture worth failing-fast on at startup
// rather than discovering after a key-takeover incident.
const minStateSecretBytes = 32

// newJWKSVerifier is overridable in tests so the env-var-table tests
// don't hit the real internet trying to fetch example.auth0.com's JWKS
// (and don't burn the 5s prime budget in airgapped CI). Production
// calls oauth.NewJWKSVerifier directly via this seam.
var newJWKSVerifier = func(ctx context.Context, issuer, audience string) (oauth.IDTokenVerifier, error) {
	return oauth.NewJWKSVerifier(ctx, issuer, audience)
}

// buildOAuthConfig assembles the oauth.Config from env. Returns
// (cfg, false) when any required env var is missing or invalid —
// the caller logs and skips route registration so a sandbox boot with
// no Auth0 configured still serves the existing Slack surface.
//
// Required env vars:
//
//	AUTH0_DOMAIN, AUTH0_CLIENT_ID, AUTH0_CLIENT_SECRET, AUTH0_AUDIENCE
//	SLACK_BASE_URL, OAUTH_STATE_SECRET, QURL_ENDPOINT
//
// ctx is the parent context for the JWKS refresh goroutine spawned
// inside NewJWKSVerifier — pass the signal-canceled context so the
// goroutine tears down on SIGTERM.
func buildOAuthConfig(ctx context.Context, provider *auth.DDBProvider) (oauth.Config, bool) {
	domain := os.Getenv("AUTH0_DOMAIN")
	clientID := os.Getenv("AUTH0_CLIENT_ID")
	clientSecret := os.Getenv("AUTH0_CLIENT_SECRET")
	audience := os.Getenv("AUTH0_AUDIENCE")
	baseURL := os.Getenv("SLACK_BASE_URL")
	stateSecret := os.Getenv("OAUTH_STATE_SECRET")
	qurlEndpoint := os.Getenv("QURL_ENDPOINT")

	if domain == "" || clientID == "" || clientSecret == "" || audience == "" ||
		baseURL == "" || stateSecret == "" || qurlEndpoint == "" {
		return oauth.Config{}, false
	}
	if len(stateSecret) < minStateSecretBytes {
		//nolint:gosec // G706: integer attribute values are not a log-injection vector; slog's JSON handler escapes them.
		slog.Error("OAUTH_STATE_SECRET is shorter than required minimum",
			"min_bytes", minStateSecretBytes, "got_bytes", len(stateSecret))
		return oauth.Config{}, false
	}

	// JWKS verifier opens the network for the initial JWKS fetch (bounded
	// inside NewJWKSVerifier). If it fails, the callback proceeds without
	// email-claim verification — the qURL key still gets minted; only the
	// success-page email line is missing.
	var verifier oauth.IDTokenVerifier
	issuer := "https://" + domain + "/"
	if v, err := newJWKSVerifier(ctx, issuer, clientID); err != nil {
		slog.Warn("JWKS verifier init failed — id_token email will not be displayed", "error", err)
	} else {
		verifier = v
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
		// SlackClient left nil for now — DM-after-success Slack-API
		// wiring is a follow-up; the success-page HTML still renders.
	}, true
}
