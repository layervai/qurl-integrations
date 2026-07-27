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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	teamsbot "github.com/layervai/qurl-integrations/apps/teams/internal"
	"github.com/layervai/qurl-integrations/apps/teams/internal/connectorimage"
	"github.com/layervai/qurl-integrations/apps/teams/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/teams/internal/teamsdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/observability"
)

const (
	listenAddr       = ":8080"
	shutdownTimeout  = 25 * time.Second
	lameduckDuration = 13 * time.Second

	envTeamsAppID                 = "TEAMS_APP_ID"
	envTeamsAppPassword           = "TEAMS_APP_PASSWORD"
	envMicrosoftAppID             = "MICROSOFT_APP_ID"
	envMicrosoftAppPassword       = "MICROSOFT_APP_PASSWORD"
	envTeamsBaseURL               = "TEAMS_BASE_URL"
	envFeedbackWebhookURL         = "FEEDBACK_WEBHOOK_URL"
	envTeamsSkipBotAuth           = "QURL_TEAMS_SKIP_BOT_AUTH"
	envQURLEndpoint               = "QURL_ENDPOINT"
	envQURLConnectorImage         = "QURL_CONNECTOR_IMAGE"
	envQURLConnectorImageFallback = "QURL_CONNECTOR_IMAGE_FALLBACK"
	envOAuthStateSecret           = "OAUTH_STATE_SECRET"
	envAuth0Domain                = "AUTH0_DOMAIN"
	envAuth0ClientID              = "AUTH0_CLIENT_ID"
	envAuth0ClientSecret          = "AUTH0_CLIENT_SECRET"
	envAuth0Audience              = "AUTH0_AUDIENCE"
	envAuth0EmailConnection       = "AUTH0_EMAIL_CONNECTION"

	connectorImageFallbackSandbox = "dev-sandbox"
	connectorImageFallbackOptIn   = envQURLConnectorImageFallback + "=" + connectorImageFallbackSandbox
	connectorImageFallbackHint    = "dev/sandbox fallback requires leaving " + envQURLConnectorImage + " empty and setting " + connectorImageFallbackOptIn

	connectorImageErrFloating        = "missing or latest tag; use a specific non-latest tag or image@sha256:<64 lowercase hex>"
	connectorImageErrLatestDigest    = "latest tag is not allowed with digest pins; drop :latest or use a specific non-latest tag before the digest"
	connectorImageErrDigestLowercase = "digest must be sha256:<64 lowercase hex>"
	connectorImageErrMalformedRef    = "invalid image reference; use lowercase image:tag or lowercase image@sha256:<64 lowercase hex>"
	connectorImageErrAmbiguousRef    = "ambiguous slashless registry ref; include a repository path such as gcr.io/<org>/<image>:v1"
	connectorImageErrMalformedDigest = "invalid digest ref; use image@sha256:<64 lowercase hex> with a full image name, not bare sha256:<digest>"
)

var version = "dev"

