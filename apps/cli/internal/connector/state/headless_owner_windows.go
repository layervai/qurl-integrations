//go:build windows

package state

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsPinnedFileWriteAccess = windows.ACCESS_MASK(
	windows.GENERIC_ALL |
		windows.GENERIC_WRITE |
		windows.FILE_WRITE_DATA |
		windows.FILE_APPEND_DATA |
		windows.WRITE_DAC |
		windows.WRITE_OWNER |
		windows.DELETE,
)

// pinnedFileWritableByAnotherUser applies the Windows equivalent of the Unix
// group/other-write check. Inherited read entries are accepted. Any allow ACE
// that lets an identity other than the current user, LocalSystem, or the local
// Administrators group replace or modify the file fails closed.
func pinnedFileWritableByAnotherUser(file *os.File, _ os.FileInfo) (bool, error) { //nolint:gocyclo // Keep one fail-closed ACL decision tree.
	currentSID, _, err := currentWindowsConnectorSecurity()
	if err != nil {
		return false, err
	}
	adminSID, adminErr := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	systemSID, systemErr := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if adminErr != nil || systemErr != nil {
		return false, errors.New("build trusted Windows headless-config identities")
	}
	trusted := func(sid *windows.SID) bool {
		return sid != nil && (sid.Equals(currentSID) || sid.Equals(adminSID) || sid.Equals(systemSID))
	}

	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, fmt.Errorf("read Windows headless-config ACL: %w", err)
	}
	if descriptor == nil {
		return false, errors.New("read Windows headless-config ACL: security descriptor is missing")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, fmt.Errorf("read Windows headless-config owner: %w", err)
	}
	if !trusted(owner) {
		return true, nil
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return false, fmt.Errorf("read Windows headless-config DACL: %w", err)
	}
	if dacl == nil {
		return true, nil
	}
	header := (*windowsConnectorACLHeader)(unsafe.Pointer(dacl)) // #nosec G103 -- Windows returns a validated ACL buffer.
	for index := uint32(0); index < uint32(header.ACECount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return false, fmt.Errorf("inspect Windows headless-config DACL entry %d: %w", index, err)
		}
		if ace == nil {
			return false, fmt.Errorf("inspect Windows headless-config DACL entry %d: entry is missing", index)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return false, errors.New("windows headless-config DACL contains an unsupported entry")
		}
		if ace.Mask&windowsPinnedFileWriteAccess == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) // #nosec G103 -- GetAce returned this SID-backed ACE.
		if sid == nil || !sid.IsValid() {
			return false, errors.New("windows headless-config DACL contains an invalid identity")
		}
		if !trusted(sid) {
			return true, nil
		}
	}
	return false, nil
}

// Enrollment credentials remain fail-closed on Windows until the headless
// bootstrap has a separately reviewed bearer-secret ACL contract.
func sensitiveFileReadableByProcess(os.FileInfo) bool { return false }

func validatePinnedFileParent(string) error { return nil }
