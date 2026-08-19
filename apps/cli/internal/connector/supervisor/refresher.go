package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	frpclient "github.com/fatedier/frp/client"
	v1 "github.com/fatedier/frp/pkg/config/v1"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/frpgen"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/knock"
)

// The FRP client owns internal physical reconnects after its first Login:
// when the control connection drops, it redials without returning to the
// supervisor. Every one of those redials must land inside a live admission
// window, so this file wraps the client's Connector seam with a knock
// refresher — re-knock (gated) before each physical dial, restamping the
// token and dial target the server returns. Sustained refresh failures
// escalate through requestRestart back to the supervisor's outer cycle.

const (
	// redialKnockGate mirrors the supervisor's knock gate for in-run
	// refreshes. It must stay well below the server-side admission open-time
	// so debounced internal reconnects still land inside the prior knock's
	// window.
	redialKnockGate = 10 * time.Second

	// redialKnockMaxFailures mirrors the supervisor's budget shape, but
	// counts physical-redial knocks inside ONE tunnel run; exhausting it
	// requests a cycle restart carrying ErrTooManyKnockFailures.
	redialKnockMaxFailures = 5

	// The three delays that can separate two consecutive dials in a storm
	// that is still FAILING. Every one is a real, checked value, not an
	// estimate: the watchdog's whole correctness rests on the settled gap
	// being longer than their sum, so they are named rather than folded
	// into a magic number.
	//
	//   - failingDialConnectBudget: transport.dialServerTimeout, which
	//     frpgen sets to defaultDialTimeoutSeconds.
	//   - failingDialLoginReadBudget: the pinned fork's Login response read
	//     deadline, hard-coded in client/control_session.go.
	//   - failingDialBackoffCeiling: the post-admission reconnect loop's
	//     backoff cap (keepControllerWorking passes 20s as maxInterval).
	//
	// A single failing attempt can burn the first two in sequence — a TCP
	// connect that succeeds slowly, then a server that never answers the
	// Login — before the loop waits out the third.
	failingDialConnectBudget   = 10 * time.Second
	failingDialLoginReadBudget = 10 * time.Second
	failingDialBackoffCeiling  = 20 * time.Second

	// failingDialPeriod is how far apart two dials of a still-failing storm
	// can be, before the per-dial re-knock's own round trip is added.
	failingDialPeriod = failingDialConnectBudget + failingDialLoginReadBudget + failingDialBackoffCeiling

	// reconnectSettledGap is the quiet period that ends a redial storm. The
	// FRP client does not dial while a control session is up, so a gap this
	// long means one of the redials succeeded and the tunnel served again.
	//
	// It MUST stay comfortably above failingDialPeriod. This is the
	// watchdog's fail-open edge: if a failing storm's own dial spacing can
	// reach the gap, every dial looks like a fresh start, the storm never
	// accumulates, and the watchdog silently never fires — leaving exactly
	// the unbounded loop this file exists to bound. 3x failingDialPeriod
	// leaves room for the re-knock round trip and for a slower server
	// without ever needing this constant re-tuned alongside them.
	//
	// The inference is one-directional, and the wording of both operator
	// lines depends on that: a long gap proves the tunnel served, but a
	// short one does NOT prove it did not. A connection that re-establishes,
	// serves briefly and drops again inside this window is counted as one
	// unbroken storm, which is why neither line claims the tunnel failed to
	// come back — only that it is not staying up. Treating a sub-gap flap as
	// a storm is deliberate and is an accepted trade: such a tunnel is not
	// serving usefully, the recovery (end the cycle, re-knock, take a fresh
	// dial target) suits it too, and sustained flapping does eventually
	// spend the knock budget and exit. The window below is sized so that
	// takes tens of minutes, not seconds.
	reconnectSettledGap = 3 * failingDialPeriod

	// reconnectStallWindow bounds how long the FRP client may sit in its own
	// post-admission reconnect loop before this package takes the cycle back.
	//
	// It exists because that loop is unbounded and unobservable: after a
	// cycle's first Login succeeds the pinned fork retries internally forever
	// (keepControllerWorking passes firstLoginExit=false, hard-coded, so the
	// LoginFailExit this package forces does not reach it) and Run stays
	// blocked, so the supervisor's knock budget, its failure classification
	// and every operator-facing message are unreachable for the rest of the
	// run. Without this watchdog a Connector in that state loops in silence.
	//
	// Sized to outlast the measured cause with margin. The tunnel-server
	// fleet is an ASG that rolls instances with an instance refresh; a
	// replacement measured 80, 83 and 80 seconds from launch to serving
	// across the three replacements of one 2026-08-18 sandbox roll. It must
	// also stay above reconnectSettledGap, or a storm that keeps resetting
	// could never reach it.
	//
	// Overrunning it is cheap and sometimes better: the cycle ends, the
	// supervisor re-knocks, and the ACK returns a fresh dial target. The
	// operator is told long before that, at reconnectStallNoticeAfter dials.
	reconnectStallWindow = 2 * reconnectSettledGap

	// reconnectStallNoticeAfter is how many dials into a storm the operator
	// notice fires. Three means the tunnel dropped and TWO redials have
	// already failed. Two would fire while only one had failed, and the dial
	// about to run might well succeed — a routine two-attempt reconnect
	// would then be announced as a tunnel that "keeps dropping".
	reconnectStallNoticeAfter = 3
)

