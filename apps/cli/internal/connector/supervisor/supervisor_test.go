package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/frpgen"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/knock"
)

const testResource = "r_public_resource"

// fakeRunner is an FRPRunner that records the dial target it was given and
// exits Run when its done channel closes (or the context does).
type fakeRunner struct {
	dialTarget string
	metadatas  map[string]string
	done       chan struct{}
	runErr     error
	closed     atomic.Bool
}

func (r *fakeRunner) Run(ctx context.Context) error {
	select {
	case <-r.done:
		return r.runErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *fakeRunner) GracefulClose(time.Duration) {
	r.closed.Store(true)
}

// runnerLog captures every runner the factory builds, in order.
type runnerLog struct {
	mu      sync.Mutex
	runners []*fakeRunner
}

func (l *runnerLog) add(r *fakeRunner) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runners = append(l.runners, r)
}

func (l *runnerLog) snapshot() []*fakeRunner {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*fakeRunner, len(l.runners))
	copy(out, l.runners)
	return out
}

// makeFactory returns a RunnerFactory building fakeRunners; runErrSeq is
// iterated through cycles with the last entry repeating.
func makeFactory(log *runnerLog, runErrSeq []error) RunnerFactory {
	var idx atomic.Int64
	return func(common *v1.ClientCommonConfig) (FRPRunner, error) {
		i := int(idx.Add(1) - 1)
		var runErr error
		if len(runErrSeq) > 0 {
			if i >= len(runErrSeq) {
				i = len(runErrSeq) - 1
			}
			runErr = runErrSeq[i]
		}
		metadatas := make(map[string]string, len(common.Metadatas))
		for k, v := range common.Metadatas {
			metadatas[k] = v
		}
		// Recorded the way FRP renders its dial string (plain concatenation,
		// not JoinHostPort) — the reason ParseResourceHost re-brackets IPv6.
		r := &fakeRunner{
			dialTarget: common.ServerAddr + ":" + strconv.Itoa(common.ServerPort),
			metadatas:  metadatas,
			done:       make(chan struct{}),
			runErr:     runErr,
		}
		log.add(r)
		return r, nil
	}
}

func waitForRunners(t *testing.T, log *runnerLog, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(log.snapshot()) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("expected %d runner(s), got %d", n, len(log.snapshot()))
}

func closeAllRunners(log *runnerLog) {
	for _, r := range log.snapshot() {
		select {
		case <-r.done:
		default:
			close(r.done)
		}
	}
}

// fakeKnocker returns scripted knock responses; the final entry repeats.
type fakeKnocker struct {
	mu      sync.Mutex
	calls   atomic.Int64
	script  []knockResp
	timesAt []time.Time
}

type knockResp struct {
	result *knock.Result
	err    error
}

func (k *fakeKnocker) Knock(context.Context) (*knock.Result, error) {
	idx := int(k.calls.Add(1) - 1)
	k.mu.Lock()
	k.timesAt = append(k.timesAt, time.Now())
	if idx >= len(k.script) {
		idx = len(k.script) - 1
	}
	resp := k.script[idx]
	k.mu.Unlock()
	return resp.result, resp.err
}

func (k *fakeKnocker) callTimes() []time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]time.Time, len(k.timesAt))
	copy(out, k.timesAt)
	return out
}

func healthyKnockResp(host string) knockResp {
	return knockResp{result: &knock.Result{
		ACTokens:     map[string]string{testResource: "ac-token"},
		ResourceHost: map[string]string{testResource: host},
	}}
}

// fakeMarker records episode-ladder transitions.
type fakeMarker struct {
	mu         sync.Mutex
	armed      []string
	cleared    int
	requestErr error
	clearErr   error
}

func (m *fakeMarker) RequestRefresh(reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.requestErr != nil {
		return m.requestErr
	}
	m.armed = append(m.armed, reason)
	return nil
}

func (m *fakeMarker) ClearRefreshMarker() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clearErr != nil {
		return m.clearErr
	}
	m.cleared++
	return nil
}

func (m *fakeMarker) snapshot() (armed []string, cleared int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.armed...), m.cleared
}

