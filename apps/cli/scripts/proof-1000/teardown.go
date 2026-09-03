package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	deleteTimeout = 2 * time.Minute
	listTimeout   = 2 * time.Minute
	listPageLimit = "100"
)

// listMaxPages bounds the teardown sweep; a var so a test can lower it.
var listMaxPages = 200

type deleteResult struct {
	ID          string `json:"id"`
	CRID        string `json:"crid"`
	Deleted     bool   `json:"deleted"`
	AlreadyGone bool   `json:"already_gone,omitempty"`
	ExitCode    int    `json:"exit_code"`
	APICalls    int    `json:"api_calls"`
	Attempts    int    `json:"attempts"`
	Error       string `json:"error,omitempty"`
}

type teardownReport struct {
	Run         string    `json:"run"`
	GeneratedAt time.Time `json:"generated_at"`
	Candidates  int       `json:"candidates"`
	Deleted     int       `json:"deleted"`
	AlreadyGone int       `json:"already_gone"`
	Failed      int       `json:"failed"`
	APICalls    int       `json:"api_calls"`
	WallSeconds float64   `json:"wall_s"`
	StillListed []string  `json:"still_listed,omitempty"`
	// UnexpectedListed are active tunnel resources whose target is this
	// run's origin but that no manifest or registry row names: a share whose
	// CRID was never recorded locally. They are reported, never deleted.
	UnexpectedListed  []string       `json:"unexpected_listed,omitempty"`
	OriginTarget      string         `json:"origin_target,omitempty"`
	RegistryRemaining []string       `json:"registry_remaining,omitempty"`
	ListPages         int            `json:"list_pages"`
	Results           []deleteResult `json:"results"`
}

type deleteJSON struct {
	Deleted     bool `json:"deleted"`
	AlreadyGone bool `json:"already_gone"`
}

