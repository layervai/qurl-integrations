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

// IsSessionConflictError is the supervisor's heuristic for "the tunnel Login
// was refused because this Connector already has a session registered
// server-side". The tunnel server keys one live session per Connector; a
// Login presenting a different RunID while the previous registration is still
// marked online is refused outright, and stays refused until the server
// releases that registration.
//
// This is the ONE stale-session condition the platform reports in words. It
// arrives as the server's LoginResp error, which the FRP client surfaces
// verbatim inside "login to the server failed: <wrapped>", so the matcher is
// substring-based like IsTokenLoginError and tolerates the wrap, case changes
// and additive suffixes.
//
// Two blind spots, both deliberate and both fail-safe (they classify as
// "not a conflict" and leave the ordinary connectivity reading in place):
//
//   - A server configured with detailed errors off replaces the sentence with
//     the bare summary "register control error", which is indistinguishable
//     from any other Login-stage refusal.
//   - A refusal that never completes a Login exchange — the connection is
//     accepted and then dropped — surfaces as a transport error from the
//     multiplexer ("session shutdown", "connection write timeout") with no
//     server-supplied reason at all. Those are byte-identical to a real
//     network outage and MUST NOT be read as a stale session; the reconnect
//     watchdog in refresher.go bounds them on duration instead of guessing.
func IsSessionConflictError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, pair := range sessionConflictNeedlePairs {
		if strings.Contains(msg, pair[0]) && strings.Contains(msg, pair[1]) {
			return true
		}
	}
	return false
}

// sessionConflictNeedlePairs is the lowercased AND-pair set
// IsSessionConflictError scans for: a candidate matches only when BOTH halves
// are present. Pairing is what keeps the match narrow — "is already online"
// alone is ordinary English that a future server could emit about a proxy, a
// route or a peer, and a false positive here tells the operator to wait out a
// stale session while the real fault is elsewhere.
//
// CROSS-REPO PIN: bound to qrts_session_conflict_login_wire_contract.json in
// this package by TestQRTSSessionConflictLoginContract, which also asserts the
// pairs do NOT match the neighboring rejections (knock rejects, the
// detail-suppressed summary, and the bare multiplexer transport errors).
// Change pairs, JSON, and the server side together.
var sessionConflictNeedlePairs = [][2]string{
	// server-owned: the duplicate-registration refusal names the conflict
	// scope and the state that blocks the Login.
	{"client_id", "is already online"},
}

// reasonDialError is the classification bucket for transport-layer failures,
// shared by the typed and substring branches below.
const reasonDialError = "dial_error"

// reasonSessionConflict is the classification bucket for a Login the server
// refused because this Connector's previous session is still registered.
const reasonSessionConflict = "session_conflict"

// classifyRunError buckets one cycle's tunnel-runtime error into a short
// reason tag for the structured-log stream; the log entry carries the
// original message verbatim for forensic detail, the reason exists for
// dashboard grouping. Most-specific first:
//
//  1. context cancellation — the caller asked to stop;
//  2. ErrTooManyKnockFailures — a knock budget exhausted;
//  3. typed net.Error — transport-layer failure, a stable stdlib surface
//     that survives FRP wording changes;
//  4. a stalled post-admission reconnect — this package's own watchdog;
//  5. the server's duplicate-session refusal, which arrives wrapped in the
//     Login-stage phrasing and so must be read before it;
//  6. the FRP client's own Login-stage phrasing, then dial-error substrings —
//     the fallback for wraps that lost the typed identity;
//  7. everything else — frp_runtime_error.
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
	if errors.Is(err, errReconnectStalled) {
		return reasonReconnectStalled
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
	if IsSessionConflictError(err) {
		return reasonSessionConflict
	}
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