// reasonReconnectStalled is the classification bucket for a cycle the
// reconnect watchdog took back.
const reasonReconnectStalled = "reconnect_stalled"

// errReconnectStalled ends a cycle whose tunnel was admitted and then could
// not re-establish inside reconnectStallWindow. Unexported on purpose: like
// the other per-cycle conditions it is retried under the supervisor's budget
// and never becomes a process exit of its own, so it stays out of the CLI's
// exported-sentinel exit-code contract. The budget exit it eventually
// produces is ErrTooManyKnockFailures, which already has a code.
var errReconnectStalled = errors.New("qURL Connector supervisor: tunnel could not re-establish after it was admitted")

// redialKnockRefresher re-knocks before physical tunnel redials. All state is
// mutex-guarded: the lock deliberately spans the knock call and the common
// config mutation, so pipelined connector dials in a future FRP version would
// serialize rather than race the ServerAddr/Metadatas writes.
type redialKnockRefresher struct {
	knocker    knock.Knocker
	resourceID string
	gate       time.Duration
	logger     *slog.Logger

	// now is the clock seam; nil means time.Now. Injected by the unit tests
	// so the gate arithmetic is deterministic instead of reading the real
	// clock: Go's monotonic reading advances on the interrupt-timer tick on
	// Windows (up to ~15.6ms), so consecutive serialized refreshes can
	// observe zero elapsed time and a real-clock test gate would collapse
	// distinct attempts into one debounce window.
	now func() time.Time

	// stallWindow and settledGap override the package defaults; zero means
	// the default. Test-only injection, like the supervisor's own knobs.
	stallWindow time.Duration
	settledGap  time.Duration

	mu                 sync.Mutex
	lastKnockAt        time.Time
	consecutiveFailure int
	requestRestart     func(error)

	// Reconnect-watchdog state, guarded by mu with everything above.
	// lastDialAt stamps the previous physical dial, stormStartedAt opens the
	// current unbroken redial storm (zero outside one), stormDials counts the
	// dials inside it, stormNoticed latches the one operator notice, and
	// stormStalled latches the one stall report.
	lastDialAt     time.Time
	stormStartedAt time.Time
	stormDials     int
	stormNoticed   bool
	stormStalled   bool
}

