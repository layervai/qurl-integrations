package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
)

// loginHealthyAfter is the wall-clock threshold past which a completed cycle
// is treated as having had a real session. The FRP runtime returns an opaque
// error class at the runner boundary, so run duration separates "the dial
// never got anywhere" from "a session existed and then ended". Whether the
// Login itself was ADMITTED is never guessed from duration: the pinned FRP
// fork's OnFirstLoginSuccess hook is the evidence, and the session-summary
// events below gate on it.
const loginHealthyAfter = 2 * time.Second

// serviceRunner is the slice of the FRP client service the cycle runner
// wraps; tests substitute fakes.
type serviceRunner interface {
	Run(ctx context.Context) error
	GracefulClose(d time.Duration)
}

// cycleRunner wraps one cycle's FRP client service with the Connector's
// session semantics: the caller-owned cycle RunID is bound to the Login and
// verified on admission, an in-run restart request (from the redial knock
// refresher's budget) cancels the service and surfaces its cause, and the
// cycle's outcome is emitted as one set of structured session events.
type cycleRunner struct {
	svc serviceRunner

	// resourceID stamps every session event from this runner.
	resourceID string

	// cycleRunID is the caller-owned native cycle RunID presented on the
	// first Login. onFirstLoginSuccess compares the RunID the server
	// accepted against this value, so a server that silently reassigns one
	// cannot keep the session.
	cycleRunID string

	// admitted latches when the FRP fork's OnFirstLoginSuccess hook fires —
	// the runner's only evidence-based "the server admitted us" signal.
	admitted atomic.Bool

	logger *slog.Logger

	cancelMu sync.Mutex
	cancel   context.CancelFunc
	err      error
}

// onFirstLoginSuccess is the FRP fork's authenticated-admission callback. The
// fork invokes it synchronously with the RunID the server returned, after the
// Login was accepted and BEFORE any proxy registration. The accepted RunID
// must be canonical and byte-identical to the cycle RunID this runner
// presented — the RunID-to-session binding is what the whole knock-then-login
// correlation rests on — so a mismatch rejects the accepted session.
//
// TODO(upstream-contract): mirrors github.com/layervai/frp v0.70.0-layerv.4
// client/service.go — ServiceOptions.OnFirstLoginSuccess, dispatched by
// runFirstLoginSuccessHook as `svr.onFirstLoginSuccess(svr.runID)` from
// inside loopLoginUntilSuccess's loginFunc: synchronously, after
// `svr.runID = sessionCtx.RunID` (the server's LoginResp.RunID, forwarded
// verbatim — the fork validates nothing, which is why the canonical-RunID
// check below is this side's job) and before both NewControl and
// `ctl.Run(proxyCfgs, visitorCfgs)`, which is the proxy and visitor
// registration. A returned error closes the session context, cancels the
// service, and comes back out of Run wrapped in a *firstLoginSuccessHookError
// whose message is fixed and whose Unwrap carries this cause. A sync.Once
// guards the dispatch, so it fires at most once per Service and a later
// internal reconnect reuses that admission without re-running this check;
// the factory NewFRPRunnerFactory returns builds one Service per cycle, which
// is what keeps the admitted latch above a per-cycle signal. If a fork bump
// moves the dispatch after registration, drops the Once, or stops honoring
// the error, update this in lockstep.
func (r *cycleRunner) onFirstLoginSuccess(runID string) error {
	if err := qurl.ValidateCycleRunID(runID); err != nil {
		return fmt.Errorf("tunnel server accepted Login with a noncanonical RunID: %w", err)
	}
	if runID != r.cycleRunID {
		// Neither value is logged: the mismatch itself is the finding, and
		// the accepted RunID is server-controlled input.
		return errors.New("tunnel server accepted Login under a different RunID than this cycle presented; refusing the session")
	}
	r.admitted.Store(true)
	r.log().Info("connector: tunnel login admitted",
		"event", "login_success",
		"resource_id", r.resourceID,
		"run_id", runID,
	)
	return nil
}

// Run executes the wrapped service under a cancelable per-cycle context, then
// emits the cycle's session events. A restart requested mid-run (refresher
// budget exhaustion) wins over the service's own return value: the restart
// signal means the current admission window is no longer trusted.
func (r *cycleRunner) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.cancelMu.Lock()
	r.cancel = cancel
	r.cancelMu.Unlock()

	start := time.Now()
	err := r.svc.Run(runCtx)
	runDuration := time.Since(start)

	r.cancelMu.Lock()
	r.cancel = nil
	restartErr := r.err
	r.cancelMu.Unlock()

	r.emitSessionEvents(ctx, runDuration, err, restartErr)
	if restartErr != nil {
		return restartErr
	}
	return err
}

