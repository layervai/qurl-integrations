package main

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
)

// egressGuard is the harness's HTTP boundary: a request to this machine goes
// through, and a request to any other host is reported and never sent. The
// harness injects it into every invocation that is not a live journey, so a
// test that omits --endpoint, or whose command stops failing before it builds
// a client, fails by name instead of sending its test credential to the
// built-in default endpoint.
type egressGuard struct {
	// report fails the owning test.
	report func(format string, args ...any)
	// next sends a request the guard lets through.
	next http.RoundTripper
}

func newEgressGuard(t *testing.T) *egressGuard {
	t.Helper()
	return &egressGuard{report: t.Errorf, next: http.DefaultTransport}
}

func (g *egressGuard) client() *http.Client { return &http.Client{Transport: g} }

func (g *egressGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if isLoopbackHost(req.URL.Hostname()) {
		return g.next.RoundTrip(req)
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	g.report("hermetic test sent %s %s to %s: pass --endpoint with a local test server, or expect the error that stops the command before it sends", req.Method, req.URL.Path, req.URL.Host)
	return nil, fmt.Errorf("test harness refused %s %s: %s is not this machine", req.Method, req.URL.Path, req.URL.Host)
}

// isLoopbackHost answers for the literal host only and never resolves a name
// itself. A loopback address passes by construction; the one name that passes
// is localhost, which is reserved for loopback and is what local test servers
// are commonly addressed by.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// egressRecorder stands in for the owning test, so a test can watch the guard
// fail without failing itself.
type egressRecorder struct {
	mu      sync.Mutex
	reports []string
}

func (r *egressRecorder) reportf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, fmt.Sprintf(format, args...))
}

func (r *egressRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reports...)
}

// countingTransport records whether the guard handed a request on.
type countingTransport struct{ sent atomic.Int32 }

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.sent.Add(1)
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, fmt.Errorf("countingTransport: %s %s was handed on", req.Method, req.URL)
}

// TestHarnessRefusesRequestsThatLeaveThisMachine pins the guard at the harness
// boundary: a command that reaches the API client with an endpoint that is not
// this machine fails the owning test, naming the request, and sends nothing.
// The first case is the regression this exists for: no endpoint at all, so the
// CLI resolves its built-in default.
func TestHarnessRefusesRequestsThatLeaveThisMachine(t *testing.T) {
	defaultHost := strings.TrimPrefix(config.DefaultEndpoint, "https://")
	for _, tc := range []struct {
		name     string
		args     []string
		env      map[string]string
		wantHost string
		wantCall string
	}{
		{name: "no endpoint resolves the default", args: []string{"list"}, wantHost: defaultHost, wantCall: "GET /v1/resources"},
		{name: "publish with no endpoint", args: []string{"publish", "https://example.com"}, wantHost: defaultHost, wantCall: "POST /v1/resources"},
		{name: "flag endpoint", args: []string{"--endpoint", "https://api.example.test", "list"}, wantHost: "api.example.test", wantCall: "GET /v1/resources"},
		{
			name: "environment endpoint", args: []string{"list"},
			env:      map[string]string{"QURL_API_KEY": testAPIKey, "QURL_ENDPOINT": "https://api.example.test"},
			wantHost: "api.example.test", wantCall: "GET /v1/resources",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder, next := &egressRecorder{}, &countingTransport{}
			res := runCLI(t, &runOpts{args: tc.args, env: tc.env, egress: &egressGuard{report: recorder.reportf, next: next}})

			if res.code == 0 {
				t.Fatalf("exit code = 0, want a failed command; stdout %q", res.stdout.String())
			}
			if sent := next.sent.Load(); sent != 0 {
				t.Fatalf("the guard handed on %d request(s), want none", sent)
			}
			reports := recorder.all()
			if len(reports) == 0 {
				t.Fatal("the guard reported nothing: the owning test would have passed")
			}
			for _, report := range reports {
				if want := "hermetic test sent " + tc.wantCall + " to " + tc.wantHost + ":"; !strings.HasPrefix(report, want) {
					t.Errorf("report = %q, want it to start %q", report, want)
				}
			}
		})
	}
}

// TestHarnessGuardIsTheDefault holds the wiring, not the guard: an invocation
// that names no guard sends through one all the same, and only a live journey
// keeps the production client. The client is read back from the command
// tree's own options, so removing the harness's default fails here.
func TestHarnessGuardIsTheDefault(t *testing.T) {
	// version sends nothing, so the default guard has nothing to report.
	res := runCLI(t, &runOpts{args: []string{"version"}})
	if res.code != 0 {
		t.Fatalf("version exit = %d, stderr %q", res.code, res.stderr.String())
	}
	if res.httpClient == nil {
		t.Fatal("a hermetic invocation kept the production HTTP client")
	}
	guard, ok := res.httpClient.Transport.(*egressGuard)
	if !ok {
		t.Fatalf("a hermetic invocation sends with %T, want the guard", res.httpClient.Transport)
	}
	if guard.next != http.DefaultTransport {
		t.Fatalf("default guard sends with %T, want the default transport", guard.next)
	}

	// The wired client itself refuses: stand in for this test and the network
	// only now, after the harness has built it.
	recorder, next := &egressRecorder{}, &countingTransport{}
	guard.report, guard.next = recorder.reportf, next
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, config.DefaultEndpoint+"/v1/resources", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := res.httpClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the harness client answered a request to the default endpoint")
	}
	if reports := recorder.all(); len(reports) != 1 || !strings.Contains(reports[0], "POST /v1/resources") || next.sent.Load() != 0 {
		t.Fatalf("reports = %q, handed on = %d; want one report naming POST /v1/resources and nothing sent", reports, next.sent.Load())
	}

	// version reads no settings and builds no client, so this is safe to run
	// with the production client in the default build.
	if live := runCLI(t, &runOpts{args: []string{"version"}, realOpener: true}); live.httpClient != nil {
		t.Fatalf("a live journey sends with %T, want the production client", live.httpClient.Transport)
	}
}

func TestEgressGuardPassesOnlyThisMachine(t *testing.T) {
	for _, tc := range []struct {
		url      string
		wantSent bool
	}{
		{"http://127.0.0.1:8080/v1/qurls", true},
		{"http://[::1]:8080/v1/qurls", true},
		{"http://localhost:8080/v1/qurls", true},
		{"https://LOCALHOST/v1/qurls", true},
		{config.DefaultEndpoint + "/v1/qurls", false},
		{"https://localhost.example.test/v1/qurls", false},
		{"https://127.0.0.1.example.test/v1/qurls", false},
		{"http://192.0.2.10/v1/qurls", false},
		{"http://0.0.0.0/v1/qurls", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			recorder, next := &egressRecorder{}, &countingTransport{}
			guard := &egressGuard{report: recorder.reportf, next: next}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.url, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := guard.RoundTrip(req)
			if err == nil {
				_ = resp.Body.Close()
				t.Fatal("RoundTrip answered; both test transports refuse")
			}
			wantSent, wantReports := int32(0), 1
			if tc.wantSent {
				wantSent, wantReports = 1, 0
			}
			if sent := next.sent.Load(); sent != wantSent {
				t.Fatalf("handed on %d request(s), want %d", sent, wantSent)
			}
			if reports := recorder.all(); len(reports) != wantReports {
				t.Fatalf("reports = %q, want %d", reports, wantReports)
			}
		})
	}
}