func commonForTest() *v1.ClientCommonConfig {
	return &v1.ClientCommonConfig{
		ServerAddr: "placeholder.example",
		ServerPort: 1,
	}
}

// testConfig assembles a fast-cycling Config over the given fakes. The
// discard logger keeps the suite's output to real test failures.
func testConfig(knocker knock.Knocker, factory RunnerFactory) *Config {
	return &Config{
		Common:           commonForTest(),
		Knocker:          knocker,
		KnockResourceID:  testResource,
		RunnerFactory:    factory,
		Logger:           slog.New(slog.DiscardHandler),
		MinKnockInterval: time.Millisecond,
		MinBackoff:       time.Millisecond,
		MaxBackoff:       2 * time.Millisecond,
	}
}

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	factory := makeFactory(&runnerLog{}, nil)
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"missing common", func(c *Config) { c.Common = nil }, "Common is required"},
		{"missing knocker", func(c *Config) { c.Knocker = nil }, "Knocker is required"},
		{"missing resource", func(c *Config) { c.KnockResourceID = "" }, "KnockResourceID is required"},
		{"missing factory", func(c *Config) { c.RunnerFactory = nil }, "RunnerFactory is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(knocker, factory)
			tc.mutate(cfg)
			if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("New error = %v, want %q", err, tc.wantErr)
			}
		})
	}
	if _, err := New(testConfig(knocker, factory)); err != nil {
		t.Fatalf("New on a complete config: %v", err)
	}
}

