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
	if err := validateUnixIPCParent(path); err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := validateUnixIPCSocket(path); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	return conn, nil
}

func validateUnixIPCParent(path string) error {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect share daemon socket directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !unixIPCPathOwnerOK(info) || info.Mode().Perm() != 0o700 {
		return errors.New("share daemon socket directory must be an owner-owned non-symlink directory with mode 0700")
	}
	return nil
}

func validateUnixIPCSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect share daemon socket: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 || !unixIPCPathOwnerOK(info) {
		return errors.New("share daemon socket must be an owner-owned non-symlink socket with mode 0600")
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: share daemon socket must be an owner-owned non-symlink socket with mode 0600", errIPCSocketRestrictionPending)
	}
	return nil
}

func unixIPCPathOwnerOK(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
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
	// IPCServer.Run passes this predictable directory through EnsureDirMode
	// before listen. That helper rejects a symlink or a directory owned by any
	// other user before it changes permissions, so a /tmp pre-creation can only
	// make startup fail closed.
	return filepath.Join(
		unixIPCRuntimeRoot,
		"layerv-qurl-"+strconv.Itoa(os.Geteuid()),
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
