//go:build clisandbox && cliproduction && windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/layervai/qurl-go/qurl"
)

func TestProductionProofWindowsCredentialFile(t *testing.T) {
	key := "lv_test_" + "012345678901234567890123456789012345678901234"
	path := filepath.Join(t.TempDir(), "api-key-env")
	// Match the actual mint step: .NET file creation can choose a different
	// default owner from Go's private-file creation on an elevated runner.
	create := exec.Command("pwsh", "-NoProfile", "-NonInteractive", "-Command",
		"[IO.File]::WriteAllText($env:PROOF_FIXTURE_KEY_FILE, $env:PROOF_FIXTURE_KEY + \"`n\")")
	create.Env = append(os.Environ(), "PROOF_FIXTURE_KEY_FILE="+path, "PROOF_FIXTURE_KEY="+key)
	if err := create.Run(); err != nil {
		t.Fatal("write native fixture key")
	}
	if err := exec.Command("icacls.exe", path, "/setowner", os.Getenv("USERNAME")).Run(); err != nil {
		t.Fatal("set fixture key owner")
	}
	command := exec.Command("icacls.exe", path, "/inheritance:r", "/grant:r",
		os.Getenv("USERNAME")+":F", "*S-1-5-18:F", "*S-1-5-32-544:F")
	if err := command.Run(); err != nil {
		t.Fatal("protect fixture key ACL")
	}
	t.Setenv("QURL_API_KEY", "")
	t.Setenv("QURL_API_KEY_FILE", path)
	if productionCredential(t) != key {
		t.Fatal("production harness did not read its protected Windows key")
	}
	t.Run("native state directory", func(t *testing.T) {
		store, err := qurl.OpenFileAgentState(filepath.Join(t.TempDir(), "agent-state.json"))
		if err != nil {
			t.Logf("unprepared Windows state directory class: %s", productionFailureClass([]byte(err.Error())))
		} else if err := store.Close(); err != nil {
			t.Fatal("close unprepared Windows state probe")
		}
		store, err = qurl.OpenFileAgentState(filepath.Join(productionStateDir(t), "agent-state.json"))
		if err != nil {
			t.Fatalf("prepared Windows state directory rejected: %s", productionFailureClass([]byte(err.Error())))
		}
		defer func() {
			if err := store.Close(); err != nil {
				t.Error("close prepared Windows state probe")
			}
		}()
		if err := store.ValidateContinuity(); err != nil {
			t.Fatal("prepared Windows state directory failed SDK continuity validation")
		}
	})
}
