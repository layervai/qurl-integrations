package daemon

import (
	"strings"
	"testing"
)

func TestParseGroupModeAcceptsOnlyCanonicalSpellings(t *testing.T) {
	for value, want := range map[string]GroupMode{"single": GroupModeSingle, "per-share": GroupModePerShare} {
		got, err := ParseGroupMode(value)
		if err != nil || got != want {
			t.Errorf("ParseGroupMode(%q) = (%q, %v), want %q", value, got, err, want)
		}
	}
	// The mode lands in the durable job definition and the job version, so a
	// near-miss is refused rather than normalized.
	for _, value := range []string{"", " single", "Single", "per_share", "pershare", "both", "single "} {
		got, err := ParseGroupMode(value)
		if err == nil || got != "" || !strings.Contains(err.Error(), "single or per-share") {
			t.Errorf("ParseGroupMode(%q) = (%q, %v), want a rejection naming both modes", value, got, err)
		}
	}
}

func TestGroupModeValuesListDefaultFirstAndAllParse(t *testing.T) {
	values := GroupModeValues()
	if len(values) != 2 || values[0] != string(DefaultGroupMode) {
		t.Fatalf("GroupModeValues() = %v, want the default %q first", values, DefaultGroupMode)
	}
	for _, value := range values {
		if _, err := ParseGroupMode(value); err != nil {
			t.Errorf("documented mode %q does not parse: %v", value, err)
		}
	}
}
