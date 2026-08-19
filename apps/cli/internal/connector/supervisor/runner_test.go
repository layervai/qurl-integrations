package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	frpproxy "github.com/fatedier/frp/client/proxy"
	qurl "github.com/layervai/qurl-go/qurl"
)

// fakeService is the serviceRunner behind cycleRunner tests.
type fakeService struct {
	runErr   error
	block    bool
	closed   atomic.Bool
	runCtx   atomic.Pointer[context.Context]
	returned chan struct{}
}

func (s *fakeService) Run(ctx context.Context) error {
	s.runCtx.Store(&ctx)
	if s.returned != nil {
		defer close(s.returned)
	}
	if s.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.runErr
}

func (s *fakeService) GracefulClose(time.Duration) { s.closed.Store(true) }

type fakeProxyStatusExporter struct {
	mu     sync.RWMutex
	status map[string]*frpproxy.WorkingStatus
}

func (e *fakeProxyStatusExporter) GetProxyStatus(name string) (*frpproxy.WorkingStatus, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	status, ok := e.status[name]
	if !ok || status == nil {
		return nil, ok
	}
	statusCopy := *status
	return &statusCopy, true
}

func (e *fakeProxyStatusExporter) set(name, phase string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.status == nil {
		e.status = make(map[string]*frpproxy.WorkingStatus)
	}
	e.status[name] = &frpproxy.WorkingStatus{Name: name, Phase: phase}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// testCycleRunID returns a canonical native cycle RunID so the runner's
// admission validation exercises the real qurl-go canonical form.
func testCycleRunID(t *testing.T) string {
	t.Helper()
	runID, err := qurl.NewCycleRunID()
	if err != nil {
		t.Fatal(err)
	}
	return runID
}

// TestRunnerCancelsCycleContextAfterOrdinaryReturn: the per-cycle context the
// runner hands the service must be canceled once Run returns, so nothing
// started under it can outlive the cycle.
func TestRunnerCancelsCycleContextAfterOrdinaryReturn(t *testing.T) {
	t.Parallel()
	svc := &fakeService{}
	r := &cycleRunner{svc: svc, resourceID: testResource, cycleRunID: testCycleRunID(t), logger: discardLogger()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := svc.runCtx.Load()
	if got == nil {
		t.Fatal("service never ran")
		return
	}
	if (*got).Err() == nil {
		t.Fatal("cycle context still live after Run returned; cycle-scoped goroutines could leak")
	}
}

// TestRunnerRestartRequestWinsOverServiceReturn: a restart requested mid-run
// cancels the service and its cause becomes the cycle's result even when the
// service itself returned the cancellation.
func TestRunnerRestartRequestWinsOverServiceReturn(t *testing.T) {
	t.Parallel()
	svc := &fakeService{block: true, returned: make(chan struct{})}
	r := &cycleRunner{svc: svc, resourceID: testResource, cycleRunID: testCycleRunID(t), logger: discardLogger()}
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	// Wait until the service observed its context, then request the restart.
	deadline := time.Now().Add(5 * time.Second)
	for svc.runCtx.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("service never started")
		}
		time.Sleep(time.Millisecond)
	}
	restartCause := fmt.Errorf("%w: redial refresh budget", ErrTooManyKnockFailures)
	r.requestRestart(restartCause)
	if err := <-done; !errors.Is(err, ErrTooManyKnockFailures) {
		t.Fatalf("Run = %v, want the restart cause", err)
	}
}

// TestRunnerGracefulCloseCancelsInFlightRun: GracefulClose must cancel a
// blocked service run and close the wrapped service.
func TestRunnerGracefulCloseCancelsInFlightRun(t *testing.T) {
	t.Parallel()
	svc := &fakeService{block: true}
	r := &cycleRunner{svc: svc, resourceID: testResource, cycleRunID: testCycleRunID(t), logger: discardLogger()}
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for svc.runCtx.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("service never started")
		}
		time.Sleep(time.Millisecond)
	}
	r.GracefulClose(time.Second)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want cancellation via GracefulClose", err)
	}
	if !svc.closed.Load() {
		t.Fatal("wrapped service was not closed")
	}
}

