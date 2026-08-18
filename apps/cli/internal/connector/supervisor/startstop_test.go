package supervisor

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"
)

// waitForGoroutineFloor polls until the process goroutine count returns to at
// most floor, failing the test on timeout. The retry loop absorbs scheduler
// lag between "channel observed closed" and "goroutine fully unwound".
func waitForGoroutineFloor(t *testing.T, floor int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= floor {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	buf := make([]byte, 1<<16)
	n := runtime.Stack(buf, true)
	t.Fatalf("goroutines = %d, want <= %d after teardown; stacks:\n%s", runtime.NumGoroutine(), floor, buf[:n])
}

// TestStartStopCleanTeardown is the teardown invariant for the Start/Stop
// surface: Stop cancels the serve loop, waits for it, maps the cancellation
// exit to nil, and leaves no goroutines behind. Deliberately not parallel so
// the goroutine accounting is stable.
func TestStartStopCleanTeardown(t *testing.T) {
	floor := runtime.NumGoroutine()
	knocker := &fakeCycleKnocker{}
	log := &runnerLog{}
	sup, err := New(testConfig(knocker, makeFactory(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRunners(t, log, 1)

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sup.Stop(stopCtx); err != nil {
		t.Fatalf("Stop = %v, want nil for a clean cancellation teardown", err)
	}
	select {
	case <-sup.Done():
	default:
		t.Fatal("Done not closed after Stop returned")
	}
	if err := sup.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err = %v, want the recorded context cancellation", err)
	}
	// Every begun native cycle must have been closed on the way out.
	knocker.mu.Lock()
	begun, ended := len(knocker.begun), len(knocker.ended)
	knocker.mu.Unlock()
	if begun == 0 || begun != ended {
		t.Fatalf("native cycles begun %d ended %d, want every begun cycle ended", begun, ended)
	}
	waitForGoroutineFloor(t, floor)
}

// TestStartParentContextCancelStopsLoop: the loop is context-driven — a
// parent cancellation tears it down without Stop, and a later Stop still
// reports the clean outcome.
func TestStartParentContextCancelStopsLoop(t *testing.T) {
	floor := runtime.NumGoroutine()
	knocker := &fakeCycleKnocker{}
	log := &runnerLog{}
	sup, err := New(testConfig(knocker, makeFactory(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRunners(t, log, 1)
	cancel()
	select {
	case <-sup.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("serve loop did not exit on parent context cancellation")
	}
	if err := sup.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after parent cancel = %v, want nil", err)
	}
	waitForGoroutineFloor(t, floor)
}

// TestStopSurfacesAutonomousFailure: when the loop already exited on its
// failure budget, Stop reports that exit instead of pretending the shutdown
// was clean.
func TestStopSurfacesAutonomousFailure(t *testing.T) {
	knocker := &fakeKnocker{script: []knockResp{{err: errors.New("assigned endpoint unreachable")}}}
	cfg := testConfig(knocker, makeFactory(&runnerLog{}, nil))
	cfg.MaxConsecutiveKnockFailures = 1
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-sup.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("serve loop did not exit on its knock budget")
	}
	if err := sup.Stop(context.Background()); !errors.Is(err, errTooManyKnockFailures) {
		t.Fatalf("Stop = %v, want the loop's errTooManyKnockFailures exit", err)
	}
	if err := sup.Err(); !errors.Is(err, errTooManyKnockFailures) {
		t.Fatalf("Err = %v, want the recorded budget exit", err)
	}
}

// TestStopBeforeStartFailsClosed pins the lifecycle contract.
func TestStopBeforeStartFailsClosed(t *testing.T) {
	t.Parallel()
	knocker := &fakeCycleKnocker{}
	sup, err := New(testConfig(knocker, makeFactory(&runnerLog{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Stop(context.Background()); !errors.Is(err, errNotStarted) {
		t.Fatalf("Stop before Start = %v, want errNotStarted", err)
	}
	if sup.Done() != nil {
		t.Fatal("Done non-nil before Start")
	}
}

// TestStopHonorsWaitDeadline: a serve loop wedged in a runner that ignores
// cancellation must not hang Stop past its context.
func TestStopHonorsWaitDeadline(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	started := make(chan struct{}, 1)
	cfg := testConfig(knocker, func(*v1.ClientCommonConfig) (FRPRunner, error) {
		started <- struct{}{}
		return &wedgedRunner{release: release}, nil
	})
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-started
	stopCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := sup.Stop(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop against a wedged runner = %v, want the wait deadline", err)
	}
	close(release) // unwedge so the loop exits and the goroutine drains
	select {
	case <-sup.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("serve loop did not exit after the wedged runner released")
	}
}

// wedgedRunner ignores context cancellation until released — the misbehaving
// runner shape TestStopHonorsWaitDeadline needs.
type wedgedRunner struct{ release chan struct{} }

func (r *wedgedRunner) Run(context.Context) error {
	<-r.release
	return nil
}

func (r *wedgedRunner) GracefulClose(time.Duration) {}
