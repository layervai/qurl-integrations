//go:build !windows

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	qurl "github.com/layervai/qurl-go/qurl"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func testNativeSessionConfig(ownerID string) (connectorshare.NativeSessionOperationAuthority, error) {
	return connectorshare.NativeSessionOperationAuthority{OwnerID: ownerID}, nil
}

type headlessTestFactory struct {
	started chan struct{}
	once    sync.Once
	closed  atomic.Int32
}

func (f *headlessTestFactory) NewGroupRunner(context.Context, *connectordaemon.GroupConfig) (connectordaemon.GroupRunner, error) {
	f.once.Do(func() { close(f.started) })
	return headlessTestRunner{}, nil
}

func (f *headlessTestFactory) Close() error {
	f.closed.Add(1)
	return nil
}

type headlessTestRunner struct{}

func (headlessTestRunner) Run(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
func (headlessTestRunner) SetRoutes(context.Context, []connectorshare.LocalHTTPRoute) error {
	return nil
}
func (headlessTestRunner) RestartRoute(context.Context, string) error { return nil }
func (headlessTestRunner) RouteStates() map[string]connectorshare.RouteState {
	return map[string]connectorshare.RouteState{}
}

func TestHiddenTestCRIDValidatorIsStrictAndCredentialFree(t *testing.T) {
	testCRID := "qe4jqpd7eaoslq7jinmjv4yikgzmcxgpjfsuobiniqnko32lpw742pueoujq"
	productionCRID := "ae4jqpd7eaoslq7jinmjv4yikgzmcxgpjfsuobiniqnko32lpw743ivbeyha"
	unknownCRID := "p44jqpd7eaoslq7jinmjv4yikgzmcxgpjfsuobiniqnko32lpw743out3lhq"
	resourceID := "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEcOtuxu2qhc3gt1E7BiEU0CLqEDlXDwzZq0JnESgMAwERX6y_XXF5Cn5SKITWIZQmUhCZ0pHHlVn7SmFUTAnTGQ"
	foreignResourceID := "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEpDu9mdM6E96ncBm5qjKn16Rjv6sWoHRQQz2ElwKSg5YQDLCvofuEb7gmId2YBKv3YXcrdc3tmBaiRzYCH9Hp6Q"

	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("api_key: must-not-be-read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		value string
		id    string
		code  int
	}{
		{name: "test", value: testCRID, id: resourceID, code: 0},
		{name: "production", value: productionCRID, id: resourceID, code: 2},
		{name: "unknown version", value: unknownCRID, id: resourceID, code: 2},
		{name: "malformed", value: "not-a-crid", id: resourceID, code: 2},
		{name: "malformed resource", value: testCRID, id: "not-a-resource", code: 2},
		{name: "noncanonical resource", value: testCRID, id: resourceID[:len(resourceID)-1] + "R", code: 2},
		{name: "foreign resource", value: testCRID, id: foreignResourceID, code: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := runCLI(t, &runOpts{
				args: []string{"daemon", "validate-test-resource", tc.value, tc.id}, configDir: configDir,
			})
			if res.code != tc.code {
				t.Fatalf("exit=%d, want %d; stderr=%s", res.code, tc.code, res.stderr.String())
			}
			if res.stdout.Len() != 0 || strings.Contains(res.stderr.String(), tc.value) || strings.Contains(res.stderr.String(), tc.id) || strings.Contains(res.stderr.String(), "must-not-be-read") {
				t.Fatalf("validator exposed input/config: stdout=%q stderr=%q", res.stdout.String(), res.stderr.String())
			}
		})
	}
}

func TestLoadHeadlessBootstrapWarmStartDoesNotRequireToken(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	configPath := writeHeadlessConfigFixture(t)
	writeHeadlessAgentState(t, stateDir, true)
	config, credential, err := loadHeadlessBootstrap(context.Background(), stateDir, configPath, filepath.Join(t.TempDir(), "absent-token"))
	if err != nil || config == nil || credential != "" {
		t.Fatalf("warm bootstrap = %+v, credential=%q, err=%v", config, credential, err)
	}
}

