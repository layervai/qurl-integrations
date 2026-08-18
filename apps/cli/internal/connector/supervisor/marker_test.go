package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"

	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// openEpisodeStore opens a real state.Store in a private directory, skipping
// on platforms where qurl-go's pinned agent state is unsupported (mirrors the
// agent package's harness guard).
func openEpisodeStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		if errors.Is(err, qurl.ErrAgentStateContinuity) && strings.Contains(err.Error(), "unsupported on this platform") {
			t.Skipf("qurl-go pinned agent state unsupported here: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestEpisodeLadderArmsRealMarkerOnBudgetExit is the supervisor half of the
// refresh ladder over the REAL state store: a knock-budget exit writes the
// unattempted marker with the shared episode reason, exactly the shape the
// agent package's Open consumes on the next start.
func TestEpisodeLadderArmsRealMarkerOnBudgetExit(t *testing.T) {
	store := openEpisodeStore(t)
	knocker := &fakeKnocker{script: []knockResp{{err: errors.New("assigned endpoint unreachable")}}}
	cfg := testConfig(knocker, makeFactory(&runnerLog{}, nil))
	cfg.Marker = store
	cfg.MaxConsecutiveKnockFailures = 2
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if runErr := sup.Run(context.Background()); !errors.Is(runErr, errTooManyKnockFailures) {
		t.Fatalf("Run = %v, want the budget exit", runErr)
	}

	marker, present, err := store.LoadRefreshMarker()
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("no refresh marker after the budget exit; the agent refresh ladder would never arm")
	}
	if marker.Attempted {
		t.Fatal("marker born Attempted; the one-refresh episode budget would already be spent")
	}
	if marker.Reason != refreshMarkerReason {
		t.Fatalf("marker reason = %q, want the shared %q", marker.Reason, refreshMarkerReason)
	}
}

// TestEpisodeLadderBudgetExitDoesNotRearmAttemptedEpisode pins the anti-storm
// bound end to end: a second budget exit inside an episode whose refresh was
// already attempted must leave the marker untouched (arming is
// presence-gated), so restarts cannot re-arm one refresh per crash.
func TestEpisodeLadderBudgetExitDoesNotRearmAttemptedEpisode(t *testing.T) {
	store := openEpisodeStore(t)
	if err := store.RequestRefresh("prior episode"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRefreshAttempted(); err != nil {
		t.Fatal(err)
	}

	knocker := &fakeKnocker{script: []knockResp{{err: errors.New("still unreachable")}}}
	cfg := testConfig(knocker, makeFactory(&runnerLog{}, nil))
	cfg.Marker = store
	cfg.MaxConsecutiveKnockFailures = 1
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if runErr := sup.Run(context.Background()); !errors.Is(runErr, errTooManyKnockFailures) {
		t.Fatalf("Run = %v, want the budget exit", runErr)
	}

	marker, present, err := store.LoadRefreshMarker()
	if err != nil {
		t.Fatal(err)
	}
	if !present || !marker.Attempted || marker.Reason != "prior episode" {
		t.Fatalf("marker = %+v present=%v, want the attempted prior episode preserved", marker, present)
	}
}

// TestEpisodeLadderHealthyCycleClearsRealMarker: a confirmed-healthy cycle
// ends the episode — the marker file is removed, so the next budget exit
// starts a fresh unattempted episode.
func TestEpisodeLadderHealthyCycleClearsRealMarker(t *testing.T) {
	store := openEpisodeStore(t)
	if err := store.RequestRefresh("prior episode"); err != nil {
		t.Fatal(err)
	}

	log := &runnerLog{}
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	cfg := testConfig(knocker, makeFactory(log, nil))
	cfg.Marker = store
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitForRunners(t, log, 1)
	close(log.snapshot()[0].done) // cycle completes healthy (nil run error)
	waitForRunners(t, log, 2)     // reconciliation for cycle 1 finished
	cancel()
	closeAllRunners(log)
	<-done

	if _, present, err := store.LoadRefreshMarker(); err != nil {
		t.Fatal(err)
	} else if present {
		t.Fatal("marker still present after a confirmed-healthy cycle; the episode never ended")
	}
}

// TestMarkerFaultsAreLoggedNotFatal: a marker store that fails must not
// change the supervisor's exit semantics in either direction.
func TestMarkerFaultsAreLoggedNotFatal(t *testing.T) {
	t.Parallel()
	marker := &fakeMarker{requestErr: errors.New("disk full"), clearErr: errors.New("disk full")}
	log := &runnerLog{}
	tokenReject := errors.New("login to the server failed: knock_invalid: knock token rejected")
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	cfg := testConfig(knocker, makeFactory(log, []error{nil, tokenReject}))
	cfg.Marker = marker
	cfg.MaxConsecutiveKnockFailures = 1
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sup.Run(context.Background()) }()
	waitForRunners(t, log, 1)
	close(log.snapshot()[0].done) // healthy cycle → clear fails, logged only
	waitForRunners(t, log, 2)
	close(log.snapshot()[1].done) // token-rejected cycle → budget exit → arm fails, logged only
	if runErr := <-done; !errors.Is(runErr, errTooManyKnockFailures) {
		t.Fatalf("Run = %v, want the budget exit despite marker faults", runErr)
	}
}
