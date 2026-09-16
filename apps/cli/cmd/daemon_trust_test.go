package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
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

func testDaemonTunnelCA(t *testing.T) string {
	t.Helper()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.TLS.Certificates[0].Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTunnelTrustFlagsBelongOnlyToDaemonRun(t *testing.T) {
	root, opts := newRoot("test", discardStreams())
	for _, args := range [][]string{{}, {"daemon"}, {"publish"}, {"start"}} {
		cmd, _, err := root.Find(args)
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"tunnel-ca-file", "tunnel-server-name"} {
			if cmd.Flags().Lookup(flag) != nil || cmd.InheritedFlags().Lookup(flag) != nil || cmd.PersistentFlags().Lookup(flag) != nil {
				t.Fatalf("%v exposes %s outside daemon run", args, flag)
			}
		}
	}
	run, _, err := root.Find([]string{"daemon", "run"})
	if err != nil {
		t.Fatal(err)
	}
	ca := testDaemonTunnelCA(t)
	if err := run.ParseFlags([]string{"--tunnel-ca-file", ca, "--tunnel-server-name", "example.com"}); err != nil {
		t.Fatal(err)
	}
	if opts.tunnelCAFile != ca || opts.tunnelServerName != "example.com" {
		t.Fatal("tunnel trust flags were not bound")
	}
}
