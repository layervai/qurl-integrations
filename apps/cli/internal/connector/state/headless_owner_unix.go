//go:build unix

package state

import (
	"os"
	"syscall"
)

func sensitiveFileReadableByProcess(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
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
