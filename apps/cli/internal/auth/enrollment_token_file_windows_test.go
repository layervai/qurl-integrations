//go:build windows

package auth

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsExternalEnrollmentToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrollment")
	if err := os.WriteFile(path, []byte("one-shot-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{windowsAPIKeyTestAccess(user.User.Sid, windows.GENERIC_ALL)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	token, err := ReadExternalEnrollmentTokenFile(path)
	if err != nil || token != "one-shot-token" {
		t.Fatalf("private file: token present %t, err %v", token != "", err)
	}
	hardlink := filepath.Join(filepath.Dir(path), "another-link")
	if err := os.Link(path, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExternalEnrollmentTokenFile(path); err == nil {
		t.Fatal("accepted multiply linked token")
	}
	if err := os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	acl, err = windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{windowsAPIKeyTestAccess(user.User.Sid, windows.GENERIC_ALL), windowsAPIKeyTestAccess(world, windows.GENERIC_READ)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExternalEnrollmentTokenFile(path); err == nil {
		t.Fatal("accepted broadly readable token")
	}
}
