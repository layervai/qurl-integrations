package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	qurl "github.com/layervai/qurl-go/qurl"
)

// clearStateEnv detaches the test from any ambient operator configuration.
func clearStateEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{EnvStateDirPrimary, EnvStateDir, EnvAgentID, "XDG_STATE_HOME"} {
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
	legacy := t.TempDir()

	t.Setenv(EnvStateDirPrimary, primary)
	t.Setenv(EnvStateDir, legacy)

	got, err := ResolveDir(override)
	if err != nil || got != override {
		t.Fatalf("ResolveDir(override) = (%q, %v), want the explicit override %q", got, err, override)
	}
	got, err = ResolveDir("")
	if err != nil || got != primary {
		t.Fatalf("ResolveDir() = (%q, %v), want %s value %q", got, err, EnvStateDirPrimary, primary)
	}
	if err := os.Unsetenv(EnvStateDirPrimary); err != nil {
		t.Fatal(err)
	}
	got, err = ResolveDir("")
	if err != nil || got != legacy {
		t.Fatalf("ResolveDir() = (%q, %v), want legacy %s value %q", got, err, EnvStateDir, legacy)
	}
}

func TestResolveDirXDGFallback(t *testing.T) {
	clearStateEnv(t)
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	got, err := ResolveDir("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(xdg, "qurl", "connector")
	if got != want {
		t.Fatalf("ResolveDir() = %q, want XDG state path %q", got, want)
	}

	// A relative XDG_STATE_HOME is ignored per the XDG spec; the home
	// fallback takes over.
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "relative/state")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir source
	got, err = ResolveDir("")
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(home, ".local", "state", "qurl", "connector")
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
	if isWindows(t) {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "loose")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
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

// openTestStore opens a Store in a fresh temp dir, skipping on platforms
// where qurl-go's pinned local agent state is unsupported (Windows today).
func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		if errors.Is(err, qurl.ErrAgentStateContinuity) && strings.Contains(err.Error(), "unsupported on this platform") {
			t.Skipf("qurl-go pinned agent state unsupported here: %v", err)
		}
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
	if err := store.RequestRefresh("still closed"); err == nil {
		t.Fatal("RequestRefresh() after Close = nil, want failure")
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

func TestStoreMarkerLifecycleThroughStore(t *testing.T) {
	store := openTestStore(t)

	if _, present, err := store.LoadRefreshMarker(); err != nil || present {
		t.Fatalf("LoadRefreshMarker on fresh dir = (present=%v, err=%v), want absent", present, err)
	}
	if err := store.RequestRefresh("sustained native NHP knock failures"); err != nil {
		t.Fatal(err)
	}
	marker, present, err := store.LoadRefreshMarker()
	if err != nil || !present {
		t.Fatalf("LoadRefreshMarker = (present=%v, err=%v), want armed marker", present, err)
	}
	if marker.Attempted || marker.Reason != "sustained native NHP knock failures" || marker.Version != refreshMarkerVersion || marker.SetAtUnix <= 0 {
		t.Fatalf("armed marker = %+v", marker)
	}
	if err := store.MarkRefreshAttempted(); err != nil {
		t.Fatal(err)
	}
	marker, present, err = store.LoadRefreshMarker()
	if err != nil || !present || !marker.Attempted {
		t.Fatalf("marker after MarkRefreshAttempted = (%+v, present=%v, err=%v), want Attempted=true", marker, present, err)
	}
	// Episode idempotency at the Store surface: re-arming does not reset the
	// consumed attempt.
	if err := store.RequestRefresh("later budget exit"); err != nil {
		t.Fatal(err)
	}
	marker, _, err = store.LoadRefreshMarker()
	if err != nil || !marker.Attempted || marker.Reason != "sustained native NHP knock failures" {
		t.Fatalf("marker after re-arm = (%+v, %v), want untouched attempted episode", marker, err)
	}
	if err := store.ClearRefreshMarker(); err != nil {
		t.Fatal(err)
	}
	if _, present, err = store.LoadRefreshMarker(); err != nil || present {
		t.Fatalf("marker after clear = (present=%v, err=%v), want absent", present, err)
	}
	// A new episode after a healthy clear starts unattempted.
	if err := store.RequestRefresh("fresh episode"); err != nil {
		t.Fatal(err)
	}
	marker, _, err = store.LoadRefreshMarker()
	if err != nil || marker.Attempted || marker.Reason != "fresh episode" {
		t.Fatalf("re-armed marker = (%+v, %v), want fresh unattempted episode", marker, err)
	}
}
