package apitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// failureRecorder stands in for t.Errorf so a test can watch the server
// report a violation without failing itself.
type failureRecorder struct {
	mu      sync.Mutex
	reports []string
}

func (f *failureRecorder) failf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, fmt.Sprintf(format, args...))
}

func (f *failureRecorder) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reports...)
}

// recordFailures swaps the server's test-failing hook for a recorder.
func recordFailures(srv *Server) *failureRecorder {
	recorder := &failureRecorder{}
	srv.mu.Lock()
	srv.failf = recorder.failf
	srv.mu.Unlock()
	return recorder
}

// postShare sends one share request with the given Authorization header
// (omitted when empty) and returns the status and body.
func postShare(t *testing.T, srv *Server, authorization string) (status int, body string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/v1/resources/"+srv.Key.CRID+"/share", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

// problemCode extracts error.code from the platform's error envelope.
func problemCode(t *testing.T, body string) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode problem %q: %v", body, err)
	}
	return envelope.Error.Code
}

// TestShareWithoutCredentialFailsTheOwningTestAndIsRefused proves the guard
// every hermetic CLI test relies on: a share request with no bearer
// credential is reported to the owning test and answered 401 `unauthorized`,
// while the same request with a credential mints a link and reports nothing.
func TestShareWithoutCredentialFailsTheOwningTestAndIsRefused(t *testing.T) {
	for name, authorization := range map[string]string{
		"no header":    "",
		"empty bearer": "Bearer ",
		"not a bearer": "Basic dXNlcjpwYXNz",
	} {
		t.Run(name, func(t *testing.T) {
			srv := NewServer(t)
			failures := recordFailures(srv)

			status, body := postShare(t, srv, authorization)
			if status != http.StatusUnauthorized || problemCode(t, body) != "unauthorized" {
				t.Fatalf("share without a credential = HTTP %d %s, want 401 unauthorized", status, body)
			}
			if strings.Contains(body, "qurl.link") {
				t.Fatalf("share without a credential minted a link: %s", body)
			}
			reports := failures.all()
			if len(reports) != 1 || !strings.Contains(reports[0], "/v1/resources/"+srv.Key.CRID+"/share") {
				t.Fatalf("reports = %q, want one naming the share route", reports)
			}

			status, body = postShare(t, srv, "Bearer device-credential")
			if status != http.StatusOK || !strings.Contains(body, "qurl.link") {
				t.Fatalf("share with a credential = HTTP %d %s, want a minted link", status, body)
			}
			if reports := failures.all(); len(reports) != 1 {
				t.Fatalf("a credentialed share was reported: %q", reports)
			}

			requests := srv.Requests()
			if len(requests) != 2 || requests[0].Header.Get("Authorization") != strings.TrimSpace(authorization) {
				t.Fatalf("recorded requests = %+v, want both, the refused one included", requests)
			}
		})
	}
}

// TestShareWithoutCredentialNeverReachesAScript proves a scripted share
// answer cannot hide the defect: the request without a credential is still
// reported and refused, and the script stays queued for the next request that
// does carry one.
func TestShareWithoutCredentialNeverReachesAScript(t *testing.T) {
	srv := NewServer(t)
	failures := recordFailures(srv)
	var scripted atomic.Int32
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/share", func(w http.ResponseWriter, _ *http.Request) {
		scripted.Add(1)
		WriteProblem(t, w, http.StatusNotFound, "resource_not_found", "Not Found", "scripted answer")
	})

	status, body := postShare(t, srv, "")
	if status != http.StatusUnauthorized || problemCode(t, body) != "unauthorized" {
		t.Fatalf("share without a credential = HTTP %d %s, want 401 unauthorized", status, body)
	}
	if scripted.Load() != 0 || len(failures.all()) != 1 {
		t.Fatalf("scripted calls = %d, reports = %q; want the script untouched and one report", scripted.Load(), failures.all())
	}

	status, body = postShare(t, srv, "Bearer device-credential")
	if status != http.StatusNotFound || problemCode(t, body) != "resource_not_found" {
		t.Fatalf("credentialed share = HTTP %d %s, want the scripted 404", status, body)
	}
	if scripted.Load() != 1 || len(failures.all()) != 1 {
		t.Fatalf("scripted calls = %d, reports = %q; want one scripted answer and no new report", scripted.Load(), failures.all())
	}
}
