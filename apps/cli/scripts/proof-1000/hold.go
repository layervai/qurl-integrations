package main

import (
	"context"
	"time"
)

// holdSummary is the steady-state phase: the daemon is re-sampled on the
// status interval and one share from the verification sample is fetched
// end to end on the fetch interval, round robin.
type holdSummary struct {
	Started         time.Time      `json:"started"`
	Finished        time.Time      `json:"finished"`
	DurationS       float64        `json:"duration_s"`
	Samples         int            `json:"status_samples"`
	DegradedSamples int            `json:"degraded_samples"`
	MinServing      int            `json:"min_serving"`
	Fetches         int            `json:"fetches"`
	FetchFailures   int            `json:"fetch_failures"`
	Curve           []statusSample `json:"curve"`
	FetchResults    []fetchResult  `json:"fetch_results"`

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
	next := 0
	fetchNext := func() {
		if opts.skipVerify || len(sample) == 0 {
			return
		}
		rec := m.record(connectorID(opts.run, sample[next%len(sample)]))
		next++
		result := fetchShare(ctx, env, o, rec)
		summary.Fetches++
		if !result.OK {
			summary.FetchFailures++
			lg.logf("hold fetch %s failed: %s", rec.ID, result.Error)
		}
		summary.FetchResults = append(summary.FetchResults, result)
	}
	takeSample := func() {
		s := takeStatusSample(ctx, resourceIDs, env.SocketPath, start)
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
			summary.Finished = time.Now()
			summary.DurationS = summary.Finished.Sub(summary.Started).Seconds()
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
