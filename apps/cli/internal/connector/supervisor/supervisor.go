// Package supervisor is the qURL Connector's knock-then-login serve loop: it
// wraps the FRP client service with a managed-restart cycle in which every
// dial is preceded by a fresh NHP knock against the assigned admission
// controller.
//
// Lifecycle of one supervised Connector session:
//
//	for ctx.Err() == nil:
//	    begin a native cycle (fresh caller-owned RunID)
//	    knock and consume the ACK's token + authoritative dial target
//	    overlay both onto a per-cycle copy of the FRP common config
//	    run the FRP client service (Login bound to the cycle RunID)
//	    when Run returns (disconnect, transient error, or shutdown):
//	        best-effort native session exit, then backoff and repeat
//
// The loop is knock-only by design: there is no static dial target and no
// DNS-discovery fallback — the NHP ACK is the sole source of truth for where
// to dial, and every failure to obtain a usable admission fails closed.
//
// Failure budget: consecutive unhealthy knock cycles (transport error, ACK
// without a usable token or dial target, or a token-rejected Login) share ONE
// counter; exhausting it exits with errTooManyKnockFailures and arms the
// state package's refresh marker, which hands recovery to the agent package's
// operator-gated assignment-refresh ladder on the next start. A confirmed
// healthy knock+login cycle clears the marker and ends the episode.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/frpgen"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/knock"
)

const (
	// minBackoff is the initial sleep between supervised reconnect cycles:
	// tight enough to recover from a transient blip, loose enough not to
	// hot-loop a misconfigured Connector.
	minBackoff = 1 * time.Second
	// maxBackoff caps the exponential backoff, so a sustained outage
	// produces roughly one retry per 30s — visible without spamming.
	maxBackoff = 30 * time.Second
	// healthyRunThreshold is how long a single tunnel cycle must run before
	// the supervisor resets the reconnect backoff to minBackoff: a
	// long-stable connection that drops once must not pay the 30s cap, while
	// a flapping one that briefly establishes cannot game the reset.
	healthyRunThreshold = 5 * time.Minute
	// gracefulCloseTimeout is how long the supervisor lets the FRP service
	// tear down between cycles, sized so its internal listeners release
	// cleanly before the next cycle rebinds.
	gracefulCloseTimeout = 5 * time.Second
	// minKnockInterval is the minimum time between two consecutive knock
	// attempts (start-to-start), the debounce that caps the knock storm a
	// flapping login could otherwise produce against the admission
	// controller. It is far below the server-side admission open-time, so
	// gated re-knocks still land inside the prior admission window.
	minKnockInterval = 10 * time.Second
	// maxConsecutiveKnockFailures is the exit threshold: this many
	// consecutive unhealthy knock cycles return errTooManyKnockFailures and
	// arm the refresh marker. The orchestrator's restart is the recovery
	// mechanism — persisted native state survives, and the next start walks
	// the agent package's refresh ladder.
	maxConsecutiveKnockFailures = 5
	// endCycleTimeout bounds the best-effort native session exit with a
	// context independent of the (commonly canceled) cycle context.
	endCycleTimeout = 3 * time.Second
	// refreshMarkerReason is the episode reason recorded when the knock
	// budget exhausts. Shared spelling with the standalone qURL Connector so
	// a state directory keeps its meaning across the two tools.
	refreshMarkerReason = "sustained native NHP knock failures"
)

// Structured-log event names for knock-cycle outcomes. Grep-stable
// identifiers shared with the knock package's decision-stream vocabulary.
const (
	eventKnockGateWait = "knock_gate_wait"
	eventKnockOK       = "knock_ok"
	eventKnockFail     = "knock_fail"
)

// FRPRunner is the smallest subset of the FRP client service the supervisor
// drives per cycle. The seam exists so tests can exercise the restart loop
// without linking a real tunnel.
type FRPRunner interface {
	Run(ctx context.Context) error
	GracefulClose(d time.Duration)
}

// RunnerFactory builds an FRPRunner for one connect cycle, called fresh every
// cycle with the knock-overlaid per-cycle common config. Returning an error
// aborts the supervisor: the configuration is unusable and retrying the same
// inputs would loop forever. Transient failures belong in Run instead.
type RunnerFactory func(common *v1.ClientCommonConfig) (FRPRunner, error)

// MarkerStore is the slice of the state package the supervisor drives for the
// sustained-failure episode ladder: arm one refresh episode when the knock
// budget exhausts, clear it on a confirmed-healthy cycle. state.Store
// satisfies it; nil disables the ladder.
type MarkerStore interface {
	RequestRefresh(reason string) error
	ClearRefreshMarker() error
}

