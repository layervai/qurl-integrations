package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
)

// noopVerifier replaces oauth.NewJWKSVerifier in tests so buildOAuthConfig
// doesn't hit the real internet trying to prime example.auth0.com's JWKS.
type noopVerifier struct{}

func (noopVerifier) VerifyEmail(_ context.Context, _ string) (string, error) {
	return "", errors.New("noopVerifier: unused in env-var tests")
}

func (noopVerifier) VerifySub(_ context.Context, _ string) (string, error) {
	return "", errors.New("noopVerifier: unused in env-var tests")
}

// newFakeProvider builds the minimum-viable DDBProvider buildOAuthConfig
// will accept. The test only inspects the (cfg, ok) return — no DDB or
// KMS calls are made through the returned provider.
func newFakeProvider() *auth.DDBProvider {
	return &auth.DDBProvider{}
}

const (
	validStateSecret          = "0123456789abcdef0123456789abcdef" // 32 bytes; matches minStateSecretBytes.
	defaultSlackBotScopesCSV  = "commands,chat:write,im:write"
	testConnectorImageRepo    = "ghcr.io/layervai/qurl-connector"
	testConnectorVersionImage = testConnectorImageRepo + ":v1.2.3"
	testConnectorLatestImage  = testConnectorImageRepo + ":latest"
)

var oauthEnvKeys = []string{
	"AUTH0_DOMAIN", "AUTH0_CLIENT_ID", "AUTH0_CLIENT_SECRET", "AUTH0_AUDIENCE",
	"SLACK_BASE_URL", "OAUTH_STATE_SECRET", "QURL_ENDPOINT",
}

var slackInstallEnvKeys = []string{
	envSlackClientID, envSlackClientSecret, "SLACK_BASE_URL",
	envSlackInstallStateSecret, "OAUTH_STATE_SECRET", envSlackBotScopes,
}

func TestNewAppLoggerEmitsJSONMsgKey(t *testing.T) {
	var buf bytes.Buffer
	newAppLogger(&buf).Info("contract check", "team_id", "T1")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("unmarshal app log %q: %v", buf.String(), err)
	}
	if rec["msg"] != "contract check" {
		t.Fatalf("msg = %v, want contract check", rec["msg"])
	}
	if rec["level"] != "INFO" {
		t.Fatalf("level = %v, want INFO", rec["level"])
	}
	if rec["team_id"] != "T1" {
		t.Fatalf("team_id = %v, want T1", rec["team_id"])
	}
}

func TestRunOperatorSubcommandHandlesSlackMarkdownValidation(t *testing.T) {
	t.Setenv(envSlackMarkdownValidationToken, "")
	t.Setenv(envSlackMarkdownValidationChannel, "")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	handled, err := runOperatorSubcommand([]string{"qurl-bot-slack", slackMarkdownValidationSubcommand}, &stdout, &stderr)
	if !handled {
		t.Fatal("operator subcommand was not handled")
	}
	if err == nil || !strings.Contains(err.Error(), envSlackMarkdownValidationToken) {
		t.Fatalf("err = %v, want validation token requirement", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("operator config error wrote stdout JSON: %q", stdout.String())
	}
}

func validEnv() map[string]string {
	return map[string]string{
		"AUTH0_DOMAIN":        "example.auth0.com",
		"AUTH0_CLIENT_ID":     "client-id",
		"AUTH0_CLIENT_SECRET": "client-secret",
		"AUTH0_AUDIENCE":      "aud",
		"SLACK_BASE_URL":      "https://slack-bot.example",
		"OAUTH_STATE_SECRET":  validStateSecret,
		"QURL_ENDPOINT":       "https://api.qurl.invalid",
	}
}

func validSlackInstallEnv() map[string]string {
	return map[string]string{
		envSlackClientID:           "111.222",
		envSlackClientSecret:       "slack-secret",
		"SLACK_BASE_URL":           "https://slack-bot.example",
		envSlackInstallStateSecret: validStateSecret,
		"OAUTH_STATE_SECRET":       "",
		envSlackBotScopes:          "",
	}
}

func TestValidateSlackBotToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{name: "unset"},
		{name: "bot token", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxb-")+10)},
		{name: "rotating bot token", token: "xoxe.xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxe.xoxb-"))},
		{name: "rotating refresh token", token: "xoxe-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxe-")), wantErr: true},
		{name: "user token", token: "xoxp-test-token", wantErr: true},
		{name: "app token", token: "xapp-test-token", wantErr: true},
		{name: "placeholder bot token", token: "xoxb-", wantErr: true},
		{name: "minimum typo-guard length bot token", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxb-"))},
		{name: "one below typo-guard length", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxb-")-1), wantErr: true},
		{name: "token with whitespace", token: "xoxb-test-token\r", wantErr: true},
		{name: "token with non-ascii", token: "xoxb-test-tokené", wantErr: true},
		{name: "token with underscore", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxb-")) + "_ok"},
		{name: "token with dot", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxb-")) + ".ok"},
		{name: "long bot token", token: "xoxb-" + strings.Repeat("a", 250)},
		{name: "maximum typo-guard length bot token", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMax-len("xoxb-"))},
		{name: "one above typo-guard length", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMax-len("xoxb-")+1), wantErr: true},
		{name: "token too long", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMax-len("xoxb-")+100), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := auth.ValidateSlackBotTokenShape(tc.token)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateSlackBotTokenShape(%q) err=%v, wantErr=%v", tc.token, err, tc.wantErr)
			}
		})
	}
}

