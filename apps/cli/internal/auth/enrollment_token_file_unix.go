//go:build unix

package auth

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openExternalEnrollmentTokenNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create external enrollment token handle")
	}
	return file, nil
}

func validateOpenExternalEnrollmentToken(_ *os.File, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("external enrollment token must be owned by the current user")
	}
	return nil
}
