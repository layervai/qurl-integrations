package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/frpgen"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/knock"
)

// The refresher suite drives the gate arithmetic through injected clocks and
// never reads the real one: Go's monotonic reading advances on the
// interrupt-timer tick on Windows (up to ~15.6ms), so a real-clock gate —
// however small — collapses serialized attempts into one debounce window and
// the budget assertions become scheduling-dependent.

// manualClock is a hand-advanced clock; reads never move it.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newManualClock() *manualClock {
	return &manualClock{t: time.Unix(1700000000, 0)}
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// steppingClock advances itself by step on every read. The refresher reads
// the clock exactly once per refresh, so each serialized call observes the
// previous one as a full step in the past — deterministically past any gate
// at or below step, regardless of scheduling.
type steppingClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (c *steppingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(c.step)
	return c.t
}

func newTestRefresher(knocker knock.Knocker, gate time.Duration, now func() time.Time) *redialKnockRefresher {
	return &redialKnockRefresher{
		knocker:    knocker,
		resourceID: testResource,
		gate:       gate,
		logger:     discardLogger(),
		now:        now,
	}
}

// TestRefresherFirstCycleHandoffSkipsKnock: when the supervisor already
// stamped this cycle's token, the refresher's first call is a handoff — no
// extra knock, and the gate starts at handoff time.
func TestRefresherFirstCycleHandoffSkipsKnock(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	r := newTestRefresher(knocker, time.Hour, clk.now)
	common := commonForTest()
	common.Metadatas = map[string]string{frpgen.MetaQURLKnockToken: "supervisor-stamped"}
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := knocker.calls.Load(); got != 0 {
		t.Fatalf("knocks during first-cycle handoff = %d, want 0", got)
	}
	// A second call inside the gate stays debounced, even most of the way in.
	clk.advance(time.Hour - time.Nanosecond)
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := knocker.calls.Load(); got != 0 {
		t.Fatalf("knocks inside the gate = %d, want 0", got)
	}
}