// TestOnFirstLoginSuccessBindsRunID pins the RunID→Login binding: the exact
// presented RunID is admitted; a noncanonical or different RunID rejects the
// session and never latches admission.
func TestOnFirstLoginSuccessBindsRunID(t *testing.T) {
	t.Parallel()
	runID := testCycleRunID(t)
	otherRunID := testCycleRunID(t)
	cases := []struct {
		name     string
		accepted string
		wantErr  string
	}{
		{"exact match admitted", runID, ""},
		{"different canonical RunID refused", otherRunID, "different RunID"},
		{"noncanonical RunID refused", "not-a-run-id", "noncanonical RunID"},
		{"empty RunID refused", "", "noncanonical RunID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &cycleRunner{resourceID: testResource, cycleRunID: runID, logger: discardLogger()}
			err := r.onFirstLoginSuccess(tc.accepted)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("onFirstLoginSuccess = %v, want admitted", err)
				}
				if !r.admitted.Load() {
					t.Fatal("admission evidence not latched")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("onFirstLoginSuccess = %v, want %q", err, tc.wantErr)
			}
			if r.admitted.Load() {
				t.Fatal("admission latched for a refused session")
			}
		})
	}
}

// TestOnFirstLoginSuccessSignalsAdmissionButNotServing pins the seam that
// motivated the readiness monitor: exact Login acceptance wakes the monitor,
// but cannot itself fire the customer-visible proxy-ready callback.
func TestOnFirstLoginSuccessSignalsAdmissionButNotServing(t *testing.T) {
	t.Parallel()
	runID := testCycleRunID(t)
	var notifications atomic.Int32
	r := &cycleRunner{
		resourceID:    testResource,
		cycleRunID:    runID,
		logger:        discardLogger(),
		loginAccepted: make(chan struct{}),
		onProxyReady:  func() { notifications.Add(1) },
	}

	if err := r.onFirstLoginSuccess(testCycleRunID(t)); err == nil {
		t.Fatal("mismatched RunID admitted")
	}
	if err := r.onFirstLoginSuccess("not-a-run-id"); err == nil {
		t.Fatal("noncanonical RunID admitted")
	}
	if got := notifications.Load(); got != 0 {
		t.Fatalf("readiness notifications after refused Logins = %d, want 0", got)
	}

	if err := r.onFirstLoginSuccess(runID); err != nil {
		t.Fatalf("exact RunID admission: %v", err)
	}
	select {
	case <-r.loginAccepted:
	default:
		t.Fatal("exact Login did not wake the readiness monitor")
	}
	if got := notifications.Load(); got != 0 {
		t.Fatalf("proxy-ready notifications at Login admission = %d, want 0", got)
	}
}

// TestProxyReadyWaitsForRunningPhase proves the monitor emits nothing for an
// admitted-but-waiting route and fires only after StatusExporter reports the
// exact configured proxy as running.
func TestProxyReadyWaitsForRunningPhase(t *testing.T) {
	t.Parallel()
	runID := testCycleRunID(t)
	exporter := &fakeProxyStatusExporter{}
	exporter.set("route-a", frpproxy.ProxyPhaseWaitStart)
	ready := make(chan struct{}, 1)
	r := &cycleRunner{
		resourceID:     testResource,
		cycleRunID:     runID,
		logger:         discardLogger(),
		proxyNames:     []string{"route-a"},
		statusExporter: exporter,
		readyTimeout:   time.Second,
		loginAccepted:  make(chan struct{}),
		onProxyReady:   func() { ready <- struct{}{} },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.monitorProxyReadiness(ctx)
		close(done)
	}()
	if err := r.onFirstLoginSuccess(runID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
		t.Fatal("proxy-ready fired while the route was still waiting for NewProxy")
	case <-time.After(3 * proxyReadyPollInterval):
	}
	exporter.set("route-a", frpproxy.ProxyPhaseRunning)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("proxy-ready did not fire after the route became running")
	}
	<-done
	if !r.serving.Load() {
		t.Fatal("serving evidence was not latched before the callback")
	}
}

