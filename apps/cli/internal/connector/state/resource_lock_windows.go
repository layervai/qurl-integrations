//go:build windows

package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const connectorResourcesLockRetry = 25 * time.Millisecond

func acquireConnectorResourcesLock(ctx context.Context, dir string) (func() error, error) {
	if ctx == nil {
		return nil, errors.New("connector resource lock context is nil")
	}
	path := filepath.Join(dir, connectorResourcesLock)
	_, sd, err := currentWindowsConnectorSecurity()
	if err != nil {
		return nil, err
	}
	security := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := openWindowsConnectorFile(path,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.SYNCHRONIZE,
		// Keep readers and another lock contender compatible, but deny delete
		// sharing so no process can replace the named lock while this handle owns it.
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.OPEN_ALWAYS, security)
	if err != nil {
		return nil, fmt.Errorf("open Windows Connector resource lock: %w", err)
	}
	closeHandle := func(cause error) (func() error, error) {
		return nil, errors.Join(cause, windows.CloseHandle(handle))
	}
	if err := validateWindowsConnectorFileHandle(handle); err != nil {
		return closeHandle(err)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		return closeHandle(errors.New("create Windows Connector resource lock handle"))
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	overlapped := &windows.Overlapped{}
	timer := time.NewTimer(connectorResourcesLockRetry)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		err = windows.LockFileEx(handle,
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errors.Join(fmt.Errorf("acquire Windows Connector resource lock: %w", err), file.Close())
		}
		timer.Reset(connectorResourcesLockRetry)
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), file.Close())
		case <-timer.C:
		}
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		return nil, errors.Join(errors.New("windows Connector resource lock path changed while acquiring ownership"),
			windows.UnlockFileEx(handle, 0, 1, 0, overlapped), file.Close())
	}
	return func() error {
		return errors.Join(windows.UnlockFileEx(handle, 0, 1, 0, overlapped), file.Close())
	}, nil
}
