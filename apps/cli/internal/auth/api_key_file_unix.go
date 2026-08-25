//go:build (linux && !android) || (darwin && !ios)

package auth

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func validateAPIKeyFilePlatform(info os.FileInfo) error {
	value, ok := info.Sys().(*syscall.Stat_t)
	if !ok || value.Nlink != 1 || (value.Uid != 0 && value.Uid != uint32(os.Geteuid())) {
		return errors.New("API-key file is not singly linked and owned by root or the effective user")
	}
	return nil
}

func openAPIKeyFileNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create API-key file handle")
	}
	return file, nil
}