// TestRefresherReKnocksAfterGateAndRestamps: exactly at the gate boundary a
// redial refresh knocks again and restamps token plus dial target from the
// fresh ACK (the gate is "at least", not "strictly more").
func TestRefresherReKnocksAfterGateAndRestamps(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	const gate = 10 * time.Second
	knocker := &fakeKnocker{script: []knockResp{
		{result: &knock.Result{
			ACTokens:     map[string]string{testResource: "fresh-token"},
			ResourceHost: map[string]string{testResource: "tunnel-b.example:9002"},
		}},
	}}
	r := newTestRefresher(knocker, gate, clk.now)
	common := commonForTest()
	common.Metadatas = map[string]string{frpgen.MetaQURLKnockToken: "stale-token"}
	if err := r.refresh(context.Background(), common, "open"); err != nil { // handoff
		t.Fatal(err)
	}
	clk.advance(gate)
	if err := r.refresh(context.Background(), common, "connect"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := knocker.calls.Load(); got != 1 {
		t.Fatalf("knocks = %d, want exactly one past-gate refresh", got)
	}
	if got := common.Metadatas[frpgen.MetaQURLKnockToken]; got != "fresh-token" {
		t.Fatalf("token = %q, want the restamped fresh-token", got)
	}
	if common.ServerAddr != "tunnel-b.example" || common.ServerPort != 9002 {
		t.Fatalf("dial target = %s:%d, want the fresh ACK's tunnel-b.example:9002", common.ServerAddr, common.ServerPort)
	}
}

// TestRefresherWithoutSupervisorTokenKnocksImmediately: with no handoff token
// (nothing stamped yet), the first refresh must knock. Runs on the nil clock
// seam deliberately, covering the production time.Now default (the branch is
// clock-value-independent: a zero lastKnockAt knocks unconditionally).
func TestRefresherWithoutSupervisorTokenKnocksImmediately(t *testing.T) {
	t.Parallel()
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	r := newTestRefresher(knocker, time.Hour, nil)
	common := commonForTest()
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := knocker.calls.Load(); got != 1 {
		t.Fatalf("knocks = %d, want an immediate knock with no handoff token", got)
	}
}

// TestRefresherConsecutiveFailuresResetAfterSuccess: the in-run budget resets
// on a successful refresh, so intermittent failures never accumulate to the
// restart escalation.
func TestRefresherConsecutiveFailuresResetAfterSuccess(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	const gate = 10 * time.Second
	boom := errors.New("knock transport failure")
	knocker := &fakeKnocker{script: []knockResp{
		{err: boom}, {err: boom}, healthyKnockResp("h.example:1"), {err: boom},
	}}
	var restarts atomic.Int64
	r := newTestRefresher(knocker, gate, clk.now)
	r.requestRestart = func(error) { restarts.Add(1) }
	common := commonForTest()
	for i := range 4 {
		err := r.refresh(context.Background(), common, "connect")
		wantErr := i != 2
		if (err != nil) != wantErr {
			t.Fatalf("refresh[%d] error = %v, want error=%v", i, err, wantErr)
		}
		clk.advance(gate)
	}
	r.mu.Lock()
	failures := r.consecutiveFailure
	r.mu.Unlock()
	if failures != 1 {
		t.Fatalf("consecutiveFailure = %d, want 1 (reset by the success, bumped by the last failure)", failures)
	}
	if restarts.Load() != 0 {
		t.Fatalf("restart requested %d times below the budget, want 0", restarts.Load())
	}
}

// TestRefresherBudgetRequestsRestartWithSentinel: exhausting the in-run
// budget escalates exactly once per threshold crossing with the supervisor's
// knock sentinel, so the outer loop treats it as a knock-budget exit.
func TestRefresherBudgetRequestsRestartWithSentinel(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	const gate = 10 * time.Second
	boom := errors.New("knock transport failure")
	knocker := &fakeKnocker{script: []knockResp{{err: boom}}}
	var restartErr atomic.Pointer[error]
	r := newTestRefresher(knocker, gate, clk.now)
	r.requestRestart = func(err error) { restartErr.Store(&err) }
	common := commonForTest()
	for range redialKnockMaxFailures {
		_ = r.refresh(context.Background(), common, "connect")
		clk.advance(gate)
	}
	if got := knocker.calls.Load(); got != redialKnockMaxFailures {
		t.Fatalf("knocks = %d, want every past-gate attempt (%d) to have fired", got, redialKnockMaxFailures)
	}
	got := restartErr.Load()
	if got == nil {
		t.Fatal("budget exhaustion never requested a restart")
	}
	if !errors.Is(*got, ErrTooManyKnockFailures) || !errors.Is(*got, boom) {
		t.Fatalf("restart cause = %v, want the sentinel wrapping the last knock error", *got)
	}
}

// TestRefresherConcurrentRefreshersCollapseToOneGatedKnock: concurrent dial
// paths must serialize through the refresher and collapse into a single
// gated knock — the lock spans knock plus restamp by design. The static
// clock makes "inside the gate" exact for every follower.
func TestRefresherConcurrentRefreshersCollapseToOneGatedKnock(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	r := newTestRefresher(knocker, time.Hour, clk.now)
	common := commonForTest()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.refresh(context.Background(), common, "connect")
		}()
	}
	wg.Wait()
	if got := knocker.calls.Load(); got != 1 {
		t.Fatalf("concurrent refreshes produced %d knocks, want 1 (rest gated)", got)
	}
	if got := common.Metadatas[frpgen.MetaQURLKnockToken]; got != "ac-token" {
		t.Fatalf("token = %q, want the single refresh's stamp", got)
	}
}

// TestRefresherConcurrentFailuresKeepBudgetExact: under concurrency the
// failure counter and the single restart escalation stay exact — no lost or
// doubled counts. The stepping clock hands every serialized call a
// past-the-gate instant, so all five attempts really knock regardless of how
// the scheduler interleaves them.
func TestRefresherConcurrentFailuresKeepBudgetExact(t *testing.T) {
	t.Parallel()
	const gate = 10 * time.Second
	clk := &steppingClock{t: time.Unix(1700000000, 0), step: gate}
	boom := errors.New("knock transport failure")
	knocker := &fakeKnocker{script: []knockResp{{err: boom}}}
	var restarts atomic.Int64
	r := newTestRefresher(knocker, gate, clk.now)
	r.requestRestart = func(error) { restarts.Add(1) }
	common := commonForTest()
	var wg sync.WaitGroup
	for range redialKnockMaxFailures {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.refresh(context.Background(), common, "connect")
		}()
	}
	wg.Wait()
	if got := knocker.calls.Load(); got != redialKnockMaxFailures {
		t.Fatalf("knocks = %d, want all %d attempts past the stepped gate", got, redialKnockMaxFailures)
	}
	r.mu.Lock()
	failures := r.consecutiveFailure
	r.mu.Unlock()
	if failures != redialKnockMaxFailures {
		t.Fatalf("consecutiveFailure = %d, want exactly %d", failures, redialKnockMaxFailures)
	}
	if restarts.Load() != 1 {
		t.Fatalf("restarts = %d, want exactly one escalation at the threshold", restarts.Load())
	}
}

