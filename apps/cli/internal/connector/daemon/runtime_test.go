package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/layervai/qurl-connector/pkg/agentstate"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/relayknock/nativeudp"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

type lockedLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(data)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type failingResourceAdmitter struct{ err error }

func (a *failingResourceAdmitter) Admit(context.Context, string, string) (connectorshare.Admission, error) {
	return connectorshare.Admission{}, a.err
}

func (*failingResourceAdmitter) Retire(context.Context, connectorshare.Admission) error { return nil }
func (*failingResourceAdmitter) MarkServingHealthy() error                              { return nil }
func (*failingResourceAdmitter) Close() error                                           { return nil }

func TestDefaultFRPCommonCannotUseEnvironmentProxy(t *testing.T) {
	const proxy = "http://user:secret@private-proxy.example:8080"
	t.Setenv("http_proxy", proxy)
	inherited := &v1.ClientCommonConfig{}
	if err := inherited.Complete(); err != nil {
		t.Fatal(err)
	}
	if inherited.Transport.ProxyURL == "" {
		t.Fatal("test no longer exercises FRP's environment-proxy default")
	}
	common, err := DefaultFRPCommon(1, 1)
	if common != nil || !errors.Is(err, ErrDirectEgressRequired) {
		t.Fatalf("proxy configuration = %#v, %v; want actionable rejection", common, err)
	}
	for _, forbidden := range []string{proxy, "secret", "private-proxy.example", "user:"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("proxy rejection exposed %q: %v", forbidden, err)
		}
	}
}

