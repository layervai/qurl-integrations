//go:build (linux && !android) || (darwin && !ios)

package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const connectorResourcesLockRetry = 25 * time.Millisecond

func acquireConnectorResourcesLock(ctx context.Context, dir string) (func() error, error) {
	path := filepath.Join(dir, connectorResourcesLock)
	file, info, err := openValidatedConnectorResourcesLock(path)
	if err != nil {
		return nil, err
	}
	fd := int(file.Fd())
	if err := waitForConnectorResourcesLock(ctx, fd); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := revalidateConnectorResourcesLock(path, info); err != nil {
		return nil, errors.Join(err, unix.Flock(fd, unix.LOCK_UN), file.Close())
	}
	return func() error {
		return errors.Join(unix.Flock(fd, unix.LOCK_UN), file.Close())
	}, nil
}

func openValidatedConnectorResourcesLock(path string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, connectorResourceFileMode)
	if err != nil {
		return nil, nil, fmt.Errorf("open no-follow lock file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, nil, errors.New("create connector resource lock handle")
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("inspect Connector resource lock: %w", err), file.Close())
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("reinspect Connector resource lock path: %w", err), file.Close())
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != connectorResourceFileMode ||
		pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || !os.SameFile(info, pathInfo) ||
		!connectorResourceOwnerOK(info) || !connectorResourceOwnerOK(pathInfo) {
		return nil, nil, errors.Join(fmt.Errorf("connector resource lock must remain the same non-symlink regular %04o file", connectorResourceFileMode), file.Close())
	}
	return file, info, nil
}

func waitForConnectorResourcesLock(ctx context.Context, fd int) error {
	timer := time.NewTimer(connectorResourcesLockRetry)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("acquire Connector resource lock: %w", err)
		}
		timer.Reset(connectorResourcesLockRetry)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func revalidateConnectorResourcesLock(path string, info os.FileInfo) error {
	pathInfo, err := os.Lstat(path)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, pathInfo) {
		return errors.New("connector resource lock path changed while acquiring ownership")
	}
	return nil
}

func connectorResourceOwnerOK(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
