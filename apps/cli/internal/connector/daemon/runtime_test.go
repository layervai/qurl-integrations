package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

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
	if runtime.GOOS == "windows" {
		t.Skip("local sharing IPC is unsupported on Windows")
	}
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
