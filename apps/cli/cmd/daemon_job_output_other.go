//go:build !windows

package main

import (
	"errors"

	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func redirectDaemonJobOutput(stdoutPath, stderrPath string, _ *output.Streams) error {
	if stdoutPath != "" || stderrPath != "" {
		return errors.New("native background-job log flags are supported only on Windows")
	}
	return nil
}
