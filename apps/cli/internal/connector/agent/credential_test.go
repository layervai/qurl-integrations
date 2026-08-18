package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearTokenEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{EnvEnrollmentToken, EnvEnrollmentTokenFile} {
		t.Setenv(name, "restore-after-test")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolveEnrollmentTokenPrecedence(t *testing.T) {
	clearTokenEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvEnrollmentTokenFile, path)
	t.Setenv(EnvEnrollmentToken, "env-token")

	// Explicit (flag) beats file beats env.
	got, err := resolveEnrollmentToken("  flag-token  ")
	if err != nil || got != "flag-token" {
		t.Fatalf("resolveEnrollmentToken(flag) = (%q, %v), want trimmed flag value", got, err)
	}
	got, err = resolveEnrollmentToken("")
	if err != nil || got != "file-token" {
		t.Fatalf("resolveEnrollmentToken(file) = (%q, %v), want trimmed file value", got, err)
	}
	if err := os.Unsetenv(EnvEnrollmentTokenFile); err != nil {
		t.Fatal(err)
	}
	got, err = resolveEnrollmentToken("")
	if err != nil || got != "env-token" {
		t.Fatalf("resolveEnrollmentToken(env) = (%q, %v), want env value", got, err)
	}
	if err := os.Unsetenv(EnvEnrollmentToken); err != nil {
		t.Fatal(err)
	}
	got, err = resolveEnrollmentToken("")
	if err != nil || got != "" {
		t.Fatalf("resolveEnrollmentToken(unset) = (%q, %v), want empty = unconfigured", got, err)
	}
}

func TestResolveEnrollmentTokenFileNeverFallsBack(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv(EnvEnrollmentToken, "env-token")

	// Missing file: the _FILE variant is authoritative once set — no
	// silent degradation to the inline env var.
	t.Setenv(EnvEnrollmentTokenFile, filepath.Join(t.TempDir(), "absent"))
	if _, err := resolveEnrollmentToken(""); err == nil || !strings.Contains(err.Error(), EnvEnrollmentTokenFile) {
		t.Fatalf("missing token file = %v, want a read failure naming %s", err, EnvEnrollmentTokenFile)
	}

	// Empty file: same posture.
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvEnrollmentTokenFile, empty)
	if _, err := resolveEnrollmentToken(""); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty token file = %v, want empty-file rejection", err)
	}
}

func TestReadSecretFileCapsSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, make([]byte, enrollmentTokenFileMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(big); !errors.Is(err, errSecretFileTooLarge) {
		t.Fatalf("oversized secret file = %v, want errSecretFileTooLarge (no silent truncation)", err)
	}
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, []byte("  tok  "), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSecretFile(ok)
	if err != nil || got != "tok" {
		t.Fatalf("readSecretFile = (%q, %v), want trimmed token", got, err)
	}
}

func TestRefreshModeTable(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"", RefreshModeManual},
		{"AUTO", RefreshModeAuto},
		{"disabled", RefreshModeDisabled},
		{" manual ", RefreshModeManual},
	} {
		raw, want := tc.raw, tc.want
		t.Setenv(EnvRefreshMode, raw)
		got, err := RefreshMode()
		if err != nil || got != want {
			t.Fatalf("RefreshMode(%q) = (%q, %v), want %q", raw, got, err, want)
		}
	}
	t.Setenv(EnvRefreshMode, "sometimes")
	if _, err := RefreshMode(); err == nil {
		t.Fatal("RefreshMode accepted unsupported policy")
	}
}