// Config is the supervisor's input.
type Config struct {
	// Common is the FRP client common config, already Complete()d by the
	// caller. The supervisor treats it as READ-ONLY and overlays a per-cycle
	// copy with each knock's token and dial target.
	Common *v1.ClientCommonConfig

	// Knocker performs the per-cycle NHP knock. Required. When it also
	// implements knock.CycleKnocker, the supervisor brackets every cycle
	// with BeginCycle/EndCycle and binds the cycle RunID to the Login.
	Knocker knock.Knocker

	// KnockResourceID keys the token and dial-target lookups in the knock
	// result. Required.
	KnockResourceID string

	// RunnerFactory builds the per-cycle FRP runner. Required.
	RunnerFactory RunnerFactory

	// Marker, when non-nil, receives the episode transitions of the refresh
	// ladder (arm on budget exhaustion, clear on confirmed-healthy cycle).
	Marker MarkerStore

	// Logger receives the supervisor's structured events; nil uses
	// slog.Default().
	Logger *slog.Logger

	// MinBackoff, MaxBackoff, HealthyRunThreshold, and MinKnockInterval
	// override the package defaults; zero means the default. Test-only
	// injection so suites drive cycles in millisecond time.
	MinBackoff          time.Duration
	MaxBackoff          time.Duration
	HealthyRunThreshold time.Duration
	MinKnockInterval    time.Duration

	// MaxConsecutiveKnockFailures overrides the exit threshold; zero means
	// the package default. Test-only injection.
	MaxConsecutiveKnockFailures int
}

// Supervisor manages the knock → overlay → dial → restart-on-disconnect loop
// for one Connector session. Construct with New, then either call Run
// (blocking) or Start/Stop. The supervisor is single-shot: all cycle state
// below is written only by the serve goroutine, enforced by the started
// latch; construct a new Supervisor instead of restarting one.
type Supervisor struct {
	cfg Config

	// started latches on the first Run/Start so a second call returns
	// errAlreadyStarted instead of corrupting single-writer state.
	started atomic.Bool

	// cycles counts completed supervised cycles; atomic only so tests can
	// read it concurrently via Cycles().
	cycles atomic.Int64

	// loginFailExitForceLogged and refreshHintLogged latch their one-shot
	// operator breadcrumbs.
	loginFailExitForceLogged bool
	refreshHintLogged        bool

	// lastKnockAt stamps the most recent knock attempt (success or failure)
	// for the start-to-start min-interval gate. Zero on the first cycle so
	// the initial knock fires without delay.
	lastKnockAt time.Time

	// consecutiveUnhealthyKnocks counts consecutive cycles that did NOT end
	// fully healthy: a knock transport error, an ACK without a usable token
	// or dial target, or a healthy knock whose Login was token-rejected. One
	// unified counter (rather than per-cause buckets) closes the
	// alternating-pattern bypass where rotating causes would reset each
	// other's budgets forever. Reset ONLY by a confirmed-healthy cycle, and
	// the reset is deferred to end-of-cycle so a token-rejected Login still
	// accumulates after its healthy knock.
	consecutiveUnhealthyKnocks int

	// Start/Stop lifecycle. lifecycleMu guards the trio; done is closed when
	// the background serve goroutine exits with finalErr recorded.
	lifecycleMu sync.Mutex
	stopCancel  context.CancelFunc
	done        chan struct{}
	finalErr    error
}

// The package's failure conditions are deliberately unexported sentinels with
// an exported predicate where a caller needs the distinction: exported Err*
// vars would enter the CLI's exported-sentinel exit-code contract, and these
// conditions are the command layer's to interpret (the budget exit via
// IsTooManyKnockFailures), never customer-rendered identities of their own.

// errAlreadyStarted is returned by Run/Start when the supervisor has already
// been started; it is single-shot by design.
var errAlreadyStarted = errors.New("qURL Connector supervisor: already started; the supervisor is single-shot")

// errNotStarted is returned by Stop when Start was never called.
var errNotStarted = errors.New("qURL Connector supervisor: Stop called before Start")

// errTooManyKnockFailures terminates the supervisor when the consecutive
// unhealthy-knock budget is exhausted. The exit arms the refresh marker; the
// orchestrator restart plus the agent package's refresh ladder is the
// recovery mechanism.
var errTooManyKnockFailures = errors.New("qURL Connector supervisor: knock failed too many times consecutively")

// IsTooManyKnockFailures reports whether err is the supervisor's
// knock-budget exit — the sustained-failure signal whose recovery path is a
// process restart through the agent package's refresh ladder. The command
// layer keys its messaging and exit decision on this predicate.
func IsTooManyKnockFailures(err error) bool {
	return errors.Is(err, errTooManyKnockFailures)
}

// errKnockACTokenMissing is returned when a successful knock ACK carries no
// usable token for the configured resource; the supervisor refuses to Login
// without one rather than moving the failure to a later server rejection.
var errKnockACTokenMissing = errors.New("qURL Connector supervisor: knock ACK missing token for resource")

// errKnockResourceHostMissing is returned when a successful knock ACK carries
// no dial target for the configured resource. There is no static fallback in
// knock mode by design.
var errKnockResourceHostMissing = errors.New("qURL Connector supervisor: knock ACK missing dial target for resource")

