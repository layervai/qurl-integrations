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
	qurlGoModule    = "github.com/layervai/qurl-go"
	cliRepoRoot     = "../../.."
	goModPath       = cliRepoRoot + "/go.mod"
)

// TestCLIUsesReleasedDirectDependencies keeps go.mod as the only source of
// truth for the first-party runtime versions shipped in the CLI. Their owning
// repositories test behavior; the packaged CLI journey tests these exact tags.
func TestCLIUsesReleasedDirectDependencies(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read %s: %v", goModPath, err)
	}
	for _, modulePath := range []string{connectorModule, qurlGoModule} {
		if err := checkReleasedDirectRequirement(goModPath, raw, modulePath); err != nil {
			t.Fatal(err)
		}
	}
}

func checkReleasedDirectRequirement(path string, raw []byte, modulePath string) error {
	parsed, err := modfile.Parse(path, raw, nil)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	for _, replacement := range parsed.Replace {
		if replacement.Old.Path == modulePath {
			return fmt.Errorf("%s: %s must not be replaced: released CLI must use the reviewed public module", path, modulePath)
		}
	}

	for _, requirement := range parsed.Require {
		if requirement.Mod.Path != modulePath {
			continue
		}
		version := requirement.Mod.Version
		if requirement.Indirect {
			return fmt.Errorf("%s: %s must be a direct requirement", path, modulePath)
		}
		if !semver.IsValid(version) || module.IsPseudoVersion(version) {
			return fmt.Errorf("%s: %s version = %q, want a tagged semantic version", path, modulePath, version)
		}
		return nil
	}

	return fmt.Errorf("%s: %s is not required directly by the released CLI", path, modulePath)
}

func TestCheckReleasedDirectRequirement(t *testing.T) {
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
			err := checkReleasedDirectRequirement("synthetic.mod", []byte(test.goMod), connectorModule)
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
