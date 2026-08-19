package supervisor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/frpgen"
)

// newWatchdogRefresher builds a refresher whose knock always succeeds, so the
// only thing under test is the reconnect watchdog. The knock gate is set wide
// on purpose: during a real storm the FRP client redials faster than the gate,
// so most calls are debounced, and the watchdog must still see every one.
func newWatchdogRefresher(now func() time.Time, logger *slog.Logger, stall, settled time.Duration) (*redialKnockRefresher, *restartRecorder) {
	rec := &restartRecorder{}
	return &redialKnockRefresher{
		knocker:        &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}},
		resourceID:     testResource,
		gate:           time.Hour,
		logger:         logger,
		now:            now,
		stallWindow:    stall,
		settledGap:     settled,
		requestRestart: rec.record,
	}, rec
}

type restartRecorder struct{ errs []error }

func (r *restartRecorder) record(err error) { r.errs = append(r.errs, err) }

// handedOffCommon is a common config carrying a supervisor-stamped token, the
// shape production always hands the refresher on its first call.
func handedOffCommon() *v1.ClientCommonConfig {
	common := commonForTest()
	common.Metadatas = map[string]string{frpgen.MetaQURLKnockToken: "supervisor-stamped"}
	return common
}

// TestWatchdogHandoffIsNotARedial: the supervisor's own dial opens the cycle,
// not a storm. If the handoff started the storm clock, a tunnel that came up
// and served for hours would carry a storm that began at t0, and the first
// drop after the window would be reported as a stall that never happened.
func TestWatchdogHandoffIsNotARedial(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	r, rec := newWatchdogRefresher(clk.now, discardLogger(), 90*time.Second, 45*time.Second)
	common := handedOffCommon()
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if !r.stormStartedAt.IsZero() || r.stormDials != 0 {
		t.Fatalf("the first-cycle handoff opened a redial storm (started %v, dials %d); it is the supervisor's own dial, not a redial",
			r.stormStartedAt, r.stormDials)
	}
	// Hours later the tunnel drops once: that first redial opens the storm at
	// the time it happened, so nothing is retroactively charged to it.
	clk.advance(4 * time.Hour)
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("first redial: %v", err)
	}
	if r.stormStartedAt != clk.now() {
		t.Fatalf("storm opened at %v, want the redial instant %v", r.stormStartedAt, clk.now())
	}
	if len(rec.errs) != 0 {
		t.Fatalf("restart requested after a single redial: %v", rec.errs)
	}
}

// TestWatchdogRestartsStalledCycle: a storm that outlives the window ends the
// cycle with the stall sentinel, which is what hands control back to the
// supervisor for a fresh cycle. Before this, the FRP client's own reconnect
// loop retried forever and Run never returned.
func TestWatchdogRestartsStalledCycle(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	r, rec := newWatchdogRefresher(clk.now, discardLogger(), 90*time.Second, 45*time.Second)
	common := handedOffCommon()
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	// Redial every 20s — the FRP reconnect loop's backoff ceiling, the
	// slowest a real storm dials.
	var last error
	for elapsed := time.Duration(0); elapsed <= 100*time.Second; elapsed += 20 * time.Second {
		clk.advance(20 * time.Second)
		last = r.refresh(context.Background(), common, "open")
		if last != nil {
			break
		}
	}
	if !errors.Is(last, errReconnectStalled) {
		t.Fatalf("refresh error after the stall window = %v, want errReconnectStalled", last)
	}
	if len(rec.errs) != 1 {
		t.Fatalf("requestRestart called %d time(s), want exactly 1", len(rec.errs))
	}
	if !errors.Is(rec.errs[0], errReconnectStalled) {
		t.Fatalf("restart cause = %v, want errReconnectStalled", rec.errs[0])
	}
	// The supervisor buckets the cycle by this sentinel, so it must survive
	// the wrapping the watchdog adds.
	if got := classifyRunError(rec.errs[0]); got != reasonReconnectStalled {
		t.Fatalf("classifyRunError(restart cause) = %q, want %q", got, reasonReconnectStalled)
	}
}

// TestWatchdogCountsDialsInsideTheKnockGate is the regression guard on the
// ordering inside refresh: the watchdog runs BEFORE the gate. A real storm
// dials every 1-20s against a 10s gate, so counting only ungated dials would
// undercount it and the window would never be reached.
func TestWatchdogCountsDialsInsideTheKnockGate(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	// gate an hour wide: every redial below is debounced.
	r, rec := newWatchdogRefresher(clk.now, discardLogger(), 60*time.Second, 45*time.Second)
	common := handedOffCommon()
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	var last error
	for i := 0; i < 40; i++ {
		clk.advance(5 * time.Second)
		if last = r.refresh(context.Background(), common, "open"); last != nil {
			break
		}
	}
	if !errors.Is(last, errReconnectStalled) {
		t.Fatalf("gated redials never reached the stall window (err = %v); the watchdog must run ahead of the gate", last)
	}
	if r.knocker.(*fakeKnocker).calls.Load() != 0 {
		t.Fatalf("the gate stopped debouncing: %d knock(s) escaped", r.knocker.(*fakeKnocker).calls.Load())
	}
	if len(rec.errs) != 1 {
		t.Fatalf("requestRestart called %d time(s), want 1", len(rec.errs))
	}
}