// TestKnockOverlaysDialTargetAndToken pins the happy-path wire effect: the
// per-cycle dial target comes from the ACK (never the placeholder base
// config) and the token lands under the Login metadata contract key.
func TestKnockOverlaysDialTargetAndToken(t *testing.T) {
	t.Parallel()
	log := &runnerLog{}
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("tunnel-a.example:9001")}}
	sup, err := New(testConfig(knocker, makeFactory(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitForRunners(t, log, 1)
	cancel()
	closeAllRunners(log)
	<-done

	runner := log.snapshot()[0]
	if runner.dialTarget != "tunnel-a.example:9001" {
		t.Fatalf("dial target = %q, want the ACK-provided tunnel-a.example:9001", runner.dialTarget)
	}
	if got := runner.metadatas[frpgen.MetaQURLKnockToken]; got != "ac-token" {
		t.Fatalf("Login metadata token = %q, want the stamped ac-token", got)
	}
}

// TestKnockForcesLoginFailExit proves every knock cycle forces fail-fast
// initial Login on the per-cycle clone while leaving the caller's Common
// untouched.
func TestKnockForcesLoginFailExit(t *testing.T) {
	t.Parallel()
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	var captured atomic.Pointer[v1.ClientCommonConfig]
	log := &runnerLog{}
	base := commonForTest()
	cfg := testConfig(knocker, func(common *v1.ClientCommonConfig) (FRPRunner, error) {
		captured.Store(common)
		r := &fakeRunner{done: make(chan struct{})}
		log.add(r)
		return r, nil
	})
	cfg.Common = base
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitForRunners(t, log, 1)
	cancel()
	closeAllRunners(log)
	<-done

	got := captured.Load()
	if got.LoginFailExit == nil || !*got.LoginFailExit {
		t.Fatalf("cycle LoginFailExit = %v, want forced true", got.LoginFailExit)
	}
	if base.LoginFailExit != nil {
		t.Fatalf("caller Common.LoginFailExit mutated to %v, want untouched nil", base.LoginFailExit)
	}
}

// TestKnockFailureBudgetExitsAndArmsMarker drives the transport-failure
// budget to exhaustion: the supervisor exits with ErrTooManyKnockFailures
// wrapping the cause, never builds a runner, and arms exactly one refresh
// episode with the shared reason string.
func TestKnockFailureBudgetExitsAndArmsMarker(t *testing.T) {
	t.Parallel()
	cause := errors.New("assigned endpoint unreachable")
	knocker := &fakeKnocker{script: []knockResp{{err: cause}}}
	marker := &fakeMarker{}
	cfg := testConfig(knocker, func(*v1.ClientCommonConfig) (FRPRunner, error) {
		t.Error("RunnerFactory called on an all-failing knock episode")
		return nil, errors.New("unreachable")
	})
	cfg.Marker = marker
	cfg.MaxConsecutiveKnockFailures = 3
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runErr := sup.Run(context.Background())
	if !errors.Is(runErr, ErrTooManyKnockFailures) || !errors.Is(runErr, cause) {
		t.Fatalf("Run = %v, want ErrTooManyKnockFailures wrapping the knock cause", runErr)
	}
	if got := knocker.calls.Load(); got != 3 {
		t.Fatalf("knock attempts = %d, want exactly the budget of 3", got)
	}
	armed, cleared := marker.snapshot()
	if len(armed) != 1 || armed[0] != refreshMarkerReason {
		t.Fatalf("armed episodes = %v, want exactly one with %q", armed, refreshMarkerReason)
	}
	if cleared != 0 {
		t.Fatalf("marker cleared %d times on an all-failing episode, want 0", cleared)
	}
}

// TestAlternatingKnockAndACKFailuresShareOneBudget closes the alternating
// bypass: transport failures and unusable ACKs must accumulate in ONE
// counter, so alternating causes still exit at the budget.
func TestAlternatingKnockAndACKFailuresShareOneBudget(t *testing.T) {
	t.Parallel()
	transportErr := errors.New("udp timeout")
	emptyToken := knockResp{result: &knock.Result{
		ACTokens:     map[string]string{},
		ResourceHost: map[string]string{testResource: "h.example:1"},
	}}
	knocker := &fakeKnocker{script: []knockResp{
		{err: transportErr}, emptyToken, {err: transportErr}, emptyToken,
	}}
	cfg := testConfig(knocker, func(*v1.ClientCommonConfig) (FRPRunner, error) {
		t.Error("RunnerFactory called with no usable admission")
		return nil, errors.New("unreachable")
	})
	cfg.MaxConsecutiveKnockFailures = 4
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runErr := sup.Run(context.Background())
	if !errors.Is(runErr, ErrTooManyKnockFailures) || !errors.Is(runErr, errKnockACTokenMissing) {
		t.Fatalf("Run = %v, want the unified budget exit wrapping the final ACK cause", runErr)
	}
	if got := knocker.calls.Load(); got != 4 {
		t.Fatalf("knock attempts = %d, want the shared budget of 4", got)
	}
}

// TestTokenRejectedLoginCountsAgainstBudget pins the deferred reconciliation:
// a healthy knock whose Login is token-rejected counts against the same
// unified budget instead of resetting it at knock time.
func TestTokenRejectedLoginCountsAgainstBudget(t *testing.T) {
	t.Parallel()
	log := &runnerLog{}
	tokenReject := errors.New("login to the server failed: knock_invalid: knock token rejected")
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	cfg := testConfig(knocker, makeFactory(log, []error{tokenReject}))
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
	runErr := <-done
	if !errors.Is(runErr, ErrTooManyKnockFailures) || !errors.Is(runErr, tokenReject) {
		t.Fatalf("Run = %v, want budget exit wrapping the token-rejected login", runErr)
	}
	armed, cleared := marker.snapshot()
	if len(armed) != 1 || cleared != 0 {
		t.Fatalf("marker transitions = armed %v cleared %d, want one armed episode and no clears", armed, cleared)
	}
}

// TestHealthyCycleResetsBudgetAndClearsEpisode proves the deferred reset: a
// token-rejected cycle bumps the budget, then a fully-healthy cycle resets it
// and clears the refresh episode, so the next rejection starts counting from
// one again (the supervisor keeps looping instead of exiting).
func TestHealthyCycleResetsBudgetAndClearsEpisode(t *testing.T) {
	t.Parallel()
	log := &runnerLog{}
	tokenReject := errors.New("login to the server failed: knock_invalid: knock token expired")
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	marker := &fakeMarker{}
	cfg := testConfig(knocker, makeFactory(log, []error{tokenReject, nil, tokenReject, nil}))
	cfg.MaxConsecutiveKnockFailures = 2
	cfg.Marker = marker
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	for i := range 4 {
		waitForRunners(t, log, i+1)
		close(log.snapshot()[i].done)
	}
	waitForRunners(t, log, 5)
	cancel()
	closeAllRunners(log)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want clean cancellation (the healthy cycles must have reset the budget)", err)
	}
	_, cleared := marker.snapshot()
	if cleared < 2 {
		t.Fatalf("marker cleared %d times, want one clear per confirmed-healthy cycle (>=2)", cleared)
	}
}

// TestMissingResourceHostFailsClosedWithoutTokenStamp proves an ACK without a
// dial target refuses the Login outright: no runner starts, and the budget
// advances.
func TestMissingResourceHostFailsClosedWithoutTokenStamp(t *testing.T) {
	t.Parallel()
	knocker := &fakeKnocker{script: []knockResp{{result: &knock.Result{
		ACTokens: map[string]string{testResource: "ac-token"},
	}}}}
	cfg := testConfig(knocker, func(*v1.ClientCommonConfig) (FRPRunner, error) {
		t.Error("RunnerFactory called without a dial target")
		return nil, errors.New("unreachable")
	})
	cfg.MaxConsecutiveKnockFailures = 1
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runErr := sup.Run(context.Background())
	if !errors.Is(runErr, ErrTooManyKnockFailures) || !errors.Is(runErr, errKnockResourceHostMissing) {
		t.Fatalf("Run = %v, want fail-closed missing-dial-target budget exit", runErr)
	}
}

// TestResourceHostValidationTable drives the overlay's dial-target contract:
// canonical host:port is required, IPv6 must be bracketed (and is re-braced
// for the dial), and port edges fail closed.
func TestResourceHostValidationTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		host     string
		wantDial string // "" means fail-closed
	}{
		{"canonical", "tunnel.example:9001", "tunnel.example:9001"},
		{"bracketed ipv6", "[2001:db8::5]:9001", "[2001:db8::5]:9001"},
		{"unbracketed ipv6", "2001:db8::5", ""},
		{"bare host", "tunnel.example", ""},
		{"port zero", "tunnel.example:0", ""},
		{"port overflow", "tunnel.example:70000", ""},
		{"port junk", "tunnel.example:abc", ""},
		{"empty host", ":9001", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			knocker := &fakeKnocker{script: []knockResp{{result: &knock.Result{
				ACTokens:     map[string]string{testResource: "ac-token"},
				ResourceHost: map[string]string{testResource: tc.host},
			}}}}
			log := &runnerLog{}
			cfg := testConfig(knocker, makeFactory(log, nil))
			cfg.MaxConsecutiveKnockFailures = 1
			sup, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantDial == "" {
				runErr := sup.Run(context.Background())
				if !errors.Is(runErr, ErrTooManyKnockFailures) || !errors.Is(runErr, errKnockResourceHostUnusable) {
					t.Fatalf("Run = %v, want fail-closed unusable dial target", runErr)
				}
				if len(log.snapshot()) != 0 {
					t.Fatal("a runner was built from an unusable dial target")
				}
				return
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- sup.Run(ctx) }()
			waitForRunners(t, log, 1)
			cancel()
			closeAllRunners(log)
			<-done
			if got := log.snapshot()[0].dialTarget; got != tc.wantDial {
				t.Fatalf("dial target = %q, want %q", got, tc.wantDial)
			}
		})
	}
}