// GracefulClose cancels any in-flight run and closes the wrapped service.
func (r *cycleRunner) GracefulClose(d time.Duration) {
	r.cancelMu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.cancelMu.Unlock()
	r.svc.GracefulClose(d)
}

// requestRestart records the first restart cause and cancels the in-flight
// run. Used by the redial knock refresher when its in-run budget exhausts.
func (r *cycleRunner) requestRestart(err error) {
	if err == nil {
		err = errors.New("connector requested restart")
	}
	r.cancelMu.Lock()
	if r.err == nil {
		r.err = err
	}
	if r.cancel != nil {
		r.cancel()
	}
	r.cancelMu.Unlock()
}

// emitSessionEvents translates one cycle's outcome into the session event
// vocabulary (login_deny / login_error / proxy_allow / teardown), the same
// decision taxonomy the standalone Connector's audit stream uses, emitted
// here as structured logs. Rules, most-specific first:
//
//   - token-rejected Login → login_deny only (no session to tear down);
//   - restart requested (any duration) → the session existed: proxy_allow if
//     admitted, plus an error teardown carrying the restart cause;
//   - short cycle with an error → login_error only (Login never completed);
//   - short cycle, no error → teardown(pre_login_cancel): the caller
//     canceled before Login completed, recorded so triage is never blind;
//   - long cycle → proxy_allow (only with admission evidence) + teardown,
//     bucketed clean or errored.
//
// login_success is deliberately NOT emitted here: onFirstLoginSuccess already
// emitted it from admission evidence, strictly before any proxy could start.
func (r *cycleRunner) emitSessionEvents(ctx context.Context, runDuration time.Duration, runErr, restartErr error) {
	latencyMS := float64(runDuration.Microseconds()) / 1000.0
	if IsTokenLoginError(runErr) {
		r.log().Warn("connector: tunnel login denied",
			"event", "login_deny",
			"resource_id", r.resourceID,
			"reason", "token_rejected",
			"err", runErr.Error(),
			"run_id", r.cycleRunID,
			"latency_ms", latencyMS,
		)
		return
	}
	healthy := runDuration >= loginHealthyAfter || restartErr != nil
	if !healthy {
		if runErr != nil {
			r.log().Warn("connector: tunnel login failed",
				"event", "login_error",
				"resource_id", r.resourceID,
				"reason", classifyRunError(runErr),
				"err", runErr.Error(),
				"run_id", r.cycleRunID,
				"latency_ms", latencyMS,
			)
			return
		}
		r.log().Info("connector: tunnel cycle canceled before login completed",
			"event", "teardown",
			"resource_id", r.resourceID,
			"reason", "pre_login_cancel",
			"run_id", r.cycleRunID,
			"latency_ms", latencyMS,
		)
		return
	}
	if r.admitted.Load() {
		r.log().Info("connector: tunnel session served",
			"event", "proxy_allow",
			"resource_id", r.resourceID,
			"run_id", r.cycleRunID,
		)
	}
	reason, errText := r.teardownCause(ctx, runErr, restartErr)
	attrs := []any{
		"event", "teardown",
		"resource_id", r.resourceID,
		"run_id", r.cycleRunID,
		"latency_ms", latencyMS,
	}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	if errText != "" {
		attrs = append(attrs, "err", errText)
		r.log().Warn("connector: tunnel session ended with an error", attrs...)
		return
	}
	r.log().Info("connector: tunnel session ended", attrs...)
}

// teardownCause buckets a healthy cycle's exit for the teardown event. A
// clean exit keeps an empty reason so dashboards split graceful from errored
// on one axis; caller cancellation is tagged without being an error.
func (r *cycleRunner) teardownCause(ctx context.Context, runErr, restartErr error) (reason, errText string) {
	switch {
	case restartErr != nil:
		return "connector_restart", restartErr.Error()
	case runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded):
		return classifyRunError(runErr), runErr.Error()
	default:
		if ctx != nil && ctx.Err() != nil {
			return "context_canceled", ""
		}
		return "", ""
	}
}

func (r *cycleRunner) log() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return slog.Default()
}
