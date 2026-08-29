//go:build !windows

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const (
	maxUnixSocketPathBytes = 100
	unixIPCRuntimeRoot     = "/tmp"
)

var dialUnixSocket = func(path string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, timeout)
}

func listenDaemonIPC(ctx context.Context, path string) (net.Listener, func() error, error) {
	if err := prepareSocket(path); err != nil {
		return nil, nil, err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on share daemon socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, nil, fmt.Errorf("restrict share daemon socket: %w", err)
	}
	cleanup := func() error {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return listener, cleanup, nil
}

func dialDaemonIPC(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

func validatePlatformIPCPath(path string) error {
	if len(path) > maxUnixSocketPathBytes {
		return errors.New("share daemon socket path is too long")
	}
	return nil
}

func platformStateSocketPath(path string) string {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || len(path) <= maxUnixSocketPathBytes {
		return path
	}
	digest := sha256.Sum256([]byte(path))
	return filepath.Join(
		unixIPCRuntimeRoot,
		"layerv-qurl-"+strconv.Itoa(os.Getuid()),
		hex.EncodeToString(digest[:16])+".sock",
	)
}

func prepareSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return errors.New("refuse non-socket share daemon IPC path")
	}
	conn, dialErr := dialUnixSocket(path, 200*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return ErrAlreadyRunning
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return fmt.Errorf("%w: existing daemon socket could not be confirmed stale: %w", ErrAlreadyRunning, dialErr)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale share daemon socket: %w", err)
	}
	return nil
}

func isUnavailableIPCError(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}