func TestReadTunnelImageConfig(t *testing.T) {
	cases := []struct {
		name              string
		image             string
		fallback          string
		wantImage         string
		wantErrText       string
		wantErrAbsentText string
	}{
		{
			name:        "unset fails closed",
			wantErrText: envQURLConnectorImage + " is required",
		},
		{
			name:      "explicit dev sandbox fallback",
			fallback:  connectorImageFallbackSandbox,
			wantImage: "",
		},
		{
			name:      "explicit fallback is case insensitive",
			fallback:  "DEV-SANDBOX",
			wantImage: "",
		},
		{
			name:      "whitespace image uses explicit fallback",
			image:     " \t ",
			fallback:  connectorImageFallbackSandbox,
			wantImage: "",
		},
		{
			name:      "version-tagged image wins",
			image:     testConnectorVersionImage,
			fallback:  "unexpected",
			wantImage: testConnectorVersionImage,
		},
		{
			name:      "digest-pinned image wins",
			image:     "localhost:5000/layervai/qurl-connector@sha256:" + strings.Repeat("a", 64),
			wantImage: "localhost:5000/layervai/qurl-connector@sha256:" + strings.Repeat("a", 64),
		},
		{
			name:        "invalid image rejected",
			image:       testConnectorImageRepo + ":bad tag",
			wantErrText: envQURLConnectorImage + ":",
		},
		{
			name:        "implicit latest routes to non-pinned message",
			image:       testConnectorImageRepo,
			wantErrText: connectorImageErrFloating,
		},
		{
			name:        "explicit latest image rejected even with fallback opt in",
			image:       testConnectorLatestImage,
			fallback:    connectorImageFallbackSandbox,
			wantErrText: connectorImageErrFloating,
		},
		{
			name:        "latest tag with digest routes to latest-digest message",
			image:       testConnectorLatestImage + "@sha256:" + strings.Repeat("a", 64),
			wantErrText: connectorImageErrLatestDigest,
		},
		{
			name:        "uppercase sha256 digest routes to lowercase-digest message",
			image:       testConnectorImageRepo + "@sha256:" + strings.Repeat("A", 64),
			wantErrText: connectorImageErrDigestLowercase,
		},
		{
			name:              "malformed reference routes to malformed-reference message",
			image:             "ghcr.io//qurl-connector:v1",
			wantErrText:       connectorImageErrMalformedRef,
			wantErrAbsentText: connectorImageFallbackHint,
		},
		{
			name:        "uppercase repository path routes to malformed-reference message",
			image:       "ghcr.io/LayerV/qurl-connector:v1",
			wantErrText: connectorImageErrMalformedRef,
		},
		{
			name:        "slashless registry-looking ref routes to ambiguous-reference message",
			image:       "gcr.io:v1",
			wantErrText: connectorImageErrAmbiguousRef,
		},
		{
			name:        "mixed-case localhost ref routes to ambiguous-reference message",
			image:       "Localhost:5000",
			wantErrText: connectorImageErrAmbiguousRef,
		},
		{
			name:        "uppercase localhost ref routes to ambiguous-reference message",
			image:       "LOCALHOST:5000",
			wantErrText: connectorImageErrAmbiguousRef,
		},
		{
			name:              "malformed digest routes to malformed-digest message",
			image:             testConnectorImageRepo + "@notadigest",
			wantErrText:       connectorImageErrMalformedDigest,
			wantErrAbsentText: connectorImageFallbackHint,
		},
		{
			name:        "uppercase bare sha256 digest routes to malformed-digest message",
			image:       "SHA256:" + strings.Repeat("a", 64),
			wantErrText: connectorImageErrMalformedDigest,
		},
		{
			name:        "unknown fallback rejected",
			fallback:    "Latest",
			wantErrText: envQURLConnectorImageFallback + "=\"Latest\"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envQURLConnectorImage, tc.image)
			t.Setenv(envQURLConnectorImageFallback, tc.fallback)

			got, err := readTunnelImageConfig()

			if tc.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("readTunnelImageConfig() err = %v, want substring %q", err, tc.wantErrText)
				}
				if tc.wantErrAbsentText != "" && strings.Contains(err.Error(), tc.wantErrAbsentText) {
					t.Fatalf("readTunnelImageConfig() err = %v, want no substring %q", err, tc.wantErrAbsentText)
				}
				return
			}
			if err != nil {
				t.Fatalf("readTunnelImageConfig() err = %v", err)
			}
			if got != tc.wantImage {
				t.Fatalf("readTunnelImageConfig() = %q, want %q", got, tc.wantImage)
			}
		})
	}
}

func TestRunValidatesTunnelImageBeforeInfraSetup(t *testing.T) {
	// run() validates the customer-rendered connector image after only the
	// prerequisite public endpoint/signing-secret checks and before infra or
	// other env setup, so this asserts the process-level startup error without
	// AWS stubs.
	t.Setenv("QURL_ENDPOINT", "https://api.qurl.invalid")
	t.Setenv("SLACK_SIGNING_SECRET", "signing-secret")
	t.Setenv(envQURLConnectorImage, "")
	t.Setenv(envQURLConnectorImageFallback, "")

	err := run()

	if err == nil || !strings.Contains(err.Error(), envQURLConnectorImage+" is required") {
		t.Fatalf("run() err = %v, want %s fail-closed error before infra/env setup", err, envQURLConnectorImage)
	}
}

func TestShutdownBudgetsLeaveLameduckDrainHeadroom(t *testing.T) {
	if lameduckDuration <= 0 {
		t.Fatalf("lameduckDuration = %s, want positive ALB drain head start", lameduckDuration)
	}
	if lameduckDuration >= shutdownTimeout {
		t.Fatalf("lameduckDuration = %s must be less than shutdownTimeout = %s", lameduckDuration, shutdownTimeout)
	}
	if got := shutdownTimeout - lameduckDuration; got < 10*time.Second {
		t.Fatalf("shutdown drain headroom = %s, want at least 10s after lameduck", got)
	}
}

