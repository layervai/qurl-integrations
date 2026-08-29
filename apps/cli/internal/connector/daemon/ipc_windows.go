//go:build windows

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const windowsDaemonPipePrefix = `\\.\pipe\layerv-qurl-share-daemon-`

func listenDaemonIPC(_ context.Context, path string) (net.Listener, func() error, error) {
	pipeName := windowsDaemonPipeName(path)
	security, err := currentWindowsIPCSecurityDescriptor()
	if err != nil {
		return nil, nil, err
	}
	listener, err := winio.ListenPipe(pipeName, &winio.PipeConfig{
		SecurityDescriptor: security,
		MessageMode:        false,
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, nil, fmt.Errorf("listen on share daemon Windows named pipe: the pipe is held by a process this user cannot inspect: %w", err)
		}
		if windowsNamedPipeCollision(err) {
			return nil, nil, fmt.Errorf("%w: Windows named pipe is already live: %w", ErrAlreadyRunning, err)
		}
		return nil, nil, fmt.Errorf("listen on share daemon Windows named pipe: %w", err)
	}
	return listener, func() error { return nil }, nil
}

func dialDaemonIPC(ctx context.Context, path string) (net.Conn, error) {
	conn, err := winio.DialPipeAccess(ctx, windowsDaemonPipeName(path), uint32(windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL))
	if err != nil {
		return nil, err
	}
	if err := validateWindowsDaemonPipeServer(conn); err != nil {
		return nil, errors.Join(fmt.Errorf("verify share daemon Windows named-pipe server: %w", err), conn.Close())
	}
	return conn, nil
}

func validatePlatformIPCPath(string) error { return nil }

func isUnavailableIPCError(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func windowsDaemonPipeName(path string) string {
	canonical := strings.ToLower(filepath.Clean(path))
	digest := sha256.Sum256([]byte(canonical))
	return windowsDaemonPipePrefix + hex.EncodeToString(digest[:])
}

func currentWindowsIPCSecurityDescriptor() (string, error) {
	userSID, err := currentWindowsIPCUserSID()
	if err != nil {
		return "", err
	}
	sid := userSID.String()
	return fmt.Sprintf("O:%sG:%sD:P(A;;GA;;;%s)(A;;GA;;;SY)(A;;GA;;;BA)", sid, sid, sid), nil
}

func currentWindowsIPCUserSID() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current Windows IPC user: %w", err)
	}
	if user == nil || user.User.Sid == nil {
		return nil, errors.New("read current Windows IPC user: token has no user SID")
	}
	return user.User.Sid, nil
}

func validateWindowsDaemonPipeServer(conn net.Conn) error {
	// TODO(upstream-contract): go-winio's pipe connection must expose Fd() as
	// the underlying Windows handle so the client can verify the server owner.
	handleConn, ok := conn.(interface{ Fd() uintptr })
	if !ok || handleConn.Fd() == 0 {
		return errors.New("named-pipe connection does not expose a valid Windows handle")
	}
	descriptor, err := windows.GetSecurityInfo(
		windows.Handle(handleConn.Fd()),
		windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("read named-pipe owner: %w", err)
	}
	if descriptor == nil {
		return errors.New("read named-pipe owner: security descriptor is nil")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read named-pipe owner SID: %w", err)
	}
	current, err := currentWindowsIPCUserSID()
	if err != nil {
		return err
	}
	if owner == nil || !owner.Equals(current) {
		return errors.New("named-pipe server is not owned by the current user")
	}
	return nil
}

func windowsNamedPipeCollision(err error) bool {
	return errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) ||
		errors.Is(err, windows.ERROR_ALREADY_EXISTS) ||
		errors.Is(err, windows.ERROR_PIPE_BUSY)
}
