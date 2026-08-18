package supervisor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/frpgen"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/knock"
)

func newTestRefresher(knocker knock.Knocker, gate time.Duration) *redialKnockRefresher {
	return &redialKnockRefresher{
		knocker:    knocker,
		resourceID: testResource,
		gate:       gate,
		logger:     discardLogger(),
	}
}

// TestRefresherFirstCycleHandoffSkipsKnock: when the supervisor already
// stamped this cycle's token, the refresher's first call is a handoff — no
// extra knock, and the gate starts at handoff time.
func TestRefresherFirstCycleHandoffSkipsKnock(t *testing.T) {
	t.Parallel()
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	r := newTestRefresher(knocker, time.Hour)
	common := commonForTest()
	common.Metadatas = map[string]string{frpgen.MetaQURLKnockToken: "supervisor-stamped"}
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := knocker.calls.Load(); got != 0 {
		t.Fatalf("knocks during first-cycle handoff = %d, want 0", got)
	}
	// A second call inside the gate stays debounced.
	if err := r.refresh(context.Background(), common, "open"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := knocker.calls.Load(); got != 0 {
		t.Fatalf("knocks inside the gate = %d, want 0", got)
	}
}

// TestRefresherReKnocksAfterGateAndRestamps: past the gate, a redial refresh
// knocks again and restamps token plus dial target from the fresh ACK.
func TestRefresherReKnocksAfterGateAndRestamps(t *testing.T) {
	t.Parallel()
	knocker := &fakeKnocker{script: []knockResp{
		{result: &knock.Result{
			ACTokens:     map[string]string{testResource: "fresh-token"},
			ResourceHost: map[string]string{testResource: "tunnel-b.example:9002"},
		}},
	}}
	r := newTestRefresher(knocker, time.Millisecond)
	common := commonForTest()
	common.Metadatas = map[string]string{frpgen.MetaQURLKnockToken: "stale-token"}
	if err := r.refresh(context.Background(), common, "open"); err != nil { // handoff
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
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
// (nothing stamped yet), the first refresh must knock.
func TestRefresherWithoutSupervisorTokenKnocksImmediately(t *testing.T) {
	t.Parallel()
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	r := newTestRefresher(knocker, time.Hour)
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
	boom := errors.New("knock transport failure")
	knocker := &fakeKnocker{script: []knockResp{
		{err: boom}, {err: boom}, healthyKnockResp("h.example:1"), {err: boom},
	}}
	var restarts atomic.Int64
	r := newTestRefresher(knocker, time.Nanosecond)
	r.requestRestart = func(error) { restarts.Add(1) }
	common := commonForTest()
	for i := range 4 {
		err := r.refresh(context.Background(), common, "connect")
		wantErr := i != 2
		if (err != nil) != wantErr {
			t.Fatalf("refresh[%d] error = %v, want error=%v", i, err, wantErr)
		}
		time.Sleep(time.Millisecond)
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
	boom := errors.New("knock transport failure")
	knocker := &fakeKnocker{script: []knockResp{{err: boom}}}
	var restartErr atomic.Pointer[error]
	r := newTestRefresher(knocker, time.Nanosecond)
	r.requestRestart = func(err error) { restartErr.Store(&err) }
	common := commonForTest()
	for range redialKnockMaxFailures {
		_ = r.refresh(context.Background(), common, "connect")
		time.Sleep(time.Millisecond)
	}
	got := restartErr.Load()
	if got == nil {
		t.Fatal("budget exhaustion never requested a restart")
	}
	if !errors.Is(*got, errTooManyKnockFailures) || !errors.Is(*got, boom) {
		t.Fatalf("restart cause = %v, want the sentinel wrapping the last knock error", *got)
	}
}

// TestRefresherConcurrentRefreshersCollapseToOneGatedKnock: concurrent dial
// paths must serialize through the refresher and collapse into a single
// gated knock — the lock spans knock plus restamp by design.
func TestRefresherConcurrentRefreshersCollapseToOneGatedKnock(t *testing.T) {
	t.Parallel()
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	r := newTestRefresher(knocker, time.Hour)
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
// doubled counts.
func TestRefresherConcurrentFailuresKeepBudgetExact(t *testing.T) {
	t.Parallel()
	boom := errors.New("knock transport failure")
	knocker := &fakeKnocker{script: []knockResp{{err: boom}}}
	var restarts atomic.Int64
	r := newTestRefresher(knocker, time.Nanosecond)
	r.requestRestart = func(error) { restarts.Add(1) }
	common := commonForTest()
	var wg sync.WaitGroup
	for range redialKnockMaxFailures {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.refresh(context.Background(), common, "connect")
			time.Sleep(time.Millisecond)
		}()
	}
	wg.Wait()
	// Serialized by r.mu; the gate is 1ns so every call really knocks.
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

// TestPhysicalDialInOpen pins which connector method owns the physical dial
// per transport shape on the pinned FRP fork.
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
		{"nil tcpmux", muxNil, true},
		{"tcpmux enabled", muxTrue, true},
		{"tcpmux disabled dials in connect", muxFalse, false},
	}
	for _, tc := range cases {
		if got := physicalDialInOpen(tc.common); got != tc.want {
			t.Fatalf("%s: physicalDialInOpen = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRefresherFailureRestampsNothing: a failed refresh leaves the prior
// stamp untouched (the stale token may still be inside its admission window;
// the forced LoginFailExit path owns recovery if it is not).
func TestRefresherFailureRestampsNothing(t *testing.T) {
	t.Parallel()
	knocker := &fakeKnocker{script: []knockResp{{err: errors.New("boom")}}}
	r := newTestRefresher(knocker, time.Nanosecond)
	common := commonForTest()
	common.Metadatas = map[string]string{frpgen.MetaQURLKnockToken: "prior-token"}
	common.ServerAddr, common.ServerPort = "prior.example", 7000
	r.mu.Lock()
	r.lastKnockAt = time.Now().Add(-time.Hour) // past the gate, not a handoff
	r.mu.Unlock()
	if err := r.refresh(context.Background(), common, "connect"); err == nil {
		t.Fatal("refresh succeeded, want the knock failure")
	}
	if common.Metadatas[frpgen.MetaQURLKnockToken] != "prior-token" || common.ServerAddr != "prior.example" {
		t.Fatal("failed refresh mutated the prior stamp")
	}
}
