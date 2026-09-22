//go:build !windows

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func privateTestTempRoot() string { return "/tmp" }
func privateCmdTarget(t *testing.T, dir string) connectorstate.LocalTarget {
	t.Helper()
	target, err := connectorstate.ParseUnixTarget("http+unix://" + filepath.Join(dir, "file.sock"))
	if err != nil {
		t.Fatal(err)
	}
	return target
}
func listenPrivateTestOrigin(target connectorstate.LocalTarget) (net.Listener, error) {
	if err := os.Remove(target.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return (&net.ListenConfig{}).Listen(context.Background(), "unix", target.SocketPath)
}
func assertPrivateOriginKilled(t *testing.T, state *os.ProcessState) {
	t.Helper()
	if status, ok := state.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("origin did not die from SIGKILL: %s", state)
	}
}
