//go:build windows

package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		windowsAPIKeyTestAccess(user.User.Sid, windows.GENERIC_ALL),
		windowsAPIKeyTestAccess(admin, windows.GENERIC_ALL),
		windowsAPIKeyTestAccess(system, windows.GENERIC_ALL),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, user.User.Sid, nil, acl, nil); err != nil {
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
	if _, err := ReadExternalEnrollmentTokenFile(path); err == nil || !strings.Contains(err.Error(), "protected private access and exactly one link") {
		t.Fatalf("hard-linked token rejection: %v", err)
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
	if _, err := ReadExternalEnrollmentTokenFile(path); err == nil || !strings.Contains(err.Error(), "protected private access and exactly one link") {
		t.Fatalf("broad-ACL token rejection: %v", err)
	}
}

func TestWindowsExternalEnrollmentTokenRejectsInheritedACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrollment")
	if err := os.WriteFile(path, []byte("one-shot-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExternalEnrollmentTokenFile(path); err == nil || !strings.Contains(err.Error(), "protected private access and exactly one link") {
		t.Fatalf("inherited-ACL token rejection: %v", err)
	}
}

func TestWindowsExternalEnrollmentTokenRejectsSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrollment")
	if err := os.WriteFile(path, []byte("one-shot-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skip("Windows symlink privilege unavailable")
		}
		t.Fatal(err)
	}
	if _, err := ReadExternalEnrollmentTokenFile(link); err == nil || !strings.Contains(err.Error(), "private regular file") {
		t.Fatalf("symlink token rejection: %v", err)
	}
}

func TestWindowsExternalEnrollmentTokenPathShape(t *testing.T) {
	for _, path := range []string{"", "relative", `C:relative`, `C:\tokens\child\..\token`, ` C:\tokens\token`, `C:\tokens\token `} {
		if err := ValidateExternalEnrollmentTokenPath(path); err == nil || !strings.Contains(err.Error(), "absolute, clean path") {
			t.Fatalf("invalid path accepted: %q (%v)", path, err)
		}
	}
	if err := ValidateExternalEnrollmentTokenPath(`C:\tokens\token`); err != nil {
		t.Fatal(err)
	}
}
