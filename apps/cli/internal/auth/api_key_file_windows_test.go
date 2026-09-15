//go:build windows

package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsAPIKeyFileRejectsBroadACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(path, []byte(testKeyEnv+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token := windows.GetCurrentProcessToken()
	var err error
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatalf("read current Windows test user: %v", err)
	}
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		windowsAPIKeyTestAccess(user.User.Sid, windows.GENERIC_ALL),
		windowsAPIKeyTestAccess(adminSID, windows.GENERIC_ALL),
		windowsAPIKeyTestAccess(systemSID, windows.GENERIC_ALL),
		windowsAPIKeyTestAccess(worldSID, windows.GENERIC_READ),
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve(lookupFrom(map[string]string{EnvAPIKeyFile: path})); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("broad Windows API-key ACL error = %v, want ErrInvalidKey", err)
	}
}

func windowsAPIKeyTestAccess(sid *windows.SID, mask windows.ACCESS_MASK) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: mask,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
