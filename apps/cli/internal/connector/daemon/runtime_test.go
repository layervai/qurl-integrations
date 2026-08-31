package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"

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

func TestNativeSessionFactoryLogsClassifiedRetryWithoutStoppingSession(t *testing.T) {
	const secret = "lv_live_SUPERSECRETVALUE0000001"
	attemptErr := errors.Join(errors.New("classified native attempt failure from Bearer "+secret),
		&qurl.ServerDenyError{ErrCode: "52005"})
	admitter := &failingResourceAdmitter{err: attemptErr}
	common, err := DefaultFRPCommon(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewNativeSessionFactory(admitter, common, "test")
	if err != nil {
		t.Fatal(err)
	}

	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	share := daemonShare("retry", 1, "on")
	session, err := factory.Start(context.Background(), &share)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(logs.String(), "classified native attempt failure") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := logs.String()
	for _, want := range []string{"share daemon session attempt failed; retrying", share.CRID, "classified native attempt failure", "Bearer ***", "retry_in="} {
		if !strings.Contains(got, want) {
			t.Fatalf("retry log %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, secret) {
		t.Fatalf("retry log contains a credential: %q", got)
	}
	diagnostic, ok := session.(diagnosticSession)
	if !ok {
		t.Fatal("native session does not expose redacted diagnostics")
	}
	state := diagnostic.Diagnostic()
	if state.State != "retrying" || state.FailureCategory != "platform_denied" ||
		state.FailureCode != "52005" || state.RetryAttempt != 1 || state.NextRetryAt == nil {
		t.Fatalf("retry diagnostic = %#v", state)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := session.Stop(stopCtx); err != nil {
		t.Fatalf("stop retrying session: %v", err)
	}
}

func TestNativeSessionCancellationFilterKeepsIndependentShutdownFailure(t *testing.T) {
	failure := errors.New("retire native admission after shutdown")
	wrappedCancellation := fmt.Errorf("resource runner stopped: %w", context.Canceled)
	got := withoutExpectedNativeSessionCancellation(errors.Join(wrappedCancellation, failure))
	if !errors.Is(got, failure) || errors.Is(got, context.Canceled) {
		t.Fatalf("filtered native shutdown error = %v, want only %v", got, failure)
	}
	if got := withoutExpectedNativeSessionCancellation(errors.Join(wrappedCancellation)); got != nil {
		t.Fatalf("filtered expected native cancellation = %v, want nil", got)
	}
}

type closeTrackingFactory struct {
	delegate *fakeFactory
	closes   atomic.Int32
}

type blockingStartFactory struct {
	entered chan struct{}
	release chan struct{}
	session Session
	closes  atomic.Int32
}

type blockingStopSession struct {
	entered chan struct{}
	release chan struct{}
	done    chan struct{}
	once    sync.Once
}

type blockingCloseFactory struct {
	entered chan struct{}
	release chan struct{}
	err     error
	closes  atomic.Int32
}

func (*blockingCloseFactory) Start(context.Context, *connectorstate.LocalShare) (Session, error) {
	return newFakeSession(), nil
}

func (f *blockingCloseFactory) Close() error {
	if f.closes.Add(1) == 1 {
		close(f.entered)
	}
	<-f.release
	return f.err
}

func (s *blockingStopSession) Done() <-chan struct{} { return s.done }
func (*blockingStopSession) Err() error              { return nil }
func (s *blockingStopSession) Stop(ctx context.Context) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		close(s.done)
		return nil
	}
}

func (f *blockingStartFactory) Start(context.Context, *connectorstate.LocalShare) (Session, error) {
	close(f.entered)
	<-f.release
	return f.session, nil
}

func (f *blockingStartFactory) Close() error {
	f.closes.Add(1)
	return nil
}

func (f *closeTrackingFactory) Start(ctx context.Context, share *connectorstate.LocalShare) (Session, error) {
	return f.delegate.Start(ctx, share)
}

func (f *closeTrackingFactory) Close() error {
	f.closes.Add(1)
	return nil
}

func newCloseTrackingFactory() *closeTrackingFactory {
	return &closeTrackingFactory{delegate: &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}}}
}

