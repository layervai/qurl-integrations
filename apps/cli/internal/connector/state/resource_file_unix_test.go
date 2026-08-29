//go:build (linux && !android) || (darwin && !ios)

package state

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpenConnectorResourceStateRefusesFinalSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("state"), connectorResourceFileMode); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	file, err := openConnectorResourceState(link)
	if file != nil {
		_ = file.Close()
		t.Fatal("no-follow state open returned a file for a symlink")
	}
	if err == nil {
		t.Fatal("no-follow state open accepted a symlink")
	}
}

func TestCreateConnectorResourceTempForcesExactModeUnderRestrictiveUmask(t *testing.T) {
	const helperPathEnv = "QURL_TEST_RESTRICTIVE_UMASK_PATH"
	if path := os.Getenv(helperPathEnv); path != "" {
		previous := syscall.Umask(0o200)
		defer syscall.Umask(previous)
		file, err := createConnectorResourceTemp(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}

	path := filepath.Join(t.TempDir(), "state.tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCreateConnectorResourceTempForcesExactModeUnderRestrictiveUmask$") //nolint:gosec // exact current test binary, no customer input.
	cmd.Env = append(os.Environ(), helperPathEnv+"="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restrictive-umask helper: %v\n%s", err, output)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != connectorResourceFileMode {
		t.Fatalf("temporary state mode = %04o, want %04o", got, connectorResourceFileMode)
	}
}