func main() {
	slog.SetDefault(newAppLogger(os.Stdout))
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func newAppLogger(w io.Writer) *slog.Logger {
	return slog.New(observability.NewRedactingJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func run() error {
	qurlEndpoint := strings.TrimRight(strings.TrimSpace(os.Getenv(envQURLEndpoint)), "/")
	if qurlEndpoint == "" {
		return fmt.Errorf("%s is required", envQURLEndpoint)
	}
	userAgent := "qurl-teams/" + version

	appID := firstNonEmpty(strings.TrimSpace(os.Getenv(envTeamsAppID)), strings.TrimSpace(os.Getenv(envMicrosoftAppID)))
	if appID == "" {
		return fmt.Errorf("%s or %s is required", envTeamsAppID, envMicrosoftAppID)
	}
	appPassword := firstNonEmpty(os.Getenv(envTeamsAppPassword), os.Getenv(envMicrosoftAppPassword))
	if strings.TrimSpace(appPassword) == "" {
		return fmt.Errorf("%s or %s is required", envTeamsAppPassword, envMicrosoftAppPassword)
	}
	tunnelImage, err := readTunnelImageConfig()
	if err != nil {
		return err
	}

	shutdownSignals := newShutdownSignalSource(syscall.SIGTERM, syscall.SIGINT)
	defer shutdownSignals.stop()

	ddbProvider, err := auth.NewDDBProvider(shutdownSignals.ctx,
		auth.WithTableName(os.Getenv(auth.EnvWorkspaceStateTable)),
		auth.WithKMSKeyARN(os.Getenv(auth.EnvWorkspaceStateKMSKeyARN)),
	)
	if err != nil {
		return fmt.Errorf("init workspace provider: %w", err)
	}
	adminStore, err := teamsdata.NewStore(shutdownSignals.ctx)
	if err != nil {
		return fmt.Errorf("init teams admin store: %w", err)
	}

	setupCfg := oauth.SetupConfig{
		StateSecret:  []byte(os.Getenv(envOAuthStateSecret)),
		TeamsBaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv(envTeamsBaseURL)), "/"),
	}
	oauthCfg, oauthEnabled, err := buildOAuthConfig(shutdownSignals.ctx, ddbProvider, adminStore)
	if err != nil {
		return err
	}

	handlerCtx, cancelHandler := context.WithCancel(context.Background())
	defer cancelHandler()

	var feedback teamsbot.FeedbackPoster
	if webhookURL := strings.TrimSpace(os.Getenv(envFeedbackWebhookURL)); webhookURL != "" {
		if _, err := teamsbot.ValidateFeedbackWebhookURL(webhookURL); err != nil {
			slog.Warn("feedback disabled", "error", err)
		} else {
			feedback = &teamsbot.WebhookFeedbackPoster{
				URL:       webhookURL,
				UserAgent: userAgent,
			}
		}
	}

	mux := http.NewServeMux()
	health := newHealthHandler()
	mux.Handle("/health", health)
	if oauthEnabled {
		oauth.RegisterRoutes(mux, oauthCfg)
	} else {
		slog.Warn("Teams OAuth routes not registered because required env vars are missing")
	}

	skipBotAuth := readBoolEnv(envTeamsSkipBotAuth)
	handler := teamsbot.NewHandler(&teamsbot.HandlerConfig{
		BaseContext:  handlerCtx,
		QURLEndpoint: qurlEndpoint,
		AuthProvider: ddbProvider,
		AdminStore:   adminStore,
		Messages: &teamsbot.ConnectorClient{
			AppID:       appID,
			AppPassword: appPassword,
		},
		TokenAuth:   teamsbot.NewIncomingTokenValidator(handlerCtx, appID),
		Setup:       setupCfg,
		Feedback:    feedback,
		TunnelImage: tunnelImage,
		SkipBotAuth: skipBotAuth,
		UserAgent:   userAgent,
	})
	mux.Handle("/teams/messages", handler)

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      75 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", listenAddr, err)
	}

	runtime := teamsShutdownHandler{
		health:  health,
		handler: handler,
	}
	shutdownDone := make(chan struct{})
	var shutdownOnce sync.Once
	runShutdown := func(duck time.Duration) {
		shutdownOnce.Do(func() {
			runShutdownSequence(server, runtime, cancelHandler, duck, shutdownTimeout, sleepContext)
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

	if skipBotAuth {
		slog.Warn("BOT FRAMEWORK AUTH DISABLED — dev/test only; do not use in production")
	}
	//nolint:gosec // Startup logging here reports only fixed process configuration, not request-derived input.
	slog.Info("teams bot listening", "addr", listenAddr, "oauth_enabled", oauthEnabled, "skip_bot_auth", skipBotAuth)
	serveErr := server.Serve(ln)

	runShutdown(0)
	shutdownSignals.stop()
	<-signalWatcherDone
	<-shutdownDone

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

func buildOAuthConfig(ctx context.Context, provider *auth.DDBProvider, adminStore *teamsdata.Store) (oauth.Config, bool, error) {
	domain := strings.TrimRight(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(os.Getenv(envAuth0Domain), "https://"), "http://")), "/")
	clientID := strings.TrimSpace(os.Getenv(envAuth0ClientID))
	clientSecret := os.Getenv(envAuth0ClientSecret)
	audience := strings.TrimSpace(os.Getenv(envAuth0Audience))
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv(envTeamsBaseURL)), "/")
	stateSecret := os.Getenv(envOAuthStateSecret)
	qurlEndpoint := strings.TrimRight(strings.TrimSpace(os.Getenv(envQURLEndpoint)), "/")
	emailConnection := strings.TrimSpace(os.Getenv(envAuth0EmailConnection))

	missing := missingOAuthEnvVars(map[string]string{
		envAuth0Domain:       domain,
		envAuth0ClientID:     clientID,
		envAuth0ClientSecret: clientSecret,
		envAuth0Audience:     audience,
		envTeamsBaseURL:      baseURL,
		envOAuthStateSecret:  stateSecret,
		envQURLEndpoint:      qurlEndpoint,
	})
	if len(missing) > 0 {
		slog.Warn("Teams OAuth routes not registered", "missing", missing)
		return oauth.Config{}, false, nil
	}
	if !strings.HasPrefix(baseURL, "https://") {
		return oauth.Config{}, false, fmt.Errorf("%s must be https://", envTeamsBaseURL)
	}
	if len(stateSecret) < oauth.StateMinSecret {
		return oauth.Config{}, false, fmt.Errorf("%s shorter than required minimum of %d bytes", envOAuthStateSecret, oauth.StateMinSecret)
	}
	if strings.ContainsRune(domain, '/') {
		return oauth.Config{}, false, fmt.Errorf("%s must be a bare host", envAuth0Domain)
	}
	if u, err := url.Parse(baseURL); err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return oauth.Config{}, false, fmt.Errorf("%s must be a bare https origin with no path", envTeamsBaseURL)
	}

	issuer := "https://" + domain + "/"
	verifier, err := oauth.NewJWKSVerifier(ctx, issuer, clientID)
	if err != nil {
		return oauth.Config{}, false, fmt.Errorf("init Auth0 JWKS verifier: %w", err)
	}

	return oauth.Config{
		Auth0Domain:          domain,
		Auth0ClientID:        clientID,
		Auth0ClientSecret:    clientSecret,
		Auth0Audience:        audience,
		Auth0EmailConnection: emailConnection,
		TeamsBaseURL:         baseURL,
		OAuthStateSecret:     []byte(stateSecret),
		Provider:             provider,
		IDTokenVerifier:      verifier,
		Minter:               &oauth.HTTPAPIKeyMinter{BaseURL: qurlEndpoint},
		AdminStore:           teamsbot.NewOAuthAdminStore(adminStore),
		BindClassifyError:    teamsbot.ClassifyOAuthBindError,
	}, true, nil
}

