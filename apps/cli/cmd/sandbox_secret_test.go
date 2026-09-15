//go:build clisandbox && windows

package main

import (
	"os"
	"strings"
	"testing"
)

func sandboxSecret(t *testing.T, name string) string {
	t.Helper()
	if path := strings.TrimSpace(os.Getenv(name + "_FILE")); path != "" {
		t.Fatalf("%s_FILE is not supported by the Windows sandbox lane until it has a protected Windows file reader", name)
	}
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return ""
}
