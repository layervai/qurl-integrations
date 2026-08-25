//go:build linux && !android

package matchedcohort

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func singleLinkOwnedByRootOrCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1 && (stat.Uid == 0 || stat.Uid == uint32(os.Geteuid()))
}

func openRegularNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create strict regular-file handle")
	}
	return file, nil
}

func commandForOpenedExecutable(ctx context.Context, executable, deployment *os.File, arguments ...string) (*exec.Cmd, string, error) {
	if executable == nil || deployment == nil {
		return nil, "", errors.New("opened executable or deployment is absent")
	}
	executablePath, deploymentPath := "/proc/self/fd/3", "/proc/self/fd/4"
	command := exec.CommandContext(ctx, executablePath, arguments...) //nolint:gosec // The exact verified open inode is fd 3.
	command.ExtraFiles = []*os.File{executable, deployment}
	return command, deploymentPath, nil
}