// TestOverlayTLSGuardRejectsIPLiteral proves the knock-overlay TLS-SNI guard:
// with TLS enabled and no explicit ServerName, an IP-literal dial target is
// refused (it would land as an unusable SNI value) while a hostname passes.
func TestOverlayTLSGuardRejectsIPLiteral(t *testing.T) {
	t.Parallel()
	enabled := true
	for _, tc := range []struct {
		name string
		host string
		ok   bool
	}{
		{"ip literal refused", "203.0.113.9:9001", false},
		{"bracketed ipv6 refused", "[2001:db8::5]:9001", false},
		{"hostname allowed", "tunnel.example:9001", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			knocker := &fakeKnocker{script: []knockResp{healthyKnockResp(tc.host)}}
			log := &runnerLog{}
			cfg := testConfig(knocker, makeFactory(log, nil))
			common := commonForTest()
			common.Transport.TLS.Enable = &enabled
			cfg.Common = common
			cfg.MaxConsecutiveKnockFailures = 1
			sup, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.ok {
				runErr := sup.Run(context.Background())
				if !errors.Is(runErr, errKnockResourceHostUnusable) {
					t.Fatalf("Run = %v, want the TLS guard's unusable dial target", runErr)
				}
				return
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- sup.Run(ctx) }()
			waitForRunners(t, log, 1)
			cancel()
			closeAllRunners(log)
			<-done
		})
	}
}

