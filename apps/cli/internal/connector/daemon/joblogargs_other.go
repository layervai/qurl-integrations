//go:build !windows

package daemon

func daemonJobLogArguments(string, string) []string { return nil }