// TestWatchdogSettledGapEndsTheStorm: a quiet period longer than the settled
// gap means a redial served, so the storm clock restarts. Without the reset a
// Connector that reconnects normally once an hour would accumulate toward the
// window and eventually restart its cycle for no reason.
func TestWatchdogSettledGapEndsTheStorm(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	r, rec := newWatchdogRefresher(clk.now, discardLogger(), 90*time.Second, 45*time.Second)
	common := handedOffCommon()
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	// Eight redials, each separated by more than the settled gap: every one
	// re-opens the storm, so the window is never reached.
	for i := 0; i < 8; i++ {
		clk.advance(46 * time.Second)
		if err := r.refresh(context.Background(), common, "open"); err != nil {
			t.Fatalf("settled redial %d: %v", i, err)
		}
	}
	if len(rec.errs) != 0 {
		t.Fatalf("a settled reconnect pattern requested a restart: %v", rec.errs)
	}
	if r.stormDials != 1 {
		t.Fatalf("stormDials = %d after settled reconnects, want 1 (each reset the storm)", r.stormDials)
	}
}

// TestWatchdogGapExactlyAtSettledResets pins the boundary as "at least": a gap
// exactly equal to the settled gap counts as settled, matching the knock
// gate's own at-least semantics.
func TestWatchdogGapExactlyAtSettledResets(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	r, _ := newWatchdogRefresher(clk.now, discardLogger(), 90*time.Second, 45*time.Second)
	common := handedOffCommon()
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	clk.advance(10 * time.Second)
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("first redial: %v", err)
	}
	clk.advance(45 * time.Second)
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("boundary redial: %v", err)
	}
	if r.stormDials != 1 {
		t.Fatalf("stormDials = %d at the exact settled boundary, want 1 (the gap resets)", r.stormDials)
	}
}

// TestWatchdogNoticeSaysWhatIsHappeningOncePerStorm: the operator hears about
// the outage on the second dial of a storm and only once, and the wording
// states the observation without naming a cause. The dial failures underneath
// are transport errors with no server-supplied reason, so any cause in this
// sentence would be a guess printed as fact — the assertion below pins that.
func TestWatchdogNoticeSaysWhatIsHappeningOncePerStorm(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	var buf bytes.Buffer
	r, _ := newWatchdogRefresher(clk.now, slog.New(slog.NewJSONHandler(&buf, nil)), 90*time.Second, 45*time.Second)
	common := handedOffCommon()
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	for i := 0; i < 4; i++ {
		clk.advance(10 * time.Second)
		if err := r.refresh(context.Background(), common, "open"); err != nil {
			t.Fatalf("redial %d: %v", i, err)
		}
	}
	if got := strings.Count(buf.String(), `"event":"reconnect_retrying"`); got != 1 {
		t.Fatalf("reconnect_retrying emitted %d time(s) across one storm, want exactly 1\nlog:\n%s", got, buf.String())
	}
	notice := buf.String()
	for _, want := range []string{"lost the tunnel connection", "consumers will time out", "gives_up_after_seconds"} {
		if !strings.Contains(notice, want) {
			t.Errorf("operator notice is missing %q; it must say what happened, its consequence, and that the wait is bounded\nlog:\n%s", want, notice)
		}
	}
	// No cause may be asserted: nothing at this layer knows why the dials
	// fail, so a named cause would be a guess printed as fact.
	for _, forbidden := range []string{"previous session", "already online", "network fault", "platform released"} {
		if strings.Contains(notice, forbidden) {
			t.Errorf("operator notice asserts the unverifiable cause %q; the transport errors underneath carry no server reason\nlog:\n%s", forbidden, notice)
		}
	}
	// A single dial is an ordinary reconnect and must stay silent.
	var quiet bytes.Buffer
	clk2 := newManualClock()
	r2, _ := newWatchdogRefresher(clk2.now, slog.New(slog.NewJSONHandler(&quiet, nil)), 90*time.Second, 45*time.Second)
	c2 := handedOffCommon()
	if err := r2.refresh(context.Background(), c2, "open"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	clk2.advance(10 * time.Second)
	if err := r2.refresh(context.Background(), c2, "open"); err != nil {
		t.Fatalf("single redial: %v", err)
	}
	if strings.Contains(quiet.String(), "reconnect_retrying") {
		t.Errorf("a single ordinary reconnect warned the operator:\n%s", quiet.String())
	}
}

// TestWatchdogWindowOutlivesTheServerRelease is the sizing guard. The server
// only releases a registration whose flow died without a close when its
// multiplexer keepalive expires (30s interval + 10s write timeout); the FRP
// reconnect loop backs off up to 20s. A window at or below that sum would give
// up while the session was about to be released, turning a recoverable wait
// into a restart loop.
func TestWatchdogWindowOutlivesTheServerRelease(t *testing.T) {
	t.Parallel()
	const serverReleaseWorstCase = 40 * time.Second
	const reconnectBackoffCeiling = 20 * time.Second
	if reconnectStallWindow <= serverReleaseWorstCase+2*reconnectBackoffCeiling {
		t.Errorf("reconnectStallWindow = %s, want more than %s (server release %s plus two dial attempts at the %s backoff ceiling)",
			reconnectStallWindow, serverReleaseWorstCase+2*reconnectBackoffCeiling, serverReleaseWorstCase, reconnectBackoffCeiling)
	}
	if reconnectSettledGap <= reconnectBackoffCeiling {
		t.Errorf("reconnectSettledGap = %s, want more than the %s reconnect backoff ceiling, or a still-failing storm looks settled between two attempts",
			reconnectSettledGap, reconnectBackoffCeiling)
	}
	if reconnectSettledGap >= reconnectStallWindow {
		t.Errorf("reconnectSettledGap = %s must stay below reconnectStallWindow = %s, or no storm can ever reach the window",
			reconnectSettledGap, reconnectStallWindow)
	}
}