// TestProxyReadyCallbackCanBeOnceGuardedAcrossRunners models the supervisor's
// reconnect shape: each fresh cycle can observe a running proxy, while a
// command can reduce those observations to one customer-visible result.
func TestProxyReadyCallbackCanBeOnceGuardedAcrossRunners(t *testing.T) {
	t.Parallel()
	var once sync.Once
	var notifications atomic.Int32
	callback := func() {
		once.Do(func() { notifications.Add(1) })
	}

	for range 2 {
		runID := testCycleRunID(t)
		exporter := &fakeProxyStatusExporter{}
		exporter.set("route-a", frpproxy.ProxyPhaseRunning)
		r := &cycleRunner{
			resourceID:     testResource,
			cycleRunID:     runID,
			logger:         discardLogger(),
			proxyNames:     []string{"route-a"},
			statusExporter: exporter,
			readyTimeout:   time.Second,
			loginAccepted:  make(chan struct{}),
			onProxyReady:   callback,
		}
		if err := r.onFirstLoginSuccess(runID); err != nil {
			t.Fatalf("admit reconnect cycle: %v", err)
		}
		r.monitorProxyReadiness(context.Background())
	}

	if got := notifications.Load(); got != 1 {
		t.Fatalf("once-guarded readiness notifications = %d, want 1", got)
	}
}

func TestProxyRegistrationRejectionCancelsRun(t *testing.T) {
	t.Parallel()
	runID := testCycleRunID(t)
	exporter := &fakeProxyStatusExporter{}
	exporter.set("route-a", frpproxy.ProxyPhaseStartErr)
	svc := &fakeService{block: true}
	var notifications atomic.Int32
	r := &cycleRunner{
		svc:            svc,
		resourceID:     testResource,
		cycleRunID:     runID,
		logger:         discardLogger(),
		proxyNames:     []string{"route-a"},
		statusExporter: exporter,
		readyTimeout:   time.Second,
		loginAccepted:  make(chan struct{}),
		onProxyReady:   func() { notifications.Add(1) },
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	waitForServiceStart(t, svc)
	if err := r.onFirstLoginSuccess(runID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrProxyNotServing) {
			t.Fatalf("Run = %v, want ErrProxyNotServing", err)
		}
	case <-time.After(time.Second):
		t.Fatal("explicit NewProxy rejection did not stop the cycle promptly")
	}
	if got := notifications.Load(); got != 0 {
		t.Fatalf("proxy-ready notifications after rejection = %d, want 0", got)
	}
}

func TestProxyReadinessTimeoutCancelsRun(t *testing.T) {
	t.Parallel()
	runID := testCycleRunID(t)
	exporter := &fakeProxyStatusExporter{}
	exporter.set("route-a", frpproxy.ProxyPhaseWaitStart)
	svc := &fakeService{block: true}
	r := &cycleRunner{
		svc:            svc,
		resourceID:     testResource,
		cycleRunID:     runID,
		logger:         discardLogger(),
		proxyNames:     []string{"route-a"},
		statusExporter: exporter,
		readyTimeout:   50 * time.Millisecond,
		loginAccepted:  make(chan struct{}),
		onProxyReady:   func() {},
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	waitForServiceStart(t, svc)
	if err := r.onFirstLoginSuccess(runID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrProxyNotServing) {
			t.Fatalf("Run = %v, want bounded ErrProxyNotServing", err)
		}
	case <-time.After(time.Second):
		t.Fatal("missing NewProxy response exceeded the readiness deadline")
	}
}