type listJSON struct {
	Resources []struct {
		CRID      string `json:"crid"`
		TargetURL string `json:"target_url"`
	} `json:"resources"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

// runTeardown deletes exactly the shares one run produced: the union of the
// run manifest and every local registry row whose Connector ID matches
// proof-<run>-NNNN. Nothing else is ever a candidate.
func runTeardown(ctx context.Context, opts *options, env *environment, stdout, stderr *os.File) int {
	lg := newLogger(stderr, filepath.Join(opts.out, "teardown.log"), env.redactor)
	defer lg.close()
	started := time.Now()
	candidates, originTarget, err := teardownCandidates(ctx, opts, env)
	if err != nil {
		lg.logf("teardown: %v", err)
		return exitUsage
	}
	rep := &teardownReport{Run: opts.run, GeneratedAt: started, Candidates: len(candidates), OriginTarget: originTarget}
	lg.logf("teardown %s: %d candidate shares", opts.run, len(candidates))
	rep.Results = deleteAll(ctx, opts, env, candidates, lg)
	for i := range rep.Results {
		r := &rep.Results[i]
		rep.APICalls += r.APICalls
		switch {
		case r.AlreadyGone:
			rep.AlreadyGone++
		case r.Deleted:
			rep.Deleted++
		default:
			rep.Failed++
		}
	}
	rep.StillListed, rep.UnexpectedListed, rep.ListPages = stillListed(ctx, env, candidates, originTarget, lg)
	rep.RegistryRemaining = registryRemaining(ctx, opts, env)
	rep.WallSeconds = time.Since(started).Seconds()
	if err := writeRedactedJSON(filepath.Join(opts.out, "teardown.json"), rep, env.redactor); err != nil {
		lg.logf("write teardown.json: %v", err)
	}
	_, _ = fmt.Fprintf(stdout, "teardown %s: candidates=%d deleted=%d already_gone=%d failed=%d still_listed=%d unexpected_listed=%d registry_remaining=%d api_calls=%d wall=%.0fs\n",
		opts.run, rep.Candidates, rep.Deleted, rep.AlreadyGone, rep.Failed, len(rep.StillListed), len(rep.UnexpectedListed), len(rep.RegistryRemaining), rep.APICalls, rep.WallSeconds)
	if len(rep.UnexpectedListed) > 0 {
		_, _ = fmt.Fprintf(stdout, "  unexpected active shares targeting %s (not deleted; no local record of them): %s\n", originTarget, strings.Join(rep.UnexpectedListed, " "))
	}
	if rep.Failed > 0 || len(rep.StillListed) > 0 || len(rep.RegistryRemaining) > 0 {
		return exitProofFailed
	}
	return exitOK
}

func teardownCandidates(ctx context.Context, opts *options, env *environment) (candidates []shareRecord, originTarget string, err error) {
	byID := map[string]shareRecord{}
	pattern := connectorIDPattern(opts.run)
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(opts.out, manifestFile)))
	if err == nil {
		var m manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, "", fmt.Errorf("parse run manifest: %w", err)
		}
		if m.Port != 0 {
			originTarget = "http://127.0.0.1:" + itoa(m.Port)
		}
		for id, rec := range m.Shares {
			if rec.CRID != "" && pattern.MatchString(id) {
				byID[id] = *rec
			}
		}
	}
	rows, err := readRegistryByConnectorID(ctx, env.StateDir)
	if err != nil {
		return nil, "", fmt.Errorf("read local share registry: %w", err)
	}
	for id := range rows {
		if pattern.MatchString(id) {
			byID[id] = shareRecord{ID: id, CRID: rows[id].CRID, ResourceID: rows[id].ResourceID}
		}
	}
	out := make([]shareRecord, 0, len(byID))
	for id := range byID {
		out = append(out, byID[id])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, originTarget, nil
}

func deleteAll(ctx context.Context, opts *options, env *environment, candidates []shareRecord, lg *logger) []deleteResult {
	results := make([]deleteResult, len(candidates))
	workers := min(opts.concurrency, 2)
	lim := newLimiter(workers)
	work := make(chan int, len(candidates))
	for i := range candidates {
		work <- i
	}
	close(work)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				results[i] = deleteOne(ctx, env, lim, &candidates[i], opts.publishRetries, lg)
			}
		}()
	}
	wg.Wait()
	return results
}

func deleteOne(ctx context.Context, env *environment, lim *limiter, rec *shareRecord, retries int, lg *logger) deleteResult {
	result := deleteResult{ID: rec.ID, CRID: rec.CRID}
	for attempt := 1; attempt <= 1+retries; attempt++ {
		if err := lim.acquire(ctx); err != nil {
			result.Error = err.Error()
			return result
		}
		res := runCLI(ctx, env.QurlBin, env.childEnv, deleteTimeout, "-v", "delete", rec.CRID, flagYes, flagOutput, outputJSON)
		throttled := res.ExitCode == cliExitRateLimited || res.Calls.TooMany > 0
		lim.release(throttled, res.Calls.RetryWaitSum)
		result.Attempts, result.ExitCode = attempt, res.ExitCode
		result.APICalls += res.Calls.Total
		if res.ExitCode == cliExitOK {
			var out deleteJSON
			_ = json.Unmarshal([]byte(res.Stdout), &out)
			result.Deleted, result.AlreadyGone, result.Error = out.Deleted || out.AlreadyGone, out.AlreadyGone, ""
			return result
		}
		result.Error = env.redactor.apply(lastErrorLine(res.Stderr))
		if !throttled && res.ExitCode != cliExitUnavailable && res.ExitCode != cliExitServerError {
			return result
		}
		lg.logf("delete %s: exit %d, retrying (attempt %d)", rec.ID, res.ExitCode, attempt)
		select {
		case <-ctx.Done():
			return result
		case <-time.After(min(defaultRetryAfter*time.Duration(attempt), maxRetryBackoff)):
		}
	}
	return result
}

// stillListed walks the whole active tunnel listing (has_more is the
// terminator, not next_cursor) and reports any candidate CRID still present,
// plus any active share that targets this run's origin without being a
// candidate. A listing that could not be completed is reported as such, never
// as a clean sweep.
func stillListed(ctx context.Context, env *environment, candidates []shareRecord, originTarget string, lg *logger) (remaining, unexpected []string, pages int) {
	want := make(map[string]string, len(candidates))
	for i := range candidates {
		want[candidates[i].CRID] = candidates[i].ID
	}
	seen := map[string]bool{}
	cursor := ""
	complete := false
	for pages < listMaxPages {
		args := []string{"list", "--status", "active", "--type", "tunnel", "--limit", listPageLimit, flagOutput, outputJSON}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		res := runCLI(ctx, env.QurlBin, env.childEnv, listTimeout, args...)
		pages++
		if res.ExitCode != cliExitOK {
			lg.logf("list page %d failed (exit %d): %s", pages, res.ExitCode, env.redactor.apply(lastErrorLine(res.Stderr)))
			return append(remaining, "<listing incomplete>"), unexpected, pages
		}
		var page listJSON
		if err := json.Unmarshal([]byte(res.Stdout), &page); err != nil {
			lg.logf("list page %d: %v", pages, err)
			return append(remaining, "<listing unparseable>"), unexpected, pages
		}
		for _, item := range page.Resources {
			if seen[item.CRID] {
				continue
			}
			seen[item.CRID] = true
			if id, ok := want[item.CRID]; ok {
				remaining = append(remaining, id)
			} else if originTarget != "" && item.TargetURL == originTarget {
				unexpected = append(unexpected, item.CRID)
			}
		}
		if !page.HasMore || page.NextCursor == "" {
			complete = true
			break
		}
		cursor = page.NextCursor
	}
	if !complete {
		lg.logf("list stopped after %d pages without reaching the end", pages)
		remaining = append(remaining, "<listing truncated at "+itoa(listMaxPages)+" pages>")
	}
	sort.Strings(remaining)
	sort.Strings(unexpected)
	return remaining, unexpected, pages
}

func registryRemaining(ctx context.Context, opts *options, env *environment) []string {
	rows, err := readRegistryByConnectorID(ctx, env.StateDir)
	if err != nil {
		return []string{"<registry unreadable: " + err.Error() + ">"}
	}
	pattern := connectorIDPattern(opts.run)
	var remaining []string
	for id := range rows {
		if pattern.MatchString(id) {
			remaining = append(remaining, id)
		}
	}
	sort.Strings(remaining)
	return remaining
}
