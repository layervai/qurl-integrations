// Command proof-1000 is the client-side harness for the "N concurrent local
// shares from one machine" proof. It publishes N loopback shares through the
// real qurl CLI against one local origin, watches the resident share daemon
// until every share serves, verifies a sample end to end through the qURL
// platform, holds at steady state while re-sampling, and writes a report.
//
// It never edits daemon state directly: every mutation goes through the CLI
// (`publish`, `delete`), and every observation comes from the daemon's
// owner-only IPC status, the local share registry, or the platform itself.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	exitOK          = 0
	exitProofFailed = 1
	exitUsage       = 2
	exitInterrupted = 130

	defaultOriginPort = 18080
	statusInterval    = 5 * time.Second
)

// options is the parsed command line for one invocation.
type options struct {
	run              string
	n                int
	concurrency      int
	port             int
	out              string
	servingDeadline  time.Duration
	hold             time.Duration
	sample           int
	fetchInterval    time.Duration
	fetchConcurrency int
	publishRetries   int
	qurlBin          string
	consumeBin       string
	endpoint         string
	teardown         bool
	skipVerify       bool
	skipHold         bool
	rerender         bool
	probe            string
	windows          []knownWindow
}

var runNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,19}$`)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "proof-1000: %v\n", err)
		return exitUsage
	}
	if opts.rerender {
		return rerenderRun(opts, stdout, stderr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env, err := resolveEnvironment(ctx, opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "proof-1000: preflight: %v\n", err)
		return exitUsage
	}
	if opts.teardown {
		return runTeardown(ctx, opts, env, stdout, stderr)
	}
	if opts.probe != "" {
		return runProbe(ctx, env, opts.probe, stdout)
	}
	return runProof(ctx, opts, env, stdout, stderr)
}

func parseOptions(args []string, stderr io.Writer) (*options, error) {
	opts := &options{}
	fs := flag.NewFlagSet("proof-1000", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.run, "run", "", "run name; Connector IDs are proof-<run>-NNNN (lowercase letters, digits, hyphens; required)")
	fs.IntVar(&opts.n, "n", 3, "number of shares to publish")
	fs.IntVar(&opts.concurrency, "concurrency", 4, "maximum publishes in flight; adapts down to 1 on rate limiting")
	fs.IntVar(&opts.port, "port", defaultOriginPort, "loopback port for the local origin (persisted per run)")
	fs.StringVar(&opts.out, "out", "", "run directory (default proof-1000-runs/<run>)")
	fs.DurationVar(&opts.servingDeadline, "serving-deadline", 10*time.Minute, "how long to wait for every share to serve")
	fs.DurationVar(&opts.hold, "hold", 5*time.Minute, "steady-state hold duration")
	fs.IntVar(&opts.sample, "sample", 0, "end-to-end sample size (0 = all when n<=100, else 100 random plus the first and last 10)")
	fs.DurationVar(&opts.fetchInterval, "fetch-interval", 10*time.Second, "interval between rolling end-to-end fetches during the hold")
	fs.IntVar(&opts.fetchConcurrency, "fetch-concurrency", 2, "concurrent end-to-end fetches during verification")
	fs.IntVar(&opts.publishRetries, "publish-retries", 6, "retries per share for rate-limited or unavailable publishes")
	fs.StringVar(&opts.qurlBin, "qurl", "qurl", "qurl binary for publish, list, status, and delete (must be the installed release CLI)")
	fs.StringVar(&opts.consumeBin, "consume-qurl", "", "qurl binary for end-to-end `get` fetches (default: same as --qurl)")
	fs.StringVar(&opts.endpoint, "endpoint", "", "qURL API endpoint for every CLI call (default: QURL_ENDPOINT, then the resident daemon's, then the CLI config)")
	fs.BoolVar(&opts.teardown, "teardown", false, "delete every proof-<run>-* share and verify they are gone")
	fs.BoolVar(&opts.skipVerify, "skip-verify", false, "skip end-to-end fetches")
	fs.BoolVar(&opts.skipHold, "skip-hold", false, "skip the steady-state hold")
	fs.StringVar(&opts.probe, "probe", "", "probe one CRID's visitor access path through the SDK and print the raw deny code or content HTTP status as JSON")
	fs.BoolVar(&opts.rerender, "rerender", false, "re-render report.md/report.json for an existing run directory from its report.json (applies --window) without touching the platform")
	fs.Func("window", "known platform window to attribute failures to, as label=<RFC3339 start>/<RFC3339 end> (repeatable)", func(value string) error {
		w, err := parseKnownWindow(value)
		if err != nil {
			return err
		}
		opts.windows = append(opts.windows, w)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.probe != "" && opts.run == "" {
		opts.run = "probe"
	}
	if !runNamePattern.MatchString(opts.run) {
		return nil, errors.New("--run is required and must match ^[a-z0-9][a-z0-9-]{0,19}$")
	}
	if opts.n < 1 || opts.n > 99999 {
		return nil, errors.New("--n must be between 1 and 99999")
	}
	if opts.concurrency < 1 || opts.fetchConcurrency < 1 {
		return nil, errors.New("--concurrency and --fetch-concurrency must be at least 1")
	}
	if opts.port < 1024 || opts.port > 65535 {
		return nil, errors.New("--port must be between 1024 and 65535")
	}
	if opts.consumeBin == "" {
		opts.consumeBin = opts.qurlBin
	}
	if opts.out == "" {
		opts.out = filepath.Join("proof-1000-runs", opts.run)
	}
	abs, err := filepath.Abs(opts.out)
	if err != nil {
		return nil, err
	}
	opts.out = abs
	return opts, nil
}

// connectorID is the Connector ID for share index i (1-based) of a run.
func connectorID(runName string, i int) string {
	return fmt.Sprintf("proof-%s-%04d", runName, i)
}

// connectorIDPattern matches every Connector ID one run can have produced,
// and nothing else: teardown deletes only what this matches.
func connectorIDPattern(runName string) *regexp.Regexp {
	return regexp.MustCompile(`^proof-` + regexp.QuoteMeta(runName) + `-[0-9]{4,5}$`)
}
