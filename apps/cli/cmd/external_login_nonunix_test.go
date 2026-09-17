//go:build !unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExternalLoginUnsupportedPlatformLeavesNamespaceUntouched(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "fresh-state")
	res := runCLI(t, &runOpts{
		args:          []string{"--supervision", "external", "login", "--enrollment-token-file", filepath.Join(t.TempDir(), "token")},
		env:           map[string]string{"LAYERV_KEY_PROVIDER": "local-key", "LAYERV_LOCAL_KEY_FD": "3"},
		shareStateDir: stateDir,
	})
	if res.code != 2 || !strings.Contains(res.stderr.String(), "unsupported on this platform") {
		t.Fatalf("exit=%d stderr=%q", res.code, res.stderr.String())
	}
	if _, err := os.Lstat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported login touched namespace: %v", err)
	}
}
