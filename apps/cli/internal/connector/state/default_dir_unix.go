//go:build !windows

package state

import (
	"os"
	"path/filepath"
	"strings"
)

func defaultStateDir() string {
	if base := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); base != "" && filepath.IsAbs(base) {
		return filepath.Join(base, stateSubdir)
	}
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" || !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(home, ".local", "state", stateSubdir)
}