func TestShutdownSequenceLameduckThenDrainPreservesInFlightRequest(t *testing.T) {
	h := internal.NewHandler(internal.Config{})
	started := make(chan struct{})
	release := make(chan struct{})

	mux := http.NewServeMux()
	mux.Handle("/health", h)
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	})

	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()
	serveErrRead := false
	t.Cleanup(func() {
		_ = srv.Close()
		if !serveErrRead {
			<-serveErr
		}
	})

	client := &http.Client{Timeout: time.Second}
	baseURL := "http://" + ln.Addr().String()
	slowResp := make(chan error, 1)
	go func() {
		resp, err := getWithContext(context.Background(), client, baseURL+"/slow")
		if err != nil {
			slowResp <- err
			return
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			_ = resp.Body.Close()
			slowResp <- err
			return
		}
		if err := resp.Body.Close(); err != nil {
			slowResp <- err
			return
		}
		if resp.StatusCode != http.StatusOK || string(body) != "done" {
			slowResp <- errors.New("unexpected slow response")
			return
		}
		slowResp <- nil
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow request did not reach handler")
	}

	handlerCanceled := make(chan struct{})
	cancelHandler := func() { close(handlerCanceled) }
	lameduckStarted := make(chan struct{})
	finishLameduck := make(chan struct{})
	sleep := func(ctx context.Context, _ time.Duration) bool {
		close(lameduckStarted)
		select {
		case <-ctx.Done():
			return false
		case <-finishLameduck:
			return true
		}
	}
	shutdownDone := make(chan struct{})
	go func() {
		runShutdownSequence(srv, h, cancelHandler, 25*time.Millisecond, 500*time.Millisecond, sleep)
		close(shutdownDone)
	}()

	select {
	case <-lameduckStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown sequence did not enter lameduck")
	}
	waitForStatus(t, client, baseURL+"/health", http.StatusServiceUnavailable)

	select {
	case <-handlerCanceled:
		t.Fatal("handler context canceled before lameduck completed")
	default:
	}

	close(finishLameduck)
	select {
	case <-handlerCanceled:
	case <-time.After(time.Second):
		t.Fatal("handler context was not canceled before HTTP drain")
	}
	close(release)

	select {
	case err := <-slowResp:
		if err != nil {
			t.Fatalf("slow request: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow request did not complete during shutdown drain")
	}

	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown sequence did not complete")
	}

	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve returned %v, want http.ErrServerClosed", err)
	}
	serveErrRead = true
}

func TestLameduckForSignal(t *testing.T) {
	if got := lameduckForSignal(syscall.SIGTERM); got != lameduckDuration {
		t.Fatalf("SIGTERM lameduck = %s, want %s", got, lameduckDuration)
	}
	if got := lameduckForSignal(syscall.SIGINT); got != 0 {
		t.Fatalf("SIGINT lameduck = %s, want immediate drain", got)
	}
}

func TestShutdownSignalSourceFirstSignalWinsAndCancelsContext(t *testing.T) {
	input := make(chan os.Signal, 2)
	stopCalls := 0
	source := newShutdownSignalSourceFromInput(input, func() {
		stopCalls++
	})
	defer source.stop()

	input <- syscall.SIGTERM

	select {
	case sig := <-source.first:
		if sig != syscall.SIGTERM {
			t.Fatalf("first signal = %v, want %v", sig, syscall.SIGTERM)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first signal")
	}

	select {
	case <-source.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for context cancellation")
	}

	input <- syscall.SIGINT
	select {
	case sig := <-source.first:
		t.Fatalf("unexpected second signal delivered: %v", sig)
	default:
	}

	source.stop()
	if stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", stopCalls)
	}
}

func TestShutdownSignalSourceStopIsIdempotentAndCancelsContext(t *testing.T) {
	input := make(chan os.Signal)
	stopCalls := 0
	source := newShutdownSignalSourceFromInput(input, func() {
		stopCalls++
	})

	source.stop()
	source.stop()

	if stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", stopCalls)
	}
	select {
	case <-source.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for context cancellation")
	}
	select {
	case <-source.stopped:
	default:
		t.Fatal("stopped channel is not closed")
	}
}

func TestShutdownSequenceImmediateDrainSkipsLameduckHealth(t *testing.T) {
	srv := &recordingShutdownServer{}
	handler := &recordingShutdownHandler{}
	canceled := false
	sleepCalled := false

	runShutdownSequence(
		srv,
		handler,
		func() { canceled = true },
		0,
		time.Second,
		func(context.Context, time.Duration) bool {
			sleepCalled = true
			return true
		},
	)

	if sleepCalled {
		t.Fatal("immediate drain should not sleep")
	}
	if len(handler.healthyCalls) != 0 {
		t.Fatalf("SetHealthy calls = %v, want none for immediate drain", handler.healthyCalls)
	}
	if !canceled {
		t.Fatal("handler context was not canceled")
	}
	if !srv.shutdownCalled {
		t.Fatal("server Shutdown was not called")
	}
	if !handler.waitCalled {
		t.Fatal("handler WaitTimeout was not called")
	}
	if handler.waitBudget <= 0 {
		t.Fatalf("WaitTimeout budget = %s, want positive remaining budget", handler.waitBudget)
	}
}

func TestShutdownSequenceBudgetExhaustedDuringLameduckStillClosesServer(t *testing.T) {
	srv := &recordingShutdownServer{err: context.DeadlineExceeded}
	handler := &recordingShutdownHandler{}
	canceled := false

	runShutdownSequence(
		srv,
		handler,
		func() { canceled = true },
		time.Second,
		25*time.Millisecond,
		func(context.Context, time.Duration) bool {
			return false
		},
	)

	if len(handler.healthyCalls) != 1 || handler.healthyCalls[0] {
		t.Fatalf("SetHealthy calls = %v, want [false]", handler.healthyCalls)
	}
	if !canceled {
		t.Fatal("handler context was not canceled")
	}
	if !srv.shutdownCalled {
		t.Fatal("server Shutdown was not called")
	}
	if !handler.waitCalled {
		t.Fatal("handler WaitTimeout was not called")
	}
	if handler.waitBudget != 0 {
		t.Fatalf("WaitTimeout budget = %s, want 0 after exhausted lameduck", handler.waitBudget)
	}
}

type recordingShutdownServer struct {
	shutdownCalled bool
	err            error
}

func (s *recordingShutdownServer) Shutdown(context.Context) error {
	s.shutdownCalled = true
	return s.err
}

type recordingShutdownHandler struct {
	healthyCalls []bool
	waitCalled   bool
	waitBudget   time.Duration
}

func (h *recordingShutdownHandler) SetHealthy(healthy bool) {
	h.healthyCalls = append(h.healthyCalls, healthy)
}

func (h *recordingShutdownHandler) WaitTimeout(d time.Duration) bool {
	h.waitCalled = true
	h.waitBudget = d
	return true
}

func waitForStatus(t *testing.T, client *http.Client, url string, want int) {
	t.Helper()
	deadline := time.After(time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("%s never returned status %d", url, want)
		case <-tick.C:
			resp, err := getWithContext(context.Background(), client, url)
			if err != nil {
				continue
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			if err := resp.Body.Close(); err != nil {
				continue
			}
			if resp.StatusCode == want {
				return
			}
		}
	}
}