// TestRefresherNilReceiverAndNilKnockerAreNoops keeps the seam safe for
// wiring paths that have no refresher.
func TestRefresherNilReceiverAndNilKnockerAreNoops(t *testing.T) {
	t.Parallel()
	var nilRefresher *redialKnockRefresher
	if err := nilRefresher.refresh(context.Background(), commonForTest(), "open"); err != nil {
		t.Fatalf("nil receiver refresh = %v, want nil", err)
	}
	r := &redialKnockRefresher{logger: discardLogger()}
	if err := r.refresh(context.Background(), commonForTest(), "open"); err != nil {
		t.Fatalf("nil knocker refresh = %v, want nil", err)
	}
}

// TestApplyKnockResultContract drives the shared overlay validation used on
// the redial path: fail closed on missing halves, unusable dial targets, and
// the IP-literal TLS guard; restamp on success.
func TestApplyKnockResultContract(t *testing.T) {
	t.Parallel()
	tlsOn := func() *v1.ClientCommonConfig {
		enabled := true
		c := commonForTest()
		c.Transport.TLS.Enable = &enabled
		return c
	}
	cases := []struct {
		name    string
		common  *v1.ClientCommonConfig
		result  *knock.Result
		wantErr error
	}{
		{"nil result", commonForTest(), nil, errNilKnockResult},
		{
			"missing token",
			commonForTest(),
			&knock.Result{ResourceHost: map[string]string{testResource: "h.example:1"}},
			errKnockACTokenMissing,
		},
		{
			"missing host",
			commonForTest(),
			&knock.Result{ACTokens: map[string]string{testResource: "tok"}},
			errKnockResourceHostMissing,
		},
		{
			"bare host",
			commonForTest(),
			&knock.Result{
				ACTokens:     map[string]string{testResource: "tok"},
				ResourceHost: map[string]string{testResource: "h.example"},
			},
			errKnockResourceHostUnusable,
		},
		{
			"tls guard rejects ip literal",
			tlsOn(),
			&knock.Result{
				ACTokens:     map[string]string{testResource: "tok"},
				ResourceHost: map[string]string{testResource: "203.0.113.9:9001"},
			},
			errKnockResourceHostUnusable,
		},
		{
			"tls with hostname passes",
			tlsOn(),
			&knock.Result{
				ACTokens:     map[string]string{testResource: "tok"},
				ResourceHost: map[string]string{testResource: "h.example:9001"},
			},
			nil,
		},
		{
			"bracketed ipv6 passes without tls",
			commonForTest(),
			&knock.Result{
				ACTokens:     map[string]string{testResource: "tok"},
				ResourceHost: map[string]string{testResource: "[2001:db8::5]:9001"},
			},
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			token, err := applyKnockResult(tc.common, testResource, tc.result)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("applyKnockResult = %v, want %v", err, tc.wantErr)
				}
				if _, stamped := tc.common.Metadatas[frpgen.MetaQURLKnockToken]; stamped {
					t.Fatal("token stamped on a fail-closed path")
				}
				return
			}
			if err != nil || token != "tok" {
				t.Fatalf("applyKnockResult = %q, %v; want tok, nil", token, err)
			}
			if tc.common.Metadatas[frpgen.MetaQURLKnockToken] != "tok" {
				t.Fatal("token not stamped on success")
			}
		})
	}
	if _, err := applyKnockResult(nil, testResource, &knock.Result{}); !errors.Is(err, errNilCommonConfig) {
		t.Fatalf("nil common = %v, want errNilCommonConfig", err)
	}
}

