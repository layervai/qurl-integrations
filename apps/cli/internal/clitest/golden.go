// Package clitest holds the golden-file helper shared by the CLI's output
// tests.
//
// Goldens are byte-exact and LF-only. The repo's .gitattributes opts
// testdata and *.golden out of line-ending conversion so a Windows checkout
// cannot rewrite them; this helper guards the other direction by refusing to
// write (or accept) output that carries a carriage return in the first
// place.
package clitest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// UpdateEnv is the environment variable that switches golden assertions into
// rewrite mode: UPDATE_GOLDEN=1 go test ./...
const UpdateEnv = "UPDATE_GOLDEN"

// Golden compares got against testdata/golden/<name>, rewriting the file
// instead when UPDATE_GOLDEN=1. Comparison is bytes.Equal — no trimming, no
// normalization — because the rendered bytes are the contract.
func Golden(t *testing.T, name string, got []byte) {
	t.Helper()

	if i := bytes.IndexByte(got, '\r'); i >= 0 {
		t.Fatalf("golden %s: output contains a carriage return at byte %d; CLI output must be LF-only", name, i)
	}

	path := filepath.Join("testdata", "golden", name)
	if os.Getenv(UpdateEnv) == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("golden %s: create dir: %v", name, err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("golden %s: write: %v", name, err)
		}
		return
	}

	want, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("golden %s: %v (run with %s=1 to create it)", name, err, UpdateEnv)
	}
	if bytes.Equal(got, want) {
		return
	}
	t.Errorf("golden %s: output mismatch (run with %s=1 to update)\n%s", name, UpdateEnv, diffSummary(want, got))
}

// diffSummary points at the first divergent byte and shows both renderings,
// which is almost always enough to see what moved.
func diffSummary(want, got []byte) string {
	limit := len(want)
	if len(got) < limit {
		limit = len(got)
	}
	at := limit
	for i := 0; i < limit; i++ {
		if want[i] != got[i] {
			at = i
			break
		}
	}
	return fmt.Sprintf("first difference at byte %d\n--- want ---\n%s\n--- got ---\n%s", at, want, got)
}
