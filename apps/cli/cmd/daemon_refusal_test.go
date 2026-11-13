//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
)

func TestNativeRefusalReportsOwnership(t *testing.T) {
	for _, refusal := range []struct {
		name  string
		flags []string
	}{
		{"incompatible job", []string{"--job-version", "999/test"}},
		{"unknown argument before job version", []string{"--future-job-argument", "--job-version", "999/test"}},
		{"invalid encoded product", []string{"--job-version", "6/test", "--client-product-b64", "%%%"}},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			dir := connectorStateTestDir(t)
			runtimeDir, err := os.MkdirTemp("/tmp", "qr-refuse-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
			args := append([]string{"daemon", "run"}, refusal.flags...)
			args = append(args, "--state-dir", dir, "--runtime-dir", runtimeDir)
			originalHold, originalArgs := waitDaemonJobRefusal, daemonJobArguments
			t.Cleanup(func() { waitDaemonJobRefusal, daemonJobArguments = originalHold, originalArgs })
			daemonJobArguments = func() []string { return args }
			holds := 0
			waitDaemonJobRefusal = func(ctx context.Context, _ time.Duration) {
				holds++
				client := connectordaemon.IPCClient{StateDir: dir, SocketPath: filepath.Join(runtimeDir, connectordaemon.SocketFile)}
				ready, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				if err := client.WaitReady(ready); !errors.Is(err, connectordaemon.ErrJobRefused) {
					t.Errorf("refused readiness: %v", err)
					return
				}
				status, running, err := client.Status(ready)
				if err != nil || !running || status.JobVersion != "0/refused" || len(status.Resources)+len(status.Running) != 0 {
					t.Errorf("refused status = %+v, %v, %v", status, running, err)
				}
				if _, err := client.ReloadIfRunning(ready); !errors.Is(err, connectordaemon.ErrJobRefused) {
					t.Errorf("refused reload: %v", err)
				}
			}
			result := runCLI(t, &runOpts{args: args, env: map[string]string{}})
			if result.code == 0 || holds != 1 {
				t.Fatalf("exit = %d, holds = %d, stderr = %s", result.code, holds, result.stderr.String())
			}
		})
	}
}
