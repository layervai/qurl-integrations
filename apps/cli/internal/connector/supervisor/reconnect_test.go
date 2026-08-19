package supervisor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
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
	// the wrapping the watchdog adds. Asserted through the path production
	// actually uses — reconcileKnockBudget's errors.Is — rather than through
	// classifyRunError, which never sees a restart cause (see loginerror.go).
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
	for _, want := range []string{"keeps dropping", "consumers will time out", "gives_up_after_seconds", "retrying_seconds"} {
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
	// Nor may it claim the tunnel never came back: a flap inside the settled
	// gap is counted as one storm, so "cannot re-establish" would be false
	// for that shape. See TestWatchdogFlapInsideTheGapIsNotCalledUnrecovered.
	for _, overclaim := range []string{"cannot re-establish", "could not re-establish", "until it recovers"} {
		if strings.Contains(notice, overclaim) {
			t.Errorf("operator notice claims non-recovery (%q), which is false for a tunnel flapping inside the settled gap\nlog:\n%s", overclaim, notice)
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

// TestWatchdogWindowOutlivesATunnelServerReplacement is the sizing guard,
// anchored to measured values rather than to guesses. Two independent bounds
// have to hold at once, and they push in opposite directions.
func TestWatchdogWindowOutlivesATunnelServerReplacement(t *testing.T) {
	t.Parallel()
	// Slowest observed launch-to-serving of a replacement instance during
	// the 2026-08-18 sandbox fleet roll.
	const serverReplacementObserved = 83 * time.Second
	if reconnectStallWindow <= serverReplacementObserved {
		t.Errorf("reconnectStallWindow = %s, want more than the %s a tunnel-server replacement took to start serving",
			reconnectStallWindow, serverReplacementObserved)
	}
	// The fail-open edge. If a still-failing storm's own dial spacing can
	// reach the settled gap, every dial resets the storm, nothing ever
	// accumulates, and the watchdog silently never fires — the exact
	// unbounded loop this package exists to bound. failingDialPeriod is the
	// sum of the three checked delays between two failing dials.
	// Requires a real margin, not merely "greater than". failingDialPeriod
	// counts only the three delays inside FRP; each dial also pays the
	// refresher's own re-knock round trip, which is a network call with no
	// compile-time bound. A gap set just above failingDialPeriod (45s was the
	// original value) would be inside that unbounded remainder and the
	// watchdog would fail open on a merely slow admission controller.
	const settledGapSafetyFactor = 2
	if reconnectSettledGap < settledGapSafetyFactor*failingDialPeriod {
		t.Errorf("reconnectSettledGap = %s, want at least %dx failingDialPeriod = %s (connect %s + login read %s + backoff ceiling %s, plus an unbounded re-knock round trip); too close to it and a failing storm resets on every dial, so the watchdog silently never fires",
			reconnectSettledGap, settledGapSafetyFactor, settledGapSafetyFactor*failingDialPeriod,
			failingDialConnectBudget, failingDialLoginReadBudget, failingDialBackoffCeiling)
	}
	// Bounded from above as well. Every assertion above is a floor, so an
	// accidental order-of-magnitude widening — which restores the silent
	// loop this package exists to remove — would pass all of them.
	const settledGapCeiling = 4 * time.Minute
	const stallWindowCeiling = 10 * time.Minute
	if reconnectSettledGap > settledGapCeiling {
		t.Errorf("reconnectSettledGap = %s, want at most %s; beyond that an ordinary reconnect stays inside one storm for minutes", reconnectSettledGap, settledGapCeiling)
	}
	if reconnectStallWindow > stallWindowCeiling {
		t.Errorf("reconnectStallWindow = %s, want at most %s; beyond that a dead tunnel is unbounded for practical purposes", reconnectStallWindow, stallWindowCeiling)
	}
	// The notice threshold's comment argues specifically why three and not
	// two: two would fire while only ONE redial had failed. Pin that claim,
	// which is otherwise only bounded from above by the flap test.
	if reconnectStallNoticeAfter < 3 {
		t.Errorf("reconnectStallNoticeAfter = %d, want at least 3 so the notice follows two failed redials; at 2 a routine two-attempt reconnect is announced as a tunnel that keeps dropping",
			reconnectStallNoticeAfter)
	}
	if reconnectSettledGap >= reconnectStallWindow {
		t.Errorf("reconnectSettledGap = %s must stay below reconnectStallWindow = %s, or no storm can ever reach the window",
			reconnectSettledGap, reconnectStallWindow)
	}
}

// TestWatchdogFlapInsideTheGapIsNotCalledUnrecovered covers the shape the
// watchdog cannot distinguish: a tunnel that re-establishes, serves briefly
// and drops again on an interval SHORTER than the settled gap. Every gap is
// short, so the dials accumulate as one storm and the cycle is eventually
// restarted — which is the right recovery for a tunnel that cannot hold 45s.
//
// What must NOT happen is the operator being told the tunnel never came back,
// because in this shape it did, repeatedly. This pins the wording against
// that specific falsehood; the notice may only say it is not staying up.
func TestWatchdogFlapInsideTheGapIsNotCalledUnrecovered(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	var buf bytes.Buffer
	r, rec := newWatchdogRefresher(clk.now, slog.New(slog.NewJSONHandler(&buf, nil)), 90*time.Second, 45*time.Second)
	common := handedOffCommon()
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	// Reconnects every 35s: below the 45s settled gap, so never "settled",
	// but the tunnel really is serving between dials.
	var last error
	for i := 0; i < 6; i++ {
		clk.advance(35 * time.Second)
		if last = r.refresh(context.Background(), common, "open"); last != nil {
			break
		}
	}
	if !errors.Is(last, errReconnectStalled) {
		t.Fatalf("a sub-gap flap never reached the stall window (err = %v); it must still be bounded", last)
	}
	if len(rec.errs) != 1 {
		t.Fatalf("requestRestart called %d time(s), want 1", len(rec.errs))
	}
	notice := buf.String()
	if !strings.Contains(notice, "keeps dropping") {
		t.Errorf("flap notice should say the tunnel is not staying up\nlog:\n%s", notice)
	}
	for _, overclaim := range []string{"cannot re-establish", "could not re-establish", "until it recovers"} {
		if strings.Contains(notice, overclaim) {
			t.Errorf("flap notice claims non-recovery (%q) but the tunnel re-established between every dial\nlog:\n%s", overclaim, notice)
		}
	}
}

// TestWatchdogStallFiresExactlyAtTheWindow pins the stall boundary as
// "at least", the same semantics the settled gap and the knock gate use. A
// strict > would let a storm that lands a dial exactly on the window run one
// more full dial interval before being taken back.
func TestWatchdogStallFiresExactlyAtTheWindow(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	r, rec := newWatchdogRefresher(clk.now, discardLogger(), 90*time.Second, 45*time.Second)
	common := handedOffCommon()
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	clk.advance(10 * time.Second) // opens the storm; elapsed is 0 here
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("storm open: %v", err)
	}
	// Accumulate to exactly the window in sub-settled-gap steps. A single
	// 90s jump would exceed the 45s settled gap and reset the storm instead,
	// which is the whole reason the two constants are ordered.
	for i, want := range []time.Duration{30 * time.Second, 60 * time.Second} {
		clk.advance(30 * time.Second)
		if err := r.refresh(context.Background(), common, "open"); err != nil {
			t.Fatalf("dial %d at elapsed %s: %v", i, want, err)
		}
	}
	clk.advance(30 * time.Second) // elapsed is now exactly 90s
	err := r.refresh(context.Background(), common, "open")
	if !errors.Is(err, errReconnectStalled) {
		t.Fatalf("dial exactly at the window returned %v, want errReconnectStalled (the boundary is at-least)", err)
	}
	if len(rec.errs) != 1 {
		t.Fatalf("requestRestart called %d time(s), want 1", len(rec.errs))
	}
}

// TestWatchdogNoticeFiresAgainAfterARecovery covers the storm-reset half of
// the once-per-storm latch: stormNoticed must clear when a settled gap ends a
// storm, or only the FIRST outage in a cycle is ever announced and every later
// one is silent until the stall window. The once-per-storm test never crosses
// a settled gap, so nothing else pins the reset.
func TestWatchdogNoticeFiresAgainAfterARecovery(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	var buf bytes.Buffer
	r, _ := newWatchdogRefresher(clk.now, slog.New(slog.NewJSONHandler(&buf, nil)), 90*time.Second, 45*time.Second)
	common := handedOffCommon()
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	storm := func(label string) {
		t.Helper()
		for i := 0; i < 3; i++ {
			clk.advance(5 * time.Second)
			if err := r.refresh(context.Background(), common, "open"); err != nil {
				t.Fatalf("%s dial %d: %v", label, i, err)
			}
		}
	}
	storm("first outage")
	clk.advance(50 * time.Second) // longer than the settled gap: the tunnel served again
	storm("second outage")
	if got := strings.Count(buf.String(), `"event":"reconnect_retrying"`); got != 2 {
		t.Fatalf("reconnect_retrying emitted %d time(s) across two separate outages, want 2 — the latch must reset when a storm ends\nlog:\n%s", got, buf.String())
	}
}

// TestWatchdogStallLineIsGreppable pins the line an operator greps when the
// watchdog takes a cycle back. It carries no assertions otherwise: the event
// name, the dial count and the elapsed detail could all be renamed or dropped
// without a single test noticing.
func TestWatchdogStallLineIsGreppable(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	var buf bytes.Buffer
	r, rec := newWatchdogRefresher(clk.now, slog.New(slog.NewJSONHandler(&buf, nil)), 90*time.Second, 45*time.Second)
	common := handedOffCommon()
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	var last error
	for i := 0; i < 12; i++ {
		clk.advance(20 * time.Second)
		if last = r.refresh(context.Background(), common, "open"); last != nil {
			break
		}
	}
	if !errors.Is(last, errReconnectStalled) {
		t.Fatalf("never stalled: %v", last)
	}
	// Isolate the stall line. Asserting against the whole buffer would let
	// the retrying notice — which carries dial_attempts too — satisfy checks
	// the stall line had dropped.
	stallLine := ""
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(l, `"event":"reconnect_stalled"`) {
			stallLine = l
			break
		}
	}
	if stallLine == "" {
		t.Fatalf("no reconnect_stalled line emitted\nlog:\n%s", buf.String())
	}
	for _, want := range []string{`"dial_attempts":`, `"stalled_seconds":`, "restarting the connection cycle"} {
		if !strings.Contains(stallLine, want) {
			t.Errorf("stall line is missing %q\nline:\n%s", want, stallLine)
		}
	}
	// The wrapped cause is what the budget exit's detail ends up quoting, so
	// it must name the shape and the evidence, not just the sentinel.
	if got := rec.errs[0].Error(); !strings.Contains(got, "no tunnel session for") || !strings.Contains(got, "dial attempts") {
		t.Errorf("stall cause = %q, want it to name the elapsed time and the dial count", got)
	}
}

// TestWatchdogReachesTheProductionConnectorSeam drives a stall through the
// seam production actually uses — knockingConnector.Open, the wrapper FRP
// calls on every physical dial — rather than calling refresh directly like
// every other test here. It pins that the wrapper forwards the watchdog's
// error to FRP instead of swallowing it, which is what makes the dial fail
// and the cycle unwind.
func TestWatchdogReachesTheProductionConnectorSeam(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	r, rec := newWatchdogRefresher(clk.now, discardLogger(), 90*time.Second, 45*time.Second)
	common := handedOffCommon()
	// TCPMux defaults on, so the physical dial — and therefore the refresh —
	// happens in Open. Guarded by TestPhysicalDialInOpen.
	if !physicalDialInOpen(common) {
		t.Fatal("test premise broken: this common config does not dial in Open")
	}
	conn := &knockingConnector{base: noopConnector{}, ctx: context.Background(), common: common, refresher: r}

	if err := conn.Open(); err != nil { // handoff
		t.Fatalf("handoff Open: %v", err)
	}
	var last error
	for i := 0; i < 12; i++ {
		clk.advance(20 * time.Second)
		if last = conn.Open(); last != nil {
			break
		}
	}
	if !errors.Is(last, errReconnectStalled) {
		t.Fatalf("Open() returned %v after a sustained storm; the connector wrapper must surface the watchdog's stall to FRP", last)
	}
	if len(rec.errs) != 1 {
		t.Fatalf("requestRestart called %d time(s) through the production seam, want 1", len(rec.errs))
	}
}

// noopConnector stands in for the FRP connector the wrapper delegates to, so
// the seam test exercises the wrapper and the watchdog without a network.
type noopConnector struct{}

func (noopConnector) Open() error { return nil }

// Connect must never be reached: TCPMux is on, so the physical dial — and
// therefore the refresh the watchdog rides on — belongs to Open. Returning an
// error rather than a nil conn makes a seam change fail loudly here instead
// of nil-dereferencing somewhere downstream.
func (noopConnector) Connect() (net.Conn, error) {
	return nil, errors.New("noopConnector.Connect called: the physical dial moved off Open, so the watchdog is attached to the wrong method")
}
func (noopConnector) Close() error { return nil }
