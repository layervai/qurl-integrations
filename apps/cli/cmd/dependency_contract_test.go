package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

const (
	connectorModule = "github.com/layervai/qurl-connector"
	connectorFloor  = "v0.8.3"
)

// TestConnectorFloorPreservesSuccessfulRecoveryHandoff keeps the released CLI
// on a connector version that persists an authenticated assignment across the
// recovery-to-serving process handoff. An older version can repeat the Hub
// recovery request after a successful recovery and make local publish fail.
// TODO(upstream-contract): Keep this floor in lockstep with qurl-connector's
// successful-refresh handoff contract and its cross-process recovery tests.
func TestConnectorFloorPreservesSuccessfulRecoveryHandoff(t *testing.T) {
	t.Parallel()

	goModPath := filepath.Join("..", "..", "..", "go.mod")
	raw, err := os.ReadFile(goModPath) //nolint:gosec // G304: the path is a repository-relative test constant.
	if err != nil {
		t.Fatalf("read %s: %v", goModPath, err)
	}
	parsed, err := modfile.ParseLax(goModPath, raw, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", goModPath, err)
	}

	for _, replacement := range parsed.Replace {
		if replacement.Old.Path == connectorModule {
			t.Fatalf("%s must not be replaced: released CLI must use the reviewed public module", connectorModule)
		}
	}

	for _, requirement := range parsed.Require {
		if requirement.Mod.Path != connectorModule {
			continue
		}
		version := requirement.Mod.Version
		if !semver.IsValid(version) || semver.Compare(version, connectorFloor) < 0 {
			t.Fatalf("%s version = %q, want %s or newer", connectorModule, version, connectorFloor)
		}
		return
	}

	t.Fatalf("%s is not required by the released CLI module", connectorModule)
}
