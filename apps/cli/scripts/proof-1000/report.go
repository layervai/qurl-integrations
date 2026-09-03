package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type failureDetail struct {
	At     time.Time `json:"at"`
	ID     string    `json:"id"`
	CRID   string    `json:"crid,omitempty"`
	Stage  string    `json:"stage"`
	Error  string    `json:"error"`
	Window string    `json:"in_window,omitempty"`
	Log    []string  `json:"daemon_log,omitempty"`
}

// knownWindow is an operator-declared platform event (a rollover, a deploy)
// that the report attributes failures to. It never changes the verdict; it
// splits every failure and degraded sample into inside/outside so a reader
// can see whether anything happened outside the declared window.
type knownWindow struct {
	Label string    `json:"label"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func parseKnownWindow(value string) (knownWindow, error) {
	label, span, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(label) == "" {
		return knownWindow{}, errors.New("window must be label=<start>/<end>")
	}
	startRaw, endRaw, ok := strings.Cut(span, "/")
	if !ok {
		return knownWindow{}, errors.New("window must be label=<start>/<end>")
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(startRaw))
	if err != nil {
		return knownWindow{}, fmt.Errorf("window %s start: %w", label, err)
	}
	end, err := time.Parse(time.RFC3339, strings.TrimSpace(endRaw))
	if err != nil {
		return knownWindow{}, fmt.Errorf("window %s end: %w", label, err)
	}
	if !end.After(start) {
		return knownWindow{}, fmt.Errorf("window %s must end after it starts", label)
	}
	return knownWindow{Label: strings.TrimSpace(label), Start: start.UTC(), End: end.UTC()}, nil
}

func windowLabel(windows []knownWindow, at time.Time) string {
	for i := range windows {
		if !at.Before(windows[i].Start) && !at.After(windows[i].End) {
			return windows[i].Label
		}
	}
	return ""
}

// estimate projects the client-side cost of the N=1000 run from what this
// run measured: the API budget bound and the measured throughput bound.
type estimate struct {
	CallsPerPublish            float64 `json:"api_calls_per_publish"`
	AssumedRatePerMinute       int     `json:"assumed_owner_rate_per_minute"`
	BudgetBoundPublishesPerMin float64 `json:"budget_bound_publishes_per_minute"`
	BudgetBoundMinutesFor1000  float64 `json:"budget_bound_minutes_for_1000"`
	MeasuredPublishesPerMin    float64 `json:"measured_publishes_per_minute"`
	MeasuredMinutesFor1000     float64 `json:"measured_minutes_for_1000"`
}

type servingWait struct {
	AllServing bool           `json:"all_serving"`
	WaitedS    float64        `json:"waited_s"`
	Last       statusSample   `json:"last"`
	Curve      []statusSample `json:"curve"`
}

type report struct {
	Run         string          `json:"run"`
	N           int             `json:"n"`
	Started     time.Time       `json:"started"`
	GeneratedAt time.Time       `json:"generated_at"`
	Interrupted bool            `json:"interrupted"`
	Passed      bool            `json:"passed"`
	Verdict     []string        `json:"verdict"`
	Windows     []knownWindow   `json:"known_windows"`
	Environment *environment    `json:"environment"`
	Publish     *publishSummary `json:"publish,omitempty"`
	ServingWait servingWait     `json:"serving_wait"`
	Verify      *verifySummary  `json:"verify,omitempty"`
	Usage       []usageSample   `json:"usage"`
	Hold        *holdSummary    `json:"hold,omitempty"`
	Origin      originStats     `json:"origin"`
	Failures    []failureDetail `json:"failures"`
	DaemonLog   []string        `json:"daemon_log"`
	Estimate    estimate        `json:"estimate"`
	Shares      []*shareRecord  `json:"shares"`
}

func (r *report) finalize(opts *options, env *environment, m *manifest) {
	r.GeneratedAt = time.Now()
	r.Shares = m.ordered()
	needles := make([]string, 0, 2*len(r.Shares))
	for _, rec := range r.Shares {
		needles = append(needles, rec.CRID, rec.ResourceID)
	}
	r.DaemonLog = collectDaemonLogs(env.LogDir, r.Started.Add(-time.Minute), r.GeneratedAt, needles, env.redactor, daemonLogLimit)
	r.Windows = opts.windows
	r.collectFailures()
	r.computeEstimate()
	r.annotateWindows()
	r.judge(!opts.skipVerify, !opts.skipHold)
}

// annotateWindows stamps every failure and status sample with the known
// window it falls in, and splits the hold and verify counts accordingly.
func (r *report) annotateWindows() {
	if r.Windows == nil {
		r.Windows = []knownWindow{}
	}
	for i := range r.Failures {
		r.Failures[i].Window = windowLabel(r.Windows, r.Failures[i].At)
	}
	for i := range r.ServingWait.Curve {
		r.ServingWait.Curve[i].Window = windowLabel(r.Windows, r.ServingWait.Curve[i].At)
	}
	if v := r.Verify; v != nil {
		v.FailedInWindow, v.FailedOutside = splitFetchFailures(r.Windows, v.Results)
	}
	if h := r.Hold; h != nil {
		h.DegradedInWindow, h.DegradedOutside = 0, 0
		for i := range h.Curve {
			h.Curve[i].Window = windowLabel(r.Windows, h.Curve[i].At)
			if !h.Curve[i].degraded() {
				continue
			}
			if h.Curve[i].Window != "" {
				h.DegradedInWindow++
			} else {
				h.DegradedOutside++
			}
		}
		h.FetchFailuresInWindow, h.FetchFailuresOutside = splitFetchFailures(r.Windows, h.FetchResults)
	}
}

func splitFetchFailures(windows []knownWindow, results []fetchResult) (inside, outside int) {
	for i := range results {
		if results[i].OK {
			continue
		}
		results[i].Window = windowLabel(windows, results[i].At)
		if results[i].Window != "" {
			inside++
		} else {
			outside++
		}
	}
	return inside, outside
}

func (r *report) collectFailures() {
	r.Failures = []failureDetail{}
	for _, rec := range r.Shares {
		if rec.Error == "" && rec.CRID != "" {
			continue
		}
		stage := "publish"
		if rec.TimedOut {
			stage = "publish-not-serving"
		}
		r.Failures = append(r.Failures, failureDetail{
			At: r.publishFailureTime(rec), ID: rec.ID, CRID: rec.CRID, Stage: stage, Error: rec.Error,
			Log: linesMentioning(r.DaemonLog, []string{rec.CRID, rec.ResourceID}, failureLogLimit),
		})
	}
	if r.Verify != nil {
		r.appendFetchFailures("verify", r.Verify.Results)
	}
	if r.Hold != nil {
		r.appendFetchFailures("hold", r.Hold.FetchResults)
	}
}

func (r *report) appendFetchFailures(stage string, results []fetchResult) {
	for i := range results {
		if results[i].OK {
			continue
		}
		r.Failures = append(r.Failures, failureDetail{
			At: results[i].At, ID: results[i].ID, CRID: results[i].CRID, Stage: stage, Error: results[i].Error,
			Log: linesMentioning(r.DaemonLog, []string{results[i].CRID}, failureLogLimit),
		})
	}
}

// publishFailureTime is the last publish event for the share, or its
// recorded publish time when no event was kept (a resumed run).
func (r *report) publishFailureTime(rec *shareRecord) time.Time {
	if r.Publish != nil {
		for i := len(r.Publish.Events) - 1; i >= 0; i-- {
			if r.Publish.Events[i].ID == rec.ID {
				return r.Publish.Events[i].At
			}
		}
	}
	if !rec.PublishedAt.IsZero() {
		return rec.PublishedAt
	}
	return r.GeneratedAt
}

func (r *report) computeEstimate() {
	e := estimate{AssumedRatePerMinute: assumedRatePerMin}
	if r.Publish != nil {
		e.CallsPerPublish = r.Publish.CallsPerPublish
		e.MeasuredPublishesPerMin = r.Publish.PublishesPerMinute
	}
	if e.CallsPerPublish > 0 {
		e.BudgetBoundPublishesPerMin = float64(assumedRatePerMin) / e.CallsPerPublish
		e.BudgetBoundMinutesFor1000 = targetN / e.BudgetBoundPublishesPerMin
	}
	if e.MeasuredPublishesPerMin > 0 {
		e.MeasuredMinutesFor1000 = targetN / e.MeasuredPublishesPerMin
	}
	r.Estimate = e
}

func (r *report) judge(verifyExpected, holdExpected bool) {
	var verdict []string
	fail := func(format string, args ...any) { verdict = append(verdict, fmt.Sprintf(format, args...)) }
	if r.Interrupted {
		fail("interrupted before every phase finished")
	}
	if r.Publish == nil {
		fail("publish phase did not run")
	} else if r.Publish.Failed > 0 {
		fail("%d publishes failed outright", r.Publish.Failed)
	}
	if !r.ServingWait.AllServing {
		fail("only %d of %d shares serving after %.0fs", r.ServingWait.Last.Serving, r.ServingWait.Last.Total, r.ServingWait.WaitedS)
	}
	r.judgeVerify(verifyExpected, fail)
	r.judgeHold(holdExpected, fail)
	r.Passed = len(verdict) == 0
	if r.Passed {
		verdict = []string{"all shares served, every sampled fetch round-tripped, steady state held"}
	}
	if len(r.Windows) > 0 {
		verdict = append(verdict, r.windowVerdict())
	}
	r.Verdict = verdict
}

func (r *report) judgeVerify(expected bool, fail func(string, ...any)) {
	switch {
	case !expected:
	case r.Verify == nil:
		fail("verification did not run")
	case r.Verify.Failed > 0:
		fail("%d of %d end-to-end fetches failed (%d inside known windows, %d outside)", r.Verify.Failed, r.Verify.Sample, r.Verify.FailedInWindow, r.Verify.FailedOutside)
	}
}

func (r *report) judgeHold(expected bool, fail func(string, ...any)) {
	switch {
	case !expected:
	case r.Hold == nil:
		fail("hold did not run")
	default:
		if r.Hold.DegradedSamples > 0 {
			fail("%d of %d hold samples saw fewer than %d serving (min %d; %d inside known windows, %d outside)",
				r.Hold.DegradedSamples, r.Hold.Samples, r.N, r.Hold.MinServing, r.Hold.DegradedInWindow, r.Hold.DegradedOutside)
		}
		if r.Hold.FetchFailures > 0 {
			fail("%d of %d hold fetches failed (%d inside known windows, %d outside)", r.Hold.FetchFailures, r.Hold.Fetches, r.Hold.FetchFailuresInWindow, r.Hold.FetchFailuresOutside)
		}
	}
}

// windowVerdict states plainly whether anything failed outside the declared
// windows: inside them, the platform event is the explanation and no tunnel
// or daemon conclusion is drawn; outside them, the failure stands on its own.
func (r *report) windowVerdict() string {
	labels := make([]string, 0, len(r.Windows))
	for i := range r.Windows {
		labels = append(labels, fmt.Sprintf("%s %s..%s", r.Windows[i].Label, r.Windows[i].Start.Format(time.RFC3339), r.Windows[i].End.Format(time.RFC3339)))
	}
	outside := 0
	for i := range r.Failures {
		if r.Failures[i].Window == "" {
			outside++
		}
	}
	degradedOutside := 0
	if r.Hold != nil {
		degradedOutside = r.Hold.DegradedOutside
	}
	if outside == 0 && degradedOutside == 0 {
		return "every failure and degraded sample falls inside the declared platform window(s) " + strings.Join(labels, "; ") + " — no tunnel or daemon conclusion is drawn from them"
	}
	return fmt.Sprintf("%d failures and %d degraded samples fall OUTSIDE the declared platform window(s) %s — those stand on their own", outside, degradedOutside, strings.Join(labels, "; "))
}

// rerenderRun re-renders an existing run directory's report from its
// report.json, applying the --window annotations. It touches nothing else.
func rerenderRun(opts *options, stdout, stderr io.Writer) int {
	path := filepath.Join(opts.out, "report.json")
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "proof-1000: rerender: %v\n", err)
		return exitUsage
	}
	var r report
	if err := json.Unmarshal(raw, &r); err != nil {
		_, _ = fmt.Fprintf(stderr, "proof-1000: rerender: parse %s: %v\n", path, err)
		return exitUsage
	}
	r.Windows = opts.windows
	// Rebuild the failure list from the stored evidence rather than trusting
	// the stored list: it carries the timestamps the window split needs.
	r.collectFailures()
	r.annotateWindows()
	r.judge(r.Verify != nil, r.Hold != nil)
	if err := writeReport(opts.out, &r, nil); err != nil {
		_, _ = fmt.Fprintf(stderr, "proof-1000: rerender: %v\n", err)
		return exitUsage
	}
	printSummary(stdout, &r, opts.out)
	if r.Passed {
		return exitOK
	}
	return exitProofFailed
}

func writeReport(dir string, r *report, red *redactor) error {
	if err := writeRedactedJSON(filepath.Join(dir, "report.json"), r, red); err != nil {
		return err
	}
	md := red.apply(renderMarkdown(r))
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(md), 0o600)
}

func printSummary(w io.Writer, r *report, dir string) {
	out := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	out("\nproof-1000 run %s (n=%d): %s\n", r.Run, r.N, passFail(r.Passed))
	for _, line := range r.Verdict {
		out("  - %s\n", line)
	}
	out("\n")
	tw := newTable(w)
	if p := r.Publish; p != nil {
		tw.row("publish", fmt.Sprintf("%d ok (%d new, %d existing, %d resumed), %d registered-not-serving, %d failed, %.0fs wall, %.1f/min, p50 %dms p95 %dms",
			p.Published+p.Existing+p.Resumed, p.Published, p.Existing, p.Resumed, p.NotServing, p.Failed, p.WallSeconds, p.PublishesPerMinute, p.PublishMSP50, p.PublishMSP95))
		tw.row("api", fmt.Sprintf("%d calls, %.1f per publish, %d x 429, %d throttle events, concurrency %d..%d",
			p.APICalls, p.CallsPerPublish, p.HTTP429, p.Throttles, p.ConcurrencyMin, p.ConcurrencyMax))
	}
	s := r.ServingWait.Last
	tw.row("serving", fmt.Sprintf("%d/%d after %.0fs (starting %d, retrying %d, failed %d, stopped %d, absent %d, missing %d)",
		s.Serving, s.Total, r.ServingWait.WaitedS, s.Starting, s.Retrying, s.Failed, s.Stopped, s.Absent, s.Missing))
	if v := r.Verify; v != nil {
		tw.row("verify", fmt.Sprintf("%d/%d fetches ok, p50 %dms p95 %dms", v.OK, v.Sample, v.WallP50, v.WallP95))
	}
	if h := r.Hold; h != nil {
		tw.row("hold", fmt.Sprintf("%.0fs, %d samples (%d degraded: %d in / %d outside known windows, min serving %d), %d fetches (%d failed: %d in / %d outside)",
			h.DurationS, h.Samples, h.DegradedSamples, h.DegradedInWindow, h.DegradedOutside, h.MinServing, h.Fetches, h.FetchFailures, h.FetchFailuresInWindow, h.FetchFailuresOutside))
	}
	for i := range r.Usage {
		u := &r.Usage[i]
		tunnel := "n/a"
		if u.TunnelPort != "" {
			tunnel = ":" + u.TunnelPort + " " + itoa(u.MachineTCPToTunnel)
		}
		tw.row("daemon "+u.Label, fmt.Sprintf("pid %d rss %dMB threads %d fds %d tcp %d %v udp %d machine-tcp %d (to tunnel port %s)",
			u.PID, u.RSSKB/1024, u.Threads, u.FDs, u.TCPEstablished, u.TCPByRemotePort, u.UDPSockets, u.MachineTCPEstablished, tunnel))
	}
	e := r.Estimate
	tw.row("n=1000", fmt.Sprintf("budget bound %.1f publishes/min at %d req/min -> %.0f min; measured %.1f/min -> %.0f min",
		e.BudgetBoundPublishesPerMin, e.AssumedRatePerMinute, e.BudgetBoundMinutesFor1000, e.MeasuredPublishesPerMin, e.MeasuredMinutesFor1000))
	tw.row("failures", itoa(len(r.Failures)))
	tw.flush()
	out("\nreport: %s\n", filepath.Join(dir, "report.md"))
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

type table struct {
	w    io.Writer
	rows [][2]string
}

func newTable(w io.Writer) *table { return &table{w: w} }

func (t *table) row(k, v string) { t.rows = append(t.rows, [2]string{k, v}) }

func (t *table) flush() {
	width := 0
	for _, r := range t.rows {
		width = max(width, len(r[0]))
	}
	for _, r := range t.rows {
		_, _ = fmt.Fprintf(t.w, "  %-*s  %s\n", width, r[0], r[1])
	}
}

func renderMarkdown(r *report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# proof-1000 run `%s` — n=%d — %s\n\n", r.Run, r.N, passFail(r.Passed))
	for _, line := range r.Verdict {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	if len(r.Windows) > 0 {
		fmt.Fprintf(&b, "\n## Declared platform windows\n\n| label | start (UTC) | end (UTC) |\n|---|---|---|\n")
		for i := range r.Windows {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", r.Windows[i].Label, r.Windows[i].Start.Format(time.RFC3339), r.Windows[i].End.Format(time.RFC3339))
		}
	}
	renderEnvironment(&b, r)
	renderPublish(&b, r)
	renderCurve(&b, "Serving curve (publish start → all serving or deadline)", r.ServingWait.Curve)
	renderVerify(&b, r)
	renderUsage(&b, r)
	if r.Hold != nil {
		fmt.Fprintf(&b, "\n## Steady-state hold\n\n%.0fs, %d status samples, %d degraded (%d inside known windows, %d outside), min serving %d, %d fetches, %d failed (%d inside, %d outside)\n",
			r.Hold.DurationS, r.Hold.Samples, r.Hold.DegradedSamples, r.Hold.DegradedInWindow, r.Hold.DegradedOutside, r.Hold.MinServing,
			r.Hold.Fetches, r.Hold.FetchFailures, r.Hold.FetchFailuresInWindow, r.Hold.FetchFailuresOutside)
		renderCurve(&b, "Hold curve", thinCurve(r.Hold.Curve, 24))
		renderHoldFetches(&b, r.Hold.FetchResults)
	}
	renderEstimate(&b, r)
	renderFailures(&b, r)
	renderShares(&b, r)
	return b.String()
}

func renderEnvironment(b *strings.Builder, r *report) {
	e := r.Environment
	fmt.Fprintf(b, "\n## Environment\n\n| | |\n|---|---|\n")
	fmt.Fprintf(b, "| started | %s |\n| generated | %s |\n", r.Started.UTC().Format(time.RFC3339), r.GeneratedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(b, "| qurl (publish/list/delete) | `%s` — %s |\n| qurl (get) | `%s` — %s |\n", e.QurlBin, e.QurlVersion, e.ConsumeBin, e.ConsumeVersion)
	fmt.Fprintf(b, "| endpoint | %s (%s) |\n| Hub trust settings | %s |\n| deployment settings (`QURL_DEPLOYMENT`) | %t |\n", e.Endpoint, e.EndpointSource, e.HubSource, e.DeploymentSet)
	fmt.Fprintf(b, "| state dir | `%s` |\n| daemon | running=%t job=%s |\n| origin | %d requests, %d distinct hosts |\n", e.StateDir, e.DaemonRunning, e.DaemonJobVersion, r.Origin.Total, len(r.Origin.ByHost))
}

func renderPublish(b *strings.Builder, r *report) {
	p := r.Publish
	if p == nil {
		return
	}
	fmt.Fprintf(b, "\n## Publish\n\n| metric | value |\n|---|---|\n")
	fmt.Fprintf(b, "| published new / found existing / resumed | %d / %d / %d |\n", p.Published, p.Existing, p.Resumed)
	fmt.Fprintf(b, "| registered but not serving at publish exit | %d |\n| failed | %d |\n| retries | %d |\n", p.NotServing, p.Failed, p.Retries)
	fmt.Fprintf(b, "| wall | %.0fs (%.1f publishes/min) |\n| per-publish wall p50 / p95 / max | %dms / %dms / %dms |\n", p.WallSeconds, p.PublishesPerMinute, p.PublishMSP50, p.PublishMSP95, p.PublishMSMax)
	fmt.Fprintf(b, "| API calls | %d total, %.1f per publish |\n| HTTP 429 | %d (%d throttle events) |\n| concurrency | %d..%d (max %d) |\n", p.APICalls, p.CallsPerPublish, p.HTTP429, p.Throttles, p.ConcurrencyMin, p.ConcurrencyMax, p.ConcurrencyMax)
	fmt.Fprintf(b, "\n| t (s) | id | outcome | wall (ms) | limit | calls | 429 | exit |\n|---|---|---|---|---|---|---|---|\n")
	for i := range p.Events {
		ev := &p.Events[i]
		fmt.Fprintf(b, "| %.0f | %s | %s | %d | %d | %d | %d | %d |\n", ev.Elapsed, ev.ID, ev.Outcome, ev.WallMS, ev.Limit, ev.Calls, ev.HTTP429, ev.ExitCode)
	}
}

func renderCurve(b *strings.Builder, title string, curve []statusSample) {
	fmt.Fprintf(b, "\n## %s\n\n| t (s) | at (UTC) | serving | starting | retrying | failed | stopped | absent | missing | failures | window | error |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n", title)
	for i := range curve {
		s := &curve[i]
		fmt.Fprintf(b, "| %.0f | %s | %d/%d | %d | %d | %d | %d | %d | %d | %s | %s | %s |\n", s.Elapsed, s.At.UTC().Format("15:04:05"), s.Serving, s.Total, s.Starting, s.Retrying, s.Failed, s.Stopped, s.Absent, s.Missing, failureMap(s.Failures), s.Window, s.Err)
	}
}

func thinCurve(curve []statusSample, keep int) []statusSample {
	if len(curve) <= keep {
		return curve
	}
	step := (len(curve) + keep - 1) / keep
	out := make([]statusSample, 0, keep+1)
	for i := 0; i < len(curve); i += step {
		out = append(out, curve[i])
	}
	if last := curve[len(curve)-1]; out[len(out)-1].At != last.At {
		out = append(out, last)
	}
	return out
}

func failureMap(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"×"+itoa(m[k]))
	}
	return strings.Join(parts, ", ")
}

func renderVerify(b *strings.Builder, r *report) {
	v := r.Verify
	if v == nil {
		return
	}
	fmt.Fprintf(b, "\n## End-to-end sample\n\n%d fetched, %d ok, %d failed, p50 %dms, p95 %dms\n\n| id | at (UTC) | ok | wall (ms) | exit | host | nonce | host ok | seen at origin | window | error |\n|---|---|---|---|---|---|---|---|---|---|---|\n",
		v.Sample, v.OK, v.Failed, v.WallP50, v.WallP95)
	for i := range v.Results {
		f := &v.Results[i]
		fmt.Fprintf(b, "| %s | %s | %t | %d | %d | %s | %t | %s | %t | %s | %s |\n", f.ID, f.At.UTC().Format("15:04:05"), f.OK, f.WallMS, f.ExitCode, f.Host, f.NonceOK, hostVerdict(f), f.RequestSeen, f.Window, f.Error)
	}
}

func renderHoldFetches(b *strings.Builder, results []fetchResult) {
	fmt.Fprintf(b, "\n### Hold fetches\n\n| at (UTC) | id | ok | wall (ms) | exit | window | error |\n|---|---|---|---|---|---|---|\n")
	for i := range results {
		f := &results[i]
		fmt.Fprintf(b, "| %s | %s | %t | %d | %d | %s | %s |\n", f.At.UTC().Format("15:04:05"), f.ID, f.OK, f.WallMS, f.ExitCode, f.Window, f.Error)
	}
}

func hostVerdict(f *fetchResult) string {
	if !f.HostChecked {
		return "unchecked"
	}
	return boolStr(f.HostOK)
}

func renderUsage(b *strings.Builder, r *report) {
	fmt.Fprintf(b, "\n## Daemon resource usage\n\n| when | pid | rss (MB) | threads | fds | tcp established (by remote port) | udp | machine tcp established | machine tcp to tunnel port | errors |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for i := range r.Usage {
		u := &r.Usage[i]
		fmt.Fprintf(b, "| %s | %d | %d | %d | %d | %d %s | %d | %d | %d (:%s) | %s |\n", u.Label, u.PID, u.RSSKB/1024, u.Threads, u.FDs, u.TCPEstablished, failureMap(u.TCPByRemotePort), u.UDPSockets, u.MachineTCPEstablished, u.MachineTCPToTunnel, u.TunnelPort, strings.Join(u.Errors, "; "))
	}
}

func renderEstimate(b *strings.Builder, r *report) {
	e := r.Estimate
	fmt.Fprintf(b, "\n## Implied cost of N=%d on this client\n\n", targetN)
	fmt.Fprintf(b, "- %.1f API calls per publish; at the assumed per-owner budget of %d requests/min that bounds publishing to %.1f shares/min → %.0f min for %d.\n",
		e.CallsPerPublish, e.AssumedRatePerMinute, e.BudgetBoundPublishesPerMin, e.BudgetBoundMinutesFor1000, targetN)
	fmt.Fprintf(b, "- Measured %.1f publishes/min in this run → %.0f min for %d at the same rate.\n", e.MeasuredPublishesPerMin, e.MeasuredMinutesFor1000, targetN)
}

func renderFailures(b *strings.Builder, r *report) {
	fmt.Fprintf(b, "\n## Failures (%d)\n\n", len(r.Failures))
	for i := range r.Failures {
		f := &r.Failures[i]
		window := "outside any declared window"
		if f.Window != "" {
			window = "inside window " + f.Window
		}
		fmt.Fprintf(b, "- **%s** (%s) at %s — %s — %s: %s\n", f.ID, f.Stage, f.At.UTC().Format(time.RFC3339), window, f.CRID, f.Error)
		for _, line := range f.Log {
			fmt.Fprintf(b, "  - `%s`\n", line)
		}
	}
	fmt.Fprintf(b, "\n## Daemon log lines in the run window (%d)\n\n", len(r.DaemonLog))
	for _, line := range r.DaemonLog {
		fmt.Fprintf(b, "    %s\n", line)
	}
}

func renderShares(b *strings.Builder, r *report) {
	fmt.Fprintf(b, "\n## Shares\n\n| id | crid | routing id | publish (ms) | existing | attempts | calls | 429 | exit | error |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range r.Shares {
		existing := "-"
		if s.FoundExisting != nil {
			existing = boolStr(*s.FoundExisting)
		}
		fmt.Fprintf(b, "| %s | %s | %s | %d | %s | %d | %d | %d | %d | %s |\n", s.ID, s.CRID, s.RoutingID, s.PublishMS, existing, s.Attempts, s.APICalls, s.HTTP429, s.ExitCode, s.Error)
	}
}
