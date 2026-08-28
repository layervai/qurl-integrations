package main

import (
	"os"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const (
	connectorModule = "github.com/layervai/qurl-connector"
	connectorFloor  = "v0.8.3"
	cliRepoRoot     = "../../.."
	goModPath       = cliRepoRoot + "/go.mod"
)

// TestConnectorFloorPreservesSuccessfulRecoveryHandoff keeps the released CLI
// on a connector version that persists an authenticated assignment across the
// recovery-to-serving process handoff. An older version can repeat the Hub
// recovery request after a successful recovery and make local publish fail.
// The floor rejects only older versions; qurl-connector's cross-process tests
// own the behavioral contract for accepted current and future versions.
// TODO(upstream-contract): Keep this floor in lockstep with qurl-connector's
// successful-refresh handoff contract and its cross-process recovery tests.
func TestConnectorFloorPreservesSuccessfulRecoveryHandoff(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read %s: %v", goModPath, err)
	}
	parsed, err := modfile.ParseLax(goModPath, raw, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", goModPath, err)
	}

	for _, replacement := range parsed.Replace {
		if replacement.Old.Path == connectorModule {
			t.Fatalf("%s: %s must not be replaced: released CLI must use the reviewed public module", goModPath, connectorModule)
		}
	}

	for _, requirement := range parsed.Require {
		if requirement.Mod.Path != connectorModule {
			continue
		}
		version := requirement.Mod.Version
		if !connectorVersionAtLeast(version, connectorFloor) {
			t.Fatalf("%s: %s version = %q, want %s or newer", goModPath, connectorModule, version, connectorFloor)
		}
		return
	}

	t.Fatalf("%s: %s is not required by the released CLI dependency graph", goModPath, connectorModule)
}

func connectorVersionAtLeast(version, floor string) bool {
	if !semver.IsValid(version) || !semver.IsValid(floor) {
		return false
	}
	if !module.IsPseudoVersion(version) {
		return semver.Compare(version, floor) >= 0
	}
	base, err := module.PseudoVersionBase(version)
	if err != nil || base == "" {
		return false
	}
	// A pseudo-version after the floor release has the floor as its canonical
	// parent (for example, v0.8.4-0... is based on v0.8.3). Comparing that
	// parent accepts later unreleased fixes without accepting a pseudo-version
	// based on the older v0.8.2 line.
	return semver.Compare(base, floor) >= 0
}

func TestConnectorVersionFloor(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		version string
		want    bool
	}{
		{version: "v0.8.3", want: true},
		{version: "v0.8.4", want: true},
		{version: "v0.8.4-rc.1", want: true},
		{version: "v0.8.4-0.20260828010203-abcdefabcdef", want: true},
		{version: "v0.8.2"},
		{version: "v0.8.3-rc.1"},
		{version: "v0.8.3-0.20260828010203-abcdefabcdef"},
		{version: "v0.0.0-20260828010203-abcdefabcdef"},
		{version: "not-a-version"},
	} {
		t.Run(test.version, func(t *testing.T) {
			t.Parallel()
			if got := connectorVersionAtLeast(test.version, connectorFloor); got != test.want {
				t.Fatalf("connectorVersionAtLeast(%q, %q) = %t, want %t", test.version, connectorFloor, got, test.want)
			}
		})
	}
}