// noteRedialLocked advances the reconnect watchdog for one physical dial and
// reports the error the dial should fail with, if any. The caller holds r.mu.
//
// Every call reaching it is a redial: the supervisor's own first dial of a
// cycle is consumed by the first-cycle handoff in refresh, and FRP does not
// dial again while a control session is up. So a run of calls separated by
// less than settledGap is precisely "the tunnel is down and not coming back",
// which is the condition the FRP client cannot report and this watchdog can.
func (r *redialKnockRefresher) noteRedialLocked(ctx context.Context, t time.Time) error {
	if !r.lastDialAt.IsZero() && t.Sub(r.lastDialAt) >= r.settled() {
		// Quiet long enough that a redial must have served: start over, so a
		// tunnel that reconnects normally never accumulates toward the window.
		r.stormStartedAt = time.Time{}
		r.stormDials = 0
		r.stormNoticed = false
		r.stormStalled = false
	}
	r.lastDialAt = t
	if r.stormStartedAt.IsZero() {
		r.stormStartedAt = t
	}
	r.stormDials++

	elapsed := t.Sub(r.stormStartedAt)
	if elapsed >= r.stall() {
		stalled := fmt.Errorf("%w: no tunnel session for %s across %d dial attempts", errReconnectStalled, elapsed.Round(time.Second), r.stormDials)
		// Latched like the notice above. Canceling the cycle does not stop
		// FRP instantly, so it can dial again before its Run observes the
		// cancellation; without this the same stall would report twice.
		// requestRestart is already idempotent on the cause — this is about
		// not emitting a duplicate operator line.
		if !r.stormStalled {
			r.stormStalled = true
			r.log().WarnContext(ctx, "connector: the tunnel connection kept dropping for too long; restarting the connection cycle",
				"event", reasonReconnectStalled,
				"resource_id", r.resourceID,
				"stalled_seconds", elapsed.Seconds(),
				"dial_attempts", r.stormDials)
			if r.requestRestart != nil {
				r.requestRestart(stalled)
			}
		}
		// Returned on every dial, latched or not: each one must still fail.
		return stalled
	}
	if r.stormDials >= reconnectStallNoticeAfter && !r.stormNoticed {
		r.stormNoticed = true
		// Said once per storm, in customer language, because this is the
		// window in which the Connector otherwise looks healthy while
		// consumers time out: the knocks below keep succeeding and the FRP
		// client logs only its own transport errors.
		//
		// Deliberately states the observation and NOT a cause. The dial
		// failures underneath are multiplexer transport errors with no
		// server-supplied reason, so naming one — a held previous session, a
		// network fault — would be a guess printed as fact. It also avoids
		// claiming the tunnel never came back: see reconnectSettledGap, a
		// flap inside the gap reads as one storm.
		r.log().WarnContext(ctx, "connector: the tunnel connection keeps dropping and is not staying up; still retrying, and consumers will time out while it is down",
			"event", "reconnect_retrying",
			"resource_id", r.resourceID,
			// Not stalled_seconds: at this point it is still retrying, and
			// the tunnel may even be coming back between dials. The stall
			// line below is the one that has stalled.
			"retrying_seconds", elapsed.Seconds(),
			"dial_attempts", r.stormDials,
			"gives_up_after_seconds", r.stall().Seconds())
	}
	return nil
}

func (r *redialKnockRefresher) stall() time.Duration {
	if r.stallWindow > 0 {
		return r.stallWindow
	}
	return reconnectStallWindow
}

func (r *redialKnockRefresher) settled() time.Duration {
	if r.settledGap > 0 {
		return r.settledGap
	}
	return reconnectSettledGap
}

