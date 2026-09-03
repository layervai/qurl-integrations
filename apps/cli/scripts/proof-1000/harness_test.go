package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestRunCLIThroughFakeBinary(t *testing.T) {
	t.Parallel()
	env := fakeEnvironment(t, []fakeRule{
		{Match: "whoami", Stdout: "owner\n", Stderr: "[debug] > GET /v1/me\n[debug] < HTTP 429, retrying in 1s\n[debug] < HTTP 200\n"},
		{Match: "delete", Stderr: "Error: rate limited\n", Exit: cliExitRateLimited},
	})
	ok := runCLI(context.Background(), env.QurlBin, env.childEnv, 20*time.Second, "-v", "whoami", "--quiet")
	if ok.ExitCode != cliExitOK || ok.Stdout != "owner\n" || ok.Calls.Total != 1 || ok.Calls.TooMany != 1 || ok.Calls.RetryWaitSum != time.Second {
		t.Fatalf("ok = %+v", ok)
	}
	limited := runCLI(context.Background(), env.QurlBin, env.childEnv, 20*time.Second, "delete", "crid", flagYes)
	if limited.ExitCode != cliExitRateLimited || lastErrorLine(limited.Stderr) != "Error: rate limited" {
		t.Fatalf("limited = %+v", limited)
	}
	missing := runCLI(context.Background(), filepath.Join(t.TempDir(), "absent"), env.childEnv, 20*time.Second, "version")
	if missing.ExitCode != -1 || missing.Err == nil {
		t.Fatalf("missing binary = %+v", missing)
	}
	unmatched := runCLI(context.Background(), env.QurlBin, env.childEnv, 20*time.Second, "nothing")
	if unmatched.ExitCode != 98 {
		t.Fatalf("unmatched = %+v", unmatched)
	}
}

func TestOriginAnswersWithNonceAndRecordsHosts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, err := startOrigin(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.close() }()
	if !strings.HasPrefix(o.targetURL(), "http://127.0.0.1:") || strings.HasSuffix(o.targetURL(), ":0") {
		t.Fatalf("targetURL = %q", o.targetURL())
	}
	for _, host := range []string{"c-one.qurl.site.example", "c-one.qurl.site.example", "c-two.qurl.site.example"} {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/path", http.NoBody)
		rec := httptest.NewRecorder()
		o.ServeHTTP(rec, req)
		var body originBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.ServerNonce != o.nonce || body.Host != host || body.Path != "/path" {
			t.Fatalf("body = %+v", body)
		}
		if seen, ok := o.sawRequest(body.RequestID); !ok || seen != host {
			t.Fatalf("request %s not recorded for %s", body.RequestID, host)
		}
	}
	stats := o.snapshot()
	if stats.Total != 3 || stats.ByHost["c-one.qurl.site.example"] != 2 || stats.ByHost["c-two.qurl.site.example"] != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	resp, err := http.Get(o.targetURL() + "/live") //nolint:noctx // test-local origin, no cancellation to propagate
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live origin answered %d", resp.StatusCode)
	}
}

