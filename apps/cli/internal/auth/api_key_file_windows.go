//go:build windows

package auth

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsAPIKeyACLHeader struct {
	Revision byte
	Sbz1     byte
	Size     uint16
	ACECount uint16
	Sbz2     uint16
}

// The DACL buffer is returned as windows.ACL, whose fields are private. Keep
// the local read-only header view tied to the exact x/sys layout at compile
// time so a dependency change cannot corrupt the security decision.
var _ [unsafe.Sizeof(windows.ACL{})]byte = [unsafe.Sizeof(windowsAPIKeyACLHeader{})]byte{}
var _ [unsafe.Offsetof(windows.ACL{}.AceCount)]byte = [unsafe.Offsetof(windowsAPIKeyACLHeader{}.ACECount)]byte{}

func validAPIKeyEnvironmentFileMode(os.FileMode) bool { return true }

func validateAPIKeyFilePathPlatform(path string, _ os.FileInfo) error {
	file, err := openAPIKeyFileNoFollow(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return validateWindowsAPIKeyFile(file)
}

func validateOpenAPIKeyFilePlatform(file *os.File, _ os.FileInfo) error {
	return validateWindowsAPIKeyFile(file)
}

func openAPIKeyFileNoFollow(path string) (*os.File, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(path16,
		windows.GENERIC_READ|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create Windows API-key file handle")
	}
	return file, nil
}

func validateWindowsAPIKeyFile(file *os.File) error { //nolint:gocognit,gocyclo // Keep one fail-closed file and ACL decision tree.
	if file == nil {
		return errors.New("windows API-key file handle is nil")
	}
	handle := windows.Handle(file.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
		info.NumberOfLinks != 1 {
		return errors.New("windows API-key file must be a non-reparse, single-link file")
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current Windows API-key user: %w", err)
	}
	if user == nil || user.User.Sid == nil {
		return errors.New("read current Windows API-key user: token has no user SID")
	}
	currentSID := user.User.Sid
	adminSID, adminErr := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	systemSID, systemErr := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if adminErr != nil || systemErr != nil {
		return errors.New("build trusted Windows API-key identities")
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read Windows API-key ACL: %w", err)
	}
	if descriptor == nil {
		return errors.New("read Windows API-key ACL: security descriptor is missing")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(currentSID) {
		return errors.New("windows API-key file is not owned by the current user")
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("windows API-key file ACL must be protected from inheritance")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return errors.New("windows API-key file has no restrictive DACL")
	}
	header := (*windowsAPIKeyACLHeader)(unsafe.Pointer(dacl)) // #nosec G103 -- Windows returns a validated ACL buffer.
	for index := uint32(0); index < uint32(header.ACECount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return fmt.Errorf("inspect Windows API-key DACL entry %d: %w", index, err)
		}
		if ace == nil {
			return fmt.Errorf("inspect Windows API-key DACL entry %d: entry is missing", index)
		}
		// A customer-supplied credential file can have extra restrictive DENY
		// entries. They cannot grant access, so accept them. Likewise, a
		// zero-mask ALLOW entry for another SID grants no access. The Connector
		// state and daemon log files are created by qurl with one canonical ACL,
		// so their validators intentionally reject either form as unexpected.
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("windows API-key DACL contains an unsupported access entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) // #nosec G103 -- GetAce returned this SID-backed ACE.
		if sid == nil || !sid.IsValid() {
			return errors.New("windows API-key DACL contains an invalid identity")
		}
		if !sid.Equals(currentSID) && !sid.Equals(adminSID) && !sid.Equals(systemSID) && ace.Mask != 0 {
			return errors.New("windows API-key DACL grants another principal access")
		}
	}
	return nil
}
