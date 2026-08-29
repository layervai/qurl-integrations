//go:build !windows && !darwin

package daemon

import (
	"path/filepath"
	"testing"
)

func TestDefaultNonMacOSUnixLogDirUsesPinnedStateNamespace(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "connector-state")
	got, err := DefaultLogDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(stateDir, "logs"); got != want {
		t.Fatalf("DefaultLogDir() = %q, want %q", got, want)
	}
}

func TestDefaultNonMacOSUnixLogDirRejectsRelativeStateNamespace(t *testing.T) {
	if _, err := DefaultLogDir("connector-state"); err == nil {
		t.Fatal("DefaultLogDir accepted a relative state directory")
	}
}