// refresh performs one gated knock and restamps common in place. The pinned
// FRP fork reads the common config synchronously from the connector dial path
// after this returns; a future FRP that reads it from background goroutines
// would require a per-refresh copy instead of in-place mutation.
func (r *redialKnockRefresher) refresh(ctx context.Context, common *v1.ClientCommonConfig, reason string) error {
	if r == nil || r.knocker == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// One clock read per refresh: the gate check and the start-to-start
	// stamp must see the same instant, and a single read is what lets the
	// injected test clock drive the arithmetic deterministically. Select the
	// clock first, then read it once — the seam replaces the clock, not the
	// reading.
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	t := now()
	if r.lastKnockAt.IsZero() && commonKnockToken(common) != "" {
		// First-cycle handoff: the supervisor already knocked and stamped
		// this cycle's token. Start the redial gate at handoff time so quick
		// connector retries stay inside the same admission window, and return
		// before the watchdog — this is the supervisor's own dial, not a
		// redial, so it must not open a storm or count toward one.
		r.lastKnockAt = t
		return nil
	}
	// Ahead of the gate on purpose: the watchdog must see EVERY physical
	// dial. During a storm the FRP client redials faster than the gate, so
	// most calls return debounced below — counting only the ungated ones
	// would undercount the storm by roughly the ratio of the two intervals.
	if err := r.noteRedialLocked(ctx, t); err != nil {
		return err
	}
	if !r.lastKnockAt.IsZero() {
		if wait := r.gate - t.Sub(r.lastKnockAt); wait > 0 {
			r.log().DebugContext(ctx, "connector: redial NHP knock skipped inside gate",
				"event", "redial_knock_gate_wait",
				"resource_id", r.resourceID,
				"reason", reason,
				"wait", wait.String())
			return nil
		}
	}

	// Stamp before Knock to keep the gate start-to-start, matching the
	// supervisor. On failure this deliberately debounces immediate connector
	// retries; if a stale token then rejects the Login, the forced
	// LoginFailExit hands control back to the supervisor cycle, which
	// performs the canonical re-knock.
	r.lastKnockAt = t
	result, err := r.knocker.Knock(ctx)
	if err != nil {
		wrapped := fmt.Errorf("redial %s knock failed: %w", reason, err)
		r.logFailure(ctx, "redial_knock_fail", "connector: redial NHP knock failed", reason, err)
		return r.recordFailureLocked(wrapped)
	}
	token, err := applyKnockResult(common, r.resourceID, result)
	if err != nil {
		wrapped := fmt.Errorf("redial %s knock unusable: %w", reason, err)
		r.logFailure(ctx, "redial_knock_unusable", "connector: redial NHP knock result unusable", reason, err)
		return r.recordFailureLocked(wrapped)
	}
	r.consecutiveFailure = 0
	r.log().InfoContext(ctx, "connector: refreshed NHP knock before tunnel dial",
		"event", "redial_knock_ok",
		"resource_id", r.resourceID,
		"server_addr", common.ServerAddr,
		"server_port", common.ServerPort,
		"reason", reason,
		"token_stamped", token != "")
	return nil
}

func (r *redialKnockRefresher) logFailure(ctx context.Context, event, message, reason string, err error) {
	r.log().WarnContext(ctx, message,
		"event", event,
		"resource_id", r.resourceID,
		"reason", reason,
		"err", err.Error(),
		"consecutive_failures", r.consecutiveFailure+1,
		"max_failures", redialKnockMaxFailures)
}

// recordFailureLocked advances the in-run budget (caller holds r.mu) and, on
// exhaustion, asks the cycle runner to restart with the supervisor's knock
// sentinel so the outer loop treats it as a knock-budget exit.
func (r *redialKnockRefresher) recordFailureLocked(err error) error {
	r.consecutiveFailure++
	if r.consecutiveFailure >= redialKnockMaxFailures && r.requestRestart != nil {
		// requestRestart only takes the runner's cancel lock and never calls
		// back into the refresher, so the lock order stays isolated.
		r.requestRestart(fmt.Errorf("%w: %d consecutive redial knock refresh failures, last error: %w", ErrTooManyKnockFailures, r.consecutiveFailure, err))
	}
	return err
}

func (r *redialKnockRefresher) log() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return slog.Default()
}

