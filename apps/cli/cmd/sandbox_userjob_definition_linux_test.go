//go:build clisandbox && linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
)

func assertPOSIXUserJobContainsNoCredential(t *testing.T, endpoint, hubHost, hubKey, apiKey, cleanupJWT string) {
	t.Helper()
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "systemd", "user", connectordaemon.DaemonJobLabel+".service")
	assertSandboxUserJobDefinition(t, path, "systemd user job", endpoint, hubHost, hubKey, apiKey, cleanupJWT)
}

func assertSandboxUserJobDefinition(t *testing.T, path, kind, endpoint, hubHost, hubKey, apiKey, cleanupJWT string) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // The platform manager derives this fixed per-user job path.
	if err != nil {
		t.Fatalf("read qURL %s definition: %v", kind, err)
	}
	definition := string(raw)
	for _, expected := range []string{endpoint, hubHost, hubKey} {
		if !strings.Contains(definition, expected) {
			t.Fatalf("qURL %s omitted required non-secret deployment identity", kind)
		}
	}
	for _, secret := range []string{apiKey, cleanupJWT} {
		if secret != "" && strings.Contains(definition, secret) {
			t.Fatalf("qURL %s persisted a bearer credential", kind)
		}
	}
}
