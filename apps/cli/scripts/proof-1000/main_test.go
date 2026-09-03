package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
)

func TestParseAPICallsCountsRequestsAndStatuses(t *testing.T) {
	t.Parallel()
	stderr := strings.Join([]string{
		"[debug] > GET /v1/me",
		"[debug] < HTTP 200",
		"[debug] > PUT /v1/resources/abc/sharing",
		"[debug] < HTTP 429, retrying in 2s",
		"[debug] < HTTP 200",
		"Error: something",
	}, "\n")
	calls := parseAPICalls(stderr)
	if calls.Total != 2 || calls.TooMany != 1 || calls.ByStatus["200"] != 2 || calls.RetryWaits != 2*time.Second {
		t.Fatalf("calls = %+v", calls)
	}
	if got := lastErrorLine(stderr); got != "Error: something" {
		t.Fatalf("lastErrorLine = %q", got)
	}
	if parseAPICalls("").ByStatus != nil {
		t.Fatal("empty stderr should yield a nil status map")
	}
}

func TestParseLaunchAgentArgs(t *testing.T) {
	t.Parallel()
	agent := parseLaunchAgentArgs([]string{
		"/usr/local/bin/qurl", "--endpoint", "https://api.example.test", "daemon", "run",
		"--state-dir", "/tmp/state", "--job-version", "3/2.1.1",
		"--hub-host", "hub.example.test", "--hub-port", "443", "--hub-server-public-key-b64", "AAAA",
	})
	if agent.stateDir != "/tmp/state" || agent.endpoint != "https://api.example.test" ||
		agent.hubHost != "hub.example.test" || agent.hubPort != "443" || agent.hubKey != "AAAA" {
		t.Fatalf("agent = %+v", agent)
	}
}

func TestConnectorIDsMatchOnlyTheirRun(t *testing.T) {
	t.Parallel()
	pattern := connectorIDPattern("r1")
	for _, id := range []string{connectorID("r1", 1), connectorID("r1", 1000), "proof-r1-12345"} {
		if !pattern.MatchString(id) {
			t.Errorf("%s should match", id)
		}
	}
	for _, id := range []string{"proof-r10-0001", "proof-r1-001", "xproof-r1-0001", "proof-r1-0001x", "local-abc"} {
		if pattern.MatchString(id) {
			t.Errorf("%s must not match", id)
		}
	}
	if got := connectorID("r1", 7); got != "proof-r1-0007" {
		t.Fatalf("connectorID = %q", got)
	}
}

func TestSelectSample(t *testing.T) {
	t.Parallel()
	if got := selectSample(3, 0, 1); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("small n should select everything: %v", got)
	}
	got := selectSample(1000, 0, 1)
	if len(got) != autoSampleRandom+2*autoSampleEdges {
		t.Fatalf("auto sample size = %d", len(got))
	}
	for i := 1; i <= autoSampleEdges; i++ {
		if got[i-1] != i || got[len(got)-i] != 1000-i+1 {
			t.Fatalf("edges missing from %v", got)
		}
	}
	again := selectSample(1000, 0, 1)
	for i := range got {
		if got[i] != again[i] {
			t.Fatal("sample selection must be deterministic for a seed")
		}
	}
}

func TestRedactorScrubsSecretsAndLiterals(t *testing.T) {
	t.Parallel()
	r := newRedactor("/Users/someone", "hub.example.test", "<hub>", "KEYVALUE==", "<hub>")
	in := `owner auth0|abc123 key lv_live_abcd Bearer tok.en link https://x/#` + strings.Repeat("a", 30) +
		` host hub.example.test key KEYVALUE== path /Users/someone/x "owner_id": "auth0|zzz"`
	out := r.apply(in)
	for _, leak := range []string{"abc123", "lv_live_abcd", "tok.en", strings.Repeat("a", 30), "hub.example.test", "KEYVALUE==", "/Users/someone", "zzz"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked in %q", leak, out)
		}
	}
	if !strings.Contains(out, "~/x") || !strings.Contains(out, "<hub>") {
		t.Fatalf("replacements missing: %q", out)
	}
}

func TestLimiterAdapts(t *testing.T) {
	t.Parallel()
	l := newLimiter(4)
	ctx := context.Background()
	for range 4 {
		if err := l.acquire(ctx); err != nil {
			t.Fatal(err)
		}
	}
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := l.acquire(blocked); err == nil {
		t.Fatal("fifth acquire should block at limit 4")
	}
	l.release(true, 20*time.Millisecond)
	if l.current() != 2 || l.throttles != 1 {
		t.Fatalf("throttle should halve the limit: %+v", l)
	}
	for range 3 {
		l.release(false, 0)
	}
	time.Sleep(30 * time.Millisecond)
	for range limiterGrowStreak {
		if err := l.acquire(ctx); err != nil {
			t.Fatal(err)
		}
		l.release(false, 0)
	}
	if l.current() != 3 || l.minSeen != 2 || l.maxSeen != 4 {
		t.Fatalf("clean streak should grow by one: %+v", l)
	}
}

