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
		if windowsNamedPipeCollision(err) {
			return nil, nil, fmt.Errorf("%w: Windows named pipe is live or inaccessible: %w", ErrAlreadyRunning, err)
		}
		return nil, nil, fmt.Errorf("listen on share daemon Windows named pipe: %w", err)
	}
	return listener, func() error { return nil }, nil
}

func dialDaemonIPC(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, windowsDaemonPipeName(path))
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
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read current Windows IPC user: %w", err)
	}
	if user == nil || user.User.Sid == nil {
		return "", errors.New("read current Windows IPC user: token has no user SID")
	}
	sid := user.User.Sid.String()
	return fmt.Sprintf("O:%sG:%sD:P(A;;GA;;;%s)(A;;GA;;;SY)(A;;GA;;;BA)", sid, sid, sid), nil
}

func windowsNamedPipeCollision(err error) bool {
	return errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) ||
		errors.Is(err, windows.ERROR_ALREADY_EXISTS) ||
		errors.Is(err, windows.ERROR_PIPE_BUSY) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
