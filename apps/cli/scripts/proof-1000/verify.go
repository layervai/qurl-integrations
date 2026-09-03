package main

import (
	"context"
	"encoding/json"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	fetchTimeout       = 2 * time.Minute
	autoSampleAll      = 100
	autoSampleRandom   = 100
	autoSampleEdges    = 10
	fetchArgFile       = "--file"
	fetchArgStdout     = "-"
	hostSuffixQURLSite = ".qurl.site."
)

// fetchResult is one end-to-end fetch through the platform: the consume CLI
// mints a share link for the CRID, the platform grants access, and the bytes
// that come back must be this origin's answer for this share's routing host.
type fetchResult struct {
	At          time.Time       `json:"at"`
	ID          string          `json:"id"`
	CRID        string          `json:"crid"`
	OK          bool            `json:"ok"`
	WallMS      int64           `json:"wall_ms"`
	ExitCode    int             `json:"exit_code"`
	APICalls    int             `json:"api_calls"`
	Host        string          `json:"host,omitempty"`
	NonceOK     bool            `json:"nonce_ok"`
	HostChecked bool            `json:"host_checked"`
	HostOK      bool            `json:"host_ok"`
	RequestSeen bool            `json:"request_seen_at_origin"`
	Error       string          `json:"error,omitempty"`
	Window      string          `json:"in_window,omitempty"`
	Diagnosis   *fetchDiagnosis `json:"diagnosis,omitempty"`
}

type verifySummary struct {
	Sample  int           `json:"sample"`
	OK      int           `json:"ok"`
	Failed  int           `json:"failed"`
	WallP50 int64         `json:"fetch_ms_p50"`
	WallP95 int64         `json:"fetch_ms_p95"`
	Results []fetchResult `json:"results"`

	FailedInWindow int `json:"failed_in_known_window"`
	FailedOutside  int `json:"failed_outside_known_window"`
}

// selectSample picks 1-based share indexes: everything when n is small,
// otherwise the first and last edges plus a seeded random middle so a rerun
// reproduces the same set.
func selectSample(n, size int, seed int64) []int {
	if size <= 0 {
		if n <= autoSampleAll {
			size = n
		} else {
			size = autoSampleRandom + 2*autoSampleEdges
		}
	}
	if size >= n {
		all := make([]int, n)
		for i := range all {
			all[i] = i + 1
		}
		return all
	}
	chosen := map[int]bool{}
	edges := min(autoSampleEdges, size/2)
	for i := 1; i <= edges; i++ {
		chosen[i], chosen[n-i+1] = true, true
	}
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // G404: sample selection, not a secret
	for len(chosen) < size {
		chosen[rng.Intn(n)+1] = true
	}
	out := make([]int, 0, len(chosen))
	for i := range chosen {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}

func fetchShare(ctx context.Context, env *environment, o *origin, rec *shareRecord) fetchResult {
	result := fetchResult{At: time.Now(), ID: rec.ID, CRID: rec.CRID}
	if rec.CRID == "" {
		result.Error = "no CRID recorded"
		return result
	}
	res := runCLI(ctx, env.ConsumeBin, env.childEnv, fetchTimeout, "-v", "get", rec.CRID, fetchArgFile, fetchArgStdout)
	result.WallMS, result.ExitCode, result.APICalls = res.Wall.Milliseconds(), res.ExitCode, res.Calls.Total
	if res.ExitCode != cliExitOK {
		result.Error = env.redactor.apply(lastErrorLine(res.Stderr))
		if result.Error == "" && res.Err != nil {
			result.Error = res.Err.Error()
		}
		if ctx.Err() == nil {
			result.Diagnosis = probeAccess(ctx, env, rec.CRID)
		}
		return result
	}
	var body originBody
	if err := json.Unmarshal([]byte(res.Stdout), &body); err != nil {
		result.Error = "body is not this origin's JSON (" + itoa(len(res.Stdout)) + " bytes)"
		return result
	}
	result.Host = body.Host
	result.NonceOK = body.ServerNonce == o.nonce
	if seenHost, ok := o.sawRequest(body.RequestID); ok && seenHost == body.Host {
		result.RequestSeen = true
	}
	if rec.RoutingID != "" {
		result.HostChecked = true
		result.HostOK = strings.HasPrefix(body.Host, rec.RoutingID+".") && strings.Contains(body.Host, hostSuffixQURLSite)
	}
	result.OK = result.NonceOK && result.RequestSeen && (!result.HostChecked || result.HostOK)
	if !result.OK {
		result.Error = "nonce_ok=" + boolStr(result.NonceOK) + " request_seen=" + boolStr(result.RequestSeen) + " host_ok=" + boolStr(result.HostOK)
		if ctx.Err() == nil {
			result.Diagnosis = probeAccess(ctx, env, rec.CRID)
		}
	}
	return result
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// verifySample fetches every selected share at bounded concurrency.
func verifySample(ctx context.Context, opts *options, env *environment, o *origin, m *manifest, indexes []int, lg *logger) *verifySummary {
	summary := &verifySummary{Sample: len(indexes)}
	results := make([]fetchResult, len(indexes))
	work := make(chan int, len(indexes))
	for i := range indexes {
		work <- i
	}
	close(work)
	var wg sync.WaitGroup
	for range opts.fetchConcurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if ctx.Err() != nil {
					return
				}
				rec := m.record(connectorID(opts.run, indexes[i]))
				results[i] = fetchShare(ctx, env, o, rec)
				if !results[i].OK {
					lg.logf("fetch %s failed: %s", rec.ID, results[i].Error)
				}
			}
		}()
	}
	wg.Wait()
	summary.Results = results
	var walls []int64
	for i := range results {
		if results[i].OK {
			summary.OK++
			walls = append(walls, results[i].WallMS)
		} else {
			summary.Failed++
		}
	}
	sort.Slice(walls, func(i, j int) bool { return walls[i] < walls[j] })
	summary.WallP50, summary.WallP95 = percentile(walls, 0.5), percentile(walls, 0.95)
	return summary
}
