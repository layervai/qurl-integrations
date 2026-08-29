//go:build unix

package state

import (
	"os"
	"syscall"
	"testing"
	"time"
)

type ownerTestFileInfo struct {
	mode os.FileMode
	uid  uint32
	gid  uint32
}

func (i ownerTestFileInfo) Name() string       { return "credential" }
func (i ownerTestFileInfo) Size() int64        { return 1 }
func (i ownerTestFileInfo) Mode() os.FileMode  { return i.mode }
func (i ownerTestFileInfo) ModTime() time.Time { return time.Time{} }
func (i ownerTestFileInfo) IsDir() bool        { return false }
func (i ownerTestFileInfo) Sys() any {
	return &syscall.Stat_t{Uid: i.uid, Gid: i.gid}
}

func TestSensitiveFileReadableByProcessRequiresTrustedOwner(t *testing.T) {
	currentUID := uint32(os.Geteuid())
	currentGID := uint32(os.Getegid())
	foreignUID := currentUID + 1

	if sensitiveFileReadableByProcess(ownerTestFileInfo{mode: 0o440, uid: foreignUID, gid: currentGID}) {
		t.Fatal("foreign-owned group-readable credential was accepted")
	}
	if !sensitiveFileReadableByProcess(ownerTestFileInfo{mode: 0o440, uid: 0, gid: currentGID}) {
		t.Fatal("root-owned dedicated-group credential was rejected")
	}
	if !sensitiveFileReadableByProcess(ownerTestFileInfo{mode: 0o400, uid: currentUID, gid: currentGID}) {
		t.Fatal("current-user-owned credential was rejected")
	}
}
