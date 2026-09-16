package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonRejectsInvalidTunnelTrustBeforeEnrollment(t *testing.T) {
	for _, args := range [][]string{
		{"--tunnel-ca-file", filepath.Join(t.TempDir(), "missing.pem")},
		{"--tunnel-server-name", "tunnel.example.com"},
	} {
		stateDir := filepath.Join(t.TempDir(), "new-state")
		command := append([]string{"daemon", "run", "--state-dir", stateDir}, args...)
		res := runCLI(t, &runOpts{args: command})
		if res.code == 0 || !strings.Contains(res.stderr.String(), "qURL tunnel") {
			t.Fatalf("invalid tunnel trust: exit=%d stderr=%s", res.code, res.stderr.String())
		}
		if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
			t.Fatalf("invalid tunnel trust touched enrollment state: %v", err)
		}
	}
}
