//go:build clisandbox

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	sandboxAPIKeyFileEnv     = "QURL_API_KEY_FILE"
	sandboxCleanupJWTFileEnv = "QURL_CLI_SANDBOX_CLEANUP_JWT_FILE"
	sandboxRunIDEnv          = "QURL_SHARING_RUN_ID"
	sandboxRunAttemptEnv     = "QURL_SHARING_RUN_ATTEMPT"
	sandboxRuntimeEnv        = "QURL_SHARING_RUNTIME"
	sandboxSecretMaxBytes    = 16 * 1024
)

var sandboxPositiveDecimal = regexp.MustCompile(`^[1-9]\d{0,19}$`)

// sandboxSecretAfterLstatHook exists only in this tagged test binary so the
// hermetic contract can deterministically prove a path swap is rejected.
var sandboxSecretAfterLstatHook func(string)

type sandboxRunNamespace struct {
	AgentID     string
	ConnectorID string
}

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

func sandboxNamespace(label string) (sandboxRunNamespace, error) {
	runID := os.Getenv(sandboxRunIDEnv)
	attempt := os.Getenv(sandboxRunAttemptEnv)
	runtimeName := os.Getenv(sandboxRuntimeEnv)
	if !sandboxPositiveDecimal.MatchString(runID) || !sandboxPositiveDecimal.MatchString(attempt) {
		return sandboxRunNamespace{}, errors.New("qURL sharing run ID and attempt must be canonical positive decimals")
	}
	if _, err := strconv.ParseUint(runID, 10, 64); err != nil {
		return sandboxRunNamespace{}, errors.New("qURL sharing run ID exceeds uint64")
	}
	if _, err := strconv.ParseUint(attempt, 10, 64); err != nil {
		return sandboxRunNamespace{}, errors.New("qURL sharing run attempt exceeds uint64")
	}
	runtimeCode := ""
	// TODO(upstream-contract): Keep this exact runtime enum in lockstep with
	// the protected lifecycle orchestrator. The short namespace code is stable.
	switch runtimeName {
	case "host":
		runtimeCode = "h"
	case "hardened_container":
		runtimeCode = "c"
	default:
		return sandboxRunNamespace{}, errors.New("qURL sharing runtime must be host or hardened_container")
	}
	labelCode := ""
	switch label {
	case "smoke":
		labelCode = "s"
	case "soak":
		labelCode = "k"
	case "crid":
		labelCode = "r"
	case "sibling-a":
		labelCode = "a"
	case "sibling-b":
		labelCode = "b"
	default:
		return sandboxRunNamespace{}, errors.New("qURL sharing test label is unsupported")
	}
	agentID := fmt.Sprintf("qurl-share-r%s-a%s-%s%s", runID, attempt, runtimeCode, labelCode)
	if len(agentID) > 64 {
		return sandboxRunNamespace{}, errors.New("qURL sharing agent identity exceeds the platform bound")
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{"qurl-sharing-sandbox-v1", runID, attempt, runtimeName, label}, "\x00")))
	connectorID := "connector-sandbox-local-publish-" + hex.EncodeToString(digest[:12])
	return sandboxRunNamespace{AgentID: agentID, ConnectorID: connectorID}, nil
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

func TestSandboxNamespaceIsCanonicalAndSeparated(t *testing.T) {
	t.Setenv(sandboxRunIDEnv, "32635672597")
	t.Setenv(sandboxRunAttemptEnv, "2")
	t.Setenv(sandboxRuntimeEnv, "host")
	first, err := sandboxNamespace("smoke")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sandboxNamespace("smoke")
	if err != nil || second != first {
		t.Fatalf("repeat namespace = %+v, %v; want %+v", second, err, first)
	}
	if !strings.HasPrefix(first.AgentID, "qurl-share-r32635672597-a2-") ||
		!strings.HasPrefix(first.ConnectorID, "connector-sandbox-local-publish-") || len(first.AgentID) > 64 {
		t.Fatalf("namespace = %+v", first)
	}
	seen := map[sandboxRunNamespace]bool{first: true}
	for _, tc := range []struct{ runtime, label string }{
		{"hardened_container", "smoke"}, {"host", "soak"}, {"host", "crid"}, {"host", "sibling-a"}, {"host", "sibling-b"},
	} {
		t.Setenv(sandboxRuntimeEnv, tc.runtime)
		got, gotErr := sandboxNamespace(tc.label)
		if gotErr != nil || seen[got] {
			t.Fatalf("namespace %s/%s = %+v, %v", tc.runtime, tc.label, got, gotErr)
		}
		seen[got] = true
	}
	t.Setenv(sandboxRunIDEnv, "18446744073709551615")
	t.Setenv(sandboxRunAttemptEnv, "18446744073709551615")
	t.Setenv(sandboxRuntimeEnv, "hardened_container")
	maximal, maxErr := sandboxNamespace("soak")
	if maxErr != nil || len(maximal.AgentID) > 64 {
		t.Fatalf("maximal namespace = %+v, %v", maximal, maxErr)
	}
	t.Setenv(sandboxRuntimeEnv, "container")
	if _, legacyErr := sandboxNamespace("smoke"); legacyErr == nil {
		t.Fatal("legacy container runtime identity was accepted")
	}
	for name, value := range map[string]string{
		"zero": "0", "leading-zero": "01", "negative": "-1", "overflow": "18446744073709551616",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(sandboxRunIDEnv, value)
			t.Setenv(sandboxRunAttemptEnv, "1")
			t.Setenv(sandboxRuntimeEnv, "host")
			if _, gotErr := sandboxNamespace("smoke"); gotErr == nil {
				t.Fatalf("run ID %q accepted", value)
			}
		})
	}
}