// TestAdvancedRunnerWithoutProxyReadyPreservesAdmissionReconnectAndLogs is a
// compatibility regression for qurl connector run. Even if readiness state is
// present, a nil callback must not opt that advanced surface into local
// publish's terminal ProxyPhaseRunning gate. Admission remains proxy_allow
// evidence, and the established redial restart cause wins unchanged.
func TestAdvancedRunnerWithoutProxyReadyPreservesAdmissionReconnectAndLogs(t *testing.T) {
	t.Parallel()
	runID := testCycleRunID(t)
	exporter := &fakeProxyStatusExporter{}
	exporter.set("route-a", frpproxy.ProxyPhaseStartErr)
	svc := &fakeService{block: true}
	var buf bytes.Buffer
	r := &cycleRunner{
		svc:            svc,
		resourceID:     testResource,
		cycleRunID:     runID,
		logger:         slog.New(slog.NewJSONHandler(&buf, nil)),
		proxyNames:     []string{"route-a"},
		statusExporter: exporter,
		readyTimeout:   time.Millisecond,
		loginAccepted:  make(chan struct{}),
		// onProxyReady deliberately remains nil: this is the advanced path.
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	waitForServiceStart(t, svc)
	if err := r.onFirstLoginSuccess(runID); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		t.Fatalf("advanced runner stopped under local-publish readiness policy: %v", err)
	case <-time.After(4 * proxyReadyPollInterval):
	}

	restartCause := fmt.Errorf("%w: redial refresh budget", ErrTooManyKnockFailures)
	r.requestRestart(restartCause)
	if err := <-done; !errors.Is(err, ErrTooManyKnockFailures) {
		t.Fatalf("Run = %v, want the advanced path's restart cause", err)
	}

	var events []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line != "" {
			events = append(events, jsonField(t, line, `"event":"`))
		}
	}
	wantEvents := []string{"login_success", "proxy_allow", "teardown"}
	if fmt.Sprint(events) != fmt.Sprint(wantEvents) {
		t.Fatalf("events = %v, want %v\nlog:\n%s", events, wantEvents, buf.String())
	}
}

// TestProxyFailureAfterPriorServingPreservesReconnectBehavior proves the
// terminal gate is initial-only across fresh supervised runners. Once local
// publish has emitted a serving result, a later NewProxy rejection must not
// terminate with the contradictory "nothing was published" error.
func TestProxyFailureAfterPriorServingPreservesReconnectBehavior(t *testing.T) {
	t.Parallel()
	var readyEver atomic.Bool
	var notifications atomic.Int32

	firstRunID := testCycleRunID(t)
	firstExporter := &fakeProxyStatusExporter{}
	firstExporter.set("route-a", frpproxy.ProxyPhaseRunning)
	first := &cycleRunner{
		resourceID:     testResource,
		cycleRunID:     firstRunID,
		logger:         discardLogger(),
		proxyNames:     []string{"route-a"},
		statusExporter: firstExporter,
		readyTimeout:   time.Second,
		loginAccepted:  make(chan struct{}),
		proxyReadyEver: &readyEver,
		onProxyReady:   func() { notifications.Add(1) },
	}
	if err := first.onFirstLoginSuccess(firstRunID); err != nil {
		t.Fatal(err)
	}
	first.monitorProxyReadiness(context.Background())
	if !readyEver.Load() || notifications.Load() != 1 {
		t.Fatalf("first serving cycle did not latch shared readiness: ready=%t callbacks=%d", readyEver.Load(), notifications.Load())
	}

	secondRunID := testCycleRunID(t)
	secondExporter := &fakeProxyStatusExporter{}
	secondExporter.set("route-a", frpproxy.ProxyPhaseStartErr)
	svc := &fakeService{block: true}
	second := &cycleRunner{
		svc:            svc,
		resourceID:     testResource,
		cycleRunID:     secondRunID,
		logger:         discardLogger(),
		proxyNames:     []string{"route-a"},
		statusExporter: secondExporter,
		readyTimeout:   time.Millisecond,
		loginAccepted:  make(chan struct{}),
		proxyReadyEver: &readyEver,
		onProxyReady:   func() { notifications.Add(1) },
	}
	done := make(chan error, 1)
	go func() { done <- second.Run(context.Background()) }()
	waitForServiceStart(t, svc)
	if err := second.onFirstLoginSuccess(secondRunID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("post-success proxy rejection terminated the reconnect: %v", err)
	case <-time.After(4 * proxyReadyPollInterval):
	}
	if got := notifications.Load(); got != 1 {
		t.Fatalf("proxy-ready callbacks = %d, want only the serving cycle", got)
	}

	restartCause := fmt.Errorf("%w: redial refresh budget", ErrTooManyKnockFailures)
	second.requestRestart(restartCause)
	if err := <-done; !errors.Is(err, ErrTooManyKnockFailures) {
		t.Fatalf("Run = %v, want normal reconnect restart cause", err)
	}
}

func TestProxyReadinessTreatsClosedAsTerminalBeforeReady(t *testing.T) {
	t.Parallel()
	exporter := &fakeProxyStatusExporter{}
	exporter.set("route-a", frpproxy.ProxyPhaseClosed)
	r := &cycleRunner{
		proxyNames:     []string{"route-a"},
		statusExporter: exporter,
	}
	ready, err := r.proxyReadiness()
	if ready || !errors.Is(err, ErrProxyNotServing) {
		t.Fatalf("proxyReadiness = (%v, %v), want terminal ErrProxyNotServing", ready, err)
	}
}

