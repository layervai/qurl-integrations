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

// TestDaemonRunRuntimeDirFlagBeatsTheEnvironment pins the native job's
// contract end to end: the daemon listens exactly where --runtime-dir says
// even when QURL_CONNECTOR_RUNTIME_DIR names another directory, so a daemon
// under launchd or systemd (whose environment is not the user's shell)
// resolves the socket the installing CLI resolved, and it secures that
// directory owner-only before it listens.
func TestDaemonRunRuntimeDirFlagBeatsTheEnvironment(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	if _, err := openOwnedTestShareRegistry(stateDir); err != nil {
		t.Fatal(err)
	}
	// Unix socket paths are bounded; the hosted macOS temp root is too long.
	root, err := os.MkdirTemp("/tmp", "qurl-rt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	flagDir, envDir := filepath.Join(root, "flag"), filepath.Join(root, "env")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := connectordaemon.IPCClient{SocketPath: filepath.Join(flagDir, connectordaemon.SocketFile)}
	ready := make(chan error, 1)
	go func() {
		defer cancel()
		readyCtx, cancelReady := context.WithTimeout(ctx, 5*time.Second)
		defer cancelReady()
		ready <- client.WaitReady(readyCtx)
	}()
	res := runCLI(t, &runOpts{
		args: []string{
			"daemon", "run", "--state-dir", stateDir, "--runtime-dir", flagDir,
			"--hub-host", "hub.nhp.layerv.ai", "--hub-port", "443",
			"--hub-server-public-key-b64", "CQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		},
		env:           map[string]string{connectordaemon.RuntimeDirEnv: envDir},
		ctx:           ctx,
		shareStateDir: stateDir,
	})
	if err := <-ready; err != nil {
		t.Fatalf("daemon never listened below --runtime-dir (exit %d stderr %s): %v", res.code, res.stderr.String(), err)
	}
	if res.code != 130 {
		t.Fatalf("daemon stop = exit %d stderr %s, want the cancellation exit", res.code, res.stderr.String())
	}
	if _, err := os.Lstat(envDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the daemon touched the environment's runtime directory: %v", err)
	}
	info, err := os.Lstat(flagDir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("--runtime-dir was not secured as an owner-only directory: %v %v", info, err)
	}
}
