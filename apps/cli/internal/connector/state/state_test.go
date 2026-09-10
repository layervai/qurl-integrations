package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	qurl "github.com/layervai/qurl-go/qurl"
)

// clearStateEnv detaches the test from any ambient operator configuration.
func clearStateEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{EnvStateDirPrimary, EnvAgentID, "XDG_STATE_HOME", "HOME", "LOCALAPPDATA", connectoragentstate.EnvKeyProvider, connectoragentstate.EnvLocalKeyFD} {
		t.Setenv(name, "restore-after-test")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolveDirPrecedence(t *testing.T) {
	clearStateEnv(t)
	override := t.TempDir()
	primary := t.TempDir()

	t.Setenv(EnvStateDirPrimary, primary)

	got, err := ResolveDir(override)
	if err != nil || got != override {
		t.Fatalf("ResolveDir(override) = (%q, %v), want the explicit override %q", got, err, override)
	}
	got, err = ResolveDir("")
	if err != nil || got != primary {
		t.Fatalf("ResolveDir() = (%q, %v), want %s value %q", got, err, EnvStateDirPrimary, primary)
	}
}

func TestResolveDirWithoutConfiguredNamespace(t *testing.T) {
	clearStateEnv(t)
	if got, err := ResolveDir(""); got != "" || !errors.Is(err, ErrNoDefaultStateDir) {
		t.Fatalf("ResolveDir() = (%q, %v), want ErrNoDefaultStateDir", got, err)
	}
}

func TestResolveDirXDGFallback(t *testing.T) {
	if isWindows(t) {
		t.Skip("XDG state paths are a Unix contract")
	}
	clearStateEnv(t)
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	got, err := ResolveDir("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(xdg, "qurl", "connector-v2")
	if got != want {
		t.Fatalf("ResolveDir() = %q, want XDG state path %q", got, want)
	}

	// A relative XDG_STATE_HOME is ignored per the XDG spec; the home
	// fallback takes over.
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "relative/state")
	t.Setenv("HOME", home)
	got, err = ResolveDir("")
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(home, ".local", "state", "qurl", "connector-v2")
	if got != want {
		t.Fatalf("ResolveDir() = %q, want home state path %q", got, want)
	}
}

func TestResolveDirTrimsAndAbsolutizes(t *testing.T) {
	clearStateEnv(t)
	dir := t.TempDir()
	t.Setenv(EnvStateDirPrimary, "  "+dir+"  ")
	got, err := ResolveDir("")
	if err != nil || got != dir {
		t.Fatalf("ResolveDir() = (%q, %v), want trimmed %q", got, err, dir)
	}
}

func TestEnsureDirModePinsOwnerOnly(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "loose")
	if !isWindows(t) {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !isWindows(t) && info.Mode().Perm() != 0o700 {
		t.Fatalf("EnsureDirMode left mode %04o, want 0700", info.Mode().Perm())
	}
	if err := EnsureDirMode(""); err == nil {
		t.Fatal("EnsureDirMode(\"\") = nil, want empty-path rejection")
	}
}

func TestConfiguredAgentIDTrims(t *testing.T) {
	clearStateEnv(t)
	if got := ConfiguredAgentID(); got != "" {
		t.Fatalf("ConfiguredAgentID() = %q with no env, want empty", got)
	}
	t.Setenv(EnvAgentID, "  agent-a  ")
	if got := ConfiguredAgentID(); got != "agent-a" {
		t.Fatalf("ConfiguredAgentID() = %q, want trimmed agent-a", got)
	}
}

func isWindows(t *testing.T) bool {
	t.Helper()
	return os.PathSeparator == '\\'
}

// secureStateTestDir creates the test namespace through the production state
// setup path. This is required on Windows, where t.TempDir() correctly retains
// an inherited ACL that the production store must reject. Symlink components
// are resolved because the connector's sealed store refuses them, and on
// macOS t.TempDir() lives below the /var alias.
func secureStateTestDir(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "state")
	if err := EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// openTestStore opens a Store in a fresh temp directory on each supported OS.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	return openTestStoreAt(t, secureStateTestDir(t))
}