func TestStatusSampleCountsStatesAndFailures(t *testing.T) {
	t.Parallel()
	var s statusSample
	s.count(&connectordaemon.ResourceDiagnostic{State: daemonStateServing})
	s.count(&connectordaemon.ResourceDiagnostic{State: daemonStateRetrying, FailureCategory: "network", FailureCode: "52005"})
	s.count(&connectordaemon.ResourceDiagnostic{State: daemonStateRetrying, FailureCategory: "network", FailureCode: "52005"})
	s.count(&connectordaemon.ResourceDiagnostic{State: daemonStateFailed, FailureCategory: "identity"})
	if s.Serving != 1 || s.Retrying != 2 || s.Failed != 1 || s.Failures["network/52005"] != 2 || s.Failures["identity/"] != 1 {
		t.Fatalf("sample = %+v", s)
	}
}

func TestPercentileAndThinCurve(t *testing.T) {
	t.Parallel()
	if percentile(nil, 0.5) != 0 || percentile([]int64{1, 2, 3, 4}, 0.5) != 2 || percentile([]int64{1, 2, 3, 4}, 0.95) != 3 {
		t.Fatal("percentile")
	}
	curve := make([]statusSample, 100)
	for i := range curve {
		curve[i].At = time.Unix(int64(i), 0)
	}
	thin := thinCurve(curve, 10)
	if len(thin) < 10 || len(thin) > 11 || thin[len(thin)-1].At != curve[99].At {
		t.Fatalf("thinCurve kept %d samples", len(thin))
	}
}

func TestParseOptionsRejectsBadInput(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{}, {"--run", "Bad_Name"}, {"--run", "r1", "--n", "0"}, {"--run", "r1", "--port", "80"}, {"--run", "r1", "extra"},
	} {
		if _, err := parseOptions(args, io.Discard); err == nil {
			t.Errorf("args %v should be rejected", args)
		}
	}
	opts, err := parseOptions([]string{"--run", "r1", "--n", "25"}, io.Discard)
	if err != nil || opts.n != 25 || opts.consumeBin != opts.qurlBin || !strings.HasSuffix(opts.out, "proof-1000-runs/r1") {
		t.Fatalf("opts = %+v err = %v", opts, err)
	}
}

func TestTunnelPortAndRemotePort(t *testing.T) {
	t.Parallel()
	if got := tunnelPort(map[string]int{"18080": 25, "7000": 25, "443": 1}, "18080"); got != "7000" {
		t.Fatalf("tunnelPort = %q", got)
	}
	if tunnelPort(nil, "18080") != "" {
		t.Fatal("empty map should yield no port")
	}
	if !remotePortIs("tcp4  0  0  192.0.2.1.54321  198.51.100.1.7000  ESTABLISHED", "7000") ||
		remotePortIs("tcp4  0  0  192.0.2.1.7000  198.51.100.1.443  ESTABLISHED", "7000") == false ||
		remotePortIs("ESTAB 0 0 192.0.2.1:54321 198.51.100.1:7001", "7000") {
		t.Fatal("remotePortIs")
	}
}

func TestKnownWindowParsingAndAnnotation(t *testing.T) {
	t.Parallel()
	w, err := parseKnownWindow("api-rollover=2026-09-03T01:37:00Z/2026-09-03T01:46:00Z")
	if err != nil || w.Label != "api-rollover" || !w.End.After(w.Start) {
		t.Fatalf("parse = %+v, %v", w, err)
	}
	for _, bad := range []string{"", "nolabel", "x=2026-09-03T01:37:00Z", "x=bad/2026-09-03T01:46:00Z", "x=2026-09-03T01:46:00Z/2026-09-03T01:37:00Z"} {
		if _, err := parseKnownWindow(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	inside := time.Date(2026, 9, 3, 1, 40, 0, 0, time.UTC)
	outside := time.Date(2026, 9, 3, 2, 0, 0, 0, time.UTC)
	r := &report{
		Windows:  []knownWindow{w},
		Failures: []failureDetail{{At: inside}, {At: outside}},
		Hold: &holdSummary{
			Curve:        []statusSample{{At: inside, Serving: 3, Total: 5}, {At: outside, Serving: 5, Total: 5}, {At: outside, Serving: 4, Total: 5}},
			FetchResults: []fetchResult{{At: inside}, {At: outside, OK: true}, {At: outside}},
		},
	}
	r.annotateWindows()
	if r.Failures[0].Window != "api-rollover" || r.Failures[1].Window != "" {
		t.Fatalf("failure windows = %+v", r.Failures)
	}
	if r.Hold.DegradedInWindow != 1 || r.Hold.DegradedOutside != 1 || r.Hold.FetchFailuresInWindow != 1 || r.Hold.FetchFailuresOutside != 1 {
		t.Fatalf("hold splits = %+v", r.Hold)
	}
	if !strings.Contains(r.windowVerdict(), "OUTSIDE") {
		t.Fatalf("verdict should flag outside failures: %s", r.windowVerdict())
	}
	r.Failures, r.Hold.Curve, r.Hold.FetchResults = r.Failures[:1], r.Hold.Curve[:2], r.Hold.FetchResults[:2]
	r.annotateWindows()
	if strings.Contains(r.windowVerdict(), "OUTSIDE") {
		t.Fatalf("verdict should attribute everything to the window: %s", r.windowVerdict())
	}
}
