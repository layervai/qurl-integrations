//go:build windows

package state

import (
	"errors"
	"path/filepath"
	"testing"

	qurl "github.com/layervai/qurl-go/qurl"
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

func TestEnsureDirModeRejectsInheritedWindowsACL(t *testing.T) {
	err := EnsureDirMode(t.TempDir())
	if !errors.Is(err, qurl.ErrInsecureAgentStatePermissions) {
		t.Fatalf("EnsureDirMode(inherited temp directory) = %v, want insecure-permissions error", err)
	}
}
