//go:build clisandbox && cliproduction && !windows

package main

import (
	"os"
	"testing"
)

func productionWriteEnrollment(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal("write protected production enrollment token")
	}
}

func productionStateDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func productionExecutable(t *testing.T, binary string) string {
	t.Helper()
	return binary
}
