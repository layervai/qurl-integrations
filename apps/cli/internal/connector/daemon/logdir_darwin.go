//go:build darwin

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// DefaultLogDir returns the owner-local macOS daemon log directory.
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
