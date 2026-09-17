package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// shortTempRoot returns a temporary directory short enough to hold a
// unix-domain socket path. Hosted macOS temp roots can exceed that bound
// before a test adds a socket name, so on unix it anchors on /tmp rather than
// on os.TempDir().
func shortTempRoot(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if runtime.GOOS != "windows" {
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, "qurl-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

// shortTempDir returns a secured state directory below shortTempRoot.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(shortTempRoot(t), "state")
	if err := connectorstate.EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func lookupEnvFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}
