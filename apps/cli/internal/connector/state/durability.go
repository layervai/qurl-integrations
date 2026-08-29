package state

import (
	"fmt"
	"os"
	"runtime"
)

// syncDir fsyncs a state directory after an atomic rename or removal.
// Windows has no meaningful directory fsync; the rename remains atomic.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir) //nolint:gosec // G304: callers pass the store's validated state directory.
	if err != nil {
		return fmt.Errorf("open state directory for sync: %w", err)
	}
	defer func() { _ = file.Close() }()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}
