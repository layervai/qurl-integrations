package daemon

import (
	"os"
	"runtime"
	"testing"
)

func shortTempDir(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if runtime.GOOS != "windows" {
		// Unix-domain socket paths are short and bounded. Hosted macOS temp
		// roots can exceed that bound before the test adds a socket name.
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "qurl-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