func TestProxyReadinessCancellationDoesNotLeakOrNotify(t *testing.T) {
	t.Parallel()
	runID := testCycleRunID(t)
	exporter := &fakeProxyStatusExporter{}
	exporter.set("route-a", frpproxy.ProxyPhaseWaitStart)
	svc := &fakeService{block: true}
	var notifications atomic.Int32
	r := &cycleRunner{
		svc:            svc,
		resourceID:     testResource,
		cycleRunID:     runID,
		logger:         discardLogger(),
		proxyNames:     []string{"route-a"},
		statusExporter: exporter,
		readyTimeout:   time.Second,
		loginAccepted:  make(chan struct{}),
		onProxyReady:   func() { notifications.Add(1) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitForServiceStart(t, svc)
	if err := r.onFirstLoginSuccess(runID); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not join its readiness monitor after cancellation")
	}
	if got := notifications.Load(); got != 0 {
		t.Fatalf("proxy-ready notifications after cancellation = %d, want 0", got)
	}
}

func waitForServiceStart(t *testing.T, svc *fakeService) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for svc.runCtx.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("service never started")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestOnFirstLoginSuccessMismatchLogsNeitherValue: the accepted RunID is
// server-controlled input and the presented one is the finding's context —
// the refusal must not echo either.
func TestOnFirstLoginSuccessMismatchLogsNeitherValue(t *testing.T) {
	t.Parallel()
	presented := testCycleRunID(t)
	accepted := testCycleRunID(t)
	r := &cycleRunner{resourceID: testResource, cycleRunID: presented, logger: discardLogger()}
	err := r.onFirstLoginSuccess(accepted)
	if err == nil {
		t.Fatal("mismatched RunID admitted")
	}
	if strings.Contains(err.Error(), presented) || strings.Contains(err.Error(), accepted) {
		t.Fatalf("refusal %q echoes a RunID value", err)
	}
}

// TestEmitSessionEventsDecisionTable pins the session-event mapping for one
// cycle: which events fire, under which reason, for each outcome class.
func TestEmitSessionEventsDecisionTable(t *testing.T) {
	tokenReject := errors.New("login to the server failed: knock_invalid: knock token rejected")
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	restart := fmt.Errorf("%w: refresh budget", ErrTooManyKnockFailures)
	cases := []struct {
		name        string
		admitted    bool
		serving     bool
		strictReady bool
		duration    time.Duration
		runErr      error
		restartErr  error
		wantEvents  []string
		wantReasons []string
	}{
		{
			name: "token rejected is a deny only", duration: 10 * time.Second, runErr: tokenReject,
			wantEvents: []string{"login_deny"}, wantReasons: []string{"token_rejected"},
		},
		{
			name: "short error cycle is a login error", duration: 50 * time.Millisecond, runErr: dialErr,
			wantEvents: []string{"login_error"}, wantReasons: []string{"dial_error"},
		},
		{
			name: "short clean cycle records the cancel", duration: 50 * time.Millisecond,
			wantEvents: []string{"teardown"}, wantReasons: []string{"pre_login_cancel"},
		},
		{
			name: "strict healthy serving clean cycle", admitted: true, serving: true, strictReady: true, duration: 10 * time.Second,
			wantEvents: []string{"proxy_allow", "teardown"}, wantReasons: []string{"", ""},
		},
		{
			name: "strict admitted but nonserving cycle has no proxy_allow", admitted: true, strictReady: true, duration: 10 * time.Second,
			wantEvents: []string{"teardown"}, wantReasons: []string{""},
		},
		{
			name: "advanced admitted cycle preserves proxy_allow", admitted: true, duration: 10 * time.Second,
			wantEvents: []string{"proxy_allow", "teardown"}, wantReasons: []string{"", ""},
		},
		{
			name: "restart request is error teardown regardless of duration", admitted: true, serving: true, strictReady: true,
			duration: 50 * time.Millisecond, restartErr: restart,
			wantEvents: []string{"proxy_allow", "teardown"}, wantReasons: []string{"", "connector_restart"},
		},
		{
			name: "proxy readiness failure never claims proxy_allow", admitted: true, strictReady: true, duration: 50 * time.Millisecond,
			restartErr: ErrProxyNotServing,
			wantEvents: []string{"teardown"}, wantReasons: []string{"proxy_not_serving"},
		},
		{
			name: "healthy errored cycle buckets the cause", admitted: true, serving: true, strictReady: true, duration: 10 * time.Second, runErr: dialErr,
			wantEvents: []string{"proxy_allow", "teardown"}, wantReasons: []string{"", "dial_error"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			r := &cycleRunner{resourceID: testResource, cycleRunID: "run-1", logger: logger}
			if tc.strictReady {
				r.onProxyReady = func() {}
			}
			r.admitted.Store(tc.admitted)
			r.serving.Store(tc.serving)
			r.emitSessionEvents(context.Background(), tc.duration, tc.runErr, tc.restartErr)

			var events, reasons []string
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				if line == "" {
					continue
				}
				events = append(events, jsonField(t, line, `"event":"`))
				reasons = append(reasons, jsonField(t, line, `"reason":"`))
			}
			if fmt.Sprint(events) != fmt.Sprint(tc.wantEvents) {
				t.Fatalf("events = %v, want %v\nlog:\n%s", events, tc.wantEvents, buf.String())
			}
			if fmt.Sprint(reasons) != fmt.Sprint(tc.wantReasons) {
				t.Fatalf("reasons = %v, want %v\nlog:\n%s", reasons, tc.wantReasons, buf.String())
			}
		})
	}
}