// TestRefreshStampsOnlyTheDialTargetAndToken pins the refresher's write set —
// ServerAddr, ServerPort and the one knock-token metadata entry — against the
// config production actually runs.
//
// It is load-bearing for the concurrency argument on redialKnockRefresher, not
// tidiness. The fork reads the stamped config without synchronization, and what
// keeps that harmless on the Open seam is that the one path FRP drives
// concurrently — knockingConnector.Connect, reached once per ReqWorkConn
// through msg.AsyncHandler — touches nothing in that config but
// Transport.Protocol and Transport.TCPMux, via physicalDialInOpen. Let a
// refresh start restamping a Transport field and those reads become a live data
// race in production rather than the latent one the unmuxed seam already has.
//
// It drives refresh rather than applyKnockResult alone because refresh is free
// to write common outside that helper, and nothing else in the package would
// notice: a `common.Transport.Protocol = "websocket"` stitched into refresh
// beside its success log passes every other test here.
//
// Two techniques, because neither covers the field alone. The marshaled-JSON
// compare catches any write that CHANGES a value, including one made through
// the *bool transport knobs — a shallow struct copy would share those pointers
// and compare each to itself. The pointer-identity checks catch a knob
// repointed at an equal value, which a value snapshot cannot see. Neither
// catches a value-preserving write through an existing pointer (`*Enable =
// true` where it is already true); that is still an unsynchronized write, and
// it is the acknowledged floor of this technique rather than a claim of total
// coverage.
func TestRefreshStampsOnlyTheDialTargetAndToken(t *testing.T) {
	t.Parallel()
	common := productionCommon(t, "write-set")
	clk := newManualClock()
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("stamped.example:7443")}}
	r := newTestRefresher(knocker, time.Hour, clk.now)

	// A real knock, not the supervisor's first-cycle handoff: the generated
	// config carries MetaClientVersion but no token, so refresh takes the
	// knocking path and actually stamps.
	if commonKnockToken(common) != "" {
		t.Fatal("test premise broken: the generated config already carries a knock token, so refresh would hand off instead of stamping")
	}
	beforeAddr, beforePort := common.ServerAddr, common.ServerPort
	beforeToken, hadToken := common.Metadatas[frpgen.MetaQURLKnockToken]
	beforeMux, beforeTLS, beforeExit := common.Transport.TCPMux, common.Transport.TLS.Enable, common.LoginFailExit
	before, err := json.Marshal(common)
	if err != nil {
		t.Fatalf("marshal the pre-stamp config: %v", err)
	}

	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := knocker.calls.Load(); got != 1 {
		t.Fatalf("knocks = %d, want exactly 1 (the stamp under test)", got)
	}
	if common.ServerAddr != "stamped.example" || common.ServerPort != 7443 {
		t.Fatalf("dial target = %s:%d, want the ACK's stamped.example:7443", common.ServerAddr, common.ServerPort)
	}
	if got := common.Metadatas[frpgen.MetaQURLKnockToken]; got != "ac-token" {
		t.Fatalf("knock token = %q, want the ACK's ac-token", got)
	}
	if common.Transport.TCPMux != beforeMux || common.Transport.TLS.Enable != beforeTLS || common.LoginFailExit != beforeExit {
		t.Fatal("refresh repointed one of the *bool knobs; the fork reads TCPMux from the work-connection goroutines, so replacing the pointer is an unsynchronized write even when the value is unchanged")
	}

	// Undo exactly the three sanctioned writes. Anything else the stamp
	// touched survives into the comparison below.
	common.ServerAddr, common.ServerPort = beforeAddr, beforePort
	if hadToken {
		common.Metadatas[frpgen.MetaQURLKnockToken] = beforeToken
	} else {
		delete(common.Metadatas, frpgen.MetaQURLKnockToken)
	}
	after, err := json.Marshal(common)
	if err != nil {
		t.Fatalf("marshal the post-stamp config: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("refresh wrote outside its dial-target-and-token set; the fork reads this config unsynchronized from the work-connection goroutines\n%s", jsonFieldDiff(t, before, after))
	}
}