func getWithContext(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// applyEnv writes every oauthEnvKeys entry — empty when absent from kvs
// — so the test doesn't depend on what was inherited from the shell.
// t.Setenv handles per-test cleanup.
func applyEnv(t *testing.T, kvs map[string]string) {
	t.Helper()
	for _, k := range oauthEnvKeys {
		t.Setenv(k, kvs[k])
	}
	t.Setenv(envAuth0ExpectedAudience, kvs[envAuth0ExpectedAudience])
	t.Setenv("AUTH0_EMAIL_CONNECTION", kvs["AUTH0_EMAIL_CONNECTION"])
	t.Setenv(envQURLBindingTTLContract, kvs[envQURLBindingTTLContract])
	t.Setenv(envQURLAPIKeyMintTTLContract, kvs[envQURLAPIKeyMintTTLContract])
}

func applySlackInstallEnv(t *testing.T, kvs map[string]string) {
	t.Helper()
	for _, k := range slackInstallEnvKeys {
		t.Setenv(k, kvs[k])
	}
}

// stubJWKSVerifier swaps newJWKSVerifier for a noop so the env-var tests
// stay hermetic. Returns a t.Cleanup-restored seam.
func stubJWKSVerifier(t *testing.T) {
	t.Helper()
	prev := newJWKSVerifier
	newJWKSVerifier = func(_ context.Context, _, _ string) (oauth.IDTokenVerifier, error) {
		return noopVerifier{}, nil
	}
	t.Cleanup(func() { newJWKSVerifier = prev })
}

func TestBuildOAuthConfigHappyPath(t *testing.T) {
	stubJWKSVerifier(t)
	applyEnv(t, validEnv())
	cfg, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true with all env vars set; cfg=%+v", cfg)
	}
	if cfg.Auth0Domain != "example.auth0.com" {
		t.Errorf("Auth0Domain: got %q", cfg.Auth0Domain)
	}
	if cfg.Auth0EmailConnection != "" {
		t.Errorf("Auth0EmailConnection: got %q want empty by default", cfg.Auth0EmailConnection)
	}
	if string(cfg.OAuthStateSecret) != validStateSecret {
		t.Errorf("OAuthStateSecret not threaded through")
	}
	if cfg.IDTokenVerifier == nil {
		t.Error("IDTokenVerifier should be wired when the stubbed factory returns nil err")
	}
	if cfg.SetupBindingReplayWindowHours != oauth.DefaultSetupBindingReplayWindowHours {
		t.Errorf("SetupBindingReplayWindowHours = %d, want default %d", cfg.SetupBindingReplayWindowHours, oauth.DefaultSetupBindingReplayWindowHours)
	}
	if cfg.APIKeyMintReplayWindowHours != oauth.DefaultAPIKeyMintReplayWindowHours {
		t.Errorf("APIKeyMintReplayWindowHours = %d, want default %d", cfg.APIKeyMintReplayWindowHours, oauth.DefaultAPIKeyMintReplayWindowHours)
	}
}

func TestBuildOAuthConfigSetupBindingReplayWindowOverride(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env[envQURLBindingTTLContract] = "12h"
	applyEnv(t, env)
	cfg, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true with all required env vars set")
	}
	if cfg.SetupBindingReplayWindowHours != 12 {
		t.Errorf("SetupBindingReplayWindowHours = %d, want 12", cfg.SetupBindingReplayWindowHours)
	}
}

func TestBuildOAuthConfigRejectsInvalidSetupBindingReplayWindow(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env[envQURLBindingTTLContract] = "90m"
	applyEnv(t, env)
	_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if ok {
		t.Fatal("expected ok=false with non-canonical binding TTL contract")
	}
	if err == nil || !strings.Contains(err.Error(), "canonical Nh form") {
		t.Fatalf("buildOAuthConfig() err = %v, want canonical-duration validation error", err)
	}
}

func TestBuildOAuthConfigAPIKeyMintReplayWindowOverride(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env[envQURLAPIKeyMintTTLContract] = "18h"
	applyEnv(t, env)
	cfg, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true with all required env vars set")
	}
	if cfg.APIKeyMintReplayWindowHours != 18 {
		t.Errorf("APIKeyMintReplayWindowHours = %d, want 18", cfg.APIKeyMintReplayWindowHours)
	}
}

func TestBuildOAuthConfigRejectsInvalidAPIKeyMintReplayWindow(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env[envQURLAPIKeyMintTTLContract] = "90m"
	applyEnv(t, env)
	_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if ok {
		t.Fatal("expected ok=false with non-canonical API-key mint TTL contract")
	}
	if err == nil || !strings.Contains(err.Error(), "canonical Nh form") {
		t.Fatalf("buildOAuthConfig() err = %v, want canonical-duration validation error", err)
	}
}