func openTestStoreAt(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	t.Parallel()
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") = nil error, want empty-path rejection")
	}
}

func TestStoreHandoffReturnsConcreteSDKStore(t *testing.T) {
	store := openTestStore(t)
	sdkStore, err := store.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sdkStore.(*qurl.FileAgentStateStore); !ok {
		t.Fatalf("Handoff() returned %T, want the concrete *qurl.FileAgentStateStore so SDK lock contracts stay active", sdkStore)
	}
	if err := store.ValidateContinuity(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentStatePresentDistinguishesOnlyTrueAbsence(t *testing.T) {
	store := openTestStore(t)
	if present, err := store.AgentStatePresent(); err != nil || present {
		t.Fatalf("AgentStatePresent on fresh store = (%v, %v), want false, nil", present, err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), AgentStateFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if present, err := store.AgentStatePresent(); err != nil || !present {
		t.Fatalf("AgentStatePresent with state entry = (%v, %v), want true, nil", present, err)
	}
	if err := os.Remove(filepath.Join(store.Dir(), AgentStateFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-agent-state", filepath.Join(store.Dir(), AgentStateFile)); err != nil {
		if isWindows(t) {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if present, err := store.AgentStatePresent(); err != nil || !present {
		t.Fatalf("AgentStatePresent with unsafe entry = (%v, %v), want true, nil so SDK validation remains authoritative", present, err)
	}
}

func TestStoreFailsClosedAfterClose(t *testing.T) {
	store := openTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close() = %v, want idempotent nil", err)
	}
	if _, err := store.Handoff(); !errors.Is(err, qurl.ErrAgentStateContinuity) {
		t.Fatalf("Handoff() after Close = %v, want state-continuity error", err)
	}
	if err := store.ValidateContinuity(); !errors.Is(err, qurl.ErrAgentStateContinuity) {
		t.Fatalf("ValidateContinuity() after Close = %v, want state-continuity error", err)
	}
	var nilStore *Store
	if err := nilStore.Close(); err != nil {
		t.Fatalf("nil Close() = %v, want nil", err)
	}
	if _, err := nilStore.Handoff(); err == nil {
		t.Fatal("nil Handoff() = nil error, want not-open rejection")
	}
	if nilStore.Dir() != "" {
		t.Fatal("nil Dir() should be empty")
	}
}

func TestOpenUnknownKeyProviderFailsClosed(t *testing.T) {
	clearStateEnv(t)
	t.Setenv(connectoragentstate.EnvKeyProvider, "not-a-provider")
	dir := secureStateTestDir(t)
	store, err := Open(dir)
	if err == nil {
		_ = store.Close()
		t.Fatal("unknown key provider accepted")
	}
	// Naming the rejected value proves the provider check failed, not an
	// earlier directory handshake.
	if !strings.Contains(err.Error(), connectoragentstate.EnvKeyProvider) || !strings.Contains(err.Error(), "not-a-provider") {
		t.Fatalf("Open() error = %v, want the provider-name refusal", err)
	}
	if _, err := os.Stat(filepath.Join(dir, AgentStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext envelope created: %v", err)
	}
}

// TestOpenLocalKeyWithoutDescriptorFailsClosed runs on every platform: the
// missing-descriptor refusal comes after the connector has validated the
// state directory, so it also proves the connector accepts a directory the
// CLI prepared (including its Windows owner-only DACL).
func TestOpenLocalKeyWithoutDescriptorFailsClosed(t *testing.T) {
	clearStateEnv(t)
	t.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderLocalKey)
	dir := secureStateTestDir(t)
	store, err := Open(dir)
	if err == nil {
		_ = store.Close()
		t.Fatal("local-key accepted without a key descriptor")
	}
	if !strings.Contains(err.Error(), connectoragentstate.EnvLocalKeyFD) {
		t.Fatalf("Open() error = %v, want the refusal naming %s", err, connectoragentstate.EnvLocalKeyFD)
	}
	for _, name := range []string{AgentStateFile, connectoragentstate.SealedAgentStateFile} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s created without a key: %v", name, err)
		}
	}
}