// errKnockResourceHostUnusable is returned when the ACK's dial target cannot
// be used safely (not canonical host:port, unbracketed IPv6, or an IP literal
// that would break the TLS handshake).
var errKnockResourceHostUnusable = errors.New("qURL Connector supervisor: knock ACK dial target unusable")

// New validates cfg and returns a Supervisor over its own copy of cfg (the
// caller's struct is not retained). Errors are configuration-shaped and never
// retried.
func New(cfg *Config) (*Supervisor, error) {
	if cfg == nil {
		return nil, errors.New("qURL Connector supervisor: Config is required")
	}
	if cfg.Common == nil {
		return nil, errors.New("qURL Connector supervisor: Common is required")
	}
	if cfg.Knocker == nil {
		return nil, errors.New("qURL Connector supervisor: Knocker is required (the serve loop is knock-only)")
	}
	if cfg.KnockResourceID == "" {
		return nil, errors.New("qURL Connector supervisor: KnockResourceID is required")
	}
	if cfg.RunnerFactory == nil {
		return nil, errors.New("qURL Connector supervisor: RunnerFactory is required")
	}
	return &Supervisor{cfg: *cfg}, nil
}

// Cycles returns the number of completed supervised reconnect cycles.
// Test-facing; behavior does not depend on it.
func (s *Supervisor) Cycles() int64 {
	return s.cycles.Load()
}

// Run executes the serve loop until ctx is canceled or a non-recoverable
// error occurs. Returns ctx.Err() on clean shutdown, errAlreadyStarted on a
// second start, errTooManyKnockFailures (marker armed) on budget exhaustion,
// or a wrapped fatal error.
func (s *Supervisor) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errAlreadyStarted
	}
	return s.serveAndSettle(ctx)
}

// Start launches the serve loop on a background goroutine and returns
// immediately. The loop stops when ctx is canceled or Stop is called; observe
// an autonomous exit via Done/Err. Single-shot with Run.
func (s *Supervisor) Start(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errAlreadyStarted
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.lifecycleMu.Lock()
	s.stopCancel = cancel
	s.done = done
	s.lifecycleMu.Unlock()
	go func() {
		err := s.serveAndSettle(runCtx)
		s.lifecycleMu.Lock()
		s.finalErr = err
		s.lifecycleMu.Unlock()
		cancel()
		close(done)
	}()
	return nil
}

// Stop cancels a Start-ed serve loop and waits for its teardown (bounded by
// ctx). A shutdown the loop reports as context cancellation returns nil; any
// other exit cause is returned as-is. Idempotent once the loop has exited.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.lifecycleMu.Lock()
	cancel, done := s.stopCancel, s.done
	s.lifecycleMu.Unlock()
	if done == nil {
		return errNotStarted
	}
	cancel()
	select {
	case <-done:
		if err := s.Err(); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("qURL Connector supervisor: waiting for serve-loop teardown: %w", ctx.Err())
	}
}

// Done reports the background serve loop's exit; it is closed after Err is
// recorded. Nil when Start was never called.
func (s *Supervisor) Done() <-chan struct{} {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.done
}

// Err returns the serve loop's final error. Valid once Done is closed.
func (s *Supervisor) Err() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.finalErr
}

// serveAndSettle runs the loop and settles the episode ladder: a knock-budget
// exit arms the refresh marker so the next start walks the agent package's
// operator-gated refresh.
func (s *Supervisor) serveAndSettle(ctx context.Context) error {
	err := s.serve(ctx)
	if errors.Is(err, errTooManyKnockFailures) {
		s.armRefreshEpisode(ctx)
	}
	return err
}

// cycleOutcome carries one completed cycle's facts back to the serve loop.
type cycleOutcome struct {
	// retryKnock is true when the knock failed under budget: no runner ran,
	// and the loop should back off and re-knock.
	retryKnock bool
	// tokenStamped is true when this cycle stamped a real token, which arms
	// the deferred end-of-cycle budget reconciliation.
	tokenStamped bool
	// runErr and runDuration describe the FRP runner's exit.
	runErr      error
	runDuration time.Duration
}

