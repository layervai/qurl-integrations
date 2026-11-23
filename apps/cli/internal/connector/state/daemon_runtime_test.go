package state

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDaemonRuntimeMarkerRoundTripAndReplacement(t *testing.T) {
	dir := secureStateTestDir(t)
	for _, runtimeDir := range []string{filepath.Join(dir, "first socket"), filepath.Join(dir, "second")} {
		if err := WriteDaemonRuntimeDir(dir, runtimeDir); err != nil {
			t.Fatal(err)
		}
		got, err := ReadDaemonRuntimeDir(dir)
		if err != nil || got != runtimeDir {
			t.Fatalf("runtime = %q, %v; want %q", got, err, runtimeDir)
		}
	}
}

func TestDaemonRuntimeReadDoesNotCreateState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	if got, err := ReadDaemonRuntimeDir(dir); err != nil || got != "" {
		t.Fatalf("missing marker = %q, %v", got, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("read created state: %v", err)
	}
}

func TestDaemonRuntimeRejectsMalformedAndOversizedMarkers(t *testing.T) {
	for _, data := range []string{"", "null", `"relative"`, `{"dir":"/tmp"}`, `"/tmp" "trailing"`, strings.Repeat("x", maxDaemonRuntimeBytes+1)} {
		t.Run(data[:min(len(data), 20)], func(t *testing.T) {
			dir := secureStateTestDir(t)
			if err := WriteDaemonRuntimeDir(dir, filepath.Join(dir, "runtime")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, daemonRuntimeFile), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadDaemonRuntimeDir(dir); err == nil {
				t.Fatal("accepted invalid marker")
			}
		})
	}
}

func TestDaemonRuntimeRejectsSymlinkAndLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink and mode contract")
	}
	dir := secureStateTestDir(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, daemonRuntimeFile)
	if err := os.Symlink(outside, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDaemonRuntimeDir(dir); err == nil {
		t.Fatal("read accepted symlink")
	}
	if err := WriteDaemonRuntimeDir(dir, filepath.Join(dir, "runtime")); err == nil {
		t.Fatal("write accepted symlink")
	}
	// #nosec G304 -- Read only the test-owned fixture to prove it was untouched.
	if data, err := os.ReadFile(outside); err != nil || string(data) != "untouched" {
		t.Fatal("outside file changed")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := WriteDaemonRuntimeDir(dir, filepath.Join(dir, "runtime")); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- Deliberately insecure fixture must be rejected.
	if err := os.Chmod(marker, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDaemonRuntimeDir(dir); err == nil {
		t.Fatal("accepted readable marker")
	}
}

func TestDaemonRuntimeOwnerRepairsRegularMarker(t *testing.T) {
	dir := secureStateTestDir(t)
	marker := filepath.Join(dir, daemonRuntimeFile)
	if err := WriteDaemonRuntimeDir(dir, filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		// #nosec G302 -- Deliberately insecure fixture must be repaired by its owner.
		if err := os.Chmod(marker, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadDaemonRuntimeDir(dir); err == nil {
		t.Fatal("client accepted invalid marker")
	}
	want := filepath.Join(dir, "new")
	if err := WriteDaemonRuntimeDir(dir, want); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadDaemonRuntimeDir(dir); err != nil || got != want {
		t.Fatalf("repaired marker = %q, %v", got, err)
	}
}
