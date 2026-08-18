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
	// requests a cycle restart carrying errTooManyKnockFailures.
	redialKnockMaxFailures = 5
)

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

	mu                 sync.Mutex
	lastKnockAt        time.Time
	consecutiveFailure int
	requestRestart     func(error)
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
	// injected test clock drive the arithmetic deterministically.
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	if r.lastKnockAt.IsZero() && commonKnockToken(common) != "" {
		// First-cycle handoff: the supervisor already knocked and stamped
		// this cycle's token. Start the redial gate at handoff time so quick
		// connector retries stay inside the same admission window.
		r.lastKnockAt = now
		return nil
	}
	if !r.lastKnockAt.IsZero() {
		if wait := r.gate - now.Sub(r.lastKnockAt); wait > 0 {
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
	r.lastKnockAt = now
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
		r.requestRestart(fmt.Errorf("%w: %d consecutive redial knock refresh failures, last error: %w", errTooManyKnockFailures, r.consecutiveFailure, err))
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
