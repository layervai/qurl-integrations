package internal

import (
	"encoding/hex"
	"testing"
)

// TestIdempotencyKey_StableForSameInputs is the load-bearing
// determinism check — Slack's slash-command retry on a 3s deadline
// miss reuses the same `(team, channel, user, trigger)` tuple, so
// the resulting Idempotency-Key MUST be identical across invocations
// or the qURL service will mint twice for one user click.
func TestIdempotencyKey_StableForSameInputs(t *testing.T) {
	t.Parallel()
	k1 := IdempotencyKey("T1", "C1", "U1", "tr_abc")
	k2 := IdempotencyKey("T1", "C1", "U1", "tr_abc")
	if k1 != k2 {
		t.Fatalf("same inputs produced different keys: %q vs %q", k1, k2)
	}
}

// TestIdempotencyKey_Length pins the wire shape — 64 hex chars per
// sha256 output. The qURL API floor is 32; we double it to leave
// room for future field additions without touching the wire contract.
func TestIdempotencyKey_Length(t *testing.T) {
	t.Parallel()
	k := IdempotencyKey("T1", "C1", "U1", "tr_abc")
	if len(k) != 64 {
		t.Errorf("key length = %d, want 64", len(k))
	}
	if _, err := hex.DecodeString(k); err != nil {
		t.Errorf("key is not valid hex: %v", err)
	}
}

// TestIdempotencyKey_DifferentInputsDiffer fences the no-collisions
// invariant for every field individually. A field that gets dropped
// from the hash by mistake (e.g., a refactor that forgets one
// `h.Write`) would let two different requests collide and the qURL
// service would silently dedupe one out.
func TestIdempotencyKey_DifferentInputsDiffer(t *testing.T) {
	t.Parallel()
	base := IdempotencyKey("T1", "C1", "U1", "tr_abc")
	cases := []struct {
		name string
		key  string
	}{
		{"team differs", IdempotencyKey("T2", "C1", "U1", "tr_abc")},
		{"channel differs", IdempotencyKey("T1", "C2", "U1", "tr_abc")},
		{"user differs", IdempotencyKey("T1", "C1", "U2", "tr_abc")},
		{"trigger differs", IdempotencyKey("T1", "C1", "U1", "tr_xyz")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.key == base {
				t.Errorf("%s: key collided with base %q", tc.name, base)
			}
		})
	}
}

// TestIdempotencyKey_FieldBoundary fences the field-boundary
// invariant. Without an unambiguous boundary between fields,
// ("ab","c") and ("a","bc") would hash identically — both are
// "abc" concatenated — and a request with a team_id that happened
// to be a prefix of another team's would collide. The hash
// length-prefixes each field with a 4-byte BE uint32, so the
// collision is structurally impossible regardless of what byte
// shape Slack ships in future ID formats.
func TestIdempotencyKey_FieldBoundary(t *testing.T) {
	t.Parallel()
	// Inputs mirror the ("ab","c") vs ("a","bc") shape from the
	// comment so the regression is obvious-by-eye.
	a := IdempotencyKey("ab", "c", "U1", "tr_abc")
	b := IdempotencyKey("a", "bc", "U1", "tr_abc")
	if a == b {
		t.Errorf("field-boundary collision: %q == %q", a, b)
	}
}

// TestIdempotencyKey_ViewIDSubstitution documents the view-submission
// usage pattern: callers pass `view.id` in place of `triggerID`. The
// helper has no knowledge of which is which, so this test exists
// purely as a regression fence — if a refactor ever splits trigger
// and view paths into separate functions, this test should fail to
// compile and force the caller doc to update.
func TestIdempotencyKey_ViewIDSubstitution(t *testing.T) {
	t.Parallel()
	fromTrigger := IdempotencyKey("T1", "C1", "U1", "tr_abc")
	fromView := IdempotencyKey("T1", "C1", "U1", "V0123456789")
	if fromTrigger == fromView {
		t.Error("trigger ID and view ID produced the same key — substitution semantic broke")
	}
}