func TestLoadHeadlessBootstrapIncompleteStateReusesEnrollmentToken(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	configPath := writeHeadlessConfigFixture(t)
	writeHeadlessAgentState(t, stateDir, false)
	tokenPath := filepath.Join(t.TempDir(), "enrollment-token")
	if err := os.WriteFile(tokenPath, []byte("same-one-time-value\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	config, credential, err := loadHeadlessBootstrap(context.Background(), stateDir, configPath, tokenPath)
	if err != nil || config == nil || credential != "same-one-time-value" {
		t.Fatalf("incomplete bootstrap = %+v, credential=%q, err=%v", config, credential, err)
	}
	if _, _, err := loadHeadlessBootstrap(context.Background(), stateDir, configPath, ""); err == nil ||
		!strings.Contains(err.Error(), "resume an incomplete") {
		t.Fatalf("missing incomplete-state token error = %v", err)
	}
}

func TestLoadHeadlessBootstrapFirstStartRequiresReadOnlyToken(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	configPath := writeHeadlessConfigFixture(t)
	tokenPath := filepath.Join(t.TempDir(), "enrollment-token")
	if err := os.WriteFile(tokenPath, []byte("one-time-value\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	_, credential, err := loadHeadlessBootstrap(context.Background(), stateDir, configPath, tokenPath)
	if err != nil || credential != "one-time-value" {
		t.Fatalf("first bootstrap credential=%q, err=%v", credential, err)
	}
	if _, _, err := loadHeadlessBootstrap(context.Background(), stateDir, configPath, ""); err == nil || !strings.Contains(err.Error(), "required for first") {
		t.Fatalf("missing first token error = %v", err)
	}
}

func TestHeadlessNativeOpenFailureDoesNotCommitShareOrExposeCredential(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	configPath := writeHeadlessConfigFixture(t)
	tokenPath := filepath.Join(t.TempDir(), "enrollment-token")
	const credential = "secret-one-time-value"
	if err := os.WriteFile(tokenPath, []byte(credential), 0o400); err != nil {
		t.Fatal(err)
	}
	original := openShareNativeRuntime
	t.Cleanup(func() { openShareNativeRuntime = original })
	openShareNativeRuntime = func(_ context.Context, config connectorshare.NativeRuntimeConfig) (*connectorshare.NativeRuntime, error) {
		if config.EnrollmentCredential != credential {
			t.Fatalf("enrollment credential = %q", config.EnrollmentCredential)
		}
		if config.SessionOperations.OwnerID != "own_cli_fixture" {
			t.Fatalf("native session authority = %#v", config.SessionOperations)
		}
		if len(config.UDPOptions) != 0 {
			t.Fatalf("native UDP options = %d, want native UDP defaults", len(config.UDPOptions))
		}
		return nil, errors.Join(errors.New("native bootstrap rejected"), qurl.ErrAssignmentKeyRejected)
	}
	opts := &globalOpts{
		version: "test", resolvedEndpoint: "https://api.example.com", redirectFRPLogs: func() {},
		resolvedShareGroupMode: connectordaemon.GroupModeSingle,
		resolveShareStateDir:   func(string) (string, error) { return stateDir, nil },
		resolveHubBootstrap:    func() (qurl.HubBootstrap, error) { return qurl.HubBootstrap{}, nil },
		resolveSessionConfig:   testNativeSessionConfig,
	}
	err := runShareDaemonWithBootstrap(context.Background(), opts, stateDir, "test-job", configPath, tokenPath)
	if err == nil || !strings.Contains(err.Error(), "native bootstrap rejected") {
		t.Fatalf("run error = %v", err)
	}
	if strings.Contains(err.Error(), credential) {
		t.Fatalf("error exposed enrollment credential: %v", err)
	}
	registry, openErr := connectorstate.OpenLocalShareRegistry(stateDir)
	if openErr != nil {
		t.Fatal(openErr)
	}
	shares, listErr := registry.List(context.Background())
	ownerID, ownerPresent, ownerErr := registry.OwnerID(context.Background())
	if listErr != nil || len(shares) != 0 || ownerErr != nil || ownerPresent || ownerID != "" {
		t.Fatalf("failed bootstrap durable state = shares %+v list %v owner %q/%v/%v", shares, listErr, ownerID, ownerPresent, ownerErr)
	}
}

func TestHeadlessDaemonRetriesTransientBootstrapInProcessThenServes(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "qurl-headless-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	configPath := writeHeadlessConfigFixture(t)
	tokenPath := filepath.Join(t.TempDir(), "enrollment-token")
	const credential = "one-time-transient-retry-value"
	if err := os.WriteFile(tokenPath, []byte(credential), 0o400); err != nil {
		t.Fatal(err)
	}
	originalBuilder := buildNativeSessionFactory
	originalWait := waitHeadlessNativeRetry
	t.Cleanup(func() {
		buildNativeSessionFactory = originalBuilder
		waitHeadlessNativeRetry = originalWait
	})
	factory := &headlessTestFactory{started: make(chan struct{})}
	var attempts atomic.Int32
	buildNativeSessionFactory = func(_ context.Context, cfg connectorshare.NativeRuntimeConfig, _ *v1.ClientCommonConfig, apiConfig *qurlapi.Config, verifyOwner bool) (connectordaemon.GroupFactory, error) {
		if !verifyOwner {
			t.Fatal("first headless bootstrap did not request authenticated owner verification")
		}
		if apiConfig == nil || apiConfig.BaseURL != "https://api.example.com" || apiConfig.Version != "test" ||
			apiConfig.Verbose == nil || apiConfig.Sleep == nil || apiConfig.NewRequestID == nil {
			t.Fatalf("headless registered-client config = %+v, want endpoint, version, and observability hooks", apiConfig)
		}
		if cfg.EnrollmentCredential != credential {
			t.Fatalf("attempt %d enrollment credential = %q", attempts.Load()+1, cfg.EnrollmentCredential)
		}
		if cfg.SessionOperations.OwnerID != "own_cli_fixture" {
			t.Fatalf("attempt %d native session authority = %#v", attempts.Load()+1, cfg.SessionOperations)
		}
		if len(cfg.UDPOptions) != 0 {
			t.Fatalf("attempt %d native UDP options = %d, want native UDP defaults", attempts.Load()+1, len(cfg.UDPOptions))
		}
		if attempts.Add(1) < 3 {
			return nil, errors.New("temporary native network failure")
		}
		return factory, nil
	}
	waitHeadlessNativeRetry = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	opts := &globalOpts{
		version: "test", resolvedEndpoint: "https://api.example.com", redirectFRPLogs: func() {}, verbose: true,
		resolvedShareGroupMode: connectordaemon.GroupModeSingle,
		sleep:                  func(time.Duration) {}, newRequestID: func() string { return "headless-test-request" },
		resolveShareStateDir: func(string) (string, error) { return stateDir, nil },
		resolveHubBootstrap:  func() (qurl.HubBootstrap, error) { return qurl.HubBootstrap{}, nil },
		resolveSessionConfig: testNativeSessionConfig,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runShareDaemonWithBootstrap(ctx, opts, stateDir, "test-job", configPath, tokenPath) }()
	select {
	case <-factory.started:
	case err := <-done:
		t.Fatalf("headless daemon exited before serving: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("headless daemon did not recover from transient bootstrap failures")
	}
	if attempts.Load() != 3 {
		t.Fatalf("native bootstrap attempts=%d, want 3 in one process", attempts.Load())
	}
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	shares, err := registry.List(context.Background())
	if err != nil || len(shares) != 1 || shares[0].DesiredState != "on" {
		t.Fatalf("headless registry after recovery = %+v err=%v", shares, err)
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, connectorstate.LocalSharesFile)) //nolint:gosec // G304: test-owned temporary state directory and fixed registry filename.
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), credential) {
		t.Fatal("local registry persisted enrollment credential")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("headless shutdown = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("headless daemon did not drain on cancellation")
	}
	if factory.closed.Load() != 1 {
		t.Fatalf("factory closes=%d, want 1", factory.closed.Load())
	}
}

func TestHeadlessNativeOpenRetryIsVisibleAndRedacted(t *testing.T) {
	originalWait := waitHeadlessNativeRetry
	t.Cleanup(func() { waitHeadlessNativeRetry = originalWait })
	waitHeadlessNativeRetry = func(context.Context, time.Duration) error { return nil }

	var logs bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(originalLogger) })

	const secret = "lv_live_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	factory := &headlessTestFactory{started: make(chan struct{})}
	attempts := 0
	got, err := openHeadlessSessionFactory(context.Background(), func(context.Context) (connectordaemon.GroupFactory, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("temporary native failure for " + secret)
		}
		return factory, nil
	})
	if err != nil || got != factory || attempts != 2 {
		t.Fatalf("headless open = %T, attempts=%d, err=%v", got, attempts, err)
	}
	text := logs.String()
	if !strings.Contains(text, "headless share daemon bootstrap failed; retrying") ||
		!strings.Contains(text, "attempt=1") || !strings.Contains(text, "retry_in=250ms") {
		t.Fatalf("retry log missing context: %q", text)
	}
	if strings.Contains(text, secret) || !strings.Contains(text, "lv_***") {
		t.Fatalf("retry log did not redact credential: %q", text)
	}
}

