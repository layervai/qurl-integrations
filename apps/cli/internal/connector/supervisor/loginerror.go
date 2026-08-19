package supervisor

import (
	"context"
	"errors"
	"net"
	"strings"
)

// IsTokenLoginError is the supervisor's heuristic for "the tunnel Login was
// rejected because the server refused our knock token". On true the cycle
// counts against the unified unhealthy-knock budget and the next cycle
// re-knocks; on false the supervisor still reconnects, so a false negative
// costs one extra cycle's log line and nothing else.
//
// Heuristic shape: substring match against a narrow needle set owned by the
// tunnel server's reject path. Substring is necessary because the FRP client
// wraps the upstream RejectReason in "login to the server failed: <wrapped>"
// before the supervisor sees it; the matcher must tolerate case changes,
// extra wrapping, and additive detail suffixes. The set deliberately excludes
// generic prose like "token expired" or "access token" — surfaces that would
// false-positive on OAuth/JWT/TLS error strings that have nothing to do with
// knock tokens.
func IsTokenLoginError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range tokenErrorNeedles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// tokenErrorNeedles is the lowercased substring set IsTokenLoginError scans
// for. Every entry must be a string the tunnel server's reject path actually
// emits in its Login RejectReason: today that is exactly the knock_invalid
// wire tag that prefixes every knock-token Login denial ("knock_invalid:
// knock token rejected", "knock_invalid: knock-token validation unavailable",
// "knock_invalid: knock token expired").
//
// CROSS-REPO PIN: this set is bound to the checked-in contract snapshot
// (qrts_knock_token_login_wire_contract.json in this package) by
// TestQRTSKnockTokenLoginContract. Change needle, JSON, and the server side
// together. History: a pre-contract needle set once pinned strings the server
// never emitted, so the classifier silently never fired on a real reject —
// the exact drift the contract turns into a test failure.
var tokenErrorNeedles = []string{
	"knock_invalid", // server-owned wire tag; prefixes every knock-token Login denial
}

// reasonDialError is the classification bucket for transport-layer failures,
// shared by the typed and substring branches below.
const reasonDialError = "dial_error"

// classifyRunError buckets one cycle's tunnel-runtime error into a short
// reason tag for the structured-log stream; the log entry carries the
// original message verbatim for forensic detail, the reason exists for
// dashboard grouping. Most-specific first:
//
//  1. context cancellation — the caller asked to stop;
//  2. ErrTooManyKnockFailures — a knock budget exhausted;
//  3. typed net.Error — transport-layer failure, a stable stdlib surface
//     that survives FRP wording changes;
//  4. the FRP client's own Login-stage phrasing, then dial-error substrings —
//     the fallback for wraps that lost the typed identity;
//  5. everything else — frp_runtime_error.
//
// Deliberately absent: errReconnectStalled. A stalled cycle always exits
// through cycleRunner's restartErr, and BOTH call sites here are unreachable
// on that path — emitSessionEvents only classifies inside its !healthy branch
// (restartErr != nil makes the cycle healthy), and teardownCause returns
// "connector_restart" for a restart before it ever classifies. A branch for it
// would be dead code with a test that looked like coverage. The stall is
// reported directly instead, as event=reconnect_stalled from the watchdog and
// event=reconnect_stall_counted from the budget.
func classifyRunError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "context_canceled"
	}
	if errors.Is(err, ErrTooManyKnockFailures) {
		return "too_many_knock_failures"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return reasonDialError
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return reasonDialError
	}
	// Order matters: the FRP client's most common Login-stage failure wraps a
	// dial error ("login to the server failed: dial tcp …: i/o timeout"), and
	// it should bucket by the Login stage that surfaced it.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "login to the server failed"):
		return "login_failed"
	case strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "dial tcp"),
		strings.Contains(msg, "dial udp"):
		return reasonDialError
	}
	return "frp_runtime_error"
}
