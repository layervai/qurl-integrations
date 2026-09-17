//go:build windows

package daemon

import (
	"path/filepath"
	"testing"
)

func TestDefaultWindowsLogDirUsesPinnedStateNamespace(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "connector-state")
	got, err := DefaultLogDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(stateDir, "logs"); got != want {
		t.Fatalf("DefaultLogDir() = %q, want %q", got, want)
	}
}
