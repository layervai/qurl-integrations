//go:build (linux && !android) || (darwin && !ios)

package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenConnectorResourceStateRefusesFinalSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("state"), connectorResourceFileMode); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	file, err := openConnectorResourceState(link)
	if file != nil {
		_ = file.Close()
		t.Fatal("no-follow state open returned a file for a symlink")
	}
	if err == nil {
		t.Fatal("no-follow state open accepted a symlink")
	}
}
