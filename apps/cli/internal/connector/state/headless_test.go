package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestLoadHeadlessConfigStrictContract(t *testing.T) {
	valid := testHeadlessYAML(t)
	cases := []struct {
		name string
		data string
		want string
	}{
		{name: "valid", data: valid},
		{name: "unknown field", data: valid + "\nfuture: true\n", want: "field future not found"},
		{name: "duplicate key", data: strings.Replace(valid, "version: 1", "version: 1\nversion: 1", 1), want: "already defined"},
		{name: "multiple documents", data: valid + "\n---\nversion: 1\n", want: "more than one YAML document"},
		{name: "zero shares", data: "version: 1\nshares: []\n", want: "exactly one share"},
		{name: "two shares", data: valid + strings.TrimPrefix(valid, "version: 1\nshares:\n"), want: "exactly one share"},
		{name: "off", data: strings.Replace(valid, "desired_state: on", "desired_state: off", 1), want: "desired_state on"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "share.yaml")
			if err := os.WriteFile(path, []byte(tc.data), 0o444); err != nil { // #nosec G306 -- read-only test config.
				t.Fatal(err)
			}
			got, err := LoadHeadlessConfig(path)
			if tc.want == "" {
				if err != nil || len(got.Shares) != 1 || got.Shares[0].DesiredState != "on" {
					t.Fatalf("LoadHeadlessConfig = %+v, %v", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReadOnlyProjectedFilesAndCredentialSafety(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "..data-token")
	if err := os.WriteFile(target, []byte("enrollment-value\n"), 0o440); err != nil { // #nosec G306 -- projected dedicated-group secret fixture.
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o440); err != nil { // #nosec G302 -- exact projected-secret mode under the test umask.
		t.Fatal(err)
	}
	link := filepath.Join(dir, "enrollment-token")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEnrollmentCredential(link)
	if err != nil || got != "enrollment-value" {
		t.Fatalf("projected credential = %q, %v", got, err)
	}

	worldReadable := filepath.Join(dir, "world-readable")
	if err := os.WriteFile(worldReadable, []byte("secret"), 0o444); err != nil { // #nosec G306 -- intentionally permissive bearer fixture.
		t.Fatal(err)
	}
	if err := os.Chmod(worldReadable, 0o444); err != nil { // #nosec G302 -- intentionally permissive bearer fixture.
		t.Fatal(err)
	}
	if _, err := ReadEnrollmentCredential(worldReadable); err == nil || !strings.Contains(err.Error(), "owner or a dedicated process group") {
		t.Fatalf("world-readable credential error = %v", err)
	}

	unsafe := filepath.Join(dir, "writable")
	if err := os.WriteFile(unsafe, []byte("secret"), 0o666); err != nil { // #nosec G306 -- intentionally unsafe fixture.
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0o666); err != nil { // #nosec G302 -- intentionally unsafe fixture.
		t.Fatal(err)
	}
	if _, err := ReadEnrollmentCredential(unsafe); err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("writable credential error = %v", err)
	}
	space := filepath.Join(dir, "space")
	if err := os.WriteFile(space, []byte("two values"), 0o400); err != nil { // #nosec G306 -- owner-read-only test secret.
		t.Fatal(err)
	}
	if _, err := ReadEnrollmentCredential(space); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("whitespace credential error = %v", err)
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, make([]byte, enrollmentMaxBytes+1), 0o400); err != nil { // #nosec G306 -- bounded oversize fixture.
		t.Fatal(err)
	}
	if _, err := ReadEnrollmentCredential(large); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize credential error = %v", err)
	}
}

func TestProjectedFileSwapFailsClosed(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("credential"), 0o400); err != nil { // #nosec G306 -- owner-only test projections.
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "token")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	original := statPinnedReadOnlyPath
	t.Cleanup(func() { statPinnedReadOnlyPath = original })
	statPinnedReadOnlyPath = func(path string) (os.FileInfo, error) {
		if err := os.Remove(link); err != nil {
			return nil, err
		}
		if err := os.Symlink(second, link); err != nil {
			return nil, err
		}
		return os.Stat(path)
	}
	if _, err := ReadEnrollmentCredential(link); err == nil || !strings.Contains(err.Error(), "changed while it was opened") {
		t.Fatalf("swap error = %v", err)
	}
}

func testHeadlessYAML(t *testing.T) string {
	t.Helper()
	binding := testResourceBinding(t, "headless-app")
	binding.CRID = testBindingCRID(t, &binding, apitest.VersionTest)
	return "version: 1\nshares:\n" +
		"  - crid: " + binding.CRID + "\n" +
		"    resource_id: " + binding.ResourceID + "\n" +
		"    connector_id: " + binding.ConnectorID + "\n" +
		"    connector_routing_id: " + binding.ConnectorRoutingID + "\n" +
		"    knock_resource_id: " + binding.KnockResourceID + "\n" +
		"    target_url: http://127.0.0.1:8080\n" +
		"    local_ip: 127.0.0.1\n" +
		"    local_port: 8080\n" +
		"    desired_state: on\n" +
		"    serving_epoch: 1\n"
}
