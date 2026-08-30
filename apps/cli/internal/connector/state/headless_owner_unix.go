//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func pinnedFileWritableByAnotherUser(_ *os.File, info os.FileInfo) (bool, error) {
	return info.Mode().Perm()&0o022 != 0, nil
}

func sensitiveFileReadableByProcess(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	if stat.Uid != 0 && int(stat.Uid) != os.Geteuid() {
		return false
	}
	perm := info.Mode().Perm()
	if perm&0o400 != 0 && int(stat.Uid) == os.Geteuid() {
		return true
	}
	if perm&0o040 == 0 {
		return false
	}
	fileGID := int(stat.Gid)
	if fileGID == os.Getegid() {
		return true
	}
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, group := range groups {
		if group == fileGID {
			return true
		}
	}
	return false
}

func validatePinnedFileParent(path string) error {
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("inspect file parent directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("file parent must be a non-symlink directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && int(stat.Uid) != os.Geteuid()) {
		return errors.New("file parent directory must be owned by root or the current user")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("file parent directory must not be writable by another local user")
	}
	return nil
}