// serve is the restart loop. Backoff applies to error and clean-exit paths
// alike (a flapping server must not loop the supervisor at full speed), grows
// exponentially with full jitter, and resets after a healthy-length run.
func (s *Supervisor) serve(ctx context.Context) error {
	s.warnIfUnknownTransportProtocol(ctx)
	backoffMin := s.minBackoff()
	backoff := backoffMin
	backoffCap := s.maxBackoff()
	healthyThreshold := s.healthyRunThreshold()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		outcome, err := s.cycle(ctx)
		if err != nil {
			return err
		}
		if !outcome.retryKnock {
			if ctx.Err() != nil {
				// Canceled mid-run: not a completed cycle. Locks the
				// documented Cycles() semantic under cancellation.
				return ctx.Err()
			}
			s.cycles.Add(1)
			s.logCycleEnd(ctx, outcome)
			if errors.Is(outcome.runErr, errTooManyKnockFailures) {
				// The in-run knock refresher exhausted its own budget and
				// asked for a restart; the supervisor exits with the same
				// sentinel so recovery is uniform.
				return outcome.runErr
			}
			if err := s.reconcileKnockBudget(ctx, outcome); err != nil {
				return err
			}
			if outcome.runDuration >= healthyThreshold {
				backoff = backoffMin
			}
		}
		if err := sleepCtx(ctx, jitterBackoff(backoff)); err != nil {
			return err
		}
		backoff = nextBackoffCap(backoff, backoffCap)
	}
}

// cycle executes one knock-then-login cycle: native cycle bracket, knock
// overlay, runner build, run, teardown. A returned error is fatal to the
// supervisor; under-budget knock failures come back as retryKnock instead.
func (s *Supervisor) cycle(ctx context.Context) (cycleOutcome, error) {
	var outcome cycleOutcome
	cycleCommon := clientCommonCopy(s.cfg.Common)
	cycleKnocker, isCycleKnocker := s.cfg.Knocker.(knock.CycleKnocker)
	if isCycleKnocker {
		if err := cycleKnocker.BeginCycle(); err != nil {
			return outcome, fmt.Errorf("qURL Connector supervisor: begin native cycle: %w", err)
		}
	}
	stamped, retry, err := s.applyKnockOverlay(ctx, cycleCommon)
	if err != nil {
		s.endNativeCycle(ctx, isCycleKnocker)
		return outcome, err
	}
	if retry {
		s.endNativeCycle(ctx, isCycleKnocker)
		outcome.retryKnock = true
		return outcome, nil
	}
	outcome.tokenStamped = stamped
	s.forceLoginFailExit(ctx, cycleCommon)

	runner, err := s.cfg.RunnerFactory(cycleCommon)
	if err != nil {
		s.endNativeCycle(ctx, isCycleKnocker)
		return outcome, fmt.Errorf("qURL Connector supervisor: RunnerFactory failed (config unusable): %w", err)
	}
	runStart := time.Now()
	outcome.runErr = runner.Run(ctx)
	// GracefulClose covers a Run that returned without a full close;
	// idempotent, and generous enough for internal listeners to release
	// before the next cycle rebinds.
	runner.GracefulClose(gracefulCloseTimeout)
	s.endNativeCycle(ctx, isCycleKnocker)
	outcome.runDuration = time.Since(runStart)
	return outcome, nil
}

// forceLoginFailExit forces the per-cycle clone to fail initial Login fast:
// the FRP client's initial-login retry loop does not know the admission
// open-time that admitted this cycle, so initial-login failures must return
// to the supervisor for a fresh knock. Logged once per supervisor.
func (s *Supervisor) forceLoginFailExit(ctx context.Context, cycleCommon *v1.ClientCommonConfig) {
	if !s.loginFailExitForceLogged {
		previous := "<nil>"
		if cycleCommon.LoginFailExit != nil {
			previous = strconv.FormatBool(*cycleCommon.LoginFailExit)
		}
		s.log().InfoContext(ctx, "connector: forcing tunnel login fail-fast because the NHP knocker owns retry pacing",
			append(s.knockLogAttrs(),
				"event", "login_fail_exit_forced",
				"configured_login_fail_exit", previous,
			)...,
		)
		s.loginFailExitForceLogged = true
	}
	forceLoginFailExit := true
	cycleCommon.LoginFailExit = &forceLoginFailExit
}

// reconcileKnockBudget is the end-of-cycle reconciliation against the unified
// unhealthy-knock budget, armed only for cycles that stamped a real token
// (failed-knock paths already counted inside applyKnockOverlay and never ran
// the tunnel).
//
// Reset rule: a healthy-knock cycle whose Login was NOT token-rejected clears
// the slate and the refresh episode. The deferred reset is what lets the
// increment below accumulate — an at-knock-time reset would erase the
// token-rejection bump. Increment rule: a token-rejected Login counts against
// the SAME budget, so a server that persistently refuses valid-looking tokens
// still reaches the orchestrator-restart recovery instead of looping forever.
func (s *Supervisor) reconcileKnockBudget(ctx context.Context, outcome cycleOutcome) error {
	if !outcome.tokenStamped {
		return nil
	}
	if IsTokenLoginError(outcome.runErr) {
		s.consecutiveUnhealthyKnocks++
		s.log().WarnContext(ctx, "connector: tunnel login rejected the knock token; will re-knock on next cycle",
			append(s.knockLogAttrs(),
				"event", "login_token_rejected",
				"err", errString(outcome.runErr),
				"consecutive_unhealthy_knocks", s.consecutiveUnhealthyKnocks,
				"max_failures", s.maxConsecutiveKnockFailures(),
			)...,
		)
		if s.consecutiveUnhealthyKnocks >= s.maxConsecutiveKnockFailures() {
			return errors.Join(
				errTooManyKnockFailures,
				fmt.Errorf("%d consecutive unhealthy knocks, last was a token-rejected login: %w", s.consecutiveUnhealthyKnocks, outcome.runErr),
			)
		}
		return nil
	}
	s.consecutiveUnhealthyKnocks = 0
	// Confirmed-healthy knock+login cycle: the single authoritative point
	// where the supervisor declares the knock path recovered and ends any
	// refresh episode. Not on a mere transport-success knock, which may
	// still fail the Login.
	s.clearRefreshEpisode(ctx)
	return nil
}

