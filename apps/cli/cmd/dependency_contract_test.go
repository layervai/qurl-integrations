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
	cliRepoRoot     = "../../.."
	goModPath       = cliRepoRoot + "/go.mod"
)

// TestConnectorUsesReleasedDirectDependency keeps go.mod as the only source of
// truth for the connector version shipped in the CLI. The connector repository
// owns its behavior tests; the packaged CLI journey tests that exact dependency.
func TestConnectorUsesReleasedDirectDependency(t *testing.T) {
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
		if requirement.Indirect {
			return fmt.Errorf("%s: %s must be a direct requirement", path, connectorModule)
		}
		if !semver.IsValid(version) || module.IsPseudoVersion(version) {
			return fmt.Errorf("%s: %s version = %q, want a tagged semantic version", path, connectorModule, version)
		}
		return nil
	}

	return fmt.Errorf("%s: %s is not required directly by the released CLI", path, connectorModule)
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
			name:    "indirect requirement",
			goMod:   "module example.com/cli\n\nrequire " + connectorModule + " v0.8.6 // indirect\n",
			wantErr: "must be a direct requirement",
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
			name:    "pseudo version",
			goMod:   "module example.com/cli\n\nrequire " + connectorModule + " v0.8.7-0.20260829010203-abcdefabcdef\n",
			wantErr: "want a tagged semantic version",
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
