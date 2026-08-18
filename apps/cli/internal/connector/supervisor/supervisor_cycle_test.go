package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/knock"
)

// fakeCycleKnocker drives the CycleKnocker contract assertions: every begun
// cycle must be ended exactly once, knocks and runner builds must observe the
// cycle's RunID, and EndCycle wipes it (mirroring the native knocker, so a
// RunID captured too late reads empty).
type fakeCycleKnocker struct {
	mu        sync.Mutex
	current   string
	begun     []string
	knocked   []string
	ended     []string
	malformed bool
	beginErr  error
	knockErr  error
	endErr    error
	endCtxErr error
	endBudget time.Duration
}

func (k *fakeCycleKnocker) BeginCycle() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.beginErr != nil {
		return k.beginErr
	}
	k.current = fmt.Sprintf("cycle-%d", len(k.begun)+1)
	k.begun = append(k.begun, k.current)
	return nil
}

func (k *fakeCycleKnocker) CycleRunID() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.current
}

func (k *fakeCycleKnocker) Knock(context.Context) (*knock.Result, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.knocked = append(k.knocked, k.current)
	if k.knockErr != nil {
		return nil, k.knockErr
	}
	result := &knock.Result{
		ACTokens:     map[string]string{testResource: "token"},
		ResourceHost: map[string]string{testResource: "tunnel.example:7000"},
	}
	if k.malformed {
		result.ResourceHost = nil
	}
	return result, nil
}

func (k *fakeCycleKnocker) EndCycle(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ended = append(k.ended, k.current)
	k.current = ""
	k.endCtxErr = ctx.Err()
	if deadline, ok := ctx.Deadline(); ok {
		k.endBudget = time.Until(deadline)
	}
	return k.endErr
}

var _ knock.CycleKnocker = (*fakeCycleKnocker)(nil)

// wantFirstCycleEnded is the ended-IDs rendering every single-cycle teardown
// assertion expects.
const wantFirstCycleEnded = "[cycle-1]"

func (k *fakeCycleKnocker) endedIDs() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return fmt.Sprint(k.ended)
}

// TestCycleKnockerExitsSessionWhenKnockFails: a knock-failure cycle still
// sends its best-effort session exit — the server may hold state from a
// request whose reply was lost.
func TestCycleKnockerExitsSessionWhenKnockFails(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("lost UDP reply")
	knocker := &fakeCycleKnocker{knockErr: wantErr}
	cfg := testConfig(knocker, func(*v1.ClientCommonConfig) (FRPRunner, error) {
		t.Error("RunnerFactory called after failed knock")
		return nil, errors.New("unreachable")
	})
	cfg.MaxConsecutiveKnockFailures = 1
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Run(context.Background()); !errors.Is(err, wantErr) || !errors.Is(err, ErrTooManyKnockFailures) {
		t.Fatalf("Run = %v, want wrapped knock error and ErrTooManyKnockFailures", err)
	}
	if got := knocker.endedIDs(); got != wantFirstCycleEnded {
		t.Fatalf("ended cycle IDs = %s, want %s", got, wantFirstCycleEnded)
	}
}

// TestCycleKnockerExitsSessionWhenRunnerFactoryFails: the fatal factory path
// must still close the begun native cycle.
func TestCycleKnockerExitsSessionWhenRunnerFactoryFails(t *testing.T) {
	t.Parallel()
	knocker := &fakeCycleKnocker{}
	wantErr := errors.New("invalid tunnel configuration")
	sup, err := New(testConfig(knocker, func(*v1.ClientCommonConfig) (FRPRunner, error) { return nil, wantErr }))
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Run(context.Background()); err == nil || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("Run = %v, want wrapped %q", err, wantErr)
	}
	if got := knocker.endedIDs(); got != wantFirstCycleEnded {
		t.Fatalf("ended cycle IDs = %s, want %s", got, wantFirstCycleEnded)
	}
}

// TestCycleKnockerExitsSessionWhenKnockACKIsUnusable: an unusable ACK ends
// the cycle before any login attempt.
func TestCycleKnockerExitsSessionWhenKnockACKIsUnusable(t *testing.T) {
	t.Parallel()
	knocker := &fakeCycleKnocker{malformed: true}
	cfg := testConfig(knocker, func(*v1.ClientCommonConfig) (FRPRunner, error) {
		t.Error("RunnerFactory called after unusable knock ACK")
		return nil, errors.New("unreachable")
	})
	cfg.MaxConsecutiveKnockFailures = 1
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Run(context.Background()); !errors.Is(err, ErrTooManyKnockFailures) {
		t.Fatalf("Run = %v, want ErrTooManyKnockFailures", err)
	}
	if got := knocker.endedIDs(); got != wantFirstCycleEnded {
		t.Fatalf("ended cycle IDs = %s, want %s", got, wantFirstCycleEnded)
	}
}

