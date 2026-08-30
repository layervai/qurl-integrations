//go:build windows

package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsHeadlessConfigAcceptsOrdinaryFileAndCredentialFailsClosed(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "share.yaml")
	if err := os.WriteFile(configPath, []byte(testHeadlessYAML(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadHeadlessConfig(configPath)
	if err != nil || len(config.Shares) != 1 {
		t.Fatalf("ordinary Windows headless config = %+v, %v", config, err)
	}
	setWindowsConnectorTestACLWithWorldMask(t, configPath, windows.GENERIC_WRITE)
	if _, err := LoadHeadlessConfig(configPath); err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("broad Windows headless-config ACL error = %v", err)
	}

	credentialPath := filepath.Join(dir, "enrollment-token")
	if err := os.WriteFile(credentialPath, []byte("one-shot-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEnrollmentCredential(credentialPath); err == nil ||
		!strings.Contains(err.Error(), "owner or a dedicated process group") {
		t.Fatalf("Windows bearer credential error = %v, want fail-closed ownership error", err)
	}
}
