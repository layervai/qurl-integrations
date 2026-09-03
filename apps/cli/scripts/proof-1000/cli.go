package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CLI exit codes this harness reacts to (apps/cli/internal/exitcode).
const (
	cliExitOK          = 0
	cliExitRateLimited = 9
	cliExitServerError = 10
	cliExitUnavailable = 11
)

const (
	flagOutput = "-o"
	outputJSON = "json"
	flagYes    = "--yes"
)

// apiCalls is what `-v` diagnostics reveal about one CLI invocation: every
// request the CLI's own transport made and how the service answered. SDK
// access-flow traffic (the platform knock and content fetch) is not logged
// there and is deliberately not counted.
type apiCalls struct {
	Total      int            `json:"total"`
	ByStatus   map[string]int `json:"by_status,omitempty"`
	TooMany    int            `json:"http_429"`
	RetryWaits time.Duration  `json:"-"`
}

type cliResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Wall     time.Duration
	Calls    apiCalls
	Err      error
}

var (
	verboseRequest  = regexp.MustCompile(`^\[debug\] > [A-Z]+ /`)
	verboseResponse = regexp.MustCompile(`^\[debug\] < HTTP (\d{3})(?:, retrying in (\S+))?`)
)

// runCLI executes one qurl command with the harness environment and a
// timeout. It never fails the harness itself: every outcome, including a
// missing binary, is a result the caller records.
func runCLI(ctx context.Context, bin string, env []string, timeout time.Duration, args ...string) *cliResult {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, args...) //nolint:gosec // G204: bin is the operator's --qurl/--consume-qurl flag; args are harness constants and CRIDs it published.
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	started := time.Now()
	err := cmd.Run()
	result := &cliResult{
		Stdout: stdout.String(), Stderr: stderr.String(), Wall: time.Since(started),
		Calls: parseAPICalls(stderr.String()),
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		result.ExitCode = cliExitOK
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	default:
		result.ExitCode = -1
		result.Err = err
	}
	if runCtx.Err() != nil && result.Err == nil {
		result.Err = runCtx.Err()
	}
	return result
}

func parseAPICalls(stderr string) apiCalls {
	calls := apiCalls{ByStatus: map[string]int{}}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if verboseRequest.MatchString(line) {
			calls.Total++
			continue
		}
		m := verboseResponse.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		calls.ByStatus[m[1]]++
		if m[1] == "429" {
			calls.TooMany++
		}
		if m[2] != "" {
			if d, err := time.ParseDuration(m[2]); err == nil {
				calls.RetryWaits += d
			}
		}
	}
	if len(calls.ByStatus) == 0 {
		calls.ByStatus = nil
	}
	return calls
}

// lastErrorLine returns the CLI's final "Error:" line, or the last
// non-diagnostic line, so a failure is explained by the CLI's own words.
func lastErrorLine(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	last := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[debug]") {
			continue
		}
		if strings.HasPrefix(line, "Error:") {
			return line
		}
		last = line
	}
	return last
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func itoa(i int) string { return strconv.Itoa(i) }