func TestManifestPersistsAndResumes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, err := loadOrCreateManifest(dir, "r1", 3, 18080, "https://api.example.test", newRedactor("/home/nobody"))
	if err != nil {
		t.Fatal(err)
	}
	rec := m.record("proof-r1-0002")
	rec.CRID, rec.ResourceID = "crid-2", "rid-2"
	m.record("proof-r1-0001").Error = "boom"
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	again, err := loadOrCreateManifest(dir, "r1", 5, 19999, "https://api.example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Port != 18080 || again.N != 5 || again.Shares["proof-r1-0002"].CRID != "crid-2" {
		t.Fatalf("resumed manifest = %+v", again)
	}
	if ids := again.resourceIDs(); len(ids) != 1 || ids[0] != "rid-2" {
		t.Fatalf("resourceIDs = %v", ids)
	}
	if ordered := again.ordered(); len(ordered) != 2 || ordered[0].ID != "proof-r1-0001" {
		t.Fatalf("ordered = %+v", ordered)
	}
	narrow, err := loadOrCreateManifest(dir, "r1", 1, 18080, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if narrow.extraShares() != 1 || len(narrow.resourceIDs()) != 0 || len(narrow.ordered()) != 1 {
		t.Fatalf("narrowed manifest: extra=%d ids=%v ordered=%d", narrow.extraShares(), narrow.resourceIDs(), len(narrow.ordered()))
	}
	if shareIndex("r1", "proof-r1-0042") != 42 || shareIndex("r1", "proof-r1-00x2") != 0 || shareIndex("r1", "proof-r2-0001") != 0 {
		t.Fatal("shareIndex")
	}
	m.record("proof-r1-0003").Error = "owner auth0|leaked at /home/nobody/x"
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, manifestFile))); err != nil || strings.Contains(string(raw), "leaked") || strings.Contains(string(raw), "/home/nobody") {
		t.Fatalf("run.json must be redacted: %s %v", raw, err)
	}
	if _, err := loadOrCreateManifest(dir, "other", 1, 1, "", nil); err == nil {
		t.Fatal("a manifest from another run must be refused")
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateManifest(dir, "r1", 1, 1, "", nil); err == nil {
		t.Fatal("a corrupt manifest must be refused")
	}
}