// TestMinKnockIntervalEnforced pins the start-to-start knock gate: two knocks
// in adjacent cycles must be at least the configured interval apart.
func TestMinKnockIntervalEnforced(t *testing.T) {
	t.Parallel()
	log := &runnerLog{}
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	cfg := testConfig(knocker, makeFactory(log, nil))
	cfg.MinKnockInterval = 150 * time.Millisecond
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitForRunners(t, log, 1)
	close(log.snapshot()[0].done)
	waitForRunners(t, log, 2)
	cancel()
	closeAllRunners(log)
	<-done

	times := knocker.callTimes()
	if len(times) < 2 {
		t.Fatalf("knock attempts = %d, want at least 2", len(times))
	}
	if gap := times[1].Sub(times[0]); gap < 140*time.Millisecond {
		t.Fatalf("knock gap = %v, want >= the 150ms gate (minus timer slack)", gap)
	}
}

// TestMetadatasIsolatedAcrossCycles proves the per-cycle clone: the token
// stamp must not leak into the caller's Common or across cycles.
func TestMetadatasIsolatedAcrossCycles(t *testing.T) {
	t.Parallel()
	log := &runnerLog{}
	knocker := &fakeKnocker{script: []knockResp{
		{result: &knock.Result{
			ACTokens:     map[string]string{testResource: "token-cycle-1"},
			ResourceHost: map[string]string{testResource: "h.example:1"},
		}},
		{result: &knock.Result{
			ACTokens:     map[string]string{testResource: "token-cycle-2"},
			ResourceHost: map[string]string{testResource: "h.example:1"},
		}},
	}}
	base := commonForTest()
	base.Metadatas = map[string]string{"client_version": "1.0.0"}
	cfg := testConfig(knocker, makeFactory(log, nil))
	cfg.Common = base
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitForRunners(t, log, 1)
	close(log.snapshot()[0].done)
	waitForRunners(t, log, 2)
	cancel()
	closeAllRunners(log)
	<-done

	if _, leaked := base.Metadatas[frpgen.MetaQURLKnockToken]; leaked {
		t.Fatal("per-cycle token stamp leaked into the caller's Common")
	}
	runners := log.snapshot()
	if got := runners[0].metadatas[frpgen.MetaQURLKnockToken]; got != "token-cycle-1" {
		t.Fatalf("cycle 1 token = %q, want token-cycle-1", got)
	}
	if got := runners[1].metadatas[frpgen.MetaQURLKnockToken]; got != "token-cycle-2" {
		t.Fatalf("cycle 2 token = %q, want its own token-cycle-2 (no cross-cycle aliasing)", got)
	}
	if got := runners[1].metadatas["client_version"]; got != "1.0.0" {
		t.Fatalf("caller-stamped metadata = %q, want preserved 1.0.0", got)
	}
}

// TestFactoryFailureIsTerminal pins the fatal path: an unusable config exits
// the supervisor (retrying identical inputs would loop forever) and still
// ends the native cycle.
func TestFactoryFailureIsTerminal(t *testing.T) {
	t.Parallel()
	factoryErr := errors.New("invalid tunnel configuration")
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	sup, err := New(testConfig(knocker, func(*v1.ClientCommonConfig) (FRPRunner, error) {
		return nil, factoryErr
	}))
	if err != nil {
		t.Fatal(err)
	}
	runErr := sup.Run(context.Background())
	if !errors.Is(runErr, factoryErr) || !strings.Contains(runErr.Error(), "config unusable") {
		t.Fatalf("Run = %v, want terminal factory error", runErr)
	}
}

