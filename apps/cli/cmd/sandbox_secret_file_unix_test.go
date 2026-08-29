//go:build clisandbox && (linux || darwin)

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const sandboxSecretMaxBytes = 16 * 1024

// sandboxSecretAfterLstatHook exists only in this tagged test binary so the
// hermetic contract can deterministically prove a path swap is rejected.
var sandboxSecretAfterLstatHook func(string)

func readSandboxSecretFile(fileEnv, inlineEnv string) (string, error) {
	if strings.TrimSpace(os.Getenv(inlineEnv)) != "" {
		return "", fmt.Errorf("%s is forbidden; use the protected %s file", inlineEnv, fileEnv)
	}
	path := os.Getenv(fileEnv)
	if path == "" || path != strings.TrimSpace(path) || strings.ContainsAny(path, "\x00\r\n") {
		return "", fmt.Errorf("%s is missing or malformed", fileEnv)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("read %s metadata: %w", fileEnv, err)
	}
	if err := validateSandboxSecretInfo(fileEnv, before); err != nil {
		return "", err
	}
	if sandboxSecretAfterLstatHook != nil {
		sandboxSecretAfterLstatHook(path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", fileEnv, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return "", fmt.Errorf("open %s: invalid file descriptor", fileEnv)
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return "", fmt.Errorf("%s file identity changed while opening", fileEnv)
	}
	if err := validateSandboxSecretInfo(fileEnv, after); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(file, sandboxSecretMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", fileEnv, err)
	}
	if len(data) > sandboxSecretMaxBytes {
		return "", fmt.Errorf("%s exceeds the protected size bound", fileEnv)
	}
	final, err := file.Stat()
	if err != nil || !os.SameFile(after, final) || final.Size() != after.Size() || !final.ModTime().Equal(after.ModTime()) {
		return "", fmt.Errorf("%s file changed while reading", fileEnv)
	}
	if err := validateSandboxSecretInfo(fileEnv, final); err != nil {
		return "", err
	}
	value := string(data)
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%s must contain one exact non-empty line", fileEnv)
	}
	return value, nil
}

func validateSandboxSecretInfo(fileEnv string, info os.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || (info.Mode().Perm() != 0o600 && info.Mode().Perm() != 0o440) {
		return fmt.Errorf("%s must name one regular 0600 or 0440 file", fileEnv)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return fmt.Errorf("%s file is not exclusively owned by the current user", fileEnv)
	}
	return nil
}

func TestReadSandboxSecretFileFailsClosed(t *testing.T) {
	const fileEnv = "TEST_SANDBOX_SECRET_FILE"
	const inlineEnv = "TEST_SANDBOX_SECRET"
	for _, name := range []string{fileEnv, inlineEnv} {
		t.Setenv(name, "")
	}
	write := func(name, value string, mode os.FileMode) string {
		t.Helper()
		path := t.TempDir() + "/" + name
		if err := os.WriteFile(path, []byte(value), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, mode := range []os.FileMode{0o600, 0o440} {
		t.Run(fmt.Sprintf("mode-%o", mode), func(t *testing.T) {
			path := write("secret", "exact-secret", mode)
			t.Setenv(fileEnv, path)
			got, err := readSandboxSecretFile(fileEnv, inlineEnv)
			if err != nil || got != "exact-secret" {
				t.Fatalf("read = %q, %v", got, err)
			}
		})
	}
	cases := map[string]func(t *testing.T) string{
		"empty":          func(t *testing.T) string { return write("secret", "", 0o600) },
		"padded":         func(t *testing.T) string { return write("secret", " secret ", 0o600) },
		"newline":        func(t *testing.T) string { return write("secret", "secret\n", 0o600) },
		"oversize":       func(t *testing.T) string { return write("secret", strings.Repeat("x", sandboxSecretMaxBytes+1), 0o600) },
		"world-readable": func(t *testing.T) string { return write("secret", "secret", 0o604) },
		"symlink": func(t *testing.T) string {
			target := write("target", "secret", 0o600)
			path := t.TempDir() + "/secret"
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			return path
		},
	}
	for name, makePath := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(inlineEnv, "")
			t.Setenv(fileEnv, makePath(t))
			if value, err := readSandboxSecretFile(fileEnv, inlineEnv); err == nil || value != "" || strings.Contains(fmt.Sprint(err), "exact-secret") {
				t.Fatalf("invalid secret read = %q, %v", value, err)
			}
		})
	}
	t.Run("inline-forbidden", func(t *testing.T) {
		t.Setenv(fileEnv, write("secret", "file-secret", 0o600))
		t.Setenv(inlineEnv, "inline-secret")
		if value, err := readSandboxSecretFile(fileEnv, inlineEnv); err == nil || value != "" || strings.Contains(fmt.Sprint(err), "inline-secret") {
			t.Fatalf("inline secret read = %q, %v", value, err)
		}
	})
	t.Run("path-swap", func(t *testing.T) {
		path := write("secret", "first-secret", 0o600)
		replacement := write("replacement", "second-secret", 0o600)
		t.Setenv(fileEnv, path)
		t.Setenv(inlineEnv, "")
		sandboxSecretAfterLstatHook = func(string) {
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
		}
		defer func() { sandboxSecretAfterLstatHook = nil }()
		if value, err := readSandboxSecretFile(fileEnv, inlineEnv); err == nil || value != "" {
			t.Fatalf("swapped secret read = %q, %v", value, err)
		}
	})
	t.Run("mode-change", func(t *testing.T) {
		path := write("secret", "secret", 0o600)
		t.Setenv(fileEnv, path)
		t.Setenv(inlineEnv, "")
		sandboxSecretAfterLstatHook = func(string) {
			if err := os.Chmod(path, 0o604); err != nil { //nolint:gosec // Deliberately weakens the fixture to prove rejection.
				t.Fatal(err)
			}
		}
		defer func() { sandboxSecretAfterLstatHook = nil }()
		if value, err := readSandboxSecretFile(fileEnv, inlineEnv); err == nil || value != "" {
			t.Fatalf("permission-changed secret read = %q, %v", value, err)
		}
	})
	t.Run("same-inode-symlink-swap", func(t *testing.T) {
		path := write("secret", "secret", 0o600)
		original := path + ".original"
		t.Setenv(fileEnv, path)
		t.Setenv(inlineEnv, "")
		sandboxSecretAfterLstatHook = func(string) {
			if err := os.Rename(path, original); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(original, path); err != nil {
				t.Fatal(err)
			}
		}
		defer func() { sandboxSecretAfterLstatHook = nil }()
		if value, err := readSandboxSecretFile(fileEnv, inlineEnv); err == nil || value != "" {
			t.Fatalf("same-inode symlink read = %q, %v", value, err)
		}
	})
}
