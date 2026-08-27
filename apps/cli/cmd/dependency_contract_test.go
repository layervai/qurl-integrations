package main

import (
	"os"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

const (
	connectorModule = "github.com/layervai/qurl-connector"
	connectorFloor  = "v0.8.3"
	goModPath       = "../../../go.mod"
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
		if !semver.IsValid(version) || semver.Compare(version, connectorFloor) < 0 {
			t.Fatalf("%s: %s version = %q, want %s or newer", goModPath, connectorModule, version, connectorFloor)
		}
		return
	}

	t.Fatalf("%s: %s is not required by the released CLI dependency graph", goModPath, connectorModule)
}
