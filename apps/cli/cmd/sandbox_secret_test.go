//go:build clisandbox

package main

import (
	"os"
	"strings"
	"testing"
)

func sandboxSecret(t *testing.T, name string) string {
	t.Helper()
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	path := strings.TrimSpace(os.Getenv(name + "_FILE"))
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path) //nolint:gosec // The private orchestrator supplies the explicit secret-file path.
	if err != nil {
		t.Fatalf("read %s_FILE: %v", name, err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		t.Fatalf("%s_FILE is empty", name)
	}
	return value
}