// jsonField extracts a quoted JSON string field with plain string slicing —
// enough for the flat slog JSON lines these tests emit. "" when absent.
func jsonField(t *testing.T, line, prefix string) string {
	t.Helper()
	i := strings.Index(line, prefix)
	if i < 0 {
		return ""
	}
	rest := line[i+len(prefix):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("unterminated field %q in %q", prefix, line)
	}
	return rest[:j]
}

// TestClassifyRunError pins the reason vocabulary.
func TestClassifyRunError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"canceled", context.Canceled, "context_canceled"},
		{"deadline", context.DeadlineExceeded, "context_canceled"},
		{"knock budget", fmt.Errorf("wrapped: %w", ErrTooManyKnockFailures), "too_many_knock_failures"},
		{"typed op error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, "dial_error"},
		{"typed timeout", wrappedTimeoutErr{}, "dial_error"},
		{"frp login phrasing wins over dial substring", errors.New("login to the server failed: dial tcp 10.0.0.1:7000: i/o timeout"), "login_failed"},
		{"identity-stripped io timeout", errors.New("work conn: i/o timeout"), "dial_error"},
		{"identity-stripped refused", errors.New("proxy: connection refused"), "dial_error"},
		{"unclassified", errors.New("session storage full"), "frp_runtime_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyRunError(tc.err); got != tc.want {
				t.Fatalf("classifyRunError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

type wrappedTimeoutErr struct{}

func (wrappedTimeoutErr) Error() string   { return "operation stalled" }
func (wrappedTimeoutErr) Timeout() bool   { return true }
func (wrappedTimeoutErr) Temporary() bool { return false }

// TestMain silences the default logger for any code path that falls back to
// slog.Default, keeping suite output to real failures.
//
// It silences one non-slog line too. TestForkDialsQUICFromOpen makes a real
// QUIC dial, and quic-go warns through the stdlib log package when it cannot
// raise the socket buffers to its 7MB target — which it cannot on a stock
// Linux runner, where net.core.rmem_max is orders of magnitude smaller and
// the process lacks CAP_NET_ADMIN for SO_RCVBUFFORCE. The knob has to be set
// here rather than with t.Setenv, which panics in a test that calls
// t.Parallel.
//
// Setting it before m.Run is early enough only because quic-go reads it on
// the connection-setup path (wrapConn, under a sync.Once) rather than in an
// init, so a bump that moved the read into package init would un-silence the
// line. Cosmetic either way: it is a log line, never a failure.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	// Errors are impossible on a literal key/value and there is nothing to
	// fall back to if it ever did fail; the only cost is a cosmetic log line.
	_ = os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	os.Exit(m.Run())
}
