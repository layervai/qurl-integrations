//go:build clisandbox && darwin

package main

import (
	"os"
	"strings"
	"testing"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
)

func assertPOSIXUserJobContainsNoCredential(t *testing.T, endpoint, hubHost, hubKey, apiKey, cleanupJWT string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path, err := connectorservice.UserJobPlistPath(home, connectordaemon.DaemonJobLabel)
	if err != nil {
		t.Fatal(err)
	}
	assertSandboxUserJobDefinition(t, path, "LaunchAgent", endpoint, hubHost, hubKey, apiKey, cleanupJWT)
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
