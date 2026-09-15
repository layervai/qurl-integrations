package apitest

import (
	"testing"

	"github.com/layervai/qurl-go/crid"
)

// TestDeriveCRIDMatchesSDK pins this package's derivation twin against the
// SDK's authoritative consumer rule: a derived CRID must parse under the
// local gate, classify into the right environment, and satisfy KeyMatches
// for exactly its own key. If the frozen contract constants here ever drift
// from the SDK, this fails.
func TestDeriveCRIDMatchesSDK(t *testing.T) {
	key := GenerateResourceKey(t)

	for _, tc := range []struct {
		version byte
		env     crid.Environment
	}{
		{VersionProduction, crid.EnvironmentProduction},
		{VersionTest, crid.EnvironmentTest},
	} {
		value := DeriveCRID(t, key.DER, tc.version)
		parsed, err := crid.Parse(value)
		if err != nil {
			t.Fatalf("derived CRID rejected by the SDK gate: %v", err)
		}
		if !parsed.Known() || parsed.Environment() != tc.env {
			t.Errorf("version %#02x: known=%v env=%v", tc.version, parsed.Known(), parsed.Environment())
		}
		ok, err := crid.KeyMatches(value, key.DER)
		if err != nil || !ok {
			t.Errorf("KeyMatches(own key) = %v, %v; want true", ok, err)
		}
		other := GenerateResourceKey(t)
		ok, err = crid.KeyMatches(value, other.DER)
		if err != nil || ok {
			t.Errorf("KeyMatches(other key) = %v, %v; want false", ok, err)
		}
	}
}

// TestFixedResourceKeyIsStable pins the golden fixture: golden files embed
// this CRID and resource id, so the fixture must never drift.
func TestFixedResourceKeyIsStable(t *testing.T) {
	key := FixedResourceKey(t)
	const wantCRID = "qhtpthw4qt7wkw7khghr6x3z4hsfyn4zbuyhnee4i6bi67yu6yytgvwdbb4q"
	if key.CRID != wantCRID {
		t.Errorf("fixture CRID drifted: %s", key.CRID)
	}
	if len(key.ResourceID) != 122 {
		t.Errorf("fixture resource id length = %d, want 122", len(key.ResourceID))
	}
	if ok, err := crid.KeyMatches(key.CRID, key.DER); err != nil || !ok {
		t.Errorf("fixture key does not derive its own CRID: %v %v", ok, err)
	}
}