// TestNativeGroupRetryLogsClassifiedRetryWithoutStoppingDaemon drives the real
// SessionGroupRunner through the native group factory with an admitter whose
// knock always fails. The group-wide retry is logged with a redacted error and
// each tracked route reports a classified retrying diagnostic; the daemon keeps
// running and stops cleanly.
func TestNativeGroupRetryLogsClassifiedRetryWithoutStoppingDaemon(t *testing.T) {
	const secret = "lv_live_SUPERSECRETVALUE0000001"
	attemptErr := errors.Join(errors.New("classified native attempt failure from Bearer "+secret),
		&qurl.ServerDenyError{ErrCode: "52005"})
	admitter := &failingResourceAdmitter{err: attemptErr}
	common, err := DefaultFRPCommon(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewNativeGroupFactory(admitter, common, "test")
	if err != nil {
		t.Fatal(err)
	}

	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"retry": daemonShare("retry", 1, "on")}}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()

	waitManagerCondition(t, func() bool {
		return manager.Diagnostics()["retry"].State == diagnosticStateRetrying
	}, "route reaches retrying diagnostic")
	state := manager.Diagnostics()["retry"]
	if state.FailureCategory != diagnosticFailurePlatformDenied || state.FailureCode != "52005" ||
		state.RetryAttempt < 1 || state.NextRetryAt == nil {
		t.Fatalf("retry diagnostic = %#v", state)
	}
	waitManagerCondition(t, func() bool {
		return strings.Contains(logs.String(), "classified native attempt failure")
	}, "redacted retry log")
	got := logs.String()
	for _, want := range []string{"retrying", "classified native attempt failure", "Bearer ***"} {
		if !strings.Contains(got, want) {
			t.Fatalf("retry log %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, secret) {
		t.Fatalf("retry log contains a credential: %q", got)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("daemon shutdown = %v, want cancellation", err)
	}
}

// stubGroupRunner is a group runner whose Run only blocks until its context
// ends. It is enough for the deferred-factory lifecycle tests, which never
// exercise routing.
type stubGroupRunner struct{}

func (stubGroupRunner) Run(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
func (stubGroupRunner) SetRoutes(context.Context, []connectorshare.LocalHTTPRoute) error {
	return nil
}
func (stubGroupRunner) RestartRoute(context.Context, string) error { return nil }
func (stubGroupRunner) RouteStates() map[string]connectorshare.RouteState {
	return map[string]connectorshare.RouteState{}
}

type closeTrackingGroupFactory struct {
	closes atomic.Int32
	err    error
}

func (f *closeTrackingGroupFactory) NewGroupRunner(_ context.Context, _ *GroupConfig) (GroupRunner, error) {
	if f.err != nil {
		return nil, f.err
	}
	return stubGroupRunner{}, nil
}

func (f *closeTrackingGroupFactory) Close() error {
	f.closes.Add(1)
	return nil
}

func groupConfigFixture() *GroupConfig {
	return &GroupConfig{
		KnockResourceID: "q_catalog_key", ResourceID: "resource-a",
		Routes: []connectorshare.LocalHTTPRoute{{
			RouteID: "connector-a", LocalIP: "127.0.0.1", LocalPort: 3000,
			ResourceID: "resource-a", ConnectorRoutingID: "routing-a",
		}},
	}
}

func TestDeferredGroupFactoryRetriesFailedInitialization(t *testing.T) {
	want := errors.New("native transport unavailable")
	delegate := &closeTrackingGroupFactory{}
	var attempts atomic.Int32
	factory, err := NewDeferredGroupFactory(func(context.Context) (GroupFactory, error) {
		if attempts.Add(1) == 1 {
			return nil, want
		}
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory.NewGroupRunner(context.Background(), groupConfigFixture()); !errors.Is(err, want) {
		t.Fatalf("first NewGroupRunner error = %v, want transient failure", err)
	}
	if _, err := factory.NewGroupRunner(context.Background(), groupConfigFixture()); err != nil {
		t.Fatalf("second NewGroupRunner did not retry initialization: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("initializer attempts = %d, want 2", attempts.Load())
	}
	if err := factory.Close(); err != nil {
		t.Fatal(err)
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("successful delegate closes = %d, want 1", delegate.closes.Load())
	}
}

func TestDeferredGroupFactoryKeepsSuccessfulRuntimeContextUntilClose(t *testing.T) {
	delegate := &closeTrackingGroupFactory{}
	var runtimeCtx context.Context
	factory, err := NewDeferredGroupFactory(func(ctx context.Context) (GroupFactory, error) {
		runtimeCtx = ctx
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	startCtx, cancelStart := context.WithCancel(context.Background())
	if _, err := factory.NewGroupRunner(startCtx, groupConfigFixture()); err != nil {
		t.Fatal(err)
	}
	cancelStart()
	if runtimeCtx == nil || runtimeCtx.Err() != nil {
		t.Fatalf("runtime context after caller cancellation = %v, want live context", runtimeCtx)
	}
	if err := factory.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(runtimeCtx.Err(), context.Canceled) {
		t.Fatalf("runtime context after Close = %v, want canceled", runtimeCtx.Err())
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("successful delegate closes = %d, want 1", delegate.closes.Load())
	}
}

func TestDeferredGroupFactoryRejectsDelegateCompletedAfterCallerCancellation(t *testing.T) {
	delegate := &closeTrackingGroupFactory{}
	started := make(chan struct{})
	release := make(chan struct{})
	factory, err := NewDeferredGroupFactory(func(context.Context) (GroupFactory, error) {
		close(started)
		<-release
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	startCtx, cancelStart := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, startErr := factory.NewGroupRunner(startCtx, groupConfigFixture())
		done <- startErr
	}()
	<-started
	cancelStart()
	close(release)
	if startErr := <-done; !errors.Is(startErr, context.Canceled) {
		t.Fatalf("NewGroupRunner error = %v, want caller cancellation", startErr)
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("delegate completed after cancellation closes = %d, want 1", delegate.closes.Load())
	}
}

func TestDeferredGroupFactorySerializesConcurrentInitialization(t *testing.T) {
	delegate := &closeTrackingGroupFactory{}
	entered := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	factory, err := NewDeferredGroupFactory(func(context.Context) (GroupFactory, error) {
		if attempts.Add(1) == 1 {
			close(entered)
		}
		<-release
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := factory.NewGroupRunner(context.Background(), groupConfigFixture())
			errs <- err
		}()
	}
	<-entered
	if attempts.Load() != 1 {
		t.Fatalf("concurrent initializer attempts before release = %d, want 1", attempts.Load())
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent NewGroupRunner: %v", err)
		}
	}
	if attempts.Load() != 1 {
		t.Fatalf("concurrent initializer attempts = %d, want 1", attempts.Load())
	}
}

func TestDeferredGroupFactoryCloseDuringInitializationClosesDelegate(t *testing.T) {
	delegate := &closeTrackingGroupFactory{}
	entered := make(chan struct{})
	factory, err := NewDeferredGroupFactory(func(ctx context.Context) (GroupFactory, error) {
		close(entered)
		<-ctx.Done()
		// Model an initializer that completed native allocation concurrently
		// with cancellation and must hand it back for cleanup.
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	startDone := make(chan error, 1)
	go func() {
		_, err := factory.NewGroupRunner(context.Background(), groupConfigFixture())
		startDone <- err
	}()
	<-entered
	if err := factory.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-startDone; !errors.Is(err, errDeferredFactoryClosed) {
		t.Fatalf("NewGroupRunner error = %v, want closed", err)
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("delegate closes = %d, want 1 before Close returns", delegate.closes.Load())
	}
	if _, err := factory.NewGroupRunner(context.Background(), groupConfigFixture()); !errors.Is(err, errDeferredFactoryClosed) {
		t.Fatalf("NewGroupRunner after Close error = %v", err)
	}
}

func TestDeferredGroupFactoryClosesDelegateReturnedWithFailure(t *testing.T) {
	delegate := &closeTrackingGroupFactory{}
	want := errors.New("native open failed after allocation")
	factory, err := NewDeferredGroupFactory(func(context.Context) (GroupFactory, error) {
		return delegate, want
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory.NewGroupRunner(context.Background(), groupConfigFixture()); !errors.Is(err, want) {
		t.Fatalf("NewGroupRunner error = %v, want failure", err)
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("failed delegate closes = %d, want 1", delegate.closes.Load())
	}
}

func TestDeferredGroupFactoryConcurrentCloseWaitsForOneCleanup(t *testing.T) {
	want := errors.New("delegate close failed")
	delegate := &blockingCloseGroupFactory{entered: make(chan struct{}), release: make(chan struct{}), err: want}
	factory, err := NewDeferredGroupFactory(func(context.Context) (GroupFactory, error) {
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory.NewGroupRunner(context.Background(), groupConfigFixture()); err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- factory.Close() }()
	<-delegate.entered
	secondEntered := make(chan struct{})
	go func() {
		close(secondEntered)
		second <- factory.Close()
	}()
	<-secondEntered
	select {
	case err := <-second:
		t.Fatalf("second Close returned before delegate cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(delegate.release)
	for index, result := range []<-chan error{first, second} {
		if err := <-result; !errors.Is(err, want) {
			t.Fatalf("Close %d error = %v, want shared cleanup failure", index+1, err)
		}
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("delegate closes = %d, want 1", delegate.closes.Load())
	}
}

type blockingCloseGroupFactory struct {
	entered chan struct{}
	release chan struct{}
	err     error
	closes  atomic.Int32
}

func (*blockingCloseGroupFactory) NewGroupRunner(context.Context, *GroupConfig) (GroupRunner, error) {
	return stubGroupRunner{}, nil
}

func (f *blockingCloseGroupFactory) Close() error {
	if f.closes.Add(1) == 1 {
		close(f.entered)
	}
	<-f.release
	return f.err
}

func TestDaemonIPCIsReadyWhileNativeAssignmentRecoveryContinues(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	started := make(chan struct{})
	recovered := make(chan struct{})
	delegate := &closeTrackingGroupFactory{}
	factory, err := NewDeferredGroupFactory(func(ctx context.Context) (GroupFactory, error) {
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-recovered:
			return delegate, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	dir := shortTempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	server := &IPCServer{SocketPath: filepath.Join(dir, SocketFile), Manager: manager, JobVersion: "1/test"}
	go func() { done <- server.Run(ctx) }()
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readyCancel()
	client := IPCClient{SocketPath: filepath.Join(dir, SocketFile)}
	if err := client.WaitReady(readyCtx); err != nil {
		t.Fatalf("IPC did not accept durable ownership during recovery: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("native initializer did not start")
	}
	if running := manager.Running(); len(running) != 0 {
		t.Fatalf("share reported running before assignment recovered: %v", running)
	}
	close(recovered)
	waitManagerCondition(t, func() bool { return manager.Running()["a"] != "" }, "share managed after recovery")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("daemon shutdown = %v, want cancellation", err)
	}
}

func TestClassifyShareFailureTreatsSessionLeaseMarginAsAssignment(t *testing.T) {
	category, code := classifyShareFailure(errors.Join(
		qurl.ErrNativeSessionOperationLeaseMargin,
		errors.New("assignment has insufficient journal margin"),
	))
	if category != diagnosticFailureAssignment || code != "" {
		t.Fatalf("classification=%q/%q, want assignment with no public code", category, code)
	}
}

func TestClassifyShareFailurePreservesNativeUDPVerification(t *testing.T) {
	tests := []struct {
		err      error
		category string
	}{
		{err: nativeudp.ErrTransport, category: diagnosticFailureNetwork},
		{err: nativeudp.ErrServerUnauthenticated, category: diagnosticFailureVerification},
		{err: errors.Join(nativeudp.ErrTransport, nativeudp.ErrServerUnauthenticated), category: diagnosticFailureVerification},
		{err: qurl.ErrMalformedReply, category: diagnosticFailureVerification},
	}
	for _, test := range tests {
		category, code := classifyShareFailure(fmt.Errorf("admit registered session: %w", test.err))
		if category != test.category || code != "" {
			t.Fatalf("classification=%q/%q, want %s with no public code", category, code, test.category)
		}
	}
}

func TestClassifyShareFailureAlwaysProducesIPCCompatibleDiagnostic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		wantCode string
	}{
		{name: "five-digit deny", err: &qurl.ServerDenyError{ErrCode: "52005"}, wantCode: "52005"},
		{name: "short deny", err: &qurl.ServerDenyError{ErrCode: "7"}},
		{name: "nondecimal deny", err: &qurl.ServerDenyError{ErrCode: "52x05"}},
		{
			name: "invalid recovery code",
			err: errors.Join(
				&qurl.CredentialRecoveryError{Code: "52401x", Phase: "hub_issue_recovery"},
				qurl.ErrRecoveryCredentialRejected,
			),
		},
		{
			name: "invalid assignment code",
			err: errors.Join(
				&qurl.AssignmentError{Code: "522010"},
				qurl.ErrAssignmentIdentityRejected,
			),
		},
		{
			name: "invalid completion code",
			err: errors.Join(
				&qurl.CompletionError{Code: "5230x"},
				qurl.ErrCompletionRequestRejected,
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			category, code := classifyShareFailure(test.err)
			if code != test.wantCode {
				t.Fatalf("classification code = %q, want %q", code, test.wantCode)
			}
			now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
			next := now.Add(time.Second)
			encoded, err := json.Marshal(IPCStatus{
				JobVersion: "1/test",
				Running:    map[string]string{"resource": "crid"},
				Resources: map[string]ResourceDiagnostic{"resource": {
					State: diagnosticStateRetrying, LastTransition: now,
					FailureCategory: category, FailureCode: code,
					RetryAttempt: 1, NextRetryAt: &next,
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			status, err := decodeIPCStatus(bytes.NewReader(encoded))
			if err != nil {
				t.Fatalf("classified diagnostic did not survive IPC decode: %v", err)
			}
			if got := status.Resources["resource"].FailureCode; got != test.wantCode {
				t.Fatalf("round-trip code = %q, want %q", got, test.wantCode)
			}
		})
	}
}

func TestClassifyShareFailurePreservesRecoveryTaxonomyCodes(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		sentinel error
		category string
	}{
		{"hub unavailable", "52400", qurl.ErrCredentialRecoveryUnavailable, diagnosticFailurePlatformDenied},
		{"credential rejected", "52401", qurl.ErrRecoveryCredentialRejected, diagnosticFailureIdentity},
		{"hub identity rejected", "52402", qurl.ErrCredentialRecoveryIdentityRejected, diagnosticFailureIdentity},
		{"revoke required", "52403", qurl.ErrCredentialRecoveryRevokeRequired, diagnosticFailureIdentity},
		{"rate limited", "52404", qurl.ErrCredentialRecoveryRateLimited, diagnosticFailurePlatformDenied},
		{"hub request rejected", "52405", qurl.ErrCredentialRecoveryRequestRejected, diagnosticFailurePlatformDenied},
		{"assignment required", "52406", qurl.ErrCredentialRecoveryAssignmentRequired, diagnosticFailureAssignment},
		{"replacement unavailable", "52410", qurl.ErrCredentialReplacementUnavailable, diagnosticFailurePlatformDenied},
		{"grant rejected", "52411", qurl.ErrCredentialRecoveryGrantRejected, diagnosticFailurePlatformDenied},
		{"cell identity rejected", "52412", qurl.ErrCredentialRecoveryIdentityRejected, diagnosticFailureIdentity},
		{"candidate conflict", "52413", qurl.ErrCredentialRecoveryCandidateConflict, diagnosticFailurePlatformDenied},
		{"cell request rejected", "52414", qurl.ErrCredentialRecoveryRequestRejected, diagnosticFailurePlatformDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			phase := "hub_issue_recovery"
			if test.code >= "52410" {
				phase = "assigned_cell_complete_recovery"
			}
			err := errors.Join(
				&qurl.CredentialRecoveryError{Code: test.code, Phase: phase},
				test.sentinel,
			)
			if test.code == "52400" || test.code == "52404" || test.code == "52410" {
				err = &qurl.CredentialRecoveryRetryRequiredError{
					Phase: phase, Attempts: 3, Elapsed: time.Second, Last: err,
				}
			}
			category, code := classifyShareFailure(err)
			if category != test.category || code != test.code {
				t.Fatalf("classification=%q/%q, want %s/%s", category, code, test.category, test.code)
			}
		})
	}
}

func TestClassifyShareFailurePreservesAssignmentIdentityCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{
			name: "assignment identity",
			err: errors.Join(
				&qurl.AssignmentError{Code: "52201"},
				qurl.ErrAssignmentIdentityRejected,
			),
			code: "52201",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			category, code := classifyShareFailure(test.err)
			if category != diagnosticFailureIdentity || code != test.code {
				t.Fatalf("classification=%q/%q, want identity/%s", category, code, test.code)
			}
		})
	}
}

func TestClassifyShareFailureRecoverySentinelsWithoutWireEnvelope(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		category string
	}{
		{"expired", qurl.ErrCredentialRecoveryExpired, diagnosticFailureIdentity},
		{"assignment refresh", qurl.ErrCredentialRecoveredAssignmentRefreshRequired, diagnosticFailureAssignment},
		{"candidate persistence", qurl.ErrCredentialRecoveryCandidatePersistence, diagnosticFailureLocalState},
		{"retry exhausted", qurl.ErrCredentialRecoveryRetryRequired, diagnosticFailurePlatformDenied},
		{"invalid response", qurl.ErrCredentialRecoveryInvalidResponse, diagnosticFailurePlatformDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			category, code := classifyShareFailure(fmt.Errorf("recover native identity: %w", test.err))
			if category != test.category || code != "" {
				t.Fatalf("classification=%q/%q, want %s with no code", category, code, test.category)
			}
		})
	}
}

func TestClassifyShareFailurePreservesPublicEnrollmentTaxonomy(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		category string
		code     string
	}{
		{
			name: "peer timeout",
			err: errors.Join(
				&qurl.EndpointNoReplyError{Endpoint: "private.invalid:443", Attempts: 3, Elapsed: time.Second},
				qurl.ErrRegistrationRecoveryRequired,
			),
			category: diagnosticFailurePeerTimeout,
		},
		{"device credential", &qurl.NativeCredentialRecoveryRequiredError{AgentID: "private-agent", Cause: qurl.ErrDeviceCredentialMissing}, diagnosticFailureIdentity, ""},
		{"registration recovery", qurl.ErrRegistrationRecoveryRequired, diagnosticFailureEnrollment, ""},
		{"registration disabled", qurl.ErrRegistrationDisabled, diagnosticFailurePlatformDenied, ""},
		{"local persistence", qurl.ErrAgentCompletionCandidatePersistence, diagnosticFailureLocalState, ""},
		{"invalid session operation", qurl.ErrInvalidNativeSessionOperation, diagnosticFailureLocalState, ""},
		{"operation conflict", agentstate.ErrSessionOperationConflict, diagnosticFailureLocalState, ""},
		{"operation journal", agentstate.ErrSessionOperationJournalCorrupt, diagnosticFailureLocalState, ""},
		{
			name: "completion identity code",
			err: errors.Join(
				&qurl.CompletionError{Code: "52301"},
				qurl.ErrCompletionIdentityRejected,
			),
			category: diagnosticFailureIdentity,
			code:     "52301",
		},
		{
			name: "completion request code",
			err: errors.Join(
				&qurl.CompletionError{Code: "52304"},
				qurl.ErrCompletionRequestRejected,
			),
			category: diagnosticFailureEnrollment,
			code:     "52304",
		},
		{
			name: "unrecognized completion code hidden",
			err: errors.Join(
				&qurl.CompletionError{Code: "99999"},
				qurl.ErrCompletionRequestRejected,
			),
			category: diagnosticFailureEnrollment,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			category, code := classifyShareFailure(fmt.Errorf("run Connector: %w", test.err))
			if category != test.category || code != test.code {
				t.Fatalf("classification=%q/%q, want %s/%s", category, code, test.category, test.code)
			}
		})
	}
}

func TestClassifyShareFailureRouteNotServingIsResourceUnavailable(t *testing.T) {
	category, code := classifyShareFailure(fmt.Errorf("route stalled: %w", connectorshare.ErrRouteNotServing))
	if category != diagnosticFailureResourceUnavailable || code != "" {
		t.Fatalf("classification=%q/%q, want resource_unavailable with no code", category, code)
	}
}

// blockingStartGroupFactory blocks inside NewGroupRunner until released, so a
// test can hold DeferredGroupFactory's activeStarts across a concurrent Close.
type blockingStartGroupFactory struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	closes  atomic.Int32
}

func (f *blockingStartGroupFactory) NewGroupRunner(context.Context, *GroupConfig) (GroupRunner, error) {
	f.once.Do(func() { close(f.entered) })
	<-f.release
	return stubGroupRunner{}, nil
}

func (f *blockingStartGroupFactory) Close() error {
	f.closes.Add(1)
	return nil
}

func TestDeferredGroupFactoryCloseWaitsForConcurrentNewGroupRunner(t *testing.T) {
	delegate := &blockingStartGroupFactory{entered: make(chan struct{}), release: make(chan struct{})}
	factory, err := NewDeferredGroupFactory(func(context.Context) (GroupFactory, error) {
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	startDone := make(chan error, 1)
	go func() {
		_, err := factory.NewGroupRunner(context.Background(), groupConfigFixture())
		startDone <- err
	}()
	<-delegate.entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- factory.Close() }()
	// Close must block while a NewGroupRunner holds activeStarts.
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned while a NewGroupRunner was in flight: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(delegate.release)
	// The in-flight NewGroupRunner observes the closed factory and drops the
	// runner rather than launching it; Close then completes and reaches the
	// delegate exactly once.
	if err := <-startDone; !errors.Is(err, errDeferredFactoryClosed) {
		t.Fatalf("concurrent NewGroupRunner error = %v, want closed", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("delegate closes = %d, want 1", delegate.closes.Load())
	}
}