func TestHeadlessWarmRestartOwnsExactlyThePersistedShare(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "qurl-headless-warm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	configPath := writeHeadlessConfigFixture(t)
	config, err := connectorstate.LoadHeadlessConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Put(context.Background(), &config.Shares[0]); err != nil {
		t.Fatal(err)
	}
	writeHeadlessAgentState(t, stateDir, true)

	originalBuilder := buildNativeSessionFactory
	t.Cleanup(func() { buildNativeSessionFactory = originalBuilder })
	factory := &headlessTestFactory{started: make(chan struct{})}
	buildNativeSessionFactory = func(_ context.Context, cfg connectorshare.NativeRuntimeConfig, _ *v1.ClientCommonConfig, _ *qurlapi.Config, verifyOwner bool) (connectordaemon.GroupFactory, error) {
		if verifyOwner {
			t.Fatal("warm headless restart repeated authenticated owner verification")
		}
		if cfg.EnrollmentCredential != "" {
			t.Fatalf("warm restart retained enrollment credential %q", cfg.EnrollmentCredential)
		}
		return factory, nil
	}
	opts := &globalOpts{
		version: "test", resolvedEndpoint: "https://api.example.com", redirectFRPLogs: func() {},
		resolvedShareGroupMode: connectordaemon.GroupModeSingle,
		resolveShareStateDir:   func(string) (string, error) { return stateDir, nil },
		resolveHubBootstrap:    func() (qurl.HubBootstrap, error) { return qurl.HubBootstrap{}, nil },
		resolveSessionConfig:   testNativeSessionConfig,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runShareDaemonWithBootstrap(ctx, opts, stateDir, "test-job", configPath, "") }()
	select {
	case <-factory.started:
	case err := <-done:
		t.Fatalf("warm headless daemon exited before serving: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("warm headless daemon did not start the persisted share")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("warm headless shutdown = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("warm headless daemon did not drain")
	}
	shares, err := registry.List(context.Background())
	if err != nil || len(shares) != 1 || shares[0].ResourceID != config.Shares[0].ResourceID {
		t.Fatalf("warm registry = %+v, %v", shares, err)
	}
}

func TestHeadlessBootstrapRejectsChangedOrAdditionalPersistedResources(t *testing.T) {
	for _, test := range []struct {
		name  string
		extra bool
	}{
		{name: "changed resource"},
		{name: "additional resource", extra: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateDir := connectorStateTestDir(t)
			configPath := writeHeadlessConfigFixture(t)
			config, err := connectorstate.LoadHeadlessConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := openOwnedTestShareRegistry(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if test.extra {
				if err := registry.Put(context.Background(), &config.Shares[0]); err != nil {
					t.Fatal(err)
				}
			}
			other := localShareFixture(apitest.NewServer(t))
			if err := registry.Put(context.Background(), &other); err != nil {
				t.Fatal(err)
			}
			writeHeadlessAgentState(t, stateDir, true)

			originalBuilder := buildNativeSessionFactory
			t.Cleanup(func() { buildNativeSessionFactory = originalBuilder })
			var opens atomic.Int32
			buildNativeSessionFactory = func(context.Context, connectorshare.NativeRuntimeConfig, *v1.ClientCommonConfig, *qurlapi.Config, bool) (connectordaemon.GroupFactory, error) {
				opens.Add(1)
				return &headlessTestFactory{started: make(chan struct{})}, nil
			}
			opts := &globalOpts{
				version: "test", resolvedEndpoint: "https://api.example.com", redirectFRPLogs: func() {},
				resolvedShareGroupMode: connectordaemon.GroupModeSingle,
				resolveShareStateDir:   func(string) (string, error) { return stateDir, nil },
				resolveHubBootstrap:    func() (qurl.HubBootstrap, error) { return qurl.HubBootstrap{}, nil },
				resolveSessionConfig:   testNativeSessionConfig,
			}
			err = runShareDaemonWithBootstrap(context.Background(), opts, stateDir, "test-job", configPath, "")
			if err == nil || !strings.Contains(err.Error(), "dedicated state volume") {
				t.Fatalf("ownership error = %v", err)
			}
			if opens.Load() != 0 {
				t.Fatalf("native session factory opened %d times before ownership rejection", opens.Load())
			}
			shares, listErr := registry.List(context.Background())
			want := 1
			if test.extra {
				want = 2
			}
			if listErr != nil || len(shares) != want {
				t.Fatalf("registry mutated on ownership rejection: %+v, %v", shares, listErr)
			}
		})
	}
}

func writeHeadlessConfigFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "share.yaml")
	data := `version: 2
owner_id: own_cli_fixture
shares:
  - crid: qhpviqz46qwcvx56glfatm3p3ooccwfcf2it4sdgjervwdkapykw2j2vj4uq
    resource_id: MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE2cTVv5_3eeYCcLLq5ROYCqcmY50HiKZ9ATglIkPnCji1E_S63UMtXba1moR8-Q6EV7oM6zwwh9_j2CDujzXvLA
    connector_id: headless-app
    connector_routing_id: c-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    knock_resource_id: q_catalog_key
    target_url: http://127.0.0.1:8080
    local_ip: 127.0.0.1
    local_port: 8080
    desired_state: on
    serving_epoch: 1
`
	if err := os.WriteFile(path, []byte(data), 0o444); err != nil { // #nosec G306 -- non-secret read-only config fixture.
		t.Fatal(err)
	}
	return path
}

func writeHeadlessAgentState(t *testing.T, stateDir string, complete bool) {
	t.Helper()
	store, err := connectorstate.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	sdkStore, err := store.Handoff()
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	state := &qurl.AgentState{AgentID: "headless-test-agent"}
	if complete {
		registeredAt := time.Now().UTC()
		state.RegisteredAt = &registeredAt
		state.DeviceAPIKey = "lv_device_headless_test"
	}
	if err := sdkStore.SaveAgentState(context.Background(), state); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
