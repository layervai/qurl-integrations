//go:build clisandbox && cliproduction && windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProductionProofWindowsCredentialFile(t *testing.T) {
	key := "lv_test_" + "012345678901234567890123456789012345678901234"
	path := filepath.Join(t.TempDir(), "api-key-env")
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal("write fixture key")
	}
	command := exec.Command("icacls.exe", path, "/inheritance:r", "/grant:r",
		os.Getenv("USERNAME")+":F", "*S-1-5-18:F", "*S-1-5-32-544:F")
	if err := command.Run(); err != nil {
		t.Fatal("protect fixture key ACL")
	}
	t.Setenv("QURL_API_KEY", "")
	t.Setenv("QURL_API_KEY_FILE", path)
	if productionCredential(t) != key {
		t.Fatal("production harness did not read its protected Windows key")
	}
}
