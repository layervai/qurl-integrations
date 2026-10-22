//go:build !unix

package state

import "testing"

// changeStamp adds nothing on a platform that has no inode number and no
// change time. The snapshot still holds each entry's name, permission bits,
// size, modification time and content.
func changeStamp(*testing.T, string) string { return "" }
