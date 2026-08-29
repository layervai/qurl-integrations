//go:build windows

package daemon

import (
	"errors"
	"path/filepath"
	"strings"
)

// DefaultLogDir returns a dedicated child of the already-pinned state
// namespace. This also keeps an explicit state-directory override coherent.
func DefaultLogDir(stateDir string) (string, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" || !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir {
		return "", errors.New("windows qURL state directory is not an exact absolute path")
	}
	return filepath.Join(stateDir, "logs"), nil
}

// The Windows UserJob manager creates and validates the complete log path with
// no-follow handles before it opens either file. Do not pre-create it through
// pathname APIs here.
func prepareDaemonLogDir(string) error { return nil }
