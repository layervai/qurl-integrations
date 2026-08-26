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
// TODO(upstream-contract): mirrors qurl-service's knock-token Login reject
// path — the knock_invalid wire tag it prefixes every knock-token denial
// with. TestQRTSKnockTokenLoginContract binds this set to the checked-in
// snapshot (qrts_knock_token_login_wire_contract.json in this package), so
// needle and JSON cannot drift apart; change needle, JSON, and the server
// side together.
//
// That guard does not cover the half this marker is for, which is why the
// marker is here and not just the pin. It compares two files in this
// package: rename the tag upstream and the needle and the snapshot still
// agree, the suite stays green, and the classifier silently stops firing.
// The snapshot is compared against the producer's own copy only when
// QRTS_KNOCK_TOKEN_LOGIN_CONTRACT points at it, and nothing in this repo
// sets that variable. That is deliberate, not an omission: the producer
// repository is private and this one is public, so a job here that fetched the
// producer's copy would need a credential and would write private bytes into a
// public CI log. The binding therefore belongs to the producer, which pulls
// this public snapshot with no credentials at all — see the consumer-contract
// workflow there.
//
// The consequence for THIS repo's CI stands: the producer side goes unchecked
// here, and a producer-side rename surfaces as a red build over there, not a
// red build here. Keep the env hook — it is what the producer's job and a
// local run against a qRTS checkout both use.
//
// Note the NewProxy reject surface is already fenced that way. This Login
// surface is not yet: qRTS has no committed producer fixture for it, only for
// the recoverable NewProxy rejects.
//
// TestForkEmitsTheLoginFailurePrefixThisPackageMatches closes a different
// half — each snapshot wire text really does reach this matcher through a
// real frps and frpc — but it rejects with the snapshot's own strings, so it
// cannot tell you the server still emits them either.
//
// History: a pre-contract needle set once pinned strings the server never
// emitted, so the classifier silently never fired on a real reject — the
// exact drift this becomes a test failure for once both sides are compared.
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
//  3. ErrProxyNotServing — authenticated but route registration failed;
//  4. typed net.Error — transport-layer failure, a stable stdlib surface
//     that survives FRP wording changes;
//  5. the FRP client's own Login-stage phrasing, then dial-error substrings —
//     the fallback for wraps that lost the typed identity;
//  6. everything else — frp_runtime_error.
//
// Deliberately absent: errReconnectStalled. A stalled cycle always exits
// through cycleRunner's restartErr, and BOTH call sites here are unreachable
// on that path — emitSessionEvents only classifies inside its !healthy branch
// (restartErr != nil makes the cycle healthy), and teardownCause returns
// "connector_restart" for a restart before it ever classifies. A branch for it
// would be dead code with a test that looked like coverage. The stall is
// reported directly instead, as event=reconnect_stalled from the watchdog and
// event=reconnect_stall_counted from the budget.
//
// TODO(upstream-contract): mirrors github.com/layervai/frp v1.0.0
// client/service.go — Run's `fmt.Errorf("login to the server failed: %v. With
// loginFailExit enabled, no additional retries will be attempted", ...)`, the
// fork's only construction of the text the login_failed case below matches.
// It is upstream prose, not a wire value: nothing in this module makes it
// fail to compile, so a reword there silently rebuckets every Login-stage
// failure as frp_runtime_error — no compile error, no test failure, just
// dashboards that stop separating "the tunnel server refused us" from "the
// tunnel ran and broke". Change the case below, contract_test.go's
// frpLoginWrap, and this version together on a fork bump.
//
// Two properties of that wrap are worth keeping in view, because both widen
// what the case is responsible for:
//
//   - The supervisor forces LoginFailExit true on every per-cycle config
//     clone (forceLoginFailExit, pinned by TestKnockForcesLoginFailExit), so
//     this is the fork's live exit for a cycle whose Login never succeeded —
//     not a corner reachable only under unusual config.
//   - The wrap is a fresh fmt.Errorf with %v, not %w. Nothing inside it is
//     retrievable with errors.Is, so a caller cancellation that races the
//     login loop reaches this switch rather than the context_canceled branch
//     above, arriving as "login to the server failed: <nil>" (the fork leaves
//     the cause empty when the cancel was not its own, and says "loginFailExit
//     enabled" either way). Both call sites classify it: emitSessionEvents
//     through its !healthy branch, teardownCause because its errors.Is guard
//     does not match.
//
// TestForkEmitsTheLoginFailurePrefixThisPackageMatches and
// TestForkLoginWrapOutranksTheDialSubstrings drive the real fork to a real
// failed Login and assert the prefix and the ordering, so a reword reddens a
// test rather than only contradicting this comment.
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
	if errors.Is(err, ErrProxyNotServing) {
		return "proxy_not_serving"
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
