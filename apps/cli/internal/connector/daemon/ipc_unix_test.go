//go:build !windows

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// shortTempRoot pins TMPDIR to a short /tmp directory for one test. Hosted
// macOS temp roots are long enough that a host-dependent root would decide
// whether a derived socket path fits sockaddr_un.
func shortTempRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "qurl-tmp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMPDIR", root)
	return root
}

func TestSocketPathUsesRuntimeDirWhenSet(t *testing.T) {
	env := map[string]string{RuntimeDirEnv: "/private/tmp/x"}
	got, err := SocketPathForStateDir(strings.Repeat("/a", 80), lookupEnvFrom(env))
	if err != nil || got != "/private/tmp/x/daemon.sock" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestSocketPathCleansRuntimeDir(t *testing.T) {
	for _, raw := range []string{" /private/tmp/x/ ", "/private/tmp/y/../x", "/private//tmp/x"} {
		got, err := SocketPathForStateDir("/s/d", lookupEnvFrom(map[string]string{RuntimeDirEnv: raw}))
		if err != nil || got != "/private/tmp/x/daemon.sock" {
			t.Fatalf("runtime dir %q: got %q err %v", raw, got, err)
		}
	}
}

func TestSocketPathIgnoresBlankRuntimeDirAndNilLookup(t *testing.T) {
	blank := lookupEnvFrom(map[string]string{RuntimeDirEnv: "  "})
	if got, err := SocketPathForStateDir("/s/d", blank); err != nil || got != "/s/d/daemon.sock" {
		t.Fatalf("blank runtime dir: got %q err %v", got, err)
	}
	if got, err := SocketPathForStateDir("/s/d", nil); err != nil || got != "/s/d/daemon.sock" {
		t.Fatalf("nil lookup: got %q err %v", got, err)
	}
}

func TestSocketPathRejectsRelativeRuntimeDir(t *testing.T) {
	for _, raw := range []string{"rel/dir", "./x", "~/x"} {
		got, err := SocketPathForStateDir("/s/d", lookupEnvFrom(map[string]string{RuntimeDirEnv: raw}))
		if err == nil || got != "" || !strings.Contains(err.Error(), RuntimeDirEnv) {
			t.Fatalf("runtime dir %q: got %q err %v, want an error naming %s", raw, got, err, RuntimeDirEnv)
		}
	}
}

func TestSocketPathRejectsOverlongRuntimeDir(t *testing.T) {
	long := "/" + strings.Repeat("r", maxUnixSocketPathBytes)
	got, err := SocketPathForStateDir("/s/d", lookupEnvFrom(map[string]string{RuntimeDirEnv: long}))
	if err == nil || got != "" || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("got %q err %v, want a too-long error", got, err)
	}
}

func TestSocketPathShortStateDirStaysInside(t *testing.T) {
	for _, stateDir := range []string{"/s/d", " /s//d/ "} {
		got, err := SocketPathForStateDir(stateDir, lookupEnvFrom(nil))
		if err != nil || got != "/s/d/daemon.sock" {
			t.Fatalf("state dir %q: got %q err %v", stateDir, got, err)
		}
	}
}

func TestSocketPathRejectsRelativeOrEmptyStateDir(t *testing.T) {
	for _, stateDir := range []string{"", "  ", "relative/state", strings.Repeat("relative-state-", 8)} {
		got, err := SocketPathForStateDir(stateDir, nil)
		if err == nil || got != "" {
			t.Fatalf("state dir %q: got %q err %v, want an error", stateDir, got, err)
		}
	}
}

func TestSocketPathLongStateDirFallsBackToTempDir(t *testing.T) {
	root := "/tmp"
	longState := strings.Repeat("/a", 80)
	got, err := SocketPathForStateDir(longState, lookupEnvFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, root+string(filepath.Separator)) {
		t.Fatalf("expected fixed-root fallback below %q, got %q", root, got)
	}
	if len(got) > maxUnixSocketPathBytes || filepath.Base(got) != SocketFile || filepath.Dir(got) == longState {
		t.Fatalf("fallback %q is not a bounded %s outside the state directory", got, SocketFile)
	}
	if dir := filepath.Base(filepath.Dir(got)); !strings.HasPrefix(dir, "qurl-"+strconv.Itoa(os.Geteuid())+"-") {
		t.Fatalf("fallback directory %q is not scoped to the current user", dir)
	}
	if again, err := SocketPathForStateDir(longState, nil); err != nil || again != got {
		t.Fatalf("fallback is not deterministic: %q then %q (%v)", got, again, err)
	}
	if other, err := SocketPathForStateDir(longState+"-other", nil); err != nil || other == got {
		t.Fatalf("different long state namespaces share one socket path %q (%v)", other, err)
	}
}

func TestSocketPathFallbackIgnoresProcessTempDirectory(t *testing.T) {
	stateDir := strings.Repeat("/a", 80)
	original, err := SocketPathForStateDir(stateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"relative", "/other-temp", strings.Repeat("/long", 40)} {
		t.Setenv("TMPDIR", root)
		got, err := SocketPathForStateDir(stateDir, nil)
		if err != nil || got != original {
			t.Fatalf("TMPDIR=%q changed socket: %q, %v", root, got, err)
		}
	}
}

func TestIPCServerAndClientAgreeOnRuntimeDirSocket(t *testing.T) {
	root := shortTempRoot(t)
	runtimeDir := filepath.Join(root, "rt")
	lookup := lookupEnvFrom(map[string]string{RuntimeDirEnv: runtimeDir})
	longState := filepath.Join(root, "state", strings.Repeat("segment-", 12))
	serverPath, err := SocketPathForStateDir(longState, lookup)
	if err != nil {
		t.Fatal(err)
	}
	clientPath, err := SocketPathForStateDir(longState, lookup)
	if err != nil || clientPath != serverPath || serverPath != filepath.Join(runtimeDir, SocketFile) {
		t.Fatalf("server %q client %q err %v, want both exactly %q", serverPath, clientPath, err, filepath.Join(runtimeDir, SocketFile))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&IPCServer{SocketPath: serverPath, Manager: emptyManager(t), JobVersion: "1/test"}).Run(ctx)
	}()
	client := IPCClient{SocketPath: clientPath}
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readyCancel()
	if err := client.WaitReady(readyCtx); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(runtimeDir); err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		t.Fatalf("runtime dir = %v, %v; want an owner-only 0700 directory", info, err)
	}
	status, running, err := client.Status(context.Background())
	if err != nil || !running || status.JobVersion != "1/test" {
		t.Fatalf("status = %+v running=%v err=%v", status, running, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("server error = %v, want context cancellation", err)
	}
}
