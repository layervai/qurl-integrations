//go:build !windows

package daemon

import (
	"fmt"
	"os"
)

func prepareDaemonLogDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create qURL daemon log directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil { // #nosec G302 -- owner-only directory mode, not a file mode.
		return fmt.Errorf("restrict qURL daemon log directory: %w", err)
	}
	return nil
}
