//go:build windows

package state

import (
	"path/filepath"
	"testing"
)

func TestResolveDirUsesWindowsLocalApplicationData(t *testing.T) {
	clearStateEnv(t)
	base := filepath.Join(t.TempDir(), "LocalAppData")
	t.Setenv("LOCALAPPDATA", base)
	got, err := ResolveDir("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "qurl", "connector-v2")
	if got != want {
		t.Fatalf("ResolveDir() = %q, want Windows local application data path %q", got, want)
	}
}
