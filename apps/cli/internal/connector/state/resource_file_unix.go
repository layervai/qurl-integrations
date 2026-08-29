//go:build (linux && !android) || (darwin && !ios)

package state

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openConnectorResourceState(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open no-follow Connector resource state: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create Connector resource state handle")
	}
	return file, nil
}

func validateConnectorResourceFile(_ string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("connector state file must be owned by the current user")
	}
	if info.Mode().Perm() != connectorResourceFileMode {
		return fmt.Errorf("connector state file has mode %04o, want %04o", info.Mode().Perm(), connectorResourceFileMode)
	}
	return nil
}

func createConnectorResourceTemp(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, connectorResourceFileMode) //nolint:gosec // fixed owner-only mode in a pinned owner-only directory.
}

func commitConnectorResourceRename(from, to string) error { return os.Rename(from, to) }
