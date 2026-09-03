package main

import (
	"context"
	"sync"
	"time"
)

// holdSummary is the steady-state phase: the daemon is re-sampled on the
// status interval and one share from the verification sample is fetched
// end to end on the fetch interval, round robin.
type holdSummary struct {
	Started         time.Time `json:"started"`
	Finished        time.Time `json:"finished"`
	DurationS       float64   `json:"duration_s"`
	Samples         int       `json:"status_samples"`
	DegradedSamples int       `json:"degraded_samples"`
	MinServing      int       `json:"min_serving"`
	Fetches         int       `json:"fetches"`
	FetchFailures   int       `json:"fetch_failures"`
	// FetchesSkipped counts fetch ticks that found the previous fetch still in
	// flight: a nonzero value means fetch latency exceeded --fetch-interval.
	FetchesSkipped int            `json:"fetches_skipped"`
	Curve          []statusSample `json:"curve"`
	FetchResults   []fetchResult  `json:"fetch_results"`

	DegradedInWindow      int `json:"degraded_samples_in_known_window"`
	DegradedOutside       int `json:"degraded_samples_outside_known_window"`
	FetchFailuresInWindow int `json:"fetch_failures_in_known_window"`
	FetchFailuresOutside  int `json:"fetch_failures_outside_known_window"`
}

func holdSteady(ctx context.Context, opts *options, env *environment, o *origin, m *manifest,
	resourceIDs []string, sample []int, start time.Time, lg *logger,
) *holdSummary {
	summary := &holdSummary{Started: time.Now(), MinServing: -1}
	holdCtx, cancel := context.WithTimeout(ctx, opts.hold)
	defer cancel()
	statusTick := time.NewTicker(statusInterval)
	defer statusTick.Stop()
	fetchTick := time.NewTicker(opts.fetchInterval)
	defer fetchTick.Stop()
	// Fetches run on their own goroutine, one in flight at most, so a slow or
	// hung fetch can neither suppress status samples nor extend the hold.
	var (
		fetchMu   sync.Mutex
		fetchWG   sync.WaitGroup
		inFlight  bool
		next      int
		skipped   int
		fetchNext = func() {
			if opts.skipVerify || len(sample) == 0 {
				return
			}
			fetchMu.Lock()
			if inFlight {
				skipped++
				fetchMu.Unlock()
				return
			}
			inFlight = true
			rec := m.record(connectorID(opts.run, sample[next%len(sample)]))
			next++
			fetchMu.Unlock()
			fetchWG.Add(1)
			go func() {
				defer fetchWG.Done()
				result := fetchShare(holdCtx, env, o, rec)
				fetchMu.Lock()
				defer fetchMu.Unlock()
				inFlight = false
				summary.Fetches++
				if !result.OK {
					summary.FetchFailures++
					lg.logf("hold fetch %s failed: %s", rec.ID, result.Error)
				}
				summary.FetchResults = append(summary.FetchResults, result)
			}()
		}
	)
	takeSample := func() {
		s := takeStatusSample(holdCtx, resourceIDs, env.SocketPath, start)
		summary.Samples++
		if s.degraded() {
			summary.DegradedSamples++
		}
		if summary.MinServing < 0 || s.Serving < summary.MinServing {
			summary.MinServing = s.Serving
		}
		summary.Curve = append(summary.Curve, s)
	}
	takeSample()
	for {
		select {
		case <-holdCtx.Done():
			fetchWG.Wait()
			summary.Finished = time.Now()
			summary.DurationS = summary.Finished.Sub(summary.Started).Seconds()
			summary.FetchesSkipped = skipped
			if summary.MinServing < 0 {
				summary.MinServing = 0
			}
			return summary
		case <-statusTick.C:
			takeSample()
		case <-fetchTick.C:
			fetchNext()
		}
	}
}
