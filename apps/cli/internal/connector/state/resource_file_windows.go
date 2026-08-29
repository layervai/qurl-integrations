//go:build windows

package state

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsConnectorFileAllAccess = windows.ACCESS_MASK(0x001f01ff)

type windowsConnectorACLHeader struct {
	Revision byte
	Sbz1     byte
	Size     uint16
	ACECount uint16
	Sbz2     uint16
}

// The DACL buffer is returned as windows.ACL, whose fields are private. Keep
// the local read-only header view tied to the exact x/sys layout at compile
// time so a dependency change cannot corrupt the security decision.
var _ [unsafe.Sizeof(windows.ACL{})]byte = [unsafe.Sizeof(windowsConnectorACLHeader{})]byte{}
var _ [unsafe.Offsetof(windows.ACL{}.AceCount)]byte = [unsafe.Offsetof(windowsConnectorACLHeader{}.ACECount)]byte{}

func currentWindowsConnectorSecurity() (*windows.SID, *windows.SECURITY_DESCRIPTOR, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, nil, fmt.Errorf("read current Windows user SID: %w", err)
	}
	if user == nil || user.User.Sid == nil {
		return nil, nil, errors.New("read current Windows user SID: token has no user SID")
	}
	sid, err := windows.StringToSid(user.User.Sid.String())
	if err != nil {
		return nil, nil, err
	}
	sidText := sid.String()
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"O:%sG:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", sidText, sidText, sidText))
	if err != nil {
		return nil, nil, err
	}
	return sid, sd, nil
}

func openWindowsConnectorFile(path string, access, shareMode, disposition uint32,
	security *windows.SecurityAttributes,
) (windows.Handle, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(path16, access, shareMode,
		security, disposition, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

func validateWindowsConnectorFileHandle(handle windows.Handle) error { //nolint:gocyclo // Keep one fail-closed file and ACL decision tree.
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
		info.NumberOfLinks != 1 {
		return errors.New("connector state file must be a non-reparse, single-link file")
	}
	currentSID, _, err := currentWindowsConnectorSecurity()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read Connector state Windows ACL: %w", err)
	}
	if sd == nil {
		return errors.New("read Connector state Windows ACL: security descriptor is missing")
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(currentSID) {
		return errors.New("connector state file is not owned by the current Windows user")
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("connector state Windows ACL must be protected from inheritance")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New("connector state file has no restrictive Windows DACL")
	}
	adminSID, adminErr := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	systemSID, systemErr := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if adminErr != nil || systemErr != nil {
		return errors.New("build trusted Windows state identities")
	}
	header := (*windowsConnectorACLHeader)(unsafe.Pointer(dacl)) // #nosec G103 -- Windows returns a validated ACL buffer.
	var currentMask windows.ACCESS_MASK
	for index := uint32(0); index < uint32(header.ACECount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return fmt.Errorf("inspect Connector state Windows DACL entry %d: %w", index, err)
		}
		if ace == nil {
			return fmt.Errorf("inspect Connector state Windows DACL entry %d: entry is missing", index)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 {
			return errors.New("connector state Windows DACL contains an unsupported entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) // #nosec G103 -- GetAce returned this SID-backed ACE.
		if sid == nil || !sid.IsValid() {
			return errors.New("connector state Windows DACL contains an invalid identity")
		}
		switch {
		case sid.Equals(currentSID):
			currentMask |= ace.Mask
		case sid.Equals(adminSID), sid.Equals(systemSID):
		default:
			return errors.New("connector state Windows DACL grants another principal access")
		}
	}
	if currentMask&windowsConnectorFileAllAccess != windowsConnectorFileAllAccess &&
		currentMask&windows.GENERIC_ALL == 0 {
		return errors.New("connector state Windows DACL does not grant the current user full control")
	}
	return nil
}

func openConnectorResourceState(path string) (*os.File, error) {
	handle, err := openWindowsConnectorFile(path,
		windows.GENERIC_READ|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.OPEN_EXISTING, nil)
	if err != nil {
		return nil, err
	}
	if err := validateWindowsConnectorFileHandle(handle); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create Connector state Windows file handle")
	}
	return file, nil
}

func validateConnectorResourceFile(path string, _ os.FileInfo) error {
	file, err := openConnectorResourceState(path)
	if err != nil {
		return err
	}
	return file.Close()
}

func createConnectorResourceTemp(path string) (*os.File, error) {
	_, sd, err := currentWindowsConnectorSecurity()
	if err != nil {
		return nil, err
	}
	security := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := openWindowsConnectorFile(path,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.CREATE_NEW, security)
	if err != nil {
		return nil, err
	}
	if err := validateWindowsConnectorFileHandle(handle); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create Connector state Windows temporary file handle")
	}
	return file, nil
}

func commitConnectorResourceRename(from, to string) error { return windows.Rename(from, to) }
