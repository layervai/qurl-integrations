//go:build !windows && !darwin

package daemon

import (
	"errors"
	"path/filepath"
	"strings"
)

// DefaultLogDir keeps non-macOS Unix logs inside the explicit, owner-bound
// state namespace used by Linux foreground and systemd-managed daemons.
func DefaultLogDir(stateDir string) (string, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" || !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir {
		return "", errors.New("qURL state directory is not an exact absolute path")
	}
	return filepath.Join(stateDir, "logs"), nil
}