func TestReadSetupBindingReplayWindowHours(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		unset       bool
		want        int
		wantErrText string
	}{
		{name: "unset defaults to upstream contract", unset: true, want: oauth.DefaultSetupBindingReplayWindowHours},
		{name: "whole hour override", raw: "12h", want: 12},
		{name: "trimmed whole hour override", raw: " 48h ", want: 48},
		{name: "malformed", raw: "24", wantErrText: "canonical Nh form"},
		{name: "zero", raw: "0h", wantErrText: "canonical Nh form"},
		{name: "leading zero", raw: "01h", wantErrText: "canonical Nh form"},
		{name: "leading plus", raw: "+24h", wantErrText: "canonical Nh form"},
		{name: "negative", raw: "-1h", wantErrText: "canonical Nh form"},
		{name: "minutes unit rejected", raw: "90m", wantErrText: "canonical Nh form"},
		{name: "compound duration rejected", raw: "2h0m0s", wantErrText: "canonical Nh form"},
		{name: "too large", raw: "999999999999999999999999999999999999h", wantErrText: "too large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				// t.Setenv cannot express a genuinely absent variable.
				// Keep this subtest non-parallel while it restores the
				// process env manually.
				oldValue, hadOldValue := os.LookupEnv(envQURLBindingTTLContract)
				if err := os.Unsetenv(envQURLBindingTTLContract); err != nil {
					t.Fatalf("unset %s: %v", envQURLBindingTTLContract, err)
				}
				t.Cleanup(func() {
					if hadOldValue {
						if err := os.Setenv(envQURLBindingTTLContract, oldValue); err != nil {
							t.Fatalf("restore %s: %v", envQURLBindingTTLContract, err)
						}
						return
					}
					if err := os.Unsetenv(envQURLBindingTTLContract); err != nil {
						t.Fatalf("restore unset %s: %v", envQURLBindingTTLContract, err)
					}
				})
			} else {
				t.Setenv(envQURLBindingTTLContract, tc.raw)
			}
			got, err := readSetupBindingReplayWindowHours()
			if tc.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("readSetupBindingReplayWindowHours() err = %v, want substring %q", err, tc.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("readSetupBindingReplayWindowHours() err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("readSetupBindingReplayWindowHours() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReadAPIKeyMintReplayWindowHours(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		unset       bool
		want        int
		wantErrText string
	}{
		{name: "unset defaults to upstream contract", unset: true, want: oauth.DefaultAPIKeyMintReplayWindowHours},
		{name: "whole hour override", raw: "18h", want: 18},
		{name: "trimmed whole hour override", raw: "36h ", want: 36},
		{name: "minutes unit rejected", raw: "90m", wantErrText: "canonical Nh form"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unset {
				oldValue, hadOldValue := os.LookupEnv(envQURLAPIKeyMintTTLContract)
				if err := os.Unsetenv(envQURLAPIKeyMintTTLContract); err != nil {
					t.Fatalf("unset %s: %v", envQURLAPIKeyMintTTLContract, err)
				}
				t.Cleanup(func() {
					if hadOldValue {
						if err := os.Setenv(envQURLAPIKeyMintTTLContract, oldValue); err != nil {
							t.Fatalf("restore %s: %v", envQURLAPIKeyMintTTLContract, err)
						}
						return
					}
					if err := os.Unsetenv(envQURLAPIKeyMintTTLContract); err != nil {
						t.Fatalf("restore unset %s: %v", envQURLAPIKeyMintTTLContract, err)
					}
				})
			} else {
				t.Setenv(envQURLAPIKeyMintTTLContract, tc.raw)
			}
			got, err := readAPIKeyMintReplayWindowHours()
			if tc.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("readAPIKeyMintReplayWindowHours() err = %v, want substring %q", err, tc.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("readAPIKeyMintReplayWindowHours() err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("readAPIKeyMintReplayWindowHours() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBuildOAuthConfigEmailConnectionOverride(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env["AUTH0_EMAIL_CONNECTION"] = "Username-Password-Authentication"
	applyEnv(t, env)
	cfg, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true with all required env vars set")
	}
	if cfg.Auth0EmailConnection != "Username-Password-Authentication" {
		t.Errorf("Auth0EmailConnection: got %q want override", cfg.Auth0EmailConnection)
	}
}

func TestBuildOAuthConfigAcceptsConfiguredExpectedAudience(t *testing.T) {
	stubJWKSVerifier(t)
	cases := []struct {
		name             string
		qurlEndpoint     string
		audience         string
		expectedAudience string
		wantAudience     string
	}{
		{
			name:             "public endpoint and audience match configured expectation",
			qurlEndpoint:     "https://api.layerv.ai",
			audience:         "https://api.layerv.ai",
			expectedAudience: "https://api.layerv.ai",
			wantAudience:     "https://api.layerv.ai",
		},
		{
			name:             "configured expected audience trims surrounding whitespace",
			qurlEndpoint:     "https://api.layerv.ai",
			audience:         "https://api.layerv.ai",
			expectedAudience: " \t\nhttps://api.layerv.ai\n ",
			wantAudience:     "https://api.layerv.ai",
		},
		{
			name:             "private endpoint uses infra-provided audience",
			qurlEndpoint:     "https://sandbox.example.invalid",
			audience:         "https://audience.example.invalid",
			expectedAudience: "https://audience.example.invalid",
			wantAudience:     "https://audience.example.invalid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv()
			env["QURL_ENDPOINT"] = tc.qurlEndpoint
			env["AUTH0_AUDIENCE"] = tc.audience
			env[envAuth0ExpectedAudience] = tc.expectedAudience
			applyEnv(t, env)

			cfg, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
			if err != nil {
				t.Fatalf("buildOAuthConfig() err = %v", err)
			}
			if !ok {
				t.Fatal("expected ok=true for matching known qURL endpoint/audience pair")
			}
			if cfg.Auth0Audience != tc.wantAudience {
				t.Fatalf("Auth0Audience = %q, want %q", cfg.Auth0Audience, tc.wantAudience)
			}
		})
	}
}

func TestBuildOAuthConfigAllowsUnconfiguredExpectedAudience(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env["QURL_ENDPOINT"] = "https://api.layerv.ai"
	env["AUTH0_AUDIENCE"] = "https://api.example.invalid"
	applyEnv(t, env)

	cfg, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if err != nil {
		t.Fatalf("buildOAuthConfig() err = %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true when infra has not configured the expected audience")
	}
	if cfg.Auth0Audience != "https://api.example.invalid" {
		t.Fatalf("Auth0Audience = %q, want raw audience preserved when expected audience is unconfigured", cfg.Auth0Audience)
	}
}

func TestBuildOAuthConfigRejectsConfiguredExpectedAudienceMismatch(t *testing.T) {
	stubJWKSVerifier(t)
	cases := []struct {
		name              string
		qurlEndpoint      string
		audience          string
		expectedAudience  string
		wantExpectedInErr string
	}{
		{
			name:              "public endpoint with wrong audience",
			qurlEndpoint:      "https://api.layerv.ai",
			audience:          "https://api.example.invalid",
			expectedAudience:  "https://api.layerv.ai",
			wantExpectedInErr: "https://api.layerv.ai",
		},
		{
			name:              "private endpoint with wrong audience",
			qurlEndpoint:      "https://sandbox.example.invalid",
			audience:          "https://api.example.invalid",
			expectedAudience:  "https://audience.example.invalid",
			wantExpectedInErr: "https://audience.example.invalid",
		},
		{
			name:              "public endpoint with trailing slash audience",
			qurlEndpoint:      "https://api.layerv.ai",
			audience:          "https://api.layerv.ai/",
			expectedAudience:  "https://api.layerv.ai",
			wantExpectedInErr: "https://api.layerv.ai",
		},
		{
			name:              "public endpoint with host case audience",
			qurlEndpoint:      "https://api.layerv.ai",
			audience:          "https://API.layerv.ai",
			expectedAudience:  "https://api.layerv.ai",
			wantExpectedInErr: "https://api.layerv.ai",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv()
			env["QURL_ENDPOINT"] = tc.qurlEndpoint
			env["AUTH0_AUDIENCE"] = tc.audience
			env[envAuth0ExpectedAudience] = tc.expectedAudience
			applyEnv(t, env)

			_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
			if ok {
				t.Fatal("expected ok=false for configured expected audience mismatch")
			}
			if err == nil ||
				!strings.Contains(err.Error(), "AUTH0_AUDIENCE") ||
				!strings.Contains(err.Error(), envAuth0ExpectedAudience) ||
				!strings.Contains(err.Error(), "QURL_ENDPOINT") ||
				!strings.Contains(err.Error(), tc.wantExpectedInErr) {
				t.Fatalf("buildOAuthConfig() err = %v, want mismatch error naming expected audience %q", err, tc.wantExpectedInErr)
			}
		})
	}
}

func TestBuildOAuthConfigRejectsAuth0AudienceSurroundingWhitespace(t *testing.T) {
	stubJWKSVerifier(t)
	cases := []struct {
		name     string
		audience string
	}{
		{name: "leading space", audience: " https://api.layerv.ai"},
		{name: "trailing space", audience: "https://api.layerv.ai "},
		{name: "newline and tab", audience: "\nhttps://api.layerv.ai\t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv()
			env["AUTH0_AUDIENCE"] = tc.audience
			applyEnv(t, env)

			_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
			if ok {
				t.Fatal("expected ok=false for AUTH0_AUDIENCE surrounding whitespace")
			}
			if err == nil ||
				!strings.Contains(err.Error(), "AUTH0_AUDIENCE") ||
				!strings.Contains(err.Error(), "surrounding whitespace") {
				t.Fatalf("buildOAuthConfig() err = %v, want surrounding whitespace error", err)
			}
		})
	}
}

func TestBuildOAuthConfigMissingVar(t *testing.T) {
	stubJWKSVerifier(t)
	for _, missing := range oauthEnvKeys {
		t.Run("missing="+missing, func(t *testing.T) {
			env := validEnv()
			delete(env, missing)
			applyEnv(t, env)
			_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
			if err != nil {
				t.Errorf("expected nil error on missing var; got %v", err)
			}
			if ok {
				t.Errorf("expected ok=false when %s is missing", missing)
			}
		})
	}
}

func TestBuildOAuthConfigShortSecret(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env["OAUTH_STATE_SECRET"] = strings.Repeat("a", 16) // half of the required minimum
	applyEnv(t, env)
	_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if ok {
		t.Error("expected ok=false on short OAUTH_STATE_SECRET")
	}
	if !errors.Is(err, errOAuthStateSecretTooShort) {
		t.Errorf("expected errOAuthStateSecretTooShort, got %v", err)
	}
}

// TestBuildOAuthConfigSecretLengthBoundary pins both sides of the
// StateMinSecret floor — one byte less rejects, exactly StateMinSecret
// accepts. A future bump of the constant on one side without the other
// would be caught here.
func TestBuildOAuthConfigSecretLengthBoundary(t *testing.T) {
	stubJWKSVerifier(t)
	t.Run("just_under", func(t *testing.T) {
		env := validEnv()
		env["OAUTH_STATE_SECRET"] = strings.Repeat("a", oauth.StateMinSecret-1)
		applyEnv(t, env)
		_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
		if ok || !errors.Is(err, errOAuthStateSecretTooShort) {
			t.Errorf("ok=%v err=%v — want ok=false + errOAuthStateSecretTooShort at StateMinSecret-1 bytes", ok, err)
		}
	})
	t.Run("exactly_at", func(t *testing.T) {
		env := validEnv()
		env["OAUTH_STATE_SECRET"] = strings.Repeat("a", oauth.StateMinSecret)
		applyEnv(t, env)
		_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
		if !ok || err != nil {
			t.Errorf("ok=%v err=%v — want ok=true at exactly StateMinSecret bytes", ok, err)
		}
	})
}

// TestBuildOAuthConfigFailsFastOnJWKSWhenAdminStoreWired fences the
// mismatched-degradation guard: when the JWKS prime fails AND AdminStore
// is wired, every callback would reject the install (no sub → no
// OwnerID → no bind → 500). Catching that at boot beats catching it
// after the first user hits /qurl setup. The sandbox path (no
// AdminStore) is the inverse — falls through to a warn + nil
// verifier so the API-key surface keeps working.
func TestBuildOAuthConfigFailsFastOnJWKSWhenAdminStoreWired(t *testing.T) {
	prev := newJWKSVerifier
	newJWKSVerifier = func(_ context.Context, _, _ string) (oauth.IDTokenVerifier, error) {
		return nil, errors.New("simulated JWKS prime failure")
	}
	t.Cleanup(func() { newJWKSVerifier = prev })

	applyEnv(t, validEnv())

	t.Run("admin store wired — must fail-fast", func(t *testing.T) {
		_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, &fakeAdminStore{})
		if ok {
			t.Error("expected ok=false when JWKS prime fails with AdminStore wired (every callback would 500)")
		}
		if err == nil || !strings.Contains(err.Error(), "JWKS") {
			t.Errorf("expected fail-fast error mentioning JWKS, got %v", err)
		}
	})

	t.Run("admin store nil — must warn-and-continue", func(t *testing.T) {
		cfg, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
		if err != nil {
			t.Fatalf("expected nil err on sandbox path, got %v", err)
		}
		if !ok {
			t.Fatal("expected ok=true on sandbox path (no AdminStore → bind is skipped, verifier is best-effort)")
		}
		if cfg.IDTokenVerifier != nil {
			t.Error("expected verifier=nil when prime failed; callback gates on this to skip claim extraction")
		}
	})
}

// fakeAdminStore is the cmd-side stand-in for oauth.AdminStore in
// tests that only need a non-nil to flip the JWKS fail-fast branch.
type fakeAdminStore struct{}

func (*fakeAdminStore) BindWorkspace(_ context.Context, _ *oauth.WorkspaceMapping, _ string) error {
	return nil
}

// TestClassifyBindErrorMapping locks the slackdata.Error.Code →
// oauth.BindConflictCode mapping that wires the callback's switch
// arm to slackdata's 409 surface. The reflect-shape fence covers
// the struct; this covers the code-string mapping which is its own
// drift surface — rename slackdata.ErrCodeWorkspaceAlreadyBound and
// the classifier silently falls through to the empty-string "generic
// failure" arm, downgrading rebind-refused to a 500.
func TestClassifyBindErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want oauth.BindConflictCode
	}{
		{
			"already bound to caller (idempotent re-entry)",
			&slackdata.Error{StatusCode: http.StatusConflict, Code: slackdata.ErrCodeWorkspaceAlreadyBoundToCaller},
			oauth.BindConflictAlreadyBoundToCaller,
		},
		{
			"already bound to different admin (rebind-refused)",
			&slackdata.Error{StatusCode: http.StatusConflict, Code: slackdata.ErrCodeWorkspaceAlreadyBound},
			oauth.BindConflictAlreadyBound,
		},
		{
			"bind held but disambig read failed (unverified)",
			&slackdata.Error{StatusCode: http.StatusConflict, Code: slackdata.ErrCodeWorkspaceBindUnverified},
			oauth.BindConflictUnverified,
		},
		{
			"non-409 *slackdata.Error → empty (generic failure)",
			&slackdata.Error{StatusCode: http.StatusServiceUnavailable, Code: "ddb_error"},
			"",
		},
		{
			"409 with unknown Code → empty (default arm)",
			&slackdata.Error{StatusCode: http.StatusConflict, Code: "future_unmapped_code"},
			"",
		},
		{
			"non-*slackdata.Error → empty",
			errors.New("plain string error"),
			"",
		},
		{"nil → empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyBindError(c.err); got != c.want {
				t.Errorf("classifyBindError(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// TestAdminStoreAdapterForwardsAllFields exercises the production
// adminStoreAdapter against a captor that satisfies slackdataBinder,
// with a non-zero CreatedAt. The reflect-shape test fences the struct
// field set; this fences the adapter's translation line so a future
// regression that drops one of TeamID / OwnerID / CreatedAt from the
// copy fails here rather than slipping through unnoticed because the
// callback passes zero values today.
func TestAdminStoreAdapterForwardsAllFields(t *testing.T) {
	captured := &capturingSlackdataStore{}
	adapter := &adminStoreAdapter{store: captured}
	want := oauth.WorkspaceMapping{
		TeamID:    "T_capture",
		OwnerID:   "auth0|capture-owner",
		CreatedAt: mustParseTime(t, "2026-05-20T12:34:56Z"),
	}
	if err := adapter.BindWorkspace(context.Background(), &want, "U_seed"); err != nil {
		t.Fatalf("BindWorkspace: %v", err)
	}
	if captured.gotMapping == nil {
		t.Fatal("adapter did not forward to the wrapped store")
	}
	if captured.gotMapping.TeamID != want.TeamID ||
		captured.gotMapping.OwnerID != want.OwnerID ||
		!captured.gotMapping.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("forwarded mapping mismatch:\nwant TeamID=%q OwnerID=%q CreatedAt=%v\ngot  TeamID=%q OwnerID=%q CreatedAt=%v",
			want.TeamID, want.OwnerID, want.CreatedAt,
			captured.gotMapping.TeamID, captured.gotMapping.OwnerID, captured.gotMapping.CreatedAt)
	}
	if captured.gotSeedAdmin != "U_seed" {
		t.Errorf("seedAdmin: got %q want %q", captured.gotSeedAdmin, "U_seed")
	}
}

// capturingSlackdataStore satisfies slackdataBinder so the production
// adminStoreAdapter can be exercised without standing up a real
// slackdata.Store.
type capturingSlackdataStore struct {
	gotMapping   *slackdata.WorkspaceMapping
	gotSeedAdmin string
}

func (c *capturingSlackdataStore) BindWorkspace(_ context.Context, m *slackdata.WorkspaceMapping, seedAdmin string) error {
	c.gotMapping = m
	c.gotSeedAdmin = seedAdmin
	return nil
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// TestAdminStoreAdapterMappingShapesMatch fences the field-for-field
// equivalence of oauth.WorkspaceMapping and slackdata.WorkspaceMapping.
// The adminStoreAdapter copies between the two by named field; a new
// field added to one and not the other would silently drop on the
// adapter's copy. Reflect-walk the field sets so the build breaks
// when they drift.
func TestAdminStoreAdapterMappingShapesMatch(t *testing.T) {
	oauthFields := structFieldSet(reflect.TypeOf(oauth.WorkspaceMapping{}))
	storeFields := structFieldSet(reflect.TypeOf(slackdata.WorkspaceMapping{}))
	if !reflect.DeepEqual(oauthFields, storeFields) {
		t.Errorf("oauth.WorkspaceMapping vs slackdata.WorkspaceMapping fields differ — adminStoreAdapter copy would silently drop the diff\noauth:     %v\nslackdata: %v", oauthFields, storeFields)
	}
}

// structFieldSet returns the {name → full type string} map for a
// struct type. Used to compare field shapes across packages without
// requiring identical declaration order. Type string (not Kind) so
// a future drift like `OwnerID OwnerID` (named-string) vs
// `OwnerID string` fails the test here rather than at the adapter
// build line — the test owns the contract end-to-end.
func structFieldSet(t reflect.Type) map[string]string {
	out := make(map[string]string, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		out[f.Name] = f.Type.String()
	}
	return out
}

// TestBuildOAuthConfigRejectsEmptyHostSlackBaseURL locks the contract
// that a parse-valid but host-less URL (e.g. "https://") is rejected.
// Without this, the resulting redirect_uri would be
// "https:///oauth/qurl/callback" — silently broken.
func TestBuildOAuthConfigRejectsEmptyHostSlackBaseURL(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env["SLACK_BASE_URL"] = "https://"
	applyEnv(t, env)
	_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if ok {
		t.Error("expected ok=false on empty-host SLACK_BASE_URL")
	}
	if err == nil {
		t.Error("expected error on empty-host SLACK_BASE_URL")
	}
}

// TestBuildOAuthConfigRejectsNonHTTPSSlackBaseURL locks the Secure-cookie
// contract: a Set-Cookie: Secure is dropped silently by browsers over
// http://, which would break the double-submit check with a misleading
// "setup must be completed in the same browser" error. Fail-fast at
// config load.
func TestBuildOAuthConfigRejectsNonHTTPSSlackBaseURL(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env["SLACK_BASE_URL"] = "http://slack-bot.example"
	applyEnv(t, env)
	_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if ok {
		t.Error("expected ok=false on http:// SLACK_BASE_URL")
	}
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Errorf("expected https:// error, got %v", err)
	}
}

// TestBuildOAuthConfigNormalizesURLEnvVars asserts SLACK_BASE_URL,
// AUTH0_DOMAIN, and QURL_ENDPOINT are normalized at config-load:
//   - trailing slashes are stripped (Auth0 rejects redirect_uri mismatches)
//   - AUTH0_DOMAIN's accidental scheme prefix is stripped (jwks.go
//     composes "https://" + domain, so "https://example.auth0.com" would
//     otherwise yield "https://https://example.auth0.com/...")
func TestBuildOAuthConfigNormalizesURLEnvVars(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env["SLACK_BASE_URL"] = "https://slack-bot.example/"
	env["AUTH0_DOMAIN"] = "https://example.auth0.com/"
	env["QURL_ENDPOINT"] = "https://api.qurl.invalid/"
	applyEnv(t, env)
	cfg, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true with trailing slashes and scheme prefix — config should normalize them")
	}
	if cfg.SlackBaseURL != "https://slack-bot.example" {
		t.Errorf("SlackBaseURL not trimmed: got %q", cfg.SlackBaseURL)
	}
	if cfg.Auth0Domain != "example.auth0.com" {
		t.Errorf("Auth0Domain not normalized (expect scheme + trailing slash stripped): got %q", cfg.Auth0Domain)
	}
}

func TestBuildSlackInstallConfigHappyPath(t *testing.T) {
	env := validSlackInstallEnv()
	applySlackInstallEnv(t, env)
	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true with Slack install env set")
	}
	if cfg.ClientID != "111.222" || cfg.ClientSecret != "slack-secret" {
		t.Fatalf("Slack client config not threaded through: %+v", cfg)
	}
	if cfg.SlackBaseURL != "https://slack-bot.example" {
		t.Fatalf("SlackBaseURL = %q", cfg.SlackBaseURL)
	}
	if string(cfg.StateSecret) != validStateSecret {
		t.Fatalf("StateSecret not threaded through")
	}
	if strings.Join(cfg.BotScopes, ",") != defaultSlackBotScopesCSV {
		t.Fatalf("default bot scopes = %v", cfg.BotScopes)
	}
	if cfg.TokenStore == nil {
		t.Fatal("TokenStore should be wired")
	}
}

func TestBuildSlackInstallConfigFallsBackToOAuthStateSecret(t *testing.T) {
	env := validSlackInstallEnv()
	env[envSlackInstallStateSecret] = ""
	env["OAUTH_STATE_SECRET"] = validStateSecret
	applySlackInstallEnv(t, env)
	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want fallback to OAUTH_STATE_SECRET", ok, err)
	}
	if string(cfg.StateSecret) != validStateSecret {
		t.Fatalf("StateSecret not sourced from OAUTH_STATE_SECRET")
	}
}

