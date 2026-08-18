package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// rootCmd builds the full command tree with real process defaults. The
// goreleaser contract test uses it to mirror release-time docs generation,
// which reads command metadata only and never runs a command.
func rootCmd(version string) *cobra.Command {
	root, _ := newRoot(version, output.Detect())
	return root
}

// testAPIKey is a shape-valid test credential for the harness environment.
const testAPIKey = "lv_test_abcdefghij0123456789"

// fixedNow is the harness clock: one day after the mock server's canned
// created_at timestamps, so relative times render deterministically.
var fixedNow = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

// runOpts configures one CLI invocation under test.
type runOpts struct {
	args  []string
	env   map[string]string // full environment; nil means just QURL_API_KEY
	stdin io.Reader
	inTTY bool
	tty   bool // stdout+stderr TTY-ness

	configDir string
	sleeps    *[]time.Duration
}

// runResult captures one invocation's streams and exit code.
type runResult struct {
	stdout bytes.Buffer
	stderr bytes.Buffer
	code   int
}

// runCLI executes the real command tree with injected process context: no
// real environment, no real TTYs, a fixed clock, and recorded sleeps.
func runCLI(t *testing.T, o *runOpts) *runResult {
	t.Helper()

	res := &runResult{}
	env := o.env
	if env == nil {
		env = map[string]string{"QURL_API_KEY": testAPIKey}
	}
	stdin := o.stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	configDir := o.configDir
	if configDir == "" {
		configDir = t.TempDir()
	}

	streams := &output.Streams{
		In:       stdin,
		Out:      &res.stdout,
		Err:      &res.stderr,
		InIsTTY:  o.inTTY,
		OutIsTTY: o.tty,
		ErrIsTTY: o.tty,
	}

	root, opts := newRoot("test", streams, func(g *globalOpts) {
		g.lookupEnv = func(key string) (string, bool) {
			v, ok := env[key]
			return v, ok
		}
		g.configDir = configDir
		g.now = func() time.Time { return fixedNow }
		g.newRequestID = func() string { return "cli-req-fixed" }
		if o.sleeps != nil {
			g.sleep = func(d time.Duration) { *o.sleeps = append(*o.sleeps, d) }
		} else {
			g.sleep = func(time.Duration) {} // tests never wall-clock sleep
		}
	})
	root.SetArgs(o.args)
	res.code = run(context.Background(), root, opts)
	return res
}

// mustEmptyStdout asserts the data stream carried nothing — the fail-closed
// contract for verification failures and errors.
func mustEmptyStdout(t *testing.T, res *runResult) {
	t.Helper()
	if res.stdout.Len() != 0 {
		t.Fatalf("stdout must be empty, got %q", res.stdout.String())
	}
}
