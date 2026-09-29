//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// systemdUserManagerAvailable reports whether this session can reach a
// systemd user manager: the host booted with systemd (sd_booted's
// /run/systemd/system test) and this user's manager has created its private
// control socket in the user runtime directory.
func systemdUserManagerAvailable(lookupEnv func(string) (string, bool)) bool {
	return systemdUserManagerAvailableAt("/run", lookupEnv)
}

func systemdUserManagerAvailableAt(runRoot string, lookupEnv func(string) (string, bool)) bool {
	if info, err := os.Stat(filepath.Join(runRoot, "systemd", "system")); err != nil || !info.IsDir() {
		return false
	}
	runtimeDir := filepath.Join(runRoot, "user", strconv.Itoa(os.Geteuid()))
	if lookupEnv != nil {
		if value, ok := lookupEnv("XDG_RUNTIME_DIR"); ok && filepath.IsAbs(strings.TrimSpace(value)) {
			runtimeDir = strings.TrimSpace(value)
		}
	}
	info, err := os.Stat(filepath.Join(runtimeDir, "systemd", "private"))
	return err == nil && info.Mode()&os.ModeSocket != 0
}
