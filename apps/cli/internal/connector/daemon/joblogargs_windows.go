//go:build windows

package daemon

func daemonJobLogArguments(stdoutPath, stderrPath string) []string {
	return []string{"--job-stdout-log", stdoutPath, "--job-stderr-log", stderrPath}
}