func TestDaemonIPCIsReadyWhileNativeAssignmentRecoveryContinues(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	started := make(chan struct{})
	recovered := make(chan struct{})
	delegate := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}}
	factory, err := NewDeferredSessionFactory(func(ctx context.Context) (SessionFactory, error) {
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
	deadline := time.Now().Add(time.Second)
	for manager.Running()["a"] == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if manager.Running()["a"] == "" {
		t.Fatal("share did not start after unattended assignment recovery")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("daemon shutdown = %v, want cancellation", err)
	}
}

func TestDeferredSessionFactoryRetriesFailedInitialization(t *testing.T) {
	want := errors.New("native transport unavailable")
	delegate := newCloseTrackingFactory()
	var attempts atomic.Int32
	factory, err := NewDeferredSessionFactory(func(context.Context) (SessionFactory, error) {
		if attempts.Add(1) == 1 {
			return nil, want
		}
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	share := daemonShare("a", 1, "on")
	if _, err := factory.Start(context.Background(), &share); !errors.Is(err, want) {
		t.Fatalf("first Start error = %v, want transient failure", err)
	}
	if _, err := factory.Start(context.Background(), &share); err != nil {
		t.Fatalf("second Start did not retry initialization: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("initializer attempts=%d, want 2", attempts.Load())
	}
	if err := factory.Close(); err != nil {
		t.Fatal(err)
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("successful delegate closes=%d, want 1", delegate.closes.Load())
	}
}

func TestDeferredSessionFactoryKeepsSuccessfulRuntimeContextUntilClose(t *testing.T) {
	delegate := newCloseTrackingFactory()
	var runtimeCtx context.Context
	factory, err := NewDeferredSessionFactory(func(ctx context.Context) (SessionFactory, error) {
		runtimeCtx = ctx
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	share := daemonShare("context-lifetime", 1, "on")
	startCtx, cancelStart := context.WithCancel(context.Background())
	session, err := factory.Start(startCtx, &share)
	if err != nil {
		t.Fatal(err)
	}
	cancelStart()
	if runtimeCtx == nil || runtimeCtx.Err() != nil {
		t.Fatalf("runtime context after Start caller cancellation = %v, want live context", runtimeCtx)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	if err := session.Stop(stopCtx); err != nil {
		stopCancel()
		t.Fatal(err)
	}
	stopCancel()
	if err := factory.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(runtimeCtx.Err(), context.Canceled) {
		t.Fatalf("runtime context after Close = %v, want canceled", runtimeCtx.Err())
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("successful delegate closes=%d, want 1", delegate.closes.Load())
	}
}

func TestDeferredSessionFactoryRejectsDelegateCompletedAfterCallerCancellation(t *testing.T) {
	delegate := newCloseTrackingFactory()
	started := make(chan struct{})
	release := make(chan struct{})
	factory, err := NewDeferredSessionFactory(func(context.Context) (SessionFactory, error) {
		close(started)
		<-release
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	startCtx, cancelStart := context.WithCancel(context.Background())
	done := make(chan error, 1)
	share := daemonShare("canceled-initialization", 1, "on")
	go func() {
		_, startErr := factory.Start(startCtx, &share)
		done <- startErr
	}()
	<-started
	cancelStart()
	close(release)
	if startErr := <-done; !errors.Is(startErr, context.Canceled) {
		t.Fatalf("Start error = %v, want caller cancellation", startErr)
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("delegate completed after cancellation closes=%d, want 1", delegate.closes.Load())
	}
}

func TestDeferredSessionFactorySerializesConcurrentInitialization(t *testing.T) {
	delegate := newCloseTrackingFactory()
	entered := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	factory, err := NewDeferredSessionFactory(func(context.Context) (SessionFactory, error) {
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
		go func(index int) {
			defer wg.Done()
			share := daemonShare(fmt.Sprintf("share-%d", index), 1, "on")
			_, err := factory.Start(context.Background(), &share)
			errs <- err
		}(i)
	}
	<-entered
	if attempts.Load() != 1 {
		t.Fatalf("concurrent initializer attempts before release=%d, want 1", attempts.Load())
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Start: %v", err)
		}
	}
	if attempts.Load() != 1 {
		t.Fatalf("concurrent initializer attempts=%d, want 1", attempts.Load())
	}
}

func TestDeferredSessionFactoryCloseDuringInitializationClosesDelegate(t *testing.T) {
	delegate := newCloseTrackingFactory()
	entered := make(chan struct{})
	factory, err := NewDeferredSessionFactory(func(ctx context.Context) (SessionFactory, error) {
		close(entered)
		<-ctx.Done()
		// Model an initializer that completed native allocation concurrently
		// with cancellation and must hand that allocation back for cleanup.
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	startDone := make(chan error, 1)
	share := daemonShare("a", 1, "on")
	go func() {
		_, err := factory.Start(context.Background(), &share)
		startDone <- err
	}()
	<-entered
	if err := factory.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-startDone; !errors.Is(err, errDeferredFactoryClosed) {
		t.Fatalf("Start error = %v, want closed", err)
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("delegate closes=%d, want 1 before Close returns", delegate.closes.Load())
	}
	if _, err := factory.Start(context.Background(), &share); !errors.Is(err, errDeferredFactoryClosed) {
		t.Fatalf("Start after Close error = %v", err)
	}
}

func TestDeferredSessionFactoryClosesDelegateReturnedWithFailure(t *testing.T) {
	delegate := newCloseTrackingFactory()
	want := errors.New("native open failed after allocation")
	factory, err := NewDeferredSessionFactory(func(context.Context) (SessionFactory, error) {
		return delegate, want
	})
	if err != nil {
		t.Fatal(err)
	}
	share := daemonShare("a", 1, "on")
	if _, err := factory.Start(context.Background(), &share); !errors.Is(err, want) {
		t.Fatalf("Start error = %v, want failure", err)
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("failed delegate closes=%d, want 1", delegate.closes.Load())
	}
}

func TestDeferredSessionFactoryCloseWaitsForConcurrentDelegateStart(t *testing.T) {
	session := &blockingStopSession{entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	delegate := &blockingStartFactory{
		entered: make(chan struct{}), release: make(chan struct{}), session: session,
	}
	factory, err := NewDeferredSessionFactory(func(context.Context) (SessionFactory, error) {
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	share := daemonShare("a", 1, "on")
	startDone := make(chan error, 1)
	go func() {
		_, err := factory.Start(context.Background(), &share)
		startDone <- err
	}()
	<-delegate.entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- factory.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned while delegate.Start was active: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(delegate.release)
	select {
	case <-session.entered:
	case <-time.After(time.Second):
		t.Fatal("Start did not stop the session created after Close began")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before the concurrent Start stopped its session: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(session.release)
	if err := <-startDone; !errors.Is(err, errDeferredFactoryClosed) {
		t.Fatalf("concurrent Start error = %v, want closed", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.done:
	default:
		t.Fatal("session returned during Close was not stopped")
	}
	if delegate.closes.Load() != 1 {
		t.Fatalf("delegate closes=%d, want 1", delegate.closes.Load())
	}
}

func TestDeferredSessionFactoryConcurrentCloseWaitsForOneCleanup(t *testing.T) {
	want := errors.New("delegate close failed")
	delegate := &blockingCloseFactory{entered: make(chan struct{}), release: make(chan struct{}), err: want}
	factory, err := NewDeferredSessionFactory(func(context.Context) (SessionFactory, error) {
		return delegate, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	share := daemonShare("a", 1, "on")
	if _, err := factory.Start(context.Background(), &share); err != nil {
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
		t.Fatalf("delegate closes=%d, want 1", delegate.closes.Load())
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

func TestClassifyShareFailurePreservesIdentityTaxonomyCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{
			name: "credential recovery",
			err: errors.Join(
				&qurl.CredentialRecoveryError{Code: "52401", Phase: "hub_issue_recovery"},
				qurl.ErrRecoveryCredentialRejected,
			),
			code: "52401",
		},
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
