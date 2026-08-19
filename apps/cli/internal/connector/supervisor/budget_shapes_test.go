package supervisor

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// runToBudgetExit drives a supervisor whose every cycle fails with runErr and
// returns the terminal error, with the budget set to two so the exit lands on
// the second cycle.
func runToBudgetExit(t *testing.T, runErr error) (*fakeMarker, error) {
	t.Helper()
	log := &runnerLog{}
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	cfg := testConfig(knocker, makeFactory(log, []error{runErr}))
	cfg.MaxConsecutiveKnockFailures = 2
	marker := &fakeMarker{}
	cfg.Marker = marker
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sup.Run(context.Background()) }()
	waitForRunners(t, log, 1)
	close(log.snapshot()[0].done)
	waitForRunners(t, log, 2)
	close(log.snapshot()[1].done)
	return marker, <-done
}

// TestReconnectStallCountsAgainstBudget: a cycle the reconnect watchdog took
// back must count too. Without it, a Connector whose tunnel is admitted and
// then never re-establishes would restart its cycle forever — the watchdog
// would bound each cycle, but the supervisor would reset the budget after
// every one and never stop.
func TestReconnectStallCountsAgainstBudget(t *testing.T) {
	t.Parallel()
	stalled := fmt.Errorf("%w: no tunnel session for 1m30s across 7 dial attempts", errReconnectStalled)
	marker, runErr := runToBudgetExit(t, stalled)
	if !errors.Is(runErr, ErrTooManyKnockFailures) || !errors.Is(runErr, errReconnectStalled) {
		t.Fatalf("Run = %v, want the budget exit wrapping the stalled reconnect", runErr)
	}
	armed, _ := marker.snapshot()
	if len(armed) != 1 {
		t.Fatalf("armed episodes = %v, want exactly one", armed)
	}
}

// TestOrdinaryLoginFailureStillResetsTheBudget is the counterweight: only the
// three named shapes count. A plain connectivity failure must keep resetting
// the budget, because the supervisor's job there is to keep retrying — turning
// every transport blip into budget pressure would make a flaky network exit
// the Connector.
func TestOrdinaryLoginFailureStillResetsTheBudget(t *testing.T) {
	t.Parallel()
	log := &runnerLog{}
	ordinary := errors.New("login to the server failed: dial tcp 10.0.0.1:7002: i/o timeout")
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	cfg := testConfig(knocker, makeFactory(log, []error{ordinary}))
	cfg.MaxConsecutiveKnockFailures = 2
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	// Four cycles is twice the budget: if this shape counted, the supervisor
	// would have exited before the fourth.
	for i := range 4 {
		waitForRunners(t, log, i+1)
		close(log.snapshot()[i].done)
	}
	waitForRunners(t, log, 5)
	cancel()
	closeAllRunners(log)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want clean cancellation; an ordinary connectivity failure must not spend the budget", err)
	}
}
