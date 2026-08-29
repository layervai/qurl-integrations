//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

type windowsDaemonLogACLHeader struct {
	Revision byte
	Sbz1     byte
	Size     uint16
	ACECount uint16
	Sbz2     uint16
}

func redirectDaemonJobOutput(stdoutPath, stderrPath string, streams *output.Streams) error {
	stdoutPath = strings.TrimSpace(stdoutPath)
	stderrPath = strings.TrimSpace(stderrPath)
	if stdoutPath == "" && stderrPath == "" {
		return nil
	}
	if streams == nil || stdoutPath == "" || stderrPath == "" || stdoutPath == stderrPath ||
		!filepath.IsAbs(stdoutPath) || !filepath.IsAbs(stderrPath) ||
		filepath.Clean(stdoutPath) != stdoutPath || filepath.Clean(stderrPath) != stderrPath {
		return errors.New("windows background-job log paths must be distinct exact absolute paths")
	}
	stdout, err := openProtectedWindowsDaemonLog(stdoutPath)
	if err != nil {
		return fmt.Errorf("open Windows background-job stdout log: %w", err)
	}
	stderr, err := openProtectedWindowsDaemonLog(stderrPath)
	if err != nil {
		_ = stdout.Close()
		return fmt.Errorf("open Windows background-job stderr log: %w", err)
	}
	// The background process owns these handles until process exit. Task
	// Scheduler starts the qurl binary directly so /End stops the real daemon.
	os.Stdout, os.Stderr = stdout, stderr
	streams.Out, streams.Err = stdout, stderr
	streams.OutIsTTY, streams.ErrIsTTY = false, false
	return nil
}

func openProtectedWindowsDaemonLog(path string) (*os.File, error) { //nolint:gocognit,gocyclo // Keep one fail-closed handle and ACL decision tree.
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(path16,
		windows.FILE_APPEND_DATA|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	closeOnError := func(cause error) (*os.File, error) {
		return nil, errors.Join(cause, windows.CloseHandle(handle))
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return closeOnError(err)
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
		info.NumberOfLinks != 1 {
		return closeOnError(errors.New("windows background-job log must be a non-reparse, single-link file"))
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return closeOnError(fmt.Errorf("read current Windows background-job identity: %w", err))
	}
	if user == nil || user.User.Sid == nil {
		return closeOnError(errors.New("read current Windows background-job identity: token has no user SID"))
	}
	adminSID, adminErr := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	systemSID, systemErr := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if adminErr != nil || systemErr != nil {
		return closeOnError(errors.New("build trusted Windows background-job identities"))
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return closeOnError(fmt.Errorf("read Windows background-job log ACL: %w", err))
	}
	if descriptor == nil {
		return closeOnError(errors.New("read Windows background-job log ACL: descriptor is missing"))
	}
	owner, _, ownerErr := descriptor.Owner()
	if ownerErr != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return closeOnError(errors.New("windows background-job log is not owned by the current user"))
	}
	control, _, controlErr := descriptor.Control()
	if controlErr != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return closeOnError(errors.New("windows background-job log ACL must be protected from inheritance"))
	}
	dacl, _, daclErr := descriptor.DACL()
	if daclErr != nil || dacl == nil {
		return closeOnError(errors.New("windows background-job log has no restrictive DACL"))
	}
	header := (*windowsDaemonLogACLHeader)(unsafe.Pointer(dacl)) // #nosec G103 -- Windows returns a validated ACL buffer.
	for index := uint32(0); index < uint32(header.ACECount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return closeOnError(fmt.Errorf("inspect Windows background-job log DACL: %w", err))
		}
		if ace == nil {
			return closeOnError(errors.New("inspect Windows background-job log DACL: entry is missing"))
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 {
			return closeOnError(errors.New("windows background-job log has an unsupported DACL entry"))
		}
		principal := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) // #nosec G103 -- GetAce returned this SID-backed ACE.
		if principal == nil || !principal.IsValid() ||
			(!principal.Equals(user.User.Sid) && !principal.Equals(adminSID) && !principal.Equals(systemSID)) {
			return closeOnError(errors.New("windows background-job log grants another principal access"))
		}
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		return closeOnError(errors.New("create Windows background-job log file handle"))
	}
	return file, nil
}
