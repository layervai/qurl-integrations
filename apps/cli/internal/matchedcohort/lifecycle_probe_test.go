//go:build linux && !android

package matchedcohort

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBinaryGetProbeUsesOnlyFileBackedKeyAndVerifiesExactBytes(t *testing.T) {
	for _, name := range []string{"QURL_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "AUTH0_CLIENT_SECRET", "AUTH0_MANAGEMENT_TOKEN"} {
		t.Setenv(name, "must-not-reach-child")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // Private test directory must be owner-only and searchable.
		t.Fatal(err)
	}
	key := filepath.Join(directory, "api-key")
	deployment := filepath.Join(directory, "deployment.json")
	log := filepath.Join(directory, "env-log")
	binary := filepath.Join(directory, "qurl")
	if err := os.WriteFile(key, []byte("lv_test_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deployment, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"set -eu\n" +
		"test \"${QURL_API_KEY+x}\" != x\n" +
		"test \"${AWS_ACCESS_KEY_ID+x}\" != x\n" +
		"test \"${AWS_SECRET_ACCESS_KEY+x}\" != x\n" +
		"test \"${AWS_SESSION_TOKEN+x}\" != x\n" +
		"test \"${GITHUB_TOKEN+x}\" != x\n" +
		"test \"${GH_TOKEN+x}\" != x\n" +
		"test \"${AUTH0_CLIENT_SECRET+x}\" != x\n" +
		"test \"${AUTH0_MANAGEMENT_TOKEN+x}\" != x\n" +
		"test \"$QURL_API_KEY_FILE\" = '" + key + "'\n" +
		"test \"$QURL_DEPLOYMENT\" != '" + deployment + "'\n" +
		"cmp -s \"$QURL_DEPLOYMENT\" '" + deployment + "'\n" +
		"printf '%s\\n' \"$5\" >>'" + log + "'\n" +
		"case \"$5\" in crid-primary) printf primary >\"$7\";; crid-sibling) printf sibling >\"$7\";; *) exit 9;; esac\n"
	if err := os.WriteFile(binary, []byte(script), 0o500); err != nil { //nolint:gosec // Executable fixture is owner-only.
		t.Fatal(err)
	}
	probe := &BinaryGetProbe{Binary: binary, APIEndpoint: "https://api.sandbox.example", APIKeyFile: key,
		BinarySHA256: stableProbeFileDigest(binary, true), DeploymentFile: deployment,
		DeploymentSHA256: stableProbeFileDigest(deployment, false), StateRoot: directory,
		Expected: map[string][]byte{"direct-a": []byte("primary"), "direct-b": []byte("sibling")}, Timeout: 10 * time.Second}
	primary := FixedIdentity{Label: "direct-a", CRID: "crid-primary"}
	sibling := FixedIdentity{Label: "direct-b", CRID: "crid-sibling"}
	if err := probe.Both(context.Background(), primary, sibling); err != nil {
		t.Fatalf("Both: %v", err)
	}
	if err := probe.Sibling(context.Background(), sibling); err != nil {
		t.Fatalf("Sibling: %v", err)
	}
	raw, err := os.ReadFile(log) //nolint:gosec // Log is a fixed child of the private test directory.
	if err != nil || strings.Count(string(raw), "crid-primary") != 1 || strings.Count(string(raw), "crid-sibling") != 2 {
		t.Fatalf("probe calls = %q, %v", raw, err)
	}
}

func TestBinaryGetProbeExecutesOnlyOpenedBinaryAndDeploymentInodes(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // Private test directory must be owner-only and searchable.
		t.Fatal(err)
	}
	key := filepath.Join(directory, "key")
	deployment := filepath.Join(directory, "deployment")
	binary := filepath.Join(directory, "qurl")
	marker := filepath.Join(directory, "marker")
	if err := os.WriteFile(key, []byte("lv_test_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deployment, []byte("reviewed-deployment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reviewed := "#!/bin/sh\nset -eu\ngrep -qx reviewed-deployment \"$QURL_DEPLOYMENT\"\nprintf reviewed >'" + marker + "'\nprintf exact >\"$7\"\n"
	if err := os.WriteFile(binary, []byte(reviewed), 0o500); err != nil { //nolint:gosec // Exact fixture executable is owner-only and not writable.
		t.Fatal(err)
	}
	probe := &BinaryGetProbe{Binary: binary, APIEndpoint: "https://api.sandbox.example", APIKeyFile: key,
		BinarySHA256: stableProbeFileDigest(binary, true), DeploymentFile: deployment,
		DeploymentSHA256: stableProbeFileDigest(deployment, false), StateRoot: directory,
		Expected: map[string][]byte{"direct-a": []byte("exact")}, Timeout: 10 * time.Second}
	probe.beforeExec = func() {
		if err := os.Rename(binary, binary+".reviewed"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf replaced >'"+marker+"'\nprintf malicious >\"$7\"\n"), 0o500); err != nil { //nolint:gosec // Deliberate replacement executable.
			t.Fatal(err)
		}
		if err := os.Rename(deployment, deployment+".reviewed"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(deployment, []byte("replacement-deployment\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err := probe.Sibling(context.Background(), FixedIdentity{Label: "direct-a", CRID: "crid-primary"})
	if err == nil || !strings.Contains(err.Error(), "authority changed during execution") {
		t.Fatalf("path replacement result = %v", err)
	}
	raw, readErr := os.ReadFile(marker) //nolint:gosec // Marker is a fixed private test child.
	if readErr != nil || string(raw) != "reviewed" {
		t.Fatalf("executed marker = %q, %v", raw, readErr)
	}
}

func TestBinaryGetProbeRejectsUnsafeMetadataBeforeExecution(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil { //nolint:gosec // Private test directory must be owner-only and searchable.
		t.Fatal(err)
	}
	for _, name := range []string{"qurl", "key", "deployment"} {
		mode := os.FileMode(0o600)
		if name == "qurl" {
			mode = 0o500
		}
		if err := os.WriteFile(filepath.Join(directory, name), []byte("x\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	probe := &BinaryGetProbe{Binary: filepath.Join(directory, "qurl"), APIEndpoint: "https://api.sandbox.example",
		APIKeyFile: filepath.Join(directory, "key"), DeploymentFile: filepath.Join(directory, "deployment"),
		StateRoot: directory, Expected: map[string][]byte{"direct-a": []byte("value")}, Timeout: time.Second}
	probe.BinarySHA256 = stableProbeFileDigest(probe.Binary, true)
	probe.DeploymentSHA256 = stableProbeFileDigest(probe.DeploymentFile, false)
	identity := FixedIdentity{Label: "direct-a", CRID: "crid-primary"}
	if err := os.Chmod(probe.APIKeyFile, 0o640); err != nil { //nolint:gosec // Deliberately unsafe group-readable mutation.
		t.Fatal(err)
	}
	if err := probe.Sibling(context.Background(), identity); err == nil {
		t.Fatal("group-readable key file reached binary")
	}
	if err := os.Chmod(probe.APIKeyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	probe.APIEndpoint = "http://api.sandbox.example"
	if err := probe.Sibling(context.Background(), identity); err == nil {
		t.Fatal("non-HTTPS endpoint reached binary")
	}
}
