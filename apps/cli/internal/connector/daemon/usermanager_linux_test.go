//go:build linux

package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestSystemdUserManagerAvailableAt(t *testing.T) {
	listenPrivate := func(t *testing.T, runtimeDir string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(runtimeDir, "systemd"), 0o700); err != nil {
			t.Fatal(err)
		}
		listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", filepath.Join(runtimeDir, "systemd", "private"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
	}
	booted := func(t *testing.T) string {
		t.Helper()
		run := shortTempRoot(t)
		if err := os.MkdirAll(filepath.Join(run, "systemd", "system"), 0o700); err != nil {
			t.Fatal(err)
		}
		return run
	}
	noEnv := func(string) (string, bool) { return "", false }

	t.Run("container without systemd", func(t *testing.T) {
		if systemdUserManagerAvailableAt(shortTempRoot(t), noEnv) {
			t.Fatal("a host that did not boot systemd has no user manager")
		}
	})
	t.Run("booted but no user manager for this user", func(t *testing.T) {
		if systemdUserManagerAvailableAt(booted(t), noEnv) {
			t.Fatal("no private socket means no user manager")
		}
	})
	t.Run("manager running but XDG_RUNTIME_DIR unset", func(t *testing.T) {
		// systemctl --user cannot find its bus here, so the probe must not
		// report a reachable manager just because /run/user/<uid> has one.
		run := booted(t)
		listenPrivate(t, filepath.Join(run, "user", strconv.Itoa(os.Geteuid())))
		if systemdUserManagerAvailableAt(run, noEnv) {
			t.Fatal("a manager this session cannot address is not reachable")
		}
		relative := func(key string) (string, bool) { return "run/user", key == "XDG_RUNTIME_DIR" }
		if systemdUserManagerAvailableAt(run, relative) {
			t.Fatal("a relative XDG_RUNTIME_DIR is not usable")
		}
	})
	t.Run("XDG_RUNTIME_DIR", func(t *testing.T) {
		run, runtimeDir := booted(t), shortTempRoot(t)
		listenPrivate(t, runtimeDir)
		env := func(key string) (string, bool) { return runtimeDir, key == "XDG_RUNTIME_DIR" }
		if !systemdUserManagerAvailableAt(run, env) {
			t.Fatal("user manager socket under XDG_RUNTIME_DIR was not found")
		}
	})
	t.Run("regular file is not a socket", func(t *testing.T) {
		run := booted(t)
		runtimeDir := filepath.Join(run, "user", strconv.Itoa(os.Geteuid()))
		env := func(key string) (string, bool) { return runtimeDir, key == "XDG_RUNTIME_DIR" }
		if err := os.MkdirAll(filepath.Join(runtimeDir, "systemd"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runtimeDir, "systemd", "private"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if systemdUserManagerAvailableAt(run, env) {
			t.Fatal("a regular file is not a user manager socket")
		}
	})
}