func TestBuildSlackInstallConfigMissingVar(t *testing.T) {
	for _, missing := range []string{envSlackClientID, envSlackClientSecret, "SLACK_BASE_URL", envSlackInstallStateSecret} {
		t.Run("missing="+missing, func(t *testing.T) {
			env := validSlackInstallEnv()
			delete(env, missing)
			if missing == envSlackInstallStateSecret {
				env["OAUTH_STATE_SECRET"] = ""
			}
			applySlackInstallEnv(t, env)
			_, ok, err := buildSlackInstallConfig(newFakeProvider())
			if err != nil {
				t.Fatalf("expected nil error on missing var, got %v", err)
			}
			if ok {
				t.Fatalf("expected ok=false when %s is missing", missing)
			}
		})
	}
}

func TestBuildSlackInstallConfigCustomScopes(t *testing.T) {
	env := validSlackInstallEnv()
	env[envSlackBotScopes] = "commands,channels:read"
	applySlackInstallEnv(t, env)
	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if strings.Join(cfg.BotScopes, ",") != defaultSlackBotScopesCSV+",channels:read" {
		t.Fatalf("custom scopes = %v", cfg.BotScopes)
	}
}

func TestBuildSlackInstallConfigUnionsRequiredScopesIntoLegacyOverride(t *testing.T) {
	env := validSlackInstallEnv()
	env[envSlackBotScopes] = "commands"
	applySlackInstallEnv(t, env)
	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want legacy override to keep required defaults", ok, err)
	}
	if strings.Join(cfg.BotScopes, ",") != defaultSlackBotScopesCSV {
		t.Fatalf("scopes = %v, want required defaults unioned into legacy override", cfg.BotScopes)
	}
}

