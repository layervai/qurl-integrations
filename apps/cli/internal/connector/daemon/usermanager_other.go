//go:build !linux

package daemon

// systemdUserManagerAvailable is Linux-only; launchd and the Windows task
// scheduler are always present, so their failures keep their own detail.
func systemdUserManagerAvailable(func(string) (string, bool)) bool { return true }
