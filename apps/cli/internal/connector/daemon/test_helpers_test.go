package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func shortTempDir(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if runtime.GOOS != "windows" {
		// Unix-domain socket paths are short and bounded. Hosted macOS temp
		// roots can exceed that bound before the test adds a socket name.
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, "qurl-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dir := filepath.Join(root, "state")
	if err := connectorstate.EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}