// TestRunSingleShot rejects a second start on the same Supervisor.
func TestRunSingleShot(t *testing.T) {
	t.Parallel()
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	log := &runnerLog{}
	sup, err := New(testConfig(knocker, makeFactory(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitForRunners(t, log, 1)
	if err := sup.Run(context.Background()); !errors.Is(err, errAlreadyStarted) {
		t.Fatalf("second Run = %v, want errAlreadyStarted", err)
	}
	if err := sup.Start(context.Background()); !errors.Is(err, errAlreadyStarted) {
		t.Fatalf("Start after Run = %v, want errAlreadyStarted", err)
	}
	cancel()
	closeAllRunners(log)
	<-done
}

// TestCyclesCountsCompletedCyclesOnly proves the Cycles semantic: completed
// cycles count, a cycle interrupted by cancellation does not.
func TestCyclesCountsCompletedCyclesOnly(t *testing.T) {
	t.Parallel()
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	log := &runnerLog{}
	sup, err := New(testConfig(knocker, makeFactory(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	waitForRunners(t, log, 1)
	close(log.snapshot()[0].done) // cycle 1 completes
	waitForRunners(t, log, 2)
	cancel() // cycle 2 interrupted
	closeAllRunners(log)
	<-done
	if got := sup.Cycles(); got != 1 {
		t.Fatalf("Cycles = %d, want exactly the one completed cycle", got)
	}
}

// TestRunnerErrTooManyKnockFailuresPropagates pins the in-run escalation
// path: a runner returning the sentinel (the redial refresher's budget)
// terminates the supervisor with it and arms the episode.
func TestRunnerErrTooManyKnockFailuresPropagates(t *testing.T) {
	t.Parallel()
	escalation := fmt.Errorf("%w: 5 consecutive redial knock refresh failures", ErrTooManyKnockFailures)
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	log := &runnerLog{}
	marker := &fakeMarker{}
	cfg := testConfig(knocker, makeFactory(log, []error{escalation}))
	cfg.Marker = marker
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sup.Run(context.Background()) }()
	waitForRunners(t, log, 1)
	close(log.snapshot()[0].done)
	if runErr := <-done; !errors.Is(runErr, ErrTooManyKnockFailures) {
		t.Fatalf("Run = %v, want the propagated sentinel", runErr)
	}
	armed, _ := marker.snapshot()
	if len(armed) != 1 {
		t.Fatalf("armed episodes = %v, want the in-run escalation to arm one", armed)
	}
}

// TestBackoffResetsAfterHealthyRun proves a long-running cycle resets the
// escalated backoff: after rapid failures push backoff toward the cap, a
// healthy-length run must bring the next reconnect back to the floor.
func TestBackoffResetsAfterHealthyRun(t *testing.T) {
	t.Parallel()
	knocker := &fakeKnocker{script: []knockResp{healthyKnockResp("h.example:1")}}
	log := &runnerLog{}
	cfg := testConfig(knocker, makeFactory(log, nil))
	cfg.MinBackoff = 2 * time.Millisecond
	cfg.MaxBackoff = 400 * time.Millisecond
	cfg.HealthyRunThreshold = 50 * time.Millisecond
	sup, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// Cycles 1-6 fail instantly, doubling backoff toward the 400ms cap.
	for i := range 6 {
		waitForRunners(t, log, i+1)
		close(log.snapshot()[i].done)
	}
	// Cycle 7 runs past the healthy threshold before ending.
	waitForRunners(t, log, 7)
	time.Sleep(60 * time.Millisecond)
	seventhEnd := time.Now()
	close(log.snapshot()[6].done)
	// The next runner must appear after roughly MinBackoff, far below the
	// escalated backoff the failure streak had reached.
	waitForRunners(t, log, 8)
	gap := time.Since(seventhEnd)
	cancel()
	closeAllRunners(log)
	<-done
	if gap > 200*time.Millisecond {
		t.Fatalf("post-healthy reconnect gap = %v, want the floor backoff (escalated backoff not reset)", gap)
	}
}

// TestIsTooManyKnockFailures pins the exported predicate the command layer
// keys its exit decision on: true for the budget exit (bare or wrapped),
// false for everything else.
func TestIsTooManyKnockFailures(t *testing.T) {
	t.Parallel()
	if !IsTooManyKnockFailures(ErrTooManyKnockFailures) {
		t.Fatal("bare sentinel not detected")
	}
	if !IsTooManyKnockFailures(fmt.Errorf("wrapped: %w", ErrTooManyKnockFailures)) {
		t.Fatal("wrapped sentinel not detected")
	}
	if IsTooManyKnockFailures(errors.New("unrelated")) || IsTooManyKnockFailures(nil) {
		t.Fatal("false positive on unrelated or nil error")
	}
}

func TestJitterBackoffBounds(t *testing.T) {
	t.Parallel()
	const d = 100 * time.Millisecond
	for range 200 {
		got := jitterBackoff(d)
		if got < d/2 || got >= d {
			t.Fatalf("jitterBackoff(%v) = %v, want within [d/2, d)", d, got)
		}
	}
	if got := jitterBackoff(0); got != 0 {
		t.Fatalf("jitterBackoff(0) = %v, want degenerate passthrough", got)
	}
}

func TestNextBackoffCap(t *testing.T) {
	t.Parallel()
	cases := []struct{ d, ceiling, want time.Duration }{
		{time.Second, 30 * time.Second, 2 * time.Second},
		{20 * time.Second, 30 * time.Second, 30 * time.Second},
		{30 * time.Second, 30 * time.Second, 30 * time.Second},
		{0, 30 * time.Second, 30 * time.Second},
		{-time.Second, 30 * time.Second, 30 * time.Second},
	}
	for _, tc := range cases {
		if got := nextBackoffCap(tc.d, tc.ceiling); got != tc.want {
			t.Fatalf("nextBackoffCap(%v, %v) = %v, want %v", tc.d, tc.ceiling, got, tc.want)
		}
	}
}

func TestParseResourceHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"tunnel.example:9001", "tunnel.example", 9001, false},
		{"[::1]:9001", "[::1]", 9001, false},
		{"10.0.0.1:80", "10.0.0.1", 80, false},
		{"tunnel.example", "", 0, true},
		{"::1", "", 0, true},
		{"tunnel.example:0", "", 0, true},
		{"tunnel.example:65536", "", 0, true},
		{":9001", "", 0, true},
	}
	for _, tc := range cases {
		host, port, err := ParseResourceHost(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseResourceHost(%q) = %q:%d, want error", tc.in, host, port)
			}
			continue
		}
		if err != nil || host != tc.wantHost || port != tc.wantPort {
			t.Fatalf("ParseResourceHost(%q) = %q:%d err %v, want %q:%d", tc.in, host, port, err, tc.wantHost, tc.wantPort)
		}
	}
}

func TestTLSEnabledAndIPLiteralHost(t *testing.T) {
	t.Parallel()
	enabled, disabled := true, false
	tlsOn := commonForTest()
	tlsOn.Transport.TLS.Enable = &enabled
	tlsOff := commonForTest()
	tlsOff.Transport.TLS.Enable = &disabled
	quic := commonForTest()
	quic.Transport.Protocol = "quic"
	wss := commonForTest()
	wss.Transport.Protocol = "wss"
	if !TLSEnabled(tlsOn) || TLSEnabled(tlsOff) || !TLSEnabled(quic) || !TLSEnabled(wss) || TLSEnabled(nil) || TLSEnabled(commonForTest()) {
		t.Fatal("TLSEnabled decision table drifted from the pinned FRP dialer semantics")
	}
	for host, want := range map[string]bool{
		"10.0.0.1": true, "[::1]": true, "::1": true,
		"tunnel.example": false, "[abc]": false, "": false,
	} {
		if got := IsIPLiteralHost(host); got != want {
			t.Fatalf("IsIPLiteralHost(%q) = %v, want %v", host, got, want)
		}
	}
}
