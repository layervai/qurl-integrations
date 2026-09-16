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

const maxUnixSocketPathBytes = 100

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
	switch {
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return fmt.Errorf("share daemon socket directory %s must be a directory, not a symlink or a file (it is %s)", dir, info.Mode())
	case !unixIPCPathOwnerOK(info):
		// Do not suggest chmod here: it would fail, and the real answer is a
		// directory this user owns.
		return fmt.Errorf("share daemon socket directory %s must be owned by the user running qurl", dir)
	case info.Mode().Perm() != 0o700:
		return fmt.Errorf(
			"share daemon socket directory %s must have mode 0700 (it is %s); run: chmod 700 %s",
			dir, info.Mode().Perm(), dir)
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

// platformSocketPath places the socket in runtimeDir when one is pinned,
// below a state directory that fits sockaddr_un otherwise, and in a bounded
// owner-only per-user directory below /tmp as the last resort.
func platformSocketPath(stateDir, runtimeDir string) (string, error) {
	if runtimeDir != "" {
		if !filepath.IsAbs(runtimeDir) {
			return "", fmt.Errorf("%s must be an absolute path", RuntimeDirEnv)
		}
		// Daemon startup sets this directory to 0700, so the filesystem root is
		// never an acceptable answer.
		if runtimeDir == string(filepath.Separator) {
			return "", fmt.Errorf("%s must name a directory, not the filesystem root", RuntimeDirEnv)
		}
		path := filepath.Join(runtimeDir, SocketFile)
		if len(path) > maxUnixSocketPathBytes {
			return "", fmt.Errorf("%s socket path is too long: %d bytes exceeds the %d-byte socket limit", RuntimeDirEnv, len(path), maxUnixSocketPathBytes)
		}
		return path, nil
	}
	path := filepath.Join(stateDir, SocketFile)
	if len(path) <= maxUnixSocketPathBytes {
		return path, nil
	}
	digest := sha256.Sum256([]byte(path))
	// This replaced a shared /tmp/layerv-qurl-<uid>/<hash>.sock: a directory
	// per namespace is what makes the 0700 EnsureDirMode below meaningful. The
	// old directory is orphaned rather than cleaned up, and an externally
	// supervised daemon started by a pre-2.6 qurl keeps listening on the old
	// address, so its supervisor must restart it after the upgrade - a native
	// job recovers on its own because the binary version is part of the job
	// version.
	//
	// IPCServer.Run passes this predictable directory through EnsureDirMode
	// before listen. That helper rejects a symlink or a directory owned by any
	// other user before it changes permissions, so a pre-creation below /tmp
	// can only make startup fail closed. The root is the literal /tmp, not
	// os.TempDir(), so foreground daemons and clients agree even when their
	// TMPDIR differs.
	const runtimeRoot = "/tmp"
	path = filepath.Join(
		runtimeRoot,
		"qurl-"+strconv.Itoa(os.Geteuid())+"-"+hex.EncodeToString(digest[:16]),
		SocketFile,
	)
	// Unreachable with today's shape (/tmp/qurl-<uid>-<32 hex>/daemon.sock is
	// at most 65 bytes against the 100-byte budget, and a uid cannot exceed 10
	// digits), but kept so a later change to the derived name cannot silently
	// exceed the limit.
	if len(path) > maxUnixSocketPathBytes {
		return "", fmt.Errorf("share daemon socket path is too long below both the state and temp directories; set %s to a short owner-only directory", RuntimeDirEnv)
	}
	return path, nil
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
