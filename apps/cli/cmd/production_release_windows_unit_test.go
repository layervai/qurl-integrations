//go:build clisandbox && cliproduction && windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"
	"golang.org/x/sys/windows"
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

func productionStateDir(t *testing.T) string {
	t.Helper()
	// Hosted-runner scratch volumes can grant unsafe ancestor control.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("find Windows proof user home")
	}
	dir, err := os.MkdirTemp(home, "qurl-production-proof-")
	if err != nil {
		t.Fatal("create Windows proof state directory")
	}
	t.Cleanup(func() {
		// Match testing.TempDir's bounded retry for closing Windows handles.
		deadline := time.Now().Add(2 * time.Second)
		for {
			err := os.RemoveAll(dir)
			if err == nil {
				return
			}
			if (!errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED)) || !time.Now().Before(deadline) {
				t.Error("remove owned Windows proof state directory")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || current == nil || current.User.Sid == nil {
		t.Fatal("read Windows proof current user SID")
	}
	sid := current.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"O:%sG:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", sid, sid, sid))
	if err != nil {
		t.Fatal("build protected Windows proof ACL")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal("read protected Windows proof DACL")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		t.Fatal("read protected Windows proof owner")
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil); err != nil {
		t.Fatal("protect owned Windows proof state directory")
	}
	return dir
}

func TestProductionProofWindowsStateCleanupAfterHandleClose(t *testing.T) {
	var dir string
	started := false
	released := make(chan error, 1)
	passed := t.Run("transient open handle", func(t *testing.T) {
		dir = productionStateDir(t)
		name, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			t.Fatal("encode owned Windows proof path")
		}
		handle, err := windows.CreateFile(name, windows.READ_CONTROL|windows.SYNCHRONIZE,
			windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			t.Fatal("hold owned Windows proof directory")
		}
		started = true
		go func() {
			time.Sleep(100 * time.Millisecond)
			released <- windows.CloseHandle(handle)
		}()
	})
	if !started {
		t.Fatal("Windows proof cleanup fixture did not start")
	}
	if err := <-released; err != nil {
		t.Fatal("close owned Windows proof directory handle")
	}
	if !passed {
		t.Fatal("Windows proof cleanup did not wait for its closing handle")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("Windows proof state directory remains after cleanup")
	}
}