func TestCollectDaemonLogsFiltersWindowAndNeedles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stamp := func(at time.Time) string { return at.Local().Format(daemonLogTimeLayout) }
	now := time.Now()
	lines := []string{
		stamp(now.Add(-2*time.Hour)) + " WARN old line crid=abc",
		stamp(now.Add(-time.Minute)) + " WARN share daemon session attempt failed; retrying crid=abc error=\"denied (errCode=\\\"52005\\\")\"",
		stamp(now.Add(-30*time.Second)) + " WARN unrelated line about nothing",
		stamp(now.Add(-20*time.Second)) + " WARN rate limit hit for owner auth0|secret123",
		"garbage without a timestamp crid=abc",
	}
	if err := os.WriteFile(filepath.Join(dir, "share-daemon.err.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	red := newRedactor("")
	got := collectDaemonLogs(dir, now.Add(-10*time.Minute), now, []string{"abc"}, red, 10)
	if len(got) != 2 || !strings.Contains(got[0], "52005") || strings.Contains(got[1], "secret123") {
		t.Fatalf("collected = %v", got)
	}
	if capped := collectDaemonLogs(dir, now.Add(-10*time.Minute), now, []string{"abc"}, red, 1); len(capped) != 1 {
		t.Fatalf("limit not honored: %v", capped)
	}
	if none := collectDaemonLogs(filepath.Join(dir, "absent"), now, now, nil, red, 5); none != nil {
		t.Fatalf("absent log dir should yield nothing: %v", none)
	}
	if lm := linesMentioning(got, []string{"52005"}, 5); len(lm) != 1 {
		t.Fatalf("linesMentioning = %v", lm)
	}
}

func sampleReport(t *testing.T) *report {
	t.Helper()
	now := time.Now()
	existing := true
	r := &report{
		Run: "r1", N: 2, Started: now.Add(-time.Minute),
		Environment: &environment{QurlBin: "qurl", QurlVersion: "qurl version 9.9.9", ConsumeBin: "qurl", ConsumeVersion: "qurl version 9.9.9", Endpoint: "https://api.example.test", StateDir: "/state"},
		Publish: &publishSummary{Started: now.Add(-time.Minute), Finished: now, Published: 1, Existing: 1, APICalls: 16, CallsPerPublish: 8, PublishesPerMinute: 30,
			Events: []publishEvent{{At: now.Add(-50 * time.Second), ID: "proof-r1-0001", Outcome: outcomePublished, WallMS: 3000, Limit: 4, Calls: 8}, {At: now.Add(-40 * time.Second), ID: "proof-r1-0002", Outcome: outcomeExisting, WallMS: 2000, Limit: 4, Calls: 8}}},
		ServingWait: servingWait{AllServing: true, Last: statusSample{Serving: 2, Total: 2}, Curve: []statusSample{{At: now.Add(-30 * time.Second), Serving: 2, Total: 2}}},
		Verify:      &verifySummary{Sample: 2, OK: 1, Failed: 1, Results: []fetchResult{{At: now.Add(-20 * time.Second), ID: "proof-r1-0001", CRID: "crid-1", OK: true, Host: "c-1.qurl.site.x", NonceOK: true, HostChecked: true, HostOK: true, RequestSeen: true}, {At: now.Add(-19 * time.Second), ID: "proof-r1-0002", CRID: "crid-2", Error: "Error: the service is busy right now", Diagnosis: &fetchDiagnosis{DenyCode: "52005"}}}},
		Usage:       []usageSample{{Label: "before", PID: 1, RSSKB: 2048, TCPByRemotePort: map[string]int{"7000": 2}, TunnelPort: "7000", Errors: []string{"x"}}, {Label: "after"}},
		Hold:        &holdSummary{Samples: 2, DegradedSamples: 1, MinServing: 1, Fetches: 1, FetchFailures: 1, Curve: []statusSample{{At: now.Add(-10 * time.Second), Serving: 1, Total: 2, Failures: map[string]int{"network/52005": 1}}, {At: now.Add(-5 * time.Second), Serving: 2, Total: 2}}, FetchResults: []fetchResult{{At: now.Add(-8 * time.Second), ID: "proof-r1-0001", CRID: "crid-1", Error: "Error: the download failed: the link answered HTTP 404"}}},
		Shares:      []*shareRecord{{ID: "proof-r1-0001", CRID: "crid-1", ResourceID: "rid-1", RoutingID: "c-1", PublishMS: 3000, Attempts: 1, APICalls: 8}, {ID: "proof-r1-0002", CRID: "crid-2", ResourceID: "rid-2", FoundExisting: &existing, Attempts: 1, APICalls: 8}, {ID: "proof-r1-0003", Attempts: 3, ExitCode: 9, Error: "Error: rate limited"}},
		DaemonLog:   []string{now.Add(-8*time.Second).Local().Format(daemonLogTimeLayout) + " WARN share daemon session attempt failed; retrying crid=crid-1"},
	}
	return r
}

func TestReportJudgesRendersAndRerenders(t *testing.T) {
	t.Parallel()
	r := sampleReport(t)
	r.GeneratedAt = time.Now()
	r.collectFailures()
	r.computeEstimate()
	r.annotateWindows()
	r.judge(true, true)
	if r.Passed || len(r.Failures) != 3 {
		t.Fatalf("passed=%t failures=%+v", r.Passed, r.Failures)
	}
	classes := map[string]bool{}
	for i := range r.Failures {
		classes[r.Failures[i].Class] = true
	}
	if !classes["nhp-ac-deny:52005"] || !classes[classContent404] || !classes[classOther] {
		t.Fatalf("classes = %v", classes)
	}
	if r.Estimate.BudgetBoundMinutesFor1000 != 80 || r.Estimate.MeasuredMinutesFor1000 <= 0 {
		t.Fatalf("estimate = %+v", r.Estimate)
	}
	md := renderMarkdown(r)
	for _, want := range []string{"# proof-1000 run `r1`", "## Publish", "## Serving curve", "## End-to-end sample", "## Daemon resource usage", "## Steady-state hold", "### Hold fetches", "## Implied cost", "## Failures (3)", "By class:", "## Shares", "nhp-ac-deny:52005", "content-404"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
	var out bytes.Buffer
	printSummary(&out, r, "/run")
	if !strings.Contains(out.String(), "FAIL") || !strings.Contains(out.String(), "budget bound") || !strings.Contains(out.String(), "daemon before") {
		t.Fatalf("summary = %s", out.String())
	}
	dir := t.TempDir()
	if err := writeReport(dir, r, newRedactor("")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	opts := &options{out: dir, windows: []knownWindow{{Label: "api-rollover", Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour)}}}
	if code := rerenderRun(opts, &stdout, &stderr); code != exitProofFailed {
		t.Fatalf("rerender exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "inside the declared platform window") {
		t.Fatalf("rerendered summary = %s", stdout.String())
	}
	if code := rerenderRun(&options{out: t.TempDir()}, &stdout, &stderr); code != exitUsage {
		t.Fatal("rerender without report.json must fail with a usage error")
	}
	passing := sampleReport(t)
	passing.Verify.Failed, passing.Verify.Results = 0, passing.Verify.Results[:1]
	passing.Hold.DegradedSamples, passing.Hold.FetchFailures, passing.Hold.FetchResults = 0, 0, nil
	passing.Shares = passing.Shares[:2]
	passing.collectFailures()
	passing.judge(true, true)
	if !passing.Passed || len(passing.Verdict) != 1 {
		t.Fatalf("passing verdict = %v", passing.Verdict)
	}
	interrupted := sampleReport(t)
	interrupted.Interrupted, interrupted.Publish, interrupted.Verify, interrupted.Hold = true, nil, nil, nil
	interrupted.judge(true, true)
	if len(interrupted.Verdict) < 3 {
		t.Fatalf("interrupted verdict = %v", interrupted.Verdict)
	}
	if got := thinCurve(nil, 3); got != nil {
		t.Fatal("thinCurve(nil)")
	}
}

func TestTeardownDeletesAndVerifiesThroughFakeCLI(t *testing.T) {
	t.Parallel()
	env := fakeEnvironment(t, []fakeRule{
		{Match: "delete crid-1", Stderr: "Error: rate limited\n", Exit: cliExitRateLimited, Times: 1},
		{Match: "delete crid-1", Stdout: `{"id":"crid-1","deleted":true}`},
		{Match: "delete crid-2", Stdout: `{"id":"crid-2","deleted":true,"already_gone":true}`},
		{Match: "delete crid-3", Stderr: "Error: not found\n", Exit: 5},
		{Match: "--cursor next", Stdout: `{"resources":[{"crid":"crid-3"}],"has_more":false}`},
		{Match: "list", Stdout: `{"resources":[{"crid":"crid-1"},{"crid":"other"},{"crid":"stray","target_url":"http://127.0.0.1:18080"},{"crid":"crid-1"}],"has_more":true,"next_cursor":"next"}`},
	})
	opts := fakeOptions(t, "r1", 3)
	lg := newLogger(io.Discard, filepath.Join(opts.out, "teardown.log"), env.redactor)
	defer lg.close()
	candidates := []shareRecord{{ID: "proof-r1-0001", CRID: "crid-1"}, {ID: "proof-r1-0002", CRID: "crid-2"}, {ID: "proof-r1-0003", CRID: "crid-3"}}
	results := deleteAll(context.Background(), opts, env, candidates, lg)
	if !results[0].Deleted || results[0].Attempts != 2 || !results[1].AlreadyGone || results[2].Deleted || results[2].ExitCode != 5 {
		t.Fatalf("results = %+v", results)
	}
	remaining, unexpected, pages := stillListed(context.Background(), env, candidates, "http://127.0.0.1:18080", lg)
	if pages != 2 || len(remaining) != 2 || remaining[0] != "proof-r1-0001" || remaining[1] != "proof-r1-0003" || len(unexpected) != 1 || unexpected[0] != "stray" {
		t.Fatalf("remaining = %v unexpected = %v pages = %d", remaining, unexpected, pages)
	}
	broken := fakeEnvironment(t, []fakeRule{{Match: "list", Stdout: "not json"}})
	if remaining, _, _ := stillListed(context.Background(), broken, candidates, "", lg); len(remaining) != 1 || !strings.Contains(remaining[0], "unparseable") {
		t.Fatalf("unparseable listing = %v", remaining)
	}
	failing := fakeEnvironment(t, []fakeRule{{Match: "list", Stderr: "Error: down\n", Exit: cliExitUnavailable}})
	if remaining, _, _ := stillListed(context.Background(), failing, candidates, "", lg); len(remaining) != 1 || !strings.Contains(remaining[0], "incomplete") {
		t.Fatalf("failed listing = %v", remaining)
	}
}

func TestEnvironmentChildEnvAndPreflight(t *testing.T) {
	t.Setenv("QURL_ENDPOINT", "")
	t.Setenv("QURL_CONNECTOR_HUB_HOST", "")
	if err := os.Unsetenv("QURL_ENDPOINT"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("QURL_CONNECTOR_HUB_HOST"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QURL_DEPLOYMENT", filepath.Join(t.TempDir(), "deployment.json"))
	agent := &launchAgent{stateDir: "/state", endpoint: "https://api.example.test", hubHost: "hub.example.test", hubPort: "443", hubKey: "KEY=="}
	env := &environment{}
	if err := env.buildChildEnv(&options{}, agent); err != nil {
		t.Fatal(err)
	}
	if env.EndpointSource != "launch-agent" || env.HubSource != "launch-agent" || env.Endpoint != "https://api.example.test" || !env.DeploymentSet {
		t.Fatalf("env = %+v", env)
	}
	if childValue(env.childEnv, "QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64") != "KEY==" {
		t.Fatal("hub triple not propagated to children")
	}
	pairs := hubLiteral(agent, env.childEnv)
	if len(pairs) < 4 || pairs[0] != "KEY==" {
		t.Fatalf("hubLiteral = %v", pairs)
	}
	if err := (&environment{}).buildChildEnv(&options{endpoint: "https://elsewhere.example.test"}, agent); err == nil {
		t.Fatal("an endpoint that contradicts the daemon's must be refused")
	}
	flagged := &environment{}
	if err := flagged.buildChildEnv(&options{endpoint: "https://api.example.test"}, nil); err != nil || flagged.EndpointSource != "flag" || flagged.HubSource != "build-default" {
		t.Fatalf("flagged env = %+v err = %v", flagged, err)
	}

	fake := fakeEnvironment(t, []fakeRule{
		{Match: "version", Stdout: "qurl version 9.9.9 (test/test)\n"},
		{Match: "whoami", Stdout: "owner\n"},
		{Match: "list", Stdout: `{"resources":[],"has_more":false}`},
	})
	if err := fake.preflight(context.Background(), &options{skipVerify: true}); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if fake.QurlVersion != "qurl version 9.9.9 (test/test)" || fake.DaemonRunning {
		t.Fatalf("preflight env = %+v", fake)
	}
	if err := fake.preflight(context.Background(), &options{}); err == nil || !strings.Contains(err.Error(), "QURL_DEPLOYMENT") {
		t.Fatalf("preflight without deployment settings should explain itself: %v", err)
	}
	unauth := fakeEnvironment(t, []fakeRule{{Match: "version", Stdout: "qurl version 1\n"}, {Match: "whoami", Stderr: "Error: no credential\n", Exit: 4}})
	if err := unauth.preflight(context.Background(), &options{skipVerify: true}); err == nil || !strings.Contains(err.Error(), "whoami") {
		t.Fatalf("unauthenticated preflight = %v", err)
	}
	if agent := parseLaunchAgentArgs(nil); agent.stateDir != "" {
		t.Fatal("empty args")
	}
}

func TestUsageSampleNeverPanics(t *testing.T) {
	t.Parallel()
	// A developer machine may have a real daemon and a CI runner has none;
	// either way the sample must be well-formed.
	env := &environment{StateDir: filepath.Join(t.TempDir(), "no-such-state")}
	sample := sampleUsage(context.Background(), env, "probe", 18080)
	if sample.Label != "probe" || sample.PID < 0 || sample.At.IsZero() {
		t.Fatalf("sample = %+v", sample)
	}
}

func TestTeardownSweepReportsTruncation(t *testing.T) {
	saved := listMaxPages
	listMaxPages = 2
	t.Cleanup(func() { listMaxPages = saved })
	env := fakeEnvironment(t, []fakeRule{{Match: "list", Stdout: `{"resources":[],"has_more":true,"next_cursor":"again"}`}})
	lg := newLogger(io.Discard, filepath.Join(t.TempDir(), "t.log"), env.redactor)
	defer lg.close()
	remaining, _, pages := stillListed(context.Background(), env, nil, "", lg)
	if pages != 2 || len(remaining) != 1 || !strings.Contains(remaining[0], "truncated") {
		t.Fatalf("remaining = %v pages = %d", remaining, pages)
	}
}

// Registry fixtures must satisfy the CLI's own registry decoder, which
// verifies that each CRID is derived from its resource's DER SPKI key. These
// are the real identities of four shares the harness published and deleted
// during its own validation: public identifiers of resources that no longer
// exist.
type fixtureIdentity struct{ ResourceID, CRID, RoutingID string }

var fixtureIdentities = []fixtureIdentity{
	{ResourceID: "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEk11gmp82EmOKDqUJw6QTbZBeh_hoh3H4e1qpFQqG9YqdcgNmEgWBpbksqpCwornfO8dUgPHyucCJwgBaiKoq9w", CRID: "qf77ayedrx5e6q4sfewmvtgoodd7nt6xeyyrt6ystzfnuwm2uk3lefw6equa", RoutingID: "c-fcnu6dzdfnzqe6eo5diw2ivlcjnplnadkvcnby6poo3qtxqfmola"},
	{ResourceID: "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE3WGq-BAh3oqzBkpEqOnK_JZx-SpvBc0zpAvB9PcwyNo4us7A4ZS-JMarvftWsxR263CgSX9rGl6Sim4ElSxn6w", CRID: "qfltugxm5eobtcxu6kffzm5ydvm4pbe3g3gj47q7ajtzeifchg6wngixw4zq", RoutingID: "c-pxgwpizasaiwn457p2cgvtehoaby2azbofgohpqb5hytjsbcivtq"},
	{ResourceID: "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEF4Gk8CAFGNI_x8uE-VyoTlYh5iB8ZAZ19Yx-qc60FDdjK3szxw8_bpcaMqeAH3ltMX1N9TWFzIUtTBl48Lmp_Q", CRID: "qg35no7hp6hymonjpt3loqsbjhqv7lyvhvho5urheglnnpi75tz4vueggk4a", RoutingID: "c-kv266uorhmu4o6lzvdgh4f3nqjnd6wmnxpml3h6et3ostmaoz42q"},
	{ResourceID: "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEpNg4AfGeVb9gAAlKGaA7mE2qyFYQQwes-x0nWssRGw_UDZ4IDy3nBKBahXBBq3Khe94JXWnW-e8wrvQ4dj1yYg", CRID: "qekur7hoxery5pf6xy6bfb6q5ndu3yhcldn6n5tycwhvzguzqxr75cmeo5za", RoutingID: "c-iygbpfwn7csmzvnrnmw2av564m252ullb4cndiak6zn5cuezas3q"},
}

func fixtureRow(connectorID string, identity, port int) connectorstate.LocalShare {
	id := fixtureIdentities[identity]
	return connectorstate.LocalShare{
		ResourceID: id.ResourceID, CRID: id.CRID, ConnectorID: connectorID,
		ConnectorRoutingID: id.RoutingID, KnockResourceID: "qurl-tunnel-server",
		TargetURL: "http://127.0.0.1:" + itoa(port), LocalIP: "127.0.0.1", LocalPort: port,
		DesiredState: "on", ServingEpoch: 1, UpdatedAt: time.Now().UTC(),
	}
}

func TestTeardownCandidatesUnionManifestAndRegistry(t *testing.T) {
	t.Parallel()
	env := fakeEnvironment(t, nil)
	opts := fakeOptions(t, "r1", 3)
	m, err := loadOrCreateManifest(opts.out, "r1", 3, 18080, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	m.record("proof-r1-0001").CRID = "crid-1"
	m.record("proof-r1-0002").Error = "never published"
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	writeRegistry(t, env.StateDir,
		fixtureRow("proof-r1-0003", 0, 18080),
		fixtureRow("local-other", 1, 3000),
		fixtureRow("proof-r10-0001", 2, 18080),
	)
	candidates, target, err := teardownCandidates(context.Background(), opts, env)
	if err != nil {
		t.Fatal(err)
	}
	if target != "http://127.0.0.1:18080" || len(candidates) != 2 || candidates[0].ID != "proof-r1-0001" || candidates[1].CRID != fixtureIdentities[0].CRID {
		t.Fatalf("candidates = %+v target = %s", candidates, target)
	}
	if remaining := registryRemaining(context.Background(), opts, env); len(remaining) != 1 || remaining[0] != "proof-r1-0003" {
		t.Fatalf("registryRemaining = %v", remaining)
	}
	if err := os.WriteFile(filepath.Join(opts.out, manifestFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := teardownCandidates(context.Background(), opts, env); err == nil {
		t.Fatal("a corrupt manifest must be refused")
	}
}

func TestPublisherOutcomesThroughFakeCLI(t *testing.T) {
	t.Parallel()
	env := fakeEnvironment(t, []fakeRule{
		publishOK("proof-r1-0001", "crid-1", false),
		publishOK("proof-r1-0002", "crid-2", true),
		{Match: " --id proof-r1-0003 ", Stderr: "[debug] > GET /v1/me\n[debug] < HTTP 429, retrying in 1ms\nError: rate limited\n", Exit: cliExitRateLimited, Times: 1},
		publishOK("proof-r1-0003", "crid-3", false),
		{Match: " --id proof-r1-0004 ", Stderr: "Error: qURL share did not become ready (daemon state retrying)\n", Exit: 1},
		{Match: " --id proof-r1-0005 ", Stdout: "{}", Exit: cliExitOK},
		{Match: " --id proof-r1-0006 ", Stderr: "Error: invalid\n", Exit: 8},
	})
	writeRegistry(t, env.StateDir, fixtureRow("proof-r1-0004", 3, 1))
	opts := fakeOptions(t, "r1", 6)
	m, err := loadOrCreateManifest(opts.out, "r1", 6, 1, "", env.redactor)
	if err != nil {
		t.Fatal(err)
	}
	o, err := startOrigin(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.close() }()
	lg := newLogger(io.Discard, filepath.Join(opts.out, "run.log"), env.redactor)
	defer lg.close()
	summary := publishAll(context.Background(), opts, env, m, o, lg)
	if summary.Published != 2 || summary.Existing != 1 || summary.NotServing != 1 || summary.Failed != 2 || summary.Retries != 1 || summary.Throttles != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if rec := m.record("proof-r1-0004"); !rec.TimedOut || rec.CRID != fixtureIdentities[3].CRID || rec.RoutingID != fixtureIdentities[3].RoutingID {
		t.Fatalf("registered-not-serving record = %+v", rec)
	}
	if rec := m.record("proof-r1-0003"); rec.Attempts != 2 || rec.HTTP429 != 1 || rec.CRID != "crid-3" {
		t.Fatalf("retried record = %+v", rec)
	}
	if rec := m.record("proof-r1-0005"); !strings.Contains(rec.Error, "no CRID") {
		t.Fatalf("empty publish JSON = %+v", rec)
	}
	if summary.CallsPerPublish <= 0 || summary.PublishMSP50 < 0 || summary.ConcurrencyMin < 1 {
		t.Fatalf("derived stats = %+v", summary)
	}
	// A second pass resumes every healthy share without calling the CLI.
	again := publishAll(context.Background(), opts, env, m, o, lg)
	if again.Resumed != 3 || again.Attempted != 3 {
		t.Fatalf("resume summary = %+v", again)
	}
}

func TestFetchShareVerifiesOriginAnswer(t *testing.T) {
	t.Parallel()
	o, err := startOrigin(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.close() }()
	req := httptest.NewRequest(http.MethodGet, "http://c-1.qurl.site.example/", http.NoBody)
	rec := httptest.NewRecorder()
	o.ServeHTTP(rec, req)
	good := strings.TrimSpace(rec.Body.String())
	env := fakeEnvironment(t, []fakeRule{
		{Match: "get crid-good", Stdout: good, Stderr: "[debug] > GET /v1/me\n[debug] < HTTP 200\n"},
		{Match: "get crid-stale", Stdout: `{"server_nonce":"other","request_id":"nope","host":"c-1.qurl.site.example","path":"/","seq":1}`},
		{Match: "get crid-busy", Stderr: "Error: the service is busy right now\n", Exit: cliExitUnavailable},
		{Match: "share crid-stale", Stderr: "Error: no\n", Exit: 5},
		{Match: "share crid-busy", Stdout: "not-a-link\n"},
	})
	ok := fetchShare(context.Background(), env, o, &shareRecord{ID: "a", CRID: "crid-good", RoutingID: "c-1"})
	if !ok.OK || !ok.HostChecked || !ok.HostOK || ok.APICalls != 1 {
		t.Fatalf("good fetch = %+v", ok)
	}
	stale := fetchShare(context.Background(), env, o, &shareRecord{ID: "b", CRID: "crid-stale", RoutingID: "c-2"})
	if stale.OK || stale.NonceOK || stale.RequestSeen || stale.HostOK || stale.Diagnosis == nil || !strings.Contains(stale.Diagnosis.Detail, "mint failed") {
		t.Fatalf("stale fetch = %+v diag=%+v", stale, stale.Diagnosis)
	}
	busy := fetchShare(context.Background(), env, o, &shareRecord{ID: "c", CRID: "crid-busy"})
	if busy.OK || busy.ExitCode != cliExitUnavailable || busy.Diagnosis == nil || busy.Diagnosis.Granted {
		t.Fatalf("busy fetch = %+v diag=%+v", busy, busy.Diagnosis)
	}
	if none := fetchShare(context.Background(), env, o, &shareRecord{ID: "d"}); none.OK || none.Error == "" {
		t.Fatal("a record without a CRID cannot be fetched")
	}
	opts := fakeOptions(t, "r1", 2)
	m, err := loadOrCreateManifest(opts.out, "r1", 2, 1, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	m.record("proof-r1-0001").CRID, m.record("proof-r1-0001").RoutingID = "crid-good", "c-1"
	m.record("proof-r1-0002").CRID = "crid-busy"
	lg := newLogger(io.Discard, filepath.Join(opts.out, "run.log"), env.redactor)
	defer lg.close()
	summary := verifySample(context.Background(), opts, env, o, m, []int{1, 2}, lg)
	if summary.OK != 1 || summary.Failed != 1 || summary.Sample != 2 {
		t.Fatalf("verify summary = %+v", summary)
	}
	// Race-instrumented child processes start slowly, so the hold budget is
	// generous; the assertions are on shape, not on exact counts.
	opts.hold, opts.fetchInterval = 4*time.Second, 200*time.Millisecond
	hold := holdSteady(context.Background(), opts, env, o, m, []string{"rid-1"}, []int{1, 2}, time.Now(), lg)
	if hold.Samples < 1 || hold.Fetches < 2 || hold.DegradedSamples != hold.Samples || hold.FetchFailures == 0 || hold.FetchFailures == hold.Fetches {
		t.Fatalf("hold = %+v", hold)
	}
}

func TestSelectSampleWithSmallExplicitSize(t *testing.T) {
	t.Parallel()
	got := selectSample(50, 5, 7)
	if len(got) != 5 || got[0] != 1 || got[1] != 2 || got[len(got)-1] != 50 {
		t.Fatalf("explicit small sample = %v", got)
	}
	if r := newRedactor("", "", "x", "keep", "<k>"); r.apply("abc keep") != "abc <k>" || len(r.literals) != 1 {
		t.Fatalf("blank literals must be skipped: %+v", r.literals)
	}
	if (*redactor)(nil).apply("same") != "same" {
		t.Fatal("nil redactor")
	}
}

func TestRerenderScrubsAnUnredactedReport(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		t.Skip("no home directory to redact")
	}
	dir := t.TempDir()
	r := sampleReport(t)
	r.Environment.StateDir = filepath.Join(home, ".local", "state", "qurl")
	r.GeneratedAt = time.Now()
	if err := writeReport(dir, r, nil); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := rerenderRun(&options{out: dir}, &stdout, &stderr); code != exitProofFailed {
		t.Fatalf("rerender exit %d: %s", code, stderr.String())
	}
	for _, name := range []string{"report.md", "report.json"} {
		raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), home) {
			t.Fatalf("%s still names the home directory after re-render", name)
		}
	}
}
