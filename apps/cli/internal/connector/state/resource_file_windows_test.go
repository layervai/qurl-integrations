//go:build windows

package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsConnectorResourceStateRejectsUnsafeACLAndHardlink(t *testing.T) {
	t.Run("resource ACL", func(t *testing.T) {
		store := openTestStore(t)
		tx, err := store.BeginConnectorResource(context.Background(), "windows-resource")
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Close(); err != nil {
			t.Fatal(err)
		}
		setWindowsConnectorTestACL(t, filepath.Join(store.Dir(), ConnectorResourcesFile), true)
		if _, err := store.BeginConnectorResource(context.Background(), "windows-resource"); err == nil {
			t.Fatal("Connector resource file with a broad Windows ACL was accepted")
		}
	})

	t.Run("lock ACL", func(t *testing.T) {
		store := openTestStore(t)
		tx, err := store.BeginConnectorResource(context.Background(), "windows-lock")
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Close(); err != nil {
			t.Fatal(err)
		}
		setWindowsConnectorTestACL(t, filepath.Join(store.Dir(), connectorResourcesLock), true)
		if _, err := store.BeginConnectorResource(context.Background(), "windows-lock"); err == nil {
			t.Fatal("Connector resource lock with a broad Windows ACL was accepted")
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		store := openTestStore(t)
		tx, err := store.BeginConnectorResource(context.Background(), "windows-hardlink")
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(store.Dir(), ConnectorResourcesFile)
		if err := os.Link(path, path+".link"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.BeginConnectorResource(context.Background(), "windows-hardlink"); err == nil {
			t.Fatal("hard-linked Connector resource file was accepted")
		}
	})
}

func TestWindowsLocalShareRegistryRejectsBroadACL(t *testing.T) {
	registry, err := openOwnedLocalShareRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	setWindowsConnectorTestACL(t, filepath.Join(registry.dir, LocalSharesFile), true)
	if _, err := registry.List(context.Background()); err == nil {
		t.Fatal("local-share registry with a broad Windows ACL was accepted")
	}
}

func TestWindowsConnectorResourceLockCannotBeReplacedWhileHeld(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	release, err := acquireConnectorResourcesLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, connectorResourcesLock)
	if err := os.Remove(path); err == nil {
		_ = release()
		t.Fatal("Windows Connector resource lock was deletable while held")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("released Windows Connector resource lock could not be removed: %v", err)
	}
}

func setWindowsConnectorTestACL(t *testing.T, path string, includeWorld bool) {
	t.Helper()
	currentSID, _, err := currentWindowsConnectorSecurity()
	if err != nil {
		t.Fatal(err)
	}
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		windowsConnectorTestAccess(currentSID, windows.GENERIC_ALL),
		windowsConnectorTestAccess(adminSID, windows.GENERIC_ALL),
		windowsConnectorTestAccess(systemSID, windows.GENERIC_ALL),
	}
	if includeWorld {
		worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, windowsConnectorTestAccess(worldSID, windows.GENERIC_READ))
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
}

func windowsConnectorTestAccess(sid *windows.SID, mask windows.ACCESS_MASK) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: mask,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
