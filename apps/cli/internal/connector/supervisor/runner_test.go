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
	"sync/atomic"
	"testing"
	"time"

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
			name: "healthy admitted clean cycle", admitted: true, duration: 10 * time.Second,
			wantEvents: []string{"proxy_allow", "teardown"}, wantReasons: []string{"", ""},
		},
		{
			name: "healthy unadmitted cycle has no proxy_allow", duration: 10 * time.Second,
			wantEvents: []string{"teardown"}, wantReasons: []string{""},
		},
		{
			name: "restart request is error teardown regardless of duration", admitted: true,
			duration: 50 * time.Millisecond, restartErr: restart,
			wantEvents: []string{"proxy_allow", "teardown"}, wantReasons: []string{"", "connector_restart"},
		},
		{
			name: "healthy errored cycle buckets the cause", admitted: true, duration: 10 * time.Second, runErr: dialErr,
			wantEvents: []string{"proxy_allow", "teardown"}, wantReasons: []string{"", "dial_error"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			r := &cycleRunner{resourceID: testResource, cycleRunID: "run-1", logger: logger}
			r.admitted.Store(tc.admitted)
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
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}