// applyKnockOverlay drives the per-cycle knock and overlays the resulting
// token and dial target onto cycleCommon. retryNextCycle means the knock was
// unhealthy but under budget: skip the tunnel, back off, re-knock. A non-nil
// error exits the supervisor (budget exhausted, or canceled). Soft failures
// never dial without a valid token and target.
func (s *Supervisor) applyKnockOverlay(ctx context.Context, cycleCommon *v1.ClientCommonConfig) (tokenForwarded, retryNextCycle bool, err error) {
	if err := s.waitKnockGate(ctx); err != nil {
		return false, false, err
	}
	// Stamp lastKnockAt BEFORE the knock fires: the gate is start-to-start,
	// so a slow agent transaction cannot compress the next gate window into
	// a knock storm.
	knockStart := time.Now()
	s.lastKnockAt = knockStart
	result, err := s.cfg.Knocker.Knock(ctx)
	knockDuration := time.Since(knockStart)
	if err != nil {
		retry, budgetErr := s.recordUnhealthyKnock(ctx, eventKnockFail, "connector: NHP knock failed", err, knockDuration, func(count int, cause error) error {
			return fmt.Errorf("%d consecutive unhealthy knocks, last error: %w", count, cause)
		})
		return false, retry, budgetErr
	}
	s.log().InfoContext(ctx, "connector: NHP knock ok",
		append(s.knockLogAttrs(),
			"event", eventKnockOK,
			slog.Duration("knock_duration", knockDuration),
		)...,
	)
	stamped, overlayErr := s.overlayKnockResult(ctx, cycleCommon, result)
	if overlayErr != nil {
		retry, budgetErr := s.recordUnhealthyKnock(ctx, "knock_ack_unusable", "connector: NHP knock ACK unusable; refusing tunnel login and will re-knock", overlayErr, knockDuration, func(count int, cause error) error {
			return fmt.Errorf("%d consecutive unhealthy knocks, last ACK error: %w", count, cause)
		})
		return false, retry, budgetErr
	}
	return stamped, false, nil
}

// waitKnockGate enforces the start-to-start minimum interval between knocks.
// The gate-wait log fires only when the gate actually sleeps: it is the only
// structured signal that a flapping Connector is being rate-limited.
func (s *Supervisor) waitKnockGate(ctx context.Context) error {
	if s.lastKnockAt.IsZero() {
		return nil
	}
	gate := s.minKnockInterval()
	elapsed := time.Since(s.lastKnockAt)
	if elapsed >= gate {
		return nil
	}
	wait := gate - elapsed
	s.log().InfoContext(ctx, "connector: knock gate active; waiting before next knock",
		append(s.knockLogAttrs(),
			"event", eventKnockGateWait,
			slog.Duration("wait", wait),
			slog.Duration("gate", gate),
		)...,
	)
	return sleepCtx(ctx, wait)
}

// recordUnhealthyKnock advances the unified budget for a failed or unusable
// knock, logs it, and decides between the under-budget retry path and the
// budget-exhausted exit (wrapping both the sentinel and the cause).
func (s *Supervisor) recordUnhealthyKnock(ctx context.Context, event, message string, cause error, knockDuration time.Duration, exhausted func(count int, cause error) error) (retryNextCycle bool, err error) {
	s.consecutiveUnhealthyKnocks++
	s.logRefreshHintIfNeeded(ctx)
	s.log().WarnContext(ctx, message,
		append(s.knockLogAttrs(),
			"event", event,
			"err", cause.Error(),
			slog.Duration("knock_duration", knockDuration),
			"consecutive_unhealthy_knocks", s.consecutiveUnhealthyKnocks,
			"max_failures", s.maxConsecutiveKnockFailures(),
		)...,
	)
	if s.consecutiveUnhealthyKnocks >= s.maxConsecutiveKnockFailures() {
		return false, errors.Join(errTooManyKnockFailures, exhausted(s.consecutiveUnhealthyKnocks, cause))
	}
	return true, nil
}

