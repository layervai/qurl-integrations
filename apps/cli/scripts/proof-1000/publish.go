package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	publishTimeout     = 3 * time.Minute
	defaultRetryAfter  = 15 * time.Second
	maxRetryBackoff    = 60 * time.Second
	limiterGrowStreak  = 8
	limiterPollBackoff = 100 * time.Millisecond

	outcomePublished = "published"
	outcomeExisting  = "existing"
	outcomeResumed   = "resumed"
	outcomeRetry     = "retry"
	outcomeTimeout   = "registered-not-serving"
	outcomeFailed    = "failed"
)

// publishEvent is one publish attempt on the timeline; the report derives
// throughput and the concurrency trace from these.
type publishEvent struct {
	At       time.Time `json:"at"`
	Elapsed  float64   `json:"elapsed_s"`
	ID       string    `json:"id"`
	Outcome  string    `json:"outcome"`
	WallMS   int64     `json:"wall_ms"`
	Limit    int       `json:"concurrency_limit"`
	Calls    int       `json:"api_calls"`
	HTTP429  int       `json:"http_429"`
	ExitCode int       `json:"exit_code"`
}

type publishSummary struct {
	Started            time.Time      `json:"started"`
	Finished           time.Time      `json:"finished"`
	WallSeconds        float64        `json:"wall_s"`
	Attempted          int            `json:"attempted"`
	Published          int            `json:"published"`
	Existing           int            `json:"found_existing"`
	Resumed            int            `json:"resumed_from_manifest"`
	NotServing         int            `json:"registered_not_serving"`
	Failed             int            `json:"failed"`
	Retries            int            `json:"retries"`
	Throttles          int            `json:"throttle_events"`
	APICalls           int            `json:"api_calls"`
	HTTP429            int            `json:"http_429"`
	CallsPerPublish    float64        `json:"api_calls_per_publish"`
	PublishMSP50       int64          `json:"publish_ms_p50"`
	PublishMSP95       int64          `json:"publish_ms_p95"`
	PublishMSMax       int64          `json:"publish_ms_max"`
	PublishesPerMinute float64        `json:"publishes_per_minute"`
	ConcurrencyMin     int            `json:"concurrency_min"`
	ConcurrencyMax     int            `json:"concurrency_max"`
	Events             []publishEvent `json:"events"`
}

// limiter is an adaptive concurrency gate: additive increase after a streak
// of clean publishes, multiplicative decrease plus a global pause on any
// rate-limit signal (an exit 9, or a 429 the CLI retried internally).
type limiter struct {
	mu         sync.Mutex
	limit      int
	maxLimit   int
	inflight   int
	streak     int
	pauseUntil time.Time
	throttles  int
	minSeen    int
	maxSeen    int
}

func newLimiter(limit int) *limiter {
	return &limiter{limit: limit, maxLimit: limit, minSeen: limit, maxSeen: limit}
}

func (l *limiter) acquire(ctx context.Context) error {
	for {
		l.mu.Lock()
		if l.inflight < l.limit && !time.Now().Before(l.pauseUntil) {
			l.inflight++
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(limiterPollBackoff):
		}
	}
}

func (l *limiter) release(throttled bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inflight--
	if throttled {
		l.throttles++
		l.streak = 0
		l.limit = max(1, l.limit/2)
		if retryAfter <= 0 {
			retryAfter = defaultRetryAfter
		}
		l.pauseUntil = time.Now().Add(retryAfter)
	} else {
		l.streak++
		if l.streak >= limiterGrowStreak && l.limit < l.maxLimit {
			l.limit++
			l.streak = 0
		}
	}
	l.minSeen = min(l.minSeen, l.limit)
	l.maxSeen = max(l.maxSeen, l.limit)
}

func (l *limiter) current() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

type publishJSON struct {
	CRID          string `json:"crid"`
	ResourceID    string `json:"resource_id"`
	FoundExisting *bool  `json:"found_existing"`
}

// publisher runs the publish phase for one manifest.
type publisher struct {
	opts    *options
	env     *environment
	m       *manifest
	target  string
	lg      *logger
	lim     *limiter
	started time.Time

	mu      sync.Mutex
	summary publishSummary
}

func publishAll(ctx context.Context, opts *options, env *environment, m *manifest, o *origin, lg *logger) *publishSummary {
	p := &publisher{opts: opts, env: env, m: m, target: o.targetURL(), lg: lg, lim: newLimiter(opts.concurrency), started: time.Now()}
	p.summary.Started = p.started
	ids := make(chan int, opts.n)
	for i := 1; i <= opts.n; i++ {
		ids <- i
	}
	close(ids)
	var wg sync.WaitGroup
	for range opts.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ids {
				if ctx.Err() != nil {
					return
				}
				p.publishOne(ctx, connectorID(opts.run, i))
			}
		}()
	}
	wg.Wait()
	p.fillFromRegistry(ctx)
	_ = m.save()
	return p.finish()
}