// applyKnockResult validates one knock result and stamps the token plus dial
// target onto common. It enforces the same fail-closed contract as the
// supervisor's own overlay: resource-keyed token and canonical host:port dial
// target, IPv6 re-bracketing, and the IP-literal TLS-SNI guard.
func applyKnockResult(common *v1.ClientCommonConfig, resourceID string, result *knock.Result) (string, error) {
	if common == nil {
		return "", errNilCommonConfig
	}
	if result == nil {
		return "", errNilKnockResult
	}
	token := result.ACTokens[resourceID]
	if token == "" {
		return "", fmt.Errorf("%w %q (available: %v)", errKnockACTokenMissing, resourceID, sortedKeys(result.ACTokens))
	}
	raw := result.ResourceHost[resourceID]
	if raw == "" {
		return "", fmt.Errorf("%w %q", errKnockResourceHostMissing, resourceID)
	}
	host, port, err := ParseResourceHost(raw)
	if err != nil {
		return "", fmt.Errorf("%w: dial target %q for resource %q: %w", errKnockResourceHostUnusable, raw, resourceID, err)
	}
	if TLSEnabled(common) && common.Transport.TLS.ServerName == "" && IsIPLiteralHost(host) {
		return "", fmt.Errorf("%w: IP-literal dial target %q with TLS enabled and empty ServerName for resource %q", errKnockResourceHostUnusable, raw, resourceID)
	}
	if common.Metadatas == nil {
		common.Metadatas = map[string]string{}
	}
	common.Metadatas[frpgen.MetaQURLKnockToken] = token
	common.ServerAddr = host
	common.ServerPort = port
	return token, nil
}

var (
	errNilCommonConfig = errors.New("nil FRP common config")
	errNilKnockResult  = errors.New("nil knock result")
)

func commonKnockToken(common *v1.ClientCommonConfig) string {
	if common == nil || common.Metadatas == nil {
		return ""
	}
	return common.Metadatas[frpgen.MetaQURLKnockToken]
}

// knockingConnector wraps the FRP client's Connector so every physical dial
// is preceded by a gated knock refresh. Which method owns the physical dial
// depends on the transport (see physicalDialInOpen); the refresh is attached
// to exactly that one so each dial refreshes once.
type knockingConnector struct {
	base      frpclient.Connector
	ctx       context.Context
	common    *v1.ClientCommonConfig
	refresher *redialKnockRefresher
}

// Open refreshes the knock first on transports whose physical dial happens
// here, then opens the underlying connector.
func (c *knockingConnector) Open() error {
	if physicalDialInOpen(c.common) {
		if err := c.refresher.refresh(c.ctx, c.common, "open"); err != nil {
			return err
		}
	}
	return c.base.Open()
}

// Connect refreshes the knock first on transports whose physical dial happens
// here, then dials through the underlying connector.
func (c *knockingConnector) Connect() (net.Conn, error) {
	if !physicalDialInOpen(c.common) {
		if err := c.refresher.refresh(c.ctx, c.common, "connect"); err != nil {
			return nil, err
		}
	}
	return c.base.Connect()
}

// Close closes the underlying connector; the refresher holds no resources.
func (c *knockingConnector) Close() error {
	return c.base.Close()
}

// newKnockingConnectorCreator returns the ConnectorCreator the FRP service
// options accept, wrapping the default connector with the refresher.
func newKnockingConnectorCreator(refresher *redialKnockRefresher) func(context.Context, *v1.ClientCommonConfig) frpclient.Connector {
	return func(ctx context.Context, common *v1.ClientCommonConfig) frpclient.Connector {
		return &knockingConnector{
			base:      frpclient.NewConnector(ctx, common),
			ctx:       ctx,
			common:    common,
			refresher: refresher,
		}
	}
}

// physicalDialInOpen reports whether the pinned FRP fork performs the
// physical connector dial from Open (QUIC, nil TCPMux, or TCPMux-enabled TCP)
// or from Connect (TCPMux explicitly disabled). Revisit on an FRP connector
// contract change.
func physicalDialInOpen(common *v1.ClientCommonConfig) bool {
	if common == nil {
		return true
	}
	if strings.EqualFold(common.Transport.Protocol, "quic") {
		return true
	}
	if common.Transport.TCPMux == nil {
		return true
	}
	return *common.Transport.TCPMux
}