// overlayKnockResult validates the knock result and stamps the token into the
// Login metadata plus the ACK dial target into ServerAddr/Port. The overlay
// is transactional: the caller sees both or neither. A non-empty token
// returns stamped=true so the budget reset can defer to the Login outcome.
func (s *Supervisor) overlayKnockResult(ctx context.Context, cycleCommon *v1.ClientCommonConfig, result *knock.Result) (bool, error) {
	if result == nil {
		// The knock package translates a nil admission into an error, so
		// reaching here means an adapter regressed that guard; fail closed
		// instead of attempting an unstamped Login.
		err := fmt.Errorf("%w %q: nil knock result", errKnockACTokenMissing, s.cfg.KnockResourceID)
		s.logOverlayReject(ctx, "knock_nil_result", nil, err)
		return false, err
	}
	token := result.ACTokens[s.cfg.KnockResourceID]
	if token == "" {
		err := fmt.Errorf("%w %q", errKnockACTokenMissing, s.cfg.KnockResourceID)
		s.logOverlayReject(ctx, "ack_token_missing", sortedKeys(result.ACTokens), err)
		return false, err
	}
	host := result.ResourceHost[s.cfg.KnockResourceID]
	if host == "" {
		err := fmt.Errorf("%w %q", errKnockResourceHostMissing, s.cfg.KnockResourceID)
		s.logOverlayReject(ctx, "ack_resource_host_missing", sortedKeys(result.ResourceHost), err)
		return false, err
	}
	newAddr, newPort, err := s.parseOverlayHost(ctx, host)
	if err != nil {
		return false, err
	}
	// TLS-SNI guard, narrowed to IP-literal dial targets: with TLS enabled
	// and no explicit ServerName, the FRP client uses ServerAddr as the SNI
	// value, and most servers refuse an IP-literal SNI. Hostname targets
	// pass through — they are valid SNI on their own.
	if TLSEnabled(cycleCommon) && cycleCommon.Transport.TLS.ServerName == "" && IsIPLiteralHost(newAddr) {
		err := fmt.Errorf("%w: IP-literal dial target %q with TLS enabled and empty ServerName for resource %q", errKnockResourceHostUnusable, host, s.cfg.KnockResourceID)
		s.logOverlayReject(ctx, "knock_overlay_tls_guard", nil, err)
		return false, err
	}
	if cycleCommon.Metadatas == nil {
		cycleCommon.Metadatas = map[string]string{}
	}
	cycleCommon.Metadatas[frpgen.MetaQURLKnockToken] = token
	cycleCommon.ServerAddr = newAddr
	cycleCommon.ServerPort = newPort
	s.log().InfoContext(ctx, "connector: dialing tunnel target from knock response",
		append(s.knockLogAttrs(),
			"event", "knock_overlay_applied",
			"server_addr", newAddr,
			"server_port", newPort,
		)...,
	)
	return true, nil
}

// parseOverlayHost parses the ACK dial target as canonical host:port, with
// the dedicated unbracketed-IPv6 diagnosis (it fails host:port parsing AND
// would be misread downstream, so it gets its own fail-closed message).
func (s *Supervisor) parseOverlayHost(ctx context.Context, host string) (addr string, port int, err error) {
	newAddr, newPort, parseErr := ParseResourceHost(host)
	if parseErr == nil {
		return newAddr, newPort, nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		err := fmt.Errorf("%w: unbracketed IPv6 literal %q for resource %q", errKnockResourceHostUnusable, host, s.cfg.KnockResourceID)
		s.logOverlayReject(ctx, "ack_resource_host_unbracketed_ipv6", nil, err)
		return "", 0, err
	}
	err = fmt.Errorf("%w: dial target %q for resource %q is not canonical host:port: %w", errKnockResourceHostUnusable, host, s.cfg.KnockResourceID, parseErr)
	s.logOverlayReject(ctx, "ack_resource_host_unusable", nil, err)
	return "", 0, err
}

func (s *Supervisor) logOverlayReject(ctx context.Context, event string, availableResourceIDs []string, err error) {
	attrs := append(s.knockLogAttrs(), "event", event, "err", err.Error())
	if availableResourceIDs != nil {
		attrs = append(attrs, "available_resource_ids", availableResourceIDs)
	}
	s.log().WarnContext(ctx, "connector: knock ACK failed validation; refusing tunnel login", attrs...)
}

// endNativeCycle closes every native cycle whose BeginCycle succeeded,
// including failure paths where a lost reply may hide server-side session
// state. Best-effort with an independent short budget, because the cycle
// context is commonly canceled during shutdown.
func (s *Supervisor) endNativeCycle(logCtx context.Context, cycleBegun bool) {
	if !cycleBegun {
		return
	}
	cycleKnocker, ok := s.cfg.Knocker.(knock.CycleKnocker)
	if !ok {
		return
	}
	exitCtx, cancelExit := context.WithTimeout(context.Background(), endCycleTimeout)
	defer cancelExit()
	if err := cycleKnocker.EndCycle(exitCtx); err != nil {
		s.log().WarnContext(logCtx, "connector: native NHP session cleanup failed",
			append(s.knockLogAttrs(),
				"event", "nhp_session_exit_failed",
				"err", err.Error(),
			)...,
		)
	}
}

