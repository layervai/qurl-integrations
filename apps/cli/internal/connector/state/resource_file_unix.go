//go:build (linux && !android) || (darwin && !ios)

package state

import (
	"errors"
	"fmt"
	"os"

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
