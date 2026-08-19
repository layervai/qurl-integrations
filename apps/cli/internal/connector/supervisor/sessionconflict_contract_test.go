package supervisor

import (
	_ "embed"
	"errors"
	"strings"
	"testing"
)

// qrtsSessionConflictLoginContract is the checked-in consumer snapshot of the
// tunnel server's duplicate-session Login refusal. The server refuses a Login
// whose Connector already has a registration marked online under a different
// RunID; this file pins the wording that refusal reaches the client with, and
// the neighboring rejections it must not be confused with.
//
//go:embed qrts_session_conflict_login_wire_contract.json
var qrtsSessionConflictLoginContractJSON []byte

type qrtsSessionConflictLoginContract struct {
	SchemaVersion        int         `json:"schema_version"`
	ConflictScope        string      `json:"conflict_scope"`
	ClientNeedlePairs    [][2]string `json:"client_needle_pairs"`
	LoginRejectWireTexts []string    `json:"login_reject_wire_texts"`
	NonConflictWireTexts []string    `json:"non_conflict_wire_texts"`
}

// TestQRTSSessionConflictLoginContract binds the snapshot to
// IsSessionConflictError: the production pair set is the contract's, every
// pinned refusal classifies through the FRP Login wrap, and every neighboring
// rejection does not. The negative half is the load-bearing one — a matcher
// that also fired on "session shutdown" would relabel a real outage as a stale
// session, which is worse than saying nothing.
func TestQRTSSessionConflictLoginContract(t *testing.T) {
	t.Parallel()
	var contract qrtsSessionConflictLoginContract
	decodeStrictJSON(t, "CLI snapshot", qrtsSessionConflictLoginContractJSON, &contract)

	if contract.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", contract.SchemaVersion)
	}
	if len(contract.ClientNeedlePairs) != len(sessionConflictNeedlePairs) {
		t.Fatalf("contract has %d needle pair(s), production has %d; update loginerror.go and the JSON together",
			len(contract.ClientNeedlePairs), len(sessionConflictNeedlePairs))
	}
	for i, want := range contract.ClientNeedlePairs {
		if sessionConflictNeedlePairs[i] != want {
			t.Errorf("sessionConflictNeedlePairs[%d] = %q, contract client_needle_pairs[%d] = %q; update loginerror.go and the JSON together",
				i, sessionConflictNeedlePairs[i], i, want)
		}
	}

	// Every pinned refusal must classify — wrapped, uppercased, further
	// wrapped, and with an additive suffix, the same tolerances the
	// knock-token matcher promises.
	for _, wireText := range contract.LoginRejectWireTexts {
		t.Run("conflict/"+wireText, func(t *testing.T) {
			t.Parallel()
			for name, variant := range map[string]string{
				"wrapped":       frpLoginWrap + wireText,
				"uppercased":    strings.ToUpper(frpLoginWrap + wireText),
				"extra_wrap":    "cycle 3: " + frpLoginWrap + wireText,
				"detail_suffix": frpLoginWrap + wireText + " (trace abc123)",
			} {
				if !IsSessionConflictError(errors.New(variant)) {
					t.Errorf("IsSessionConflictError(%s %q) = false, want true", name, variant)
				}
			}
		})
	}

	// Every neighboring rejection must NOT classify. These are the strings a
	// Connector actually sees around this one: the knock-token reject, the
	// summary a detail-suppressing server sends instead of the sentence, and
	// the bare multiplexer transport errors that carry no server reason.
	for _, wireText := range contract.NonConflictWireTexts {
		if IsSessionConflictError(errors.New(frpLoginWrap + wireText)) {
			t.Errorf("IsSessionConflictError(%q) = true, want false — a neighboring rejection must not read as a stale session", wireText)
		}
	}

	// Each half of a pair alone must not classify: pairing is what keeps
	// ordinary prose containing one half from tripping the matcher.
	for _, pair := range contract.ClientNeedlePairs {
		for _, half := range pair {
			if IsSessionConflictError(errors.New(frpLoginWrap + half)) {
				t.Errorf("IsSessionConflictError matched on the lone needle %q; both halves of a pair are required", half)
			}
		}
	}
	if IsSessionConflictError(nil) {
		t.Error("IsSessionConflictError(nil) = true, want false")
	}
}

// TestClassifyRunErrorSessionConflictOutranksLoginFailed pins the ordering
// inside classifyRunError: the server's refusal arrives wrapped in the FRP
// client's Login-stage phrasing, so a matcher checked after that phrasing
// would bucket every conflict as the generic login_failed and the reason tag
// would never appear on a dashboard.
func TestClassifyRunErrorSessionConflictOutranksLoginFailed(t *testing.T) {
	t.Parallel()
	err := errors.New(frpLoginWrap + "client_id [c1] for user [u1] is already online")
	if got := classifyRunError(err); got != reasonSessionConflict {
		t.Errorf("classifyRunError(%q) = %q, want %q", err, got, reasonSessionConflict)
	}
	if got := classifyRunError(errors.New(frpLoginWrap + "connection refused")); got != "login_failed" {
		t.Errorf("classifyRunError of an ordinary login failure = %q, want login_failed", got)
	}
	stalled := errors.New("wrapped: " + errReconnectStalled.Error())
	if got := classifyRunError(stalled); got == reasonReconnectStalled {
		t.Errorf("classifyRunError matched the stall sentinel by text %q; it must match by identity so a lookalike string cannot forge it", stalled)
	}
	if got := classifyRunError(errors.Join(errReconnectStalled, errors.New("detail"))); got != reasonReconnectStalled {
		t.Errorf("classifyRunError(joined stall sentinel) = %q, want %q", got, reasonReconnectStalled)
	}
}
