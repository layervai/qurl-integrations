package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

const validTestHubKeyB64 = "CQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

const testEnrollmentToken = "one-shot-token"

// lifecycleEnv pins a hermetic environment for Open: a valid custom Hub
// triple, a private state dir, and no ambient operator overrides.
func lifecycleEnv(t *testing.T) string {
	t.Helper()
	t.Setenv(hub.EnvHost, "hub.test.nhp.layerv.ai")
	t.Setenv(hub.EnvPort, "443")
	t.Setenv(hub.EnvServerPublicKey, validTestHubKeyB64)
	dir := t.TempDir()
	t.Setenv(state.EnvStateDirPrimary, dir)
	for _, name := range []string{state.EnvStateDir, state.EnvAgentID, EnvRefreshMode, EnvEnrollmentToken, EnvEnrollmentTokenFile, EnvKnockResourceID} {
		t.Setenv(name, "restore-after-test")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	// Skip on platforms where qurl-go's pinned local agent state is
	// unsupported (Windows today) — every Open path needs the state store.
	probe, err := state.Open(dir)
	if err != nil {
		if errors.Is(err, qurl.ErrAgentStateContinuity) && strings.Contains(err.Error(), "unsupported on this platform") {
			t.Skipf("qurl-go pinned agent state unsupported here: %v", err)
		}
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

type seamCalls struct {
	register int
	open     int
	refresh  int
}

// installSeams swaps the qurl-go lifecycle entry points for fakes and
// restores them on cleanup.
func installSeams(
	t *testing.T,
	calls *seamCalls,
	register func(ctx context.Context, credential string, store qurl.AgentStateStore, opts ...qurl.AgentRuntimeRegistrationOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error),
	open func(ctx context.Context, store qurl.AgentStateStore, opts ...qurl.AgentRuntimeOpenOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error),
	refresh func(ctx context.Context, h qurl.HubBootstrap, store qurl.AgentStateStore, opts ...qurl.AgentRuntimeRefreshOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error),
) {
	t.Helper()
	origRegister, origOpen, origRefresh := registerAgentRuntime, openRegisteredAgentRuntime, refreshAgentRuntime
	t.Cleanup(func() {
		registerAgentRuntime, openRegisteredAgentRuntime, refreshAgentRuntime = origRegister, origOpen, origRefresh
	})
	registerAgentRuntime = func(ctx context.Context, credential string, store qurl.AgentStateStore, opts ...qurl.AgentRuntimeRegistrationOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
		calls.register++
		if register == nil {
			t.Fatal("unexpected registerAgentRuntime call")
		}
		return register(ctx, credential, store, opts...)
	}
	openRegisteredAgentRuntime = func(ctx context.Context, store qurl.AgentStateStore, opts ...qurl.AgentRuntimeOpenOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
		calls.open++
		if open == nil {
			t.Fatal("unexpected openRegisteredAgentRuntime call")
		}
		return open(ctx, store, opts...)
	}
	refreshAgentRuntime = func(ctx context.Context, h qurl.HubBootstrap, store qurl.AgentStateStore, opts ...qurl.AgentRuntimeRefreshOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
		calls.refresh++
		if refresh == nil {
			t.Fatal("unexpected refreshAgentRuntime call")
		}
		return refresh(ctx, h, store, opts...)
	}
}

func fakeRuntimePair(t *testing.T, agentID string) (*qurl.Client, *qurl.AgentRuntimeBinding) {
	t.Helper()
	client, err := qurl.NewClient(qurl.BearerToken("device-credential"), qurl.WithBaseURL("https://api.test.layerv.ai"))
	if err != nil {
		t.Fatal(err)
	}
	return client, &qurl.AgentRuntimeBinding{
		AgentID:        agentID,
		NHPUDPEndpoint: qurl.NHPUDPEndpoint{Host: "hub.test.nhp.layerv.ai", Port: 443},
	}
}

func testConfig() *Config {
	return &Config{
		APIBaseURL: "https://api.test.layerv.ai/v1",
		Version:    "1.2.3",
		Logger:     slog.New(slog.DiscardHandler),
	}
}

// armMarker arms (and optionally consumes) a refresh episode in dir.
func armMarker(t *testing.T, dir, reason string, attempted bool) {
	t.Helper()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.RequestRefresh(reason); err != nil {
		t.Fatal(err)
	}
	if attempted {
		if err := store.MarkRefreshAttempted(); err != nil {
			t.Fatal(err)
		}
	}
}

func readMarker(t *testing.T, dir string) (state.RefreshMarker, bool) {
	t.Helper()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	marker, present, err := store.LoadRefreshMarker()
	if err != nil {
		t.Fatal(err)
	}
	return marker, present
}

func TestOpenWarmOpenSuccess(t *testing.T) {
	lifecycleEnv(t)
	calls := &seamCalls{}
	wantClient, wantBinding := fakeRuntimePair(t, "agent-a")
	installSeams(t, calls, nil, func(_ context.Context, store qurl.AgentStateStore, _ ...qurl.AgentRuntimeOpenOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
		if _, ok := store.(*qurl.FileAgentStateStore); !ok {
			t.Errorf("warm open received %T, want the concrete file store handoff", store)
		}
		return wantClient, wantBinding, nil
	}, nil)

	runtime, err := Open(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()
	if runtime.Client != wantClient || runtime.Binding != wantBinding || runtime.AgentID != "agent-a" {
		t.Fatalf("runtime = %+v", runtime)
	}
	if runtime.Hub.Host != "hub.test.nhp.layerv.ai" || runtime.Hub.Port != 443 {
		t.Fatalf("runtime hub = %+v", runtime.Hub)
	}
	if calls.open != 1 || calls.register != 0 || calls.refresh != 0 {
		t.Fatalf("seam calls = %+v, want warm open only", calls)
	}
	if err := runtime.ValidateContinuity(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenFirstRegistrationRequiresToken pins the zero-network token-required
// path: no stored identity plus no configured token refuses with the exported
// sentinel BEFORE the registration seam (the network transaction) is invoked.
func TestOpenFirstRegistrationRequiresToken(t *testing.T) {
	lifecycleEnv(t)
	calls := &seamCalls{}
	installSeams(t, calls,
		func(context.Context, string, qurl.AgentStateStore, ...qurl.AgentRuntimeRegistrationOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
			t.Error("registerAgentRuntime must not be called without an enrollment credential")
			return nil, nil, errors.New("unreachable")
		},
		func(context.Context, qurl.AgentStateStore, ...qurl.AgentRuntimeOpenOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
			return nil, nil, fmt.Errorf("no persisted state: %w", qurl.ErrAgentStateNotFound)
		}, nil)

	_, err := Open(context.Background(), testConfig())
	if !errors.Is(err, ErrEnrollmentTokenRequired) {
		t.Fatalf("Open = %v, want ErrEnrollmentTokenRequired", err)
	}
	for _, want := range []string{EnvEnrollmentToken, EnvEnrollmentTokenFile} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("token-required guidance %q missing from %v", want, err)
		}
	}
	if calls.register != 0 {
		t.Fatalf("register seam calls = %d, want 0 (the refusal must be zero-network)", calls.register)
	}
}

func TestOpenRegistersWithTokenAndMetadata(t *testing.T) {
	lifecycleEnv(t)
	t.Setenv(state.EnvAgentID, "agent-a")
	calls := &seamCalls{}
	wantClient, wantBinding := fakeRuntimePair(t, "agent-a")
	installSeams(t, calls,
		func(_ context.Context, credential string, store qurl.AgentStateStore, opts ...qurl.AgentRuntimeRegistrationOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
			if credential != testEnrollmentToken {
				t.Errorf("credential = %q, want the flag-first token", credential)
			}
			if _, ok := store.(*qurl.FileAgentStateStore); !ok {
				t.Errorf("registration received %T, want the concrete file store handoff", store)
			}
			if len(opts) == 0 {
				t.Error("registration options missing (hub/metadata/key kinds)")
			}
			return wantClient, wantBinding, nil
		},
		func(context.Context, qurl.AgentStateStore, ...qurl.AgentRuntimeOpenOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
			return nil, nil, qurl.ErrAgentStateNotFound
		}, nil)

	cfg := testConfig()
	cfg.EnrollmentToken = testEnrollmentToken
	runtime, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()
	if runtime.AgentID != "agent-a" || calls.register != 1 {
		t.Fatalf("runtime agent = %q, register calls = %d", runtime.AgentID, calls.register)
	}
}

func TestOpenRegistrationStallEnrichment(t *testing.T) {
	lifecycleEnv(t)
	tests := []struct {
		name         string
		cause        error
		wantEnriched bool
	}{
		{name: "no reply is enriched", cause: fmt.Errorf("assignment: %w", qurl.ErrEndpointNoReply), wantEnriched: true},
		{name: "deadline is enriched", cause: context.DeadlineExceeded, wantEnriched: true},
		{name: "recovery sentinel is enriched", cause: qurl.ErrRegistrationRecoveryRequired, wantEnriched: true},
		{name: "authenticated denial stays specific", cause: qurl.ErrKeyRejected, wantEnriched: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := &seamCalls{}
			installSeams(t, calls,
				func(context.Context, string, qurl.AgentStateStore, ...qurl.AgentRuntimeRegistrationOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
					return nil, nil, tt.cause
				},
				func(context.Context, qurl.AgentStateStore, ...qurl.AgentRuntimeOpenOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
					return nil, nil, qurl.ErrAgentStateNotFound
				}, nil)
			cfg := testConfig()
			cfg.EnrollmentToken = testEnrollmentToken
			_, err := Open(context.Background(), cfg)
			if err == nil || !errors.Is(err, tt.cause) {
				t.Fatalf("Open = %v, want cause preserved", err)
			}
			var stalled *registrationStalledError
			if got := errors.As(err, &stalled); got != tt.wantEnriched {
				t.Fatalf("stall enrichment = %v on %v, want %v", got, err, tt.wantEnriched)
			}
			if tt.wantEnriched && !strings.Contains(err.Error(), hub.EnvHost) {
				t.Fatalf("enriched stall %v does not name the Hub override to check", err)
			}
		})
	}
}

func TestOpenOperatorCancelStaysBare(t *testing.T) {
	lifecycleEnv(t)
	calls := &seamCalls{}
	ctx, cancel := context.WithCancel(context.Background())
	installSeams(t, calls,
		func(context.Context, string, qurl.AgentStateStore, ...qurl.AgentRuntimeRegistrationOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
			cancel() // the operator interrupts while registration is in flight
			return nil, nil, context.Canceled
		},
		func(context.Context, qurl.AgentStateStore, ...qurl.AgentRuntimeOpenOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
			return nil, nil, qurl.ErrAgentStateNotFound
		}, nil)
	cfg := testConfig()
	cfg.EnrollmentToken = testEnrollmentToken
	_, err := Open(ctx, cfg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open = %v, want context.Canceled", err)
	}
	var stalled *registrationStalledError
	if errors.As(err, &stalled) {
		t.Fatalf("operator cancel was reinterpreted as a stall: %v", err)
	}
}

func TestOpenLeaseExpiredArmsMarkerAndManualGateHolds(t *testing.T) {
	dir := lifecycleEnv(t)
	calls := &seamCalls{}
	installSeams(t, calls, nil,
		func(context.Context, qurl.AgentStateStore, ...qurl.AgentRuntimeOpenOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
			return nil, nil, fmt.Errorf("warm open: %w", qurl.ErrAssignmentLeaseExpired)
		}, nil)

	_, err := Open(context.Background(), testConfig())
	if !errors.Is(err, ErrRefreshApprovalRequired) || !strings.Contains(err.Error(), EnvRefreshMode) {
		t.Fatalf("Open under manual mode = %v, want ErrRefreshApprovalRequired naming %s", err, EnvRefreshMode)
	}
	marker, present := readMarker(t, dir)
	if !present || marker.Attempted || marker.Reason != "assigned NHP cell lease expired" {
		t.Fatalf("marker after lease expiry = (%+v, present=%v), want an armed unattempted episode", marker, present)
	}
	if calls.refresh != 0 {
		t.Fatalf("refresh calls = %d, want 0 while the manual gate holds", calls.refresh)
	}
}

func TestOpenRefreshDisabledFailsClosed(t *testing.T) {
	dir := lifecycleEnv(t)
	armMarker(t, dir, "sustained failures", false)
	t.Setenv(EnvRefreshMode, "disabled")
	calls := &seamCalls{}
	installSeams(t, calls, nil, nil, nil)

	_, err := Open(context.Background(), testConfig())
	if !errors.Is(err, ErrRefreshDisabled) || !strings.Contains(err.Error(), dir) {
		t.Fatalf("Open under disabled mode = %v, want ErrRefreshDisabled naming the state dir", err)
	}
}

// TestOpenRefreshModeFlagOverridesEnv pins the Config.RefreshMode plumbing:
// an explicit manual override holds the gate even when the environment says
// auto, so the command's flag is authoritative over ambient configuration.
func TestOpenRefreshModeFlagOverridesEnv(t *testing.T) {
	dir := lifecycleEnv(t)
	armMarker(t, dir, "sustained failures", false)
	t.Setenv(EnvRefreshMode, "auto")
	calls := &seamCalls{}
	installSeams(t, calls, nil, nil, nil)

	cfg := testConfig()
	cfg.RefreshMode = RefreshModeManual
	_, err := Open(context.Background(), cfg)
	if !errors.Is(err, ErrRefreshApprovalRequired) {
		t.Fatalf("Open with explicit manual over env auto = %v, want ErrRefreshApprovalRequired", err)
	}
	if calls.refresh != 0 {
		t.Fatalf("refresh calls = %d, want 0 under the explicit manual gate", calls.refresh)
	}
}

func TestOpenRefreshAutoConsumesEpisodeOnce(t *testing.T) {
	dir := lifecycleEnv(t)
	armMarker(t, dir, "sustained failures", false)
	t.Setenv(EnvRefreshMode, "auto")
	calls := &seamCalls{}
	wantClient, wantBinding := fakeRuntimePair(t, "agent-a")
	installSeams(t, calls, nil, nil,
		func(_ context.Context, h qurl.HubBootstrap, store qurl.AgentStateStore, _ ...qurl.AgentRuntimeRefreshOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
			if h.Host != "hub.test.nhp.layerv.ai" {
				t.Errorf("refresh hub = %+v, want the custom triple threaded through", h)
			}
			if _, ok := store.(*qurl.FileAgentStateStore); !ok {
				t.Errorf("refresh received %T, want the concrete file store handoff", store)
			}
			// The one-per-episode budget must be consumed BEFORE Hub I/O so a
			// crash mid-refresh cannot re-arm another refresh.
			marker, present := readMarker(t, dir)
			if !present || !marker.Attempted {
				t.Errorf("marker during refresh = (%+v, present=%v), want already attempted", marker, present)
			}
			return wantClient, wantBinding, nil
		})

	runtime, err := Open(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()
	if calls.refresh != 1 || calls.open != 0 || calls.register != 0 {
		t.Fatalf("seam calls = %+v, want refresh only", calls)
	}
	// The marker survives a successful refresh: only a confirmed-healthy
	// knock clears it.
	marker, present := readMarker(t, dir)
	if !present || !marker.Attempted {
		t.Fatalf("marker after refresh = (%+v, present=%v), want the attempted episode retained", marker, present)
	}
}

func TestOpenAttemptedMarkerBlocksSecondRefresh(t *testing.T) {
	dir := lifecycleEnv(t)
	armMarker(t, dir, "sustained failures", true)
	t.Setenv(EnvRefreshMode, "auto")
	calls := &seamCalls{}
	warmErr := errors.New("still failing after the consumed refresh")
	installSeams(t, calls, nil,
		func(context.Context, qurl.AgentStateStore, ...qurl.AgentRuntimeOpenOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
			return nil, nil, warmErr
		}, nil)

	_, err := Open(context.Background(), testConfig())
	if !errors.Is(err, ErrRefreshAlreadyAttempted) || !errors.Is(err, warmErr) {
		t.Fatalf("Open = %v, want the already-attempted episode error joined with the warm-open cause", err)
	}
	if calls.refresh != 0 {
		t.Fatalf("refresh calls = %d, want the consumed episode to block a second refresh", calls.refresh)
	}
}

func TestOpenCorruptMarkerIsClearedFailSafe(t *testing.T) {
	dir := lifecycleEnv(t)
	if err := os.WriteFile(filepath.Join(dir, state.RefreshMarkerFile), []byte("{torn"), 0o644); err != nil { //nolint:gosec // G306: non-secret marker fixture.
		t.Fatal(err)
	}
	calls := &seamCalls{}
	wantClient, wantBinding := fakeRuntimePair(t, "agent-a")
	installSeams(t, calls, nil, func(context.Context, qurl.AgentStateStore, ...qurl.AgentRuntimeOpenOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
		return wantClient, wantBinding, nil
	}, nil)

	runtime, err := Open(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("Open with corrupt marker = %v, want fail-safe clear + ordinary warm open", err)
	}
	defer func() { _ = runtime.Close() }()
	if _, present := readMarker(t, dir); present {
		t.Fatal("corrupt marker survived the fail-safe clear")
	}
}

func TestOpenConfiguredAgentIdentityConflictFailsClosed(t *testing.T) {
	lifecycleEnv(t)
	t.Setenv(state.EnvAgentID, "agent-configured")
	calls := &seamCalls{}
	client, binding := fakeRuntimePair(t, "agent-persisted")
	installSeams(t, calls, nil, func(context.Context, qurl.AgentStateStore, ...qurl.AgentRuntimeOpenOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
		return client, binding, nil
	}, nil)

	_, err := Open(context.Background(), testConfig())
	if !errors.Is(err, ErrIdentityConflict) || !strings.Contains(err.Error(), "agent-configured") || !strings.Contains(err.Error(), "agent-persisted") {
		t.Fatalf("Open = %v, want ErrIdentityConflict naming both values", err)
	}
}

func TestOpenIncompleteRuntimeFailsClosed(t *testing.T) {
	lifecycleEnv(t)
	calls := &seamCalls{}
	_, binding := fakeRuntimePair(t, "agent-a")
	installSeams(t, calls, nil, func(context.Context, qurl.AgentStateStore, ...qurl.AgentRuntimeOpenOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
		return nil, binding, nil // nil client with a live binding
	}, nil)

	_, err := Open(context.Background(), testConfig())
	if err == nil || !strings.Contains(err.Error(), "incomplete native agent runtime") {
		t.Fatalf("Open = %v, want incomplete-runtime rejection", err)
	}
}

func TestOpenDarkHubFailsClosedBeforeAnySDKCall(t *testing.T) {
	lifecycleEnv(t)
	for _, name := range []string{hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey} {
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	calls := &seamCalls{}
	installSeams(t, calls, nil, nil, nil)

	_, err := Open(context.Background(), testConfig())
	if err == nil || !strings.Contains(err.Error(), "no pinned production Hub key") {
		t.Fatalf("Open on a dark build = %v, want the fail-closed Hub error", err)
	}
	if calls.open != 0 && calls.register != 0 && calls.refresh != 0 {
		t.Fatalf("seam calls = %+v, want none before the trust root resolves", calls)
	}
}

func TestRuntimeCloseIsIdempotentAndNilSafe(t *testing.T) {
	var nilRuntime *Runtime
	if err := nilRuntime.Close(); err != nil {
		t.Fatalf("nil Close = %v", err)
	}
	if err := nilRuntime.ValidateContinuity(); !errors.Is(err, qurl.ErrAgentStateContinuity) {
		t.Fatalf("nil ValidateContinuity = %v, want state-continuity error", err)
	}
	runtime := &Runtime{}
	if err := runtime.Close(); err != nil {
		t.Fatalf("empty Close = %v", err)
	}
}