// TestEndNativeCycleUsesIndependentTimeout: the session exit must run under
// its own live budget even when the cycle context is already canceled — the
// common shutdown shape.
func TestEndNativeCycleUsesIndependentTimeout(t *testing.T) {
	t.Parallel()
	knocker := &fakeCycleKnocker{current: "cycle-1"}
	sup := &Supervisor{cfg: Config{Knocker: knocker, KnockResourceID: testResource}}
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	sup.endNativeCycle(parent, true)

	knocker.mu.Lock()
	defer knocker.mu.Unlock()
	if knocker.endCtxErr != nil {
		t.Fatalf("EndCycle context error = %v, want an independent live context", knocker.endCtxErr)
	}
	if knocker.endBudget <= 0 || knocker.endBudget > endCycleTimeout {
		t.Fatalf("EndCycle timeout budget = %v, want a live budget within %v", knocker.endBudget, endCycleTimeout)
	}
}

// TestCycleKnockerBeginErrorStopsBeforeKnock: a failed BeginCycle is fatal
// and nothing else runs — no knock, no runner, no session exit for a cycle
// that never began.
func TestCycleKnockerBeginErrorStopsBeforeKnock(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("cycle run ID unavailable")
	knocker := &fakeCycleKnocker{beginErr: wantErr}
	sup, err := New(testConfig(knocker, func(*v1.ClientCommonConfig) (FRPRunner, error) {
		t.Error("RunnerFactory called after BeginCycle failure")
		return nil, errors.New("unreachable")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Run(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Run = %v, want wrapped BeginCycle error", err)
	}
	knocker.mu.Lock()
	defer knocker.mu.Unlock()
	if len(knocker.begun) != 0 || len(knocker.knocked) != 0 || len(knocker.ended) != 0 {
		t.Fatalf("failed BeginCycle state = begun %v, knocked %v, ended %v; want all empty", knocker.begun, knocker.knocked, knocker.ended)
	}
}

// TestCycleKnockerEndErrorDoesNotMaskPrimaryFailure: EndCycle is best-effort;
// its error is logged, never joined into the cycle's primary failure.
func TestCycleKnockerEndErrorDoesNotMaskPrimaryFailure(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("lost UDP reply")
	endErr := errors.New("session exit reply unavailable")
	knocker := &fakeCycleKnocker{knockErr: wantErr, endErr: endErr}
	cfg := testConfig(knocker, func(*v1.ClientCommonConfig) (FRPRunner, error) {
		t.Error("RunnerFactory called after failed knock")
		return nil, errors.New("unreachable")
	})
	cfg.MaxConsecutiveKnockFailures = 1
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gotErr := sup.Run(context.Background())
	if !errors.Is(gotErr, wantErr) || errors.Is(gotErr, endErr) {
		t.Fatalf("Run = %v, want the primary knock error without the best-effort EndCycle error", gotErr)
	}
	if got := knocker.endedIDs(); got != wantFirstCycleEnded {
		t.Fatalf("ended cycle IDs = %s, want %s", got, wantFirstCycleEnded)
	}
}

// TestCycleKnockerCorrelatesKnockRunnerAndExit: across multiple cycles, the
// begun / knocked / runner-observed / ended RunID sequences must be
// identical — the one-RunID-per-cycle correlation the whole admission chain
// rests on.
func TestCycleKnockerCorrelatesKnockRunnerAndExit(t *testing.T) {
	t.Parallel()
	knocker := &fakeCycleKnocker{}
	runners := &runnerLog{}
	var factoryIDs []string
	var factoryMu sync.Mutex
	factory := func(common *v1.ClientCommonConfig) (FRPRunner, error) {
		factoryMu.Lock()
		factoryIDs = append(factoryIDs, knocker.CycleRunID())
		factoryMu.Unlock()
		runner := &fakeRunner{dialTarget: common.ServerAddr, done: make(chan struct{})}
		runners.add(runner)
		return runner, nil
	}
	sup, err := New(testConfig(knocker, factory))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitForRunners(t, runners, 1)
	close(runners.snapshot()[0].done)
	waitForRunners(t, runners, 2)
	cancel()
	close(runners.snapshot()[1].done)
	<-done

	knocker.mu.Lock()
	defer knocker.mu.Unlock()
	factoryMu.Lock()
	defer factoryMu.Unlock()
	want := []string{"cycle-1", "cycle-2"}
	for label, got := range map[string][]string{
		"begun": knocker.begun, "knocked": knocker.knocked,
		"runner": factoryIDs, "ended": knocker.ended,
	} {
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s cycle IDs = %v, want %v", label, got, want)
		}
	}
}
