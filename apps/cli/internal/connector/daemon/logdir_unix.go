//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultLogDir returns the owner-local daemon log directory on Unix.
func DefaultLogDir(string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	home = strings.TrimSpace(home)
	if !filepath.IsAbs(home) {
		return "", errors.New("user home is not absolute")
	}
	return filepath.Join(home, "Library", "Logs", "qurl"), nil
}

func prepareDaemonLogDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create qURL daemon log directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil { // #nosec G302 -- owner-only directory mode, not a file mode.
		return fmt.Errorf("restrict qURL daemon log directory: %w", err)
	}
	return nil
}