func (p *publisher) publishOne(ctx context.Context, id string) {
	rec := p.m.record(id)
	if rec.CRID != "" && rec.ResourceID != "" && !rec.TimedOut && rec.Error == "" {
		rec.Resumed = true
		p.event(id, outcomeResumed, 0, 0, 0, 0)
		return
	}
	for attempt := 1; attempt <= 1+p.opts.publishRetries; attempt++ {
		if err := p.lim.acquire(ctx); err != nil {
			return
		}
		limit := p.lim.current()
		res := runCLI(ctx, p.env.QurlBin, p.env.childEnv, publishTimeout,
			"-v", "publish", p.target, "--id", id, flagOutput, outputJSON)
		throttled := res.ExitCode == cliExitRateLimited || res.Calls.TooMany > 0
		p.lim.release(throttled, res.Calls.RetryWaits)
		p.m.mu.Lock()
		rec.Attempts++
		rec.APICalls += res.Calls.Total
		rec.HTTP429 += res.Calls.TooMany
		rec.ExitCode = res.ExitCode
		p.m.mu.Unlock()
		outcome := p.applyResult(ctx, rec, res)
		p.event(id, outcome, res.Wall.Milliseconds(), limit, res.Calls.Total, res.Calls.TooMany)
		_ = p.m.save()
		if outcome != outcomeRetry {
			return
		}
		backoff := res.Calls.RetryWaits
		if backoff <= 0 {
			backoff = min(defaultRetryAfter*time.Duration(attempt), maxRetryBackoff)
		}
		p.lg.logf("%s: exit %d, retrying in %s (attempt %d/%d)", id, res.ExitCode, backoff, attempt, 1+p.opts.publishRetries)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// applyResult records one attempt's outcome. A non-zero exit with the share
// already in the local registry is "registered, not yet serving": the daemon
// owns it from here and the serving wait will report on it, so the harness
// does not spend another publish's worth of API budget to re-ask.
func (p *publisher) applyResult(ctx context.Context, rec *shareRecord, res *cliResult) string {
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	if res.ExitCode == cliExitOK {
		var out publishJSON
		if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil || out.CRID == "" {
			rec.Error = "publish succeeded but printed no CRID: " + p.env.redactor.apply(strings.TrimSpace(res.Stdout))
			return outcomeFailed
		}
		rec.CRID, rec.ResourceID, rec.FoundExisting = out.CRID, out.ResourceID, out.FoundExisting
		rec.PublishedAt, rec.PublishMS, rec.TimedOut, rec.Error = time.Now().UTC(), res.Wall.Milliseconds(), false, ""
		if out.FoundExisting != nil && *out.FoundExisting {
			return outcomeExisting
		}
		return outcomePublished
	}
	rec.Error = p.env.redactor.apply(lastErrorLine(res.Stderr))
	if res.Err != nil && rec.Error == "" {
		rec.Error = res.Err.Error()
	}
	if registered := p.lookupRegistry(ctx, rec.ID); registered != nil {
		rec.CRID, rec.ResourceID, rec.RoutingID = registered.CRID, registered.ResourceID, registered.ConnectorRoutingID
		rec.TimedOut = true
		return outcomeTimeout
	}
	switch res.ExitCode {
	case cliExitRateLimited, cliExitServerError, cliExitUnavailable:
		return outcomeRetry
	default:
		return outcomeFailed
	}
}

func (p *publisher) event(id, outcome string, wallMS int64, limit, calls, tooMany int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.summary.Events = append(p.summary.Events, publishEvent{
		At: now, Elapsed: now.Sub(p.started).Seconds(), ID: id, Outcome: outcome, WallMS: wallMS,
		Limit: limit, Calls: calls, HTTP429: tooMany, ExitCode: p.m.record(id).ExitCode,
	})
	switch outcome {
	case outcomePublished:
		p.summary.Published++
	case outcomeExisting:
		p.summary.Existing++
	case outcomeResumed:
		p.summary.Resumed++
	case outcomeTimeout:
		p.summary.NotServing++
	case outcomeFailed:
		p.summary.Failed++
	case outcomeRetry:
		p.summary.Retries++
	}
	if outcome != outcomeResumed {
		p.summary.Attempted++
		p.summary.APICalls += calls
		p.summary.HTTP429 += tooMany
	}
}

func (p *publisher) finish() *publishSummary {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := &p.summary
	s.Finished = time.Now()
	s.WallSeconds = s.Finished.Sub(s.Started).Seconds()
	s.Throttles = p.lim.throttles
	s.ConcurrencyMin, s.ConcurrencyMax = p.lim.minSeen, p.lim.maxSeen
	var walls []int64
	for i := range s.Events {
		ev := &s.Events[i]
		if ev.Outcome == outcomePublished || ev.Outcome == outcomeExisting {
			walls = append(walls, ev.WallMS)
		}
	}
	sort.Slice(walls, func(i, j int) bool { return walls[i] < walls[j] })
	s.PublishMSP50, s.PublishMSP95 = percentile(walls, 0.5), percentile(walls, 0.95)
	if len(walls) > 0 {
		s.PublishMSMax = walls[len(walls)-1]
	}
	completed := s.Published + s.Existing + s.NotServing
	if completed > 0 {
		s.CallsPerPublish = float64(s.APICalls) / float64(completed)
	}
	if s.WallSeconds > 0 {
		s.PublishesPerMinute = float64(completed) / s.WallSeconds * 60
	}
	return s
}

func (p *publisher) lookupRegistry(ctx context.Context, id string) *registryRow {
	rows, err := readRegistryByConnectorID(ctx, p.env.StateDir)
	if err != nil {
		return nil
	}
	row, ok := rows[id]
	if !ok {
		return nil
	}
	return &row
}

// fillFromRegistry copies each share's routing identity from the owner-only
// local registry so the end-to-end check can pin the Host the origin sees.
func (p *publisher) fillFromRegistry(ctx context.Context) {
	rows, err := readRegistryByConnectorID(ctx, p.env.StateDir)
	if err != nil {
		p.lg.logf("local share registry unavailable; routing ids unknown: %v", err)
		return
	}
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	for id, rec := range p.m.Shares {
		if row, ok := rows[id]; ok {
			rec.RoutingID = row.ConnectorRoutingID
			if rec.ResourceID == "" {
				rec.CRID, rec.ResourceID = row.CRID, row.ResourceID
			}
		}
	}
}