// armRefreshEpisode records the sustained-failure episode so the next start
// walks the agent package's operator-gated refresh ladder. Arming is
// episode-idempotent in the state package; a write fault only logs — a
// Connector that cannot write the breadcrumb merely loses this restart's
// self-heal.
func (s *Supervisor) armRefreshEpisode(ctx context.Context) {
	if s.cfg.Marker == nil {
		return
	}
	if err := s.cfg.Marker.RequestRefresh(refreshMarkerReason); err != nil {
		s.log().WarnContext(ctx, "connector: failed to record assignment refresh request",
			append(s.knockLogAttrs(),
				"event", "registration_refresh_request_failed",
				"err", err.Error(),
			)...,
		)
	}
}

// clearRefreshEpisode ends the current refresh episode after a
// confirmed-healthy cycle, so steady-state restarts stay on the efficient
// persisted-assignment path.
func (s *Supervisor) clearRefreshEpisode(ctx context.Context) {
	if s.cfg.Marker == nil {
		return
	}
	if err := s.cfg.Marker.ClearRefreshMarker(); err != nil {
		s.log().WarnContext(ctx, "connector: failed to clear assignment-refresh marker after a healthy cycle",
			append(s.knockLogAttrs(),
				"event", "registration_refresh_clear_failed",
				"err", err.Error(),
			)...,
		)
	}
}

// logRefreshHintIfNeeded explains the persisted-identity refresh policy once
// after repeated knock failures, so operators get the recovery path without
// implying that refresh spends the enrollment credential.
func (s *Supervisor) logRefreshHintIfNeeded(ctx context.Context) {
	const threshold = 2
	if s.refreshHintLogged || s.consecutiveUnhealthyKnocks < threshold {
		return
	}
	s.refreshHintLogged = true
	s.log().WarnContext(ctx, "connector: repeated NHP knock failures; native assignment refresh uses the persisted device identity, not the enrollment credential",
		append(s.knockLogAttrs(),
			"event", "assignment_refresh_hint",
			"consecutive_unhealthy_knocks", s.consecutiveUnhealthyKnocks,
			"refresh_mode_env", "LAYERV_AGENT_REGISTRATION_REFRESH_MODE",
			"remediation", "after the knock-failure budget is exhausted, the Connector records a refresh marker and stops: manual (default) requires one explicitly approved start with LAYERV_AGENT_REGISTRATION_REFRESH_MODE=auto; automatic orchestrator restarts are not approval; disabled does not refresh",
		)...,
	)
}

func (s *Supervisor) logCycleEnd(ctx context.Context, outcome cycleOutcome) {
	if outcome.runErr == nil {
		s.log().InfoContext(ctx, "connector: tunnel connection ended cleanly; reconnecting",
			append(s.knockLogAttrs(), "run_seconds", outcome.runDuration.Seconds())...,
		)
		return
	}
	s.log().WarnContext(ctx, "connector: tunnel connection ended with an error; reconnecting after backoff",
		append(s.knockLogAttrs(),
			"err", outcome.runErr.Error(),
			"run_seconds", outcome.runDuration.Seconds(),
		)...,
	)
}

// knockLogAttrs returns the structured-log attrs every knock-cycle log line
// shares, so the emit sites cannot drift on field name or type.
func (s *Supervisor) knockLogAttrs() []any {
	return []any{
		"knock_resource_id", s.cfg.KnockResourceID,
		"prior_cycles", s.cycles.Load(),
	}
}

func (s *Supervisor) log() *slog.Logger {
	if s.cfg.Logger != nil {
		return s.cfg.Logger
	}
	return slog.Default()
}

// Config-or-default accessors. Zero config values inherit the package
// defaults; tests inject small values to drive cycles in millisecond time.
func (s *Supervisor) minBackoff() time.Duration {
	if s.cfg.MinBackoff > 0 {
		return s.cfg.MinBackoff
	}
	return minBackoff
}

func (s *Supervisor) maxBackoff() time.Duration {
	if s.cfg.MaxBackoff > 0 {
		return s.cfg.MaxBackoff
	}
	return maxBackoff
}

func (s *Supervisor) healthyRunThreshold() time.Duration {
	if s.cfg.HealthyRunThreshold > 0 {
		return s.cfg.HealthyRunThreshold
	}
	return healthyRunThreshold
}

func (s *Supervisor) minKnockInterval() time.Duration {
	if s.cfg.MinKnockInterval > 0 {
		return s.cfg.MinKnockInterval
	}
	return minKnockInterval
}

func (s *Supervisor) maxConsecutiveKnockFailures() int {
	if s.cfg.MaxConsecutiveKnockFailures > 0 {
		return s.cfg.MaxConsecutiveKnockFailures
	}
	return maxConsecutiveKnockFailures
}