// jsonFieldDiff reports the top-level config keys that differ between two
// marshaled snapshots. The raw blobs run to hundreds of bytes on one line, so
// dumping both leaves the reader to spot a few changed characters by eye.
func jsonFieldDiff(t *testing.T, before, after []byte) string {
	t.Helper()
	var b, a map[string]any
	if err := json.Unmarshal(before, &b); err != nil {
		return fmt.Sprintf("before: %s\nafter:  %s", before, after)
	}
	if err := json.Unmarshal(after, &a); err != nil {
		return fmt.Sprintf("before: %s\nafter:  %s", before, after)
	}
	keys := make([]string, 0, len(b)+len(a))
	for k := range b {
		keys = append(keys, k)
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var diffs []string
	for _, k := range keys {
		bv, av := fmt.Sprintf("%v", b[k]), fmt.Sprintf("%v", a[k])
		if bv != av {
			diffs = append(diffs, fmt.Sprintf("  %s: %s -> %s", k, bv, av))
		}
	}
	if len(diffs) == 0 {
		return "(no top-level key differs; the change is nested inside an equal-printing value)"
	}
	return strings.Join(diffs, "\n")
}

// TestPhysicalDialInOpen pins which connector method owns the physical dial
// per transport shape on the pinned FRP fork.
//
// The two TCPMux-off rows must agree. The fork reads the field through
// lo.FromPtr, which flattens nil to false, so an unset TCPMux takes the same
// unmuxed path as an explicit false — the config that never ran
// ClientCommonConfig.Complete is not a config with muxing on.
// TestForkDialsFromConnectWithoutTCPMux proves that against the real
// connector; this table is the predicate's side of the same fact.
func TestPhysicalDialInOpen(t *testing.T) {
	t.Parallel()
	muxOn, muxOff := true, false
	quic := commonForTest()
	quic.Transport.Protocol = "QUIC"
	muxNil := commonForTest()
	muxTrue := commonForTest()
	muxTrue.Transport.TCPMux = &muxOn
	muxFalse := commonForTest()
	muxFalse.Transport.TCPMux = &muxOff
	cases := []struct {
		name   string
		common *v1.ClientCommonConfig
		want   bool
	}{
		{"nil common", nil, true},
		{"quic case-insensitive", quic, true},
		{"unset tcpmux dials in connect", muxNil, false},
		{"tcpmux enabled", muxTrue, true},
		{"tcpmux disabled dials in connect", muxFalse, false},
	}
	for _, tc := range cases {
		if got := physicalDialInOpen(tc.common); got != tc.want {
			t.Fatalf("%s: physicalDialInOpen = %v, want %v", tc.name, got, tc.want)
		}
	}
	// A backstop against a future edit to the TABLE, not to the predicate:
	// the two rows above already pin both values, so this can only fire once
	// one of them is deleted or relaxed. That is worth keeping — the unset row
	// is exactly the one that was wrong before, so losing it again should not
	// be silent — but it is not what guards the predicate. The empirical guard
	// is TestForkDialsFromConnectWithoutTCPMux.
	if physicalDialInOpen(muxNil) != physicalDialInOpen(muxFalse) {
		t.Fatal("unset and explicitly-disabled TCPMux disagree, but the fork treats them identically")
	}
}

// TestRefresherFailureRestampsNothing: a failed refresh leaves the prior
// stamp untouched (the stale token may still be inside its admission window;
// the forced LoginFailExit path owns recovery if it is not).
func TestRefresherFailureRestampsNothing(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	const gate = 10 * time.Second
	knocker := &fakeKnocker{script: []knockResp{{err: errors.New("boom")}}}
	r := newTestRefresher(knocker, gate, clk.now)
	common := commonForTest()
	common.Metadatas = map[string]string{frpgen.MetaQURLKnockToken: "prior-token"}
	common.ServerAddr, common.ServerPort = "prior.example", 7000
	r.mu.Lock()
	r.lastKnockAt = clk.now() // a prior refresh, not a handoff
	r.mu.Unlock()
	clk.advance(gate)
	if err := r.refresh(context.Background(), common, "connect"); err == nil {
		t.Fatal("refresh succeeded, want the knock failure")
	}
	if common.Metadatas[frpgen.MetaQURLKnockToken] != "prior-token" || common.ServerAddr != "prior.example" {
		t.Fatal("failed refresh mutated the prior stamp")
	}
}