// A stale `views:write` in a SLACK_BOT_SCOPES override is stripped at config
// load (see slackinstall.DropUnsupportedScopes), so the install flow keeps
// working off the valid scopes instead of breaking every install with
// invalid_scope or aborting startup. Mixed case confirms the wiring uses the
// case-insensitive helper; the drop decision itself is unit-tested in the
// slackinstall package.
func TestBuildSlackInstallConfigStripsViewsWriteOverride(t *testing.T) {
	env := validSlackInstallEnv()
	env[envSlackBotScopes] = "commands,Views:Write"
	applySlackInstallEnv(t, env)
	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want config load to succeed", ok, err)
	}
	if strings.Join(cfg.BotScopes, ",") != defaultSlackBotScopesCSV {
		t.Fatalf("scopes = %v, want views:write stripped", cfg.BotScopes)
	}
}

// If a SLACK_BOT_SCOPES override strips to nothing (only views:write), config
// load keeps the required defaults rather than aborting startup.
func TestBuildSlackInstallConfigUsesDefaultsWhenOverrideStripsToEmpty(t *testing.T) {
	env := validSlackInstallEnv()
	env[envSlackBotScopes] = "views:write"
	applySlackInstallEnv(t, env)
	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want config load to succeed with defaults", ok, err)
	}
	if strings.Join(cfg.BotScopes, ",") != defaultSlackBotScopesCSV {
		t.Fatalf("scopes = %v, want required defaults", cfg.BotScopes)
	}
}

func TestSlackInstallConfigRejectsBadBaseURL(t *testing.T) {
	env := validSlackInstallEnv()
	env["SLACK_BASE_URL"] = "http://slack-bot.example"
	applySlackInstallEnv(t, env)
	_, ok, err := buildSlackInstallConfig(newFakeProvider())
	if ok {
		t.Fatal("ok=true, want bad Slack install base URL to fail at config build")
	}
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("err=%v, want https error", err)
	}
}
