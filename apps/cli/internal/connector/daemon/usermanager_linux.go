//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strings"
)

// systemdUserManagerAvailable reports whether this session can reach a
// systemd user manager: the host booted with systemd (sd_booted's
// /run/systemd/system test), XDG_RUNTIME_DIR is set, and a user manager has
// created its private control socket there. That socket is a proxy for "a
// user manager started", not the bus systemctl --user dials: a running
// manager whose user bus is absent probes as available, and its raw
// "Failed to connect to bus" error passes through. The probe is consulted
// only after the manager already failed, so it decides the message and exit
// code, never whether a working host fails.
//
// An unset or relative XDG_RUNTIME_DIR counts as unreachable even when a
// manager runs under /run/user/<uid>: systemctl --user cannot find its bus
// without it ("Failed to connect to bus: No medium found").
func systemdUserManagerAvailable(lookupEnv func(string) (string, bool)) bool {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return systemdUserManagerAvailableAt("/run", lookupEnv)
}

func systemdUserManagerAvailableAt(runRoot string, lookupEnv func(string) (string, bool)) bool {
	if info, err := os.Stat(filepath.Join(runRoot, "systemd", "system")); err != nil || !info.IsDir() {
		return false
	}
	value, ok := lookupEnv("XDG_RUNTIME_DIR")
	runtimeDir := strings.TrimSpace(value)
	if !ok || !filepath.IsAbs(runtimeDir) {
		return false
	}
	info, err := os.Stat(filepath.Join(runtimeDir, "systemd", "private"))
	return err == nil && info.Mode()&os.ModeSocket != 0
}
