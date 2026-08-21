package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// errBudgetExitNotReached is returned on the unreachable path after t.Fatalf,
// which stops the test goroutine before any caller can observe it. It exists
// so the helper never returns a nil error beside a nil value.
var errBudgetExitNotReached = errors.New("supervisor did not reach the budget exit")

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
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	done := make(chan error, 1)
	go func() { done <- sup.Run(runCtx) }()
	waitForRunners(t, log, 1)
	close(log.snapshot()[0].done)
	waitForRunners(t, log, 2)
	close(log.snapshot()[1].done)
	// Bounded on purpose. If the shape under test stops counting against the
	// budget, the supervisor never exits and a bare <-done would hang until
	// the package-wide go-test timeout, reporting a goroutine dump instead of
	// naming the assertion that broke.
	select {
	case err := <-done:
		return marker, err
	case <-time.After(10 * time.Second):
		cancelRun()
		closeAllRunners(log)
		<-done
		t.Fatalf("supervisor never reached the budget exit: this cycle shape is not counting against the unhealthy-knock budget")
		return nil, errBudgetExitNotReached
	}
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
	// The exit's detail is what triage reads to learn the LAST cause without
	// unwrapping. A summary naming the wrong shape would be worse than none,
	// and errors.Is above cannot see it.
	if got := runErr.Error(); !strings.Contains(got, "a stalled reconnect") {
		t.Errorf("budget exit detail = %q, want it to name the stalled reconnect as the last cause", got)
	}
	if strings.Contains(runErr.Error(), "token-rejected") {
		t.Errorf("budget exit detail names a token rejection for a stalled reconnect: %q", runErr.Error())
	}
	armed, _ := marker.snapshot()
	if len(armed) != 1 {
		t.Fatalf("armed episodes = %v, want exactly one", armed)
	}
}

// TestReconnectStallBookingLineIsGreppable pins the supervisor's own line for
// a stalled cycle. It is a different layer and a different event name from the
// watchdog's detection line (reconnect_stalled), and it is the one that
// carries the budget position — the number an operator needs to know how close
// the Connector is to giving up. Nothing else asserts it.
func TestReconnectStallBookingLineIsGreppable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := &runnerLog{}
	stalled := fmt.Errorf("%w: no tunnel session for 4m0s across 9 dial attempts", errReconnectStalled)
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	cfg := testConfig(knocker, makeFactory(log, []error{stalled}))
	cfg.MaxConsecutiveKnockFailures = 2
	cfg.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
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
	<-done

	out := buf.String()
	for _, want := range []string{
		`"event":"reconnect_stall_counted"`, // distinct from the watchdog's reconnect_stalled
		`"consecutive_unhealthy_knocks":`,   // where the Connector is against the budget
		`"max_failures":`,
		"kept dropping",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("supervisor stall booking line is missing %q\nlog:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"event":"login_token_rejected"`) {
		t.Errorf("a stalled reconnect was booked as a token rejection\nlog:\n%s", out)
	}
}

// TestOrdinaryLoginFailureStillResetsTheBudget is the counterweight: only the
// two named shapes count. A plain connectivity failure must keep resetting
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
