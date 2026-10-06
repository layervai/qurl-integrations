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
	g.report("hermetic test sent %s %s to %s: pass --endpoint with a local test server, or expect the error that stops the command before it sends", req.Method, req.URL.Path, req.URL.Host)
	return nil, fmt.Errorf("test harness refused %s %s: %s is not this machine", req.Method, req.URL.Path, req.URL.Host)
}

// isLoopbackHost answers for the literal host only. A name is never resolved:
// only an address that cannot leave this machine by construction passes.
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
		{name: "no endpoint resolves the default", args: []string{"list"}, wantHost: defaultHost, wantCall: "GET /v1/"},
		{name: "publish with no endpoint", args: []string{"publish", "https://example.com"}, wantHost: defaultHost, wantCall: "POST /v1/resources"},
		{name: "flag endpoint", args: []string{"--endpoint", "https://api.example.test", "list"}, wantHost: "api.example.test", wantCall: "GET /v1/"},
		{
			name: "environment endpoint", args: []string{"list"},
			env:      map[string]string{"QURL_API_KEY": testAPIKey, "QURL_ENDPOINT": "https://api.example.test"},
			wantHost: "api.example.test", wantCall: "GET /v1/",
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
				if !strings.Contains(report, tc.wantCall) || !strings.Contains(report, " to "+tc.wantHost+":") {
					t.Errorf("report = %q, want it to name %q and host %q", report, tc.wantCall, tc.wantHost)
				}
			}
		})
	}
}

// TestHarnessGuardIsTheDefault holds the wiring, not the guard: an invocation
// that names no guard still gets one, and it reports to the test it was given.
func TestHarnessGuardIsTheDefault(t *testing.T) {
	recorder := &egressRecorder{}
	guard := newEgressGuard(t)
	if guard.next != http.DefaultTransport {
		t.Fatalf("default guard sends with %T, want the default transport", guard.next)
	}
	guard.report, guard.next = recorder.reportf, &countingTransport{}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, config.DefaultEndpoint+"/v1/resources", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := guard.client().Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the guard answered a request to the default endpoint")
	}
	if reports := recorder.all(); len(reports) != 1 || !strings.Contains(reports[0], "POST /v1/resources") {
		t.Fatalf("reports = %q, want one naming POST /v1/resources", reports)
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
			sent, reported := next.sent.Load() == 1, len(recorder.all()) == 1
			if sent != tc.wantSent || reported == tc.wantSent {
				t.Fatalf("sent = %v, reported = %v; want sent = %v and reported = %v", sent, reported, tc.wantSent, !tc.wantSent)
			}
		})
	}
}