// jitterBackoff returns a random duration in [d/2, d) — full jitter, so a
// fleet-wide server restart cannot synchronize every Connector's reconnect
// into a thundering herd. rand/v2 is process-wide concurrency-safe.
func jitterBackoff(d time.Duration) time.Duration {
	if d <= time.Nanosecond {
		return d
	}
	half := d / 2
	return half + rand.N(d-half) //nolint:gosec // G404: backoff jitter needs spread, not cryptographic strength.
}

// nextBackoffCap doubles the backoff up to ceiling. A non-positive input is
// clamped straight to ceiling so no future caller can hot-loop on zero.
func nextBackoffCap(d, ceiling time.Duration) time.Duration {
	if d <= 0 {
		return ceiling
	}
	d *= 2
	if d > ceiling {
		return ceiling
	}
	return d
}

// sleepCtx sleeps for d or until ctx is canceled; returns ctx.Err() on
// cancellation.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// clientCommonCopy makes the per-cycle clone the supervisor overlays: a
// shallow copy with Metadatas deep-cloned, because the per-cycle token stamp
// must neither alias across cycles nor leak into the caller's Common. Other
// reference-typed fields stay shallow-aliased — the supervisor does not write
// to them; extend the clone before mutating any other nested field.
func clientCommonCopy(in *v1.ClientCommonConfig) *v1.ClientCommonConfig {
	out := *in
	if in.Metadatas != nil {
		clone := make(map[string]string, len(in.Metadatas))
		for k, v := range in.Metadatas {
			clone[k] = v
		}
		out.Metadatas = clone
	}
	return &out
}

// ParseResourceHost parses the ACK dial-target contract into addr/port. It
// accepts canonical "host:port" including bracketed IPv6 literals;
// unbracketed IPv6 and bare hosts fail so callers can never fall back to a
// static dial target by accident. Bracketing is restored on IPv6 hosts so the
// downstream dial string stays unambiguous.
func ParseResourceHost(resourceHost string) (addr string, port int, err error) {
	host, port, err := splitHostPort(resourceHost)
	if err != nil {
		return "", 0, err
	}
	if strings.TrimSpace(host) == "" {
		return "", 0, errors.New("host is empty")
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		host = "[" + host + "]"
	}
	return host, port, nil
}

// splitHostPort splits a canonical "host:port" string with a validated
// numeric port.
func splitHostPort(hp string) (addr string, port int, err error) {
	host, portStr, err := net.SplitHostPort(hp)
	if err != nil {
		return "", 0, fmt.Errorf("net.SplitHostPort(%q): %w", hp, err)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("port %q is not numeric: %w", portStr, err)
	}
	if port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("port %d out of range", port)
	}
	return host, port, nil
}

// IsIPLiteralHost reports whether host parses as an IPv4 or IPv6 literal,
// accepting bracketed IPv6 forms. Used by the overlay's TLS-SNI guard, which
// fires only when the rewritten ServerAddr would be an IP literal.
func IsIPLiteralHost(host string) bool {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	return net.ParseIP(host) != nil
}

// TLSEnabled mirrors the pinned FRP client's own decision for whether the
// dialer wraps the connection in TLS: the explicit enable flag, or a
// TLS-implying transport protocol.
func TLSEnabled(common *v1.ClientCommonConfig) bool {
	if common == nil {
		return false
	}
	if common.Transport.TLS.Enable != nil && *common.Transport.TLS.Enable {
		return true
	}
	switch common.Transport.Protocol {
	case "wss", "quic":
		return true
	}
	return false
}

// knownTransportProtocols lists the transport protocol values the TLS guard
// above has been audited against for the pinned FRP version; empty means the
// FRP default (tcp). The guard is fail-open for unknown protocols, so the
// one-shot warning below is the operator breadcrumb to re-audit TLSEnabled
// when a future FRP bump introduces a new TLS-implying protocol.
var knownTransportProtocols = map[string]struct{}{
	"":          {},
	"tcp":       {},
	"kcp":       {},
	"quic":      {},
	"websocket": {},
	"wss":       {},
}

var knownTransportProtocolList = func() []string {
	out := make([]string, 0, len(knownTransportProtocols))
	for k := range knownTransportProtocols {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}()

func (s *Supervisor) warnIfUnknownTransportProtocol(ctx context.Context) {
	proto := s.cfg.Common.Transport.Protocol
	if _, ok := knownTransportProtocols[proto]; ok {
		return
	}
	s.log().WarnContext(ctx, "connector: transport protocol is outside the TLS guard's audited set; re-audit TLSEnabled if this protocol implies TLS",
		"protocol", proto,
		"known_protocols", knownTransportProtocolList,
	)
}

// sortedKeys returns the lex-sorted keys of m for deterministic log output.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// errString renders err for structured logs, "" for nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
