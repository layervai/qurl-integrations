package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const (
	connectorModule = "github.com/layervai/qurl-connector"
	connectorFloor  = "v0.8.6"
	cliRepoRoot     = "../../.."
	goModPath       = cliRepoRoot + "/go.mod"
)

// TestConnectorFloorPreservesSuccessfulRecoveryHandoff keeps the released CLI
// on a connector version that persists an authenticated assignment across the
// recovery-to-serving process handoff and reports bounded, redacted retry
// failures. An older version can repeat the Hub recovery request after a
// successful recovery or hide the rejection that prevents local publish.
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
	if err := checkConnectorRequirement(goModPath, raw); err != nil {
		t.Fatal(err)
	}
}

func checkConnectorRequirement(path string, raw []byte) error {
	parsed, err := modfile.Parse(path, raw, nil)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	for _, replacement := range parsed.Replace {
		if replacement.Old.Path == connectorModule {
			return fmt.Errorf("%s: %s must not be replaced: released CLI must use the reviewed public module", path, connectorModule)
		}
	}

	for _, requirement := range parsed.Require {
		if requirement.Mod.Path != connectorModule {
			continue
		}
		version := requirement.Mod.Version
		if !connectorVersionAtLeast(version, connectorFloor) {
			return fmt.Errorf("%s: %s version = %q, want %s or newer", path, connectorModule, version, connectorFloor)
		}
		return nil
	}

	return fmt.Errorf("%s: %s is not required by the released CLI dependency graph", path, connectorModule)
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
	// parent (for example, v0.8.7-0... is based on v0.8.6). Comparing that
	// parent accepts later unreleased fixes without accepting a pseudo-version
	// based on the older v0.8.5 line.
	return semver.Compare(base, floor) >= 0
}

func TestConnectorVersionFloor(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		version string
		want    bool
	}{
		{version: "v0.8.6", want: true},
		{version: "v0.8.7-rc.1", want: true},
		{version: "v0.8.7-0.20260829010203-abcdefabcdef", want: true},
		{version: "v0.8.2"},
		{version: "v0.8.3"},
		{version: "v0.8.4"},
		{version: "v0.8.5"},
		{version: "v0.8.5-rc.1"},
		{version: "v0.8.5-0.20260829010203-abcdefabcdef"},
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

func TestCheckConnectorRequirement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		goMod   string
		wantErr string
	}{
		{
			name:  "direct requirement",
			goMod: "module example.com/cli\n\nrequire " + connectorModule + " v0.8.6\n",
		},
		{
			name:  "indirect requirement",
			goMod: "module example.com/cli\n\nrequire " + connectorModule + " v0.8.6 // indirect\n",
		},
		{
			name:  "incompatible requirement",
			goMod: "module example.com/cli\n\nrequire " + connectorModule + " v0.8.6+incompatible\n",
		},
		{
			name:    "missing requirement",
			goMod:   "module example.com/cli\n",
			wantErr: "is not required",
		},
		{
			name: "replaced requirement",
			goMod: "module example.com/cli\n\nrequire " + connectorModule + " v0.8.6\n" +
				"replace " + connectorModule + " => ../qurl-connector\n",
			wantErr: "must not be replaced",
		},
		{
			name:    "older requirement",
			goMod:   "module example.com/cli\n\nrequire " + connectorModule + " v0.8.5\n",
			wantErr: "want v0.8.6 or newer",
		},
		{
			name:    "malformed go.mod",
			goMod:   "module (\n",
			wantErr: "parse synthetic.mod",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := checkConnectorRequirement("synthetic.mod", []byte(test.goMod))
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("checkConnectorRequirement() error = %v, want text %q", err, test.wantErr)
			}
		})
	}
}
