//go:build windows

package state

import (
	"os"
	"path/filepath"
	"strings"
)

func defaultStateDir() string {
	base := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if base == "" || !filepath.IsAbs(base) {
		if cache, err := os.UserCacheDir(); err == nil {
			base = strings.TrimSpace(cache)
		}
	}
	if base == "" || !filepath.IsAbs(base) {
		return ""
	}
	return filepath.Join(base, stateSubdir)
}