func missingOAuthEnvVars(vals map[string]string) []string {
	keys := []string{
		envAuth0Domain,
		envAuth0ClientID,
		envAuth0ClientSecret,
		envAuth0Audience,
		envTeamsBaseURL,
		envOAuthStateSecret,
		envQURLEndpoint,
	}
	var missing []string
	for _, k := range keys {
		if strings.TrimSpace(vals[k]) == "" {
			missing = append(missing, k)
		}
	}
	return missing
}

func readBoolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func readTunnelImageConfig() (string, error) {
	image := strings.TrimSpace(os.Getenv(envQURLConnectorImage))
	if err := teamsbot.ValidateTunnelImageRef(image); err != nil {
		return "", fmt.Errorf("%s: %w", envQURLConnectorImage, err)
	}
	if image != "" {
		switch connectorimage.ClassifyPin(image) {
		case connectorimage.Accepted:
			return image, nil
		case connectorimage.LatestDigest:
			return "", fmt.Errorf("%s: %s", envQURLConnectorImage, connectorImageErrLatestDigest)
		case connectorimage.UppercaseDigest:
			return "", fmt.Errorf("%s: %s", envQURLConnectorImage, connectorImageErrDigestLowercase)
		case connectorimage.MalformedReference:
			return "", fmt.Errorf("%s: %s", envQURLConnectorImage, connectorImageErrMalformedRef)
		case connectorimage.AmbiguousReference:
			return "", fmt.Errorf("%s: %s", envQURLConnectorImage, connectorImageErrAmbiguousRef)
		case connectorimage.MalformedDigest:
			return "", fmt.Errorf("%s: %s", envQURLConnectorImage, connectorImageErrMalformedDigest)
		case connectorimage.Floating:
			return "", fmt.Errorf("%s: %s; %s", envQURLConnectorImage, connectorImageErrFloating, connectorImageFallbackHint)
		}
		return "", fmt.Errorf("%s could not validate image pinning", envQURLConnectorImage)
	}

	rawFallback := strings.TrimSpace(os.Getenv(envQURLConnectorImageFallback))
	switch strings.ToLower(rawFallback) {
	case connectorImageFallbackSandbox:
		return "", nil
	case "":
		return "", fmt.Errorf("%s is required unless %s explicitly opts into the dev/sandbox fallback", envQURLConnectorImage, connectorImageFallbackOptIn)
	default:
		return "", fmt.Errorf("%s=%q is unsupported; set %s only for dev/sandbox, or set %s to a specific non-latest tag or digest", envQURLConnectorImageFallback, rawFallback, connectorImageFallbackOptIn, envQURLConnectorImage)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func remainingShutdownBudget(start time.Time, budget time.Duration) time.Duration {
	if budget <= 0 {
		return 0
	}
	remaining := budget - time.Since(start)
	if remaining < 0 {
		return 0
	}
	return remaining
}

type healthHandler struct {
	healthy atomic.Bool
}

func newHealthHandler() *healthHandler {
	h := &healthHandler{}
	h.healthy.Store(true)
	return h
}

func (h *healthHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if h != nil && !h.healthy.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *healthHandler) SetHealthy(healthy bool) {
	if h == nil {
		return
	}
	h.healthy.Store(healthy)
}

type teamsShutdownHandler struct {
	health  *healthHandler
	handler *teamsbot.Handler
}

func (h teamsShutdownHandler) SetHealthy(healthy bool) {
	if h.health != nil {
		h.health.SetHealthy(healthy)
	}
}

func (h teamsShutdownHandler) WaitTimeout(d time.Duration) bool {
	if h.handler == nil {
		return true
	}
	return h.handler.WaitTimeout(d)
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
	drainBudget := remainingShutdownBudget(shutdownStart, timeout)
	if lameduckBudgetExhausted {
		drainBudget = 0
	}
	if !handler.WaitTimeout(drainBudget) {
		slog.Warn("teams async drain timed out — exiting with workers still in flight", "budget", drainBudget)
	}
}

func lameduckForSignal(sig os.Signal) time.Duration {
	if sig == syscall.SIGTERM {
		return lameduckDuration
	}
	return 0
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
