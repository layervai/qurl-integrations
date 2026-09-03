package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	daemonLogLimit    = 400
	failureLogLimit   = 8
	sampleSeed        = 1000
	assumedRatePerMin = 100
	targetN           = 1000
)

type logger struct {
	mu  sync.Mutex
	w   io.Writer
	f   *os.File
	red *redactor
}

func newLogger(w io.Writer, path string, red *redactor) *logger {
	l := &logger{w: w, red: red}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		l.f, _ = os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	return l
}

func (l *logger) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := time.Now().UTC().Format("15:04:05") + " " + l.red.apply(fmt.Sprintf(format, args...)) + "\n"
	_, _ = io.WriteString(l.w, line)
	if l.f != nil {
		_, _ = l.f.WriteString(line)
	}
}

func (l *logger) close() {
	if l.f != nil {
		_ = l.f.Close()
	}
}

// runProof is the proof itself, phase by phase. Every phase leaves its
// evidence in the report even when a later phase is interrupted.
func runProof(ctx context.Context, opts *options, env *environment, stdout, stderr *os.File) int {
	lg := newLogger(stderr, filepath.Join(opts.out, "run.log"), env.redactor)
	defer lg.close()
	m, err := loadOrCreateManifest(opts.out, opts.run, opts.n, opts.port, env.Endpoint)
	if err != nil {
		lg.logf("manifest: %v", err)
		return exitUsage
	}
	if m.Port != opts.port {
		lg.logf("run %s was created with origin port %d; keeping it so shares are not restarted", opts.run, m.Port)
	}
	o, err := startOrigin(ctx, m.Port)
	if err != nil {
		lg.logf("origin: %v", err)
		return exitUsage
	}
	defer func() { _ = o.close() }()
	lg.logf("run %s: n=%d origin=%s state=%s qurl=%s consume=%s endpoint=%s hub=%s",
		opts.run, opts.n, o.targetURL(), env.StateDir, env.QurlVersion, env.ConsumeVersion, env.Endpoint, env.HubSource)

	rep := &report{Run: opts.run, N: opts.n, Environment: env, Started: time.Now()}
	rep.Usage = append(rep.Usage, sampleUsage(ctx, env, "before", m.Port))
	runPhases(ctx, opts, env, o, m, rep, lg)
	rep.Interrupted = ctx.Err() != nil
	rep.Origin = o.snapshot()
	rep.finalize(opts, env, m)
	if err := writeReport(opts.out, rep, env.redactor); err != nil {
		lg.logf("write report: %v", err)
	}
	printSummary(stdout, rep, opts.out)
	switch {
	case rep.Interrupted:
		return exitInterrupted
	case rep.Passed:
		return exitOK
	default:
		return exitProofFailed
	}
}

func runPhases(ctx context.Context, opts *options, env *environment, o *origin, m *manifest, rep *report, lg *logger) {
	lg.logf("phase 1: publishing %d shares at concurrency <= %d", opts.n, opts.concurrency)
	rep.Publish = publishAll(ctx, opts, env, m, o, lg)
	lg.logf("publish: published=%d existing=%d resumed=%d registered-not-serving=%d failed=%d api_calls=%d 429s=%d wall=%.0fs",
		rep.Publish.Published, rep.Publish.Existing, rep.Publish.Resumed, rep.Publish.NotServing, rep.Publish.Failed,
		rep.Publish.APICalls, rep.Publish.HTTP429, rep.Publish.WallSeconds)
	if ctx.Err() != nil {
		return
	}
	resourceIDs := m.resourceIDs()
	lg.logf("phase 2: waiting up to %s for %d resources to serve", opts.servingDeadline, len(resourceIDs))
	waitStart := time.Now()
	last, all := waitAllServing(ctx, resourceIDs, env.SocketPath, rep.Started, opts.servingDeadline, statusInterval, func(s statusSample) {
		rep.ServingWait.Curve = append(rep.ServingWait.Curve, s)
		lg.logf("status: serving=%d starting=%d retrying=%d failed=%d stopped=%d absent=%d missing=%d of %d %s",
			s.Serving, s.Starting, s.Retrying, s.Failed, s.Stopped, s.Absent, s.Missing, s.Total, s.Err)
	})
	rep.ServingWait.AllServing, rep.ServingWait.Last = all, last
	rep.ServingWait.WaitedS = time.Since(waitStart).Seconds()
	rep.Usage = append(rep.Usage, sampleUsage(ctx, env, "after-publish", m.Port))
	if ctx.Err() != nil {
		return
	}
	sample := selectSample(opts.n, opts.sample, sampleSeed)
	if !opts.skipVerify {
		lg.logf("phase 3: fetching %d shares end to end", len(sample))
		rep.Verify = verifySample(ctx, opts, env, o, m, sample, lg)
		lg.logf("verify: ok=%d failed=%d p50=%dms p95=%dms", rep.Verify.OK, rep.Verify.Failed, rep.Verify.WallP50, rep.Verify.WallP95)
	}
	rep.Usage = append(rep.Usage, sampleUsage(ctx, env, "steady-state", m.Port))
	if ctx.Err() != nil || opts.skipHold {
		return
	}
	lg.logf("phase 4: holding for %s", opts.hold)
	rep.Hold = holdSteady(ctx, opts, env, o, m, resourceIDs, sample, rep.Started, lg)
	lg.logf("hold: samples=%d degraded=%d min_serving=%d fetches=%d fetch_failures=%d",
		rep.Hold.Samples, rep.Hold.DegradedSamples, rep.Hold.MinServing, rep.Hold.Fetches, rep.Hold.FetchFailures)
	rep.Usage = append(rep.Usage, sampleUsage(ctx, env, "after-hold", m.Port))
}
