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

// sharePath is the share route for the server's own resource.
func sharePath(srv *Server) string {
	return "/v1/resources/" + srv.Key.CRID + "/share"
}

// send makes one request with a JSON body and the given Authorization header
// (omitted when empty) and returns the status and body.
func send(t *testing.T, srv *Server, method, path, authorization string) (status int, body string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader("{}"))
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

// sharedCRID extracts data.crid from a share answer.
func sharedCRID(t *testing.T, body string) string {
	t.Helper()
	var envelope struct {
		Data struct {
			CRID string `json:"crid"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode share answer %q: %v", body, err)
	}
	return envelope.Data.CRID
}

// TestShareWithoutCredentialFailsTheOwningTestAndIsRefused proves the guard
// every hermetic CLI test relies on: a share request with no bearer
// credential is reported to the owning test and answered 401 `unauthorized`,
// while the same request with a credential mints a link and reports nothing.
func TestShareWithoutCredentialFailsTheOwningTestAndIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, authorization string }{
		{"no header", ""},
		{"empty bearer", "Bearer "},
		{"not a bearer", "Basic dXNlcjpwYXNz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(t)
			failures := recordFailures(srv)

			status, body := send(t, srv, http.MethodPost, sharePath(srv), tc.authorization)
			if status != http.StatusUnauthorized || problemCode(t, body) != "unauthorized" {
				t.Fatalf("share without a credential = HTTP %d %s, want 401 unauthorized", status, body)
			}
			if strings.Contains(body, "qurl.link") {
				t.Fatalf("share without a credential minted a link: %s", body)
			}
			reports := failures.all()
			if len(reports) != 1 || !strings.Contains(reports[0], sharePath(srv)) {
				t.Fatalf("reports = %q, want one naming the share route", reports)
			}

			status, body = send(t, srv, http.MethodPost, sharePath(srv), "Bearer device-credential")
			if status != http.StatusOK || !strings.Contains(body, "qurl.link") {
				t.Fatalf("share with a credential = HTTP %d %s, want a minted link", status, body)
			}
			if reports := failures.all(); len(reports) != 1 {
				t.Fatalf("a credentialed share was reported: %q", reports)
			}

			requests := srv.Requests()
			if len(requests) != 2 || requests[0].Header.Get("Authorization") != strings.TrimSpace(tc.authorization) {
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
	srv.Script(http.MethodPost, sharePath(srv), func(w http.ResponseWriter, _ *http.Request) {
		scripted.Add(1)
		WriteProblem(t, w, http.StatusNotFound, "resource_not_found", "Not Found", "scripted answer")
	})

	status, body := send(t, srv, http.MethodPost, sharePath(srv), "")
	if status != http.StatusUnauthorized || problemCode(t, body) != "unauthorized" {
		t.Fatalf("share without a credential = HTTP %d %s, want 401 unauthorized", status, body)
	}
	if scripted.Load() != 0 || len(failures.all()) != 1 {
		t.Fatalf("scripted calls = %d, reports = %q; want the script untouched and one report", scripted.Load(), failures.all())
	}

	status, body = send(t, srv, http.MethodPost, sharePath(srv), "Bearer device-credential")
	if status != http.StatusNotFound || problemCode(t, body) != "resource_not_found" {
		t.Fatalf("credentialed share = HTTP %d %s, want the scripted 404", status, body)
	}
	if scripted.Load() != 1 || len(failures.all()) != 1 {
		t.Fatalf("scripted calls = %d, reports = %q; want one scripted answer and no new report", scripted.Load(), failures.all())
	}
}

// TestShareResourceIDMatchesOnlyTheShareRoute pins the route shape the
// credential rule applies to: the share operator with exactly one resource
// segment, with or without the version prefix, and nothing else that merely
// ends in /share.
func TestShareResourceIDMatchesOnlyTheShareRoute(t *testing.T) {
	for _, tc := range []struct {
		name, path, wantID string
		want               bool
	}{
		{name: "versioned", path: "/v1/resources/qexample/share", wantID: "qexample", want: true},
		{name: "unversioned", path: "/resources/qexample/share", wantID: "qexample", want: true},
		{name: "unrelated suffix", path: "/v1/admin/share"},
		{name: "bare suffix", path: "/share"},
		{name: "nested resource", path: "/v1/resources/qexample/sessions/share"},
		{name: "empty resource", path: "/v1/resources//share"},
		{name: "no resource", path: "/v1/resources/share"},
		{name: "different version", path: "/v2/resources/qexample/share"},
		{name: "doubled version", path: "/v1/v1/resources/qexample/share"},
		{name: "other prefix", path: "/proxy/v1/resources/qexample/share"},
		{name: "sharing lifecycle", path: "/v1/resources/qexample/sharing"},
		{name: "trailing slash", path: "/v1/resources/qexample/share/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := shareResourceID(tc.path)
			if ok != tc.want || id != tc.wantID {
				t.Fatalf("shareResourceID(%q) = %q, %t; want %q, %t", tc.path, id, ok, tc.wantID, tc.want)
			}
		})
	}
}

// TestOnlyTheShareRouteIsHeldToTheCredentialRule proves a route that is not
// the share operator is left alone even when its path ends in /share: with no
// credential it is neither reported nor answered 401, and a script queued for
// it still runs. The share route under another method is not a share request
// either.
func TestOnlyTheShareRouteIsHeldToTheCredentialRule(t *testing.T) {
	srv := NewServer(t)
	failures := recordFailures(srv)

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/v1/admin/share"},
		{http.MethodPost, "/share"},
		{http.MethodPost, "/v1/resources/" + srv.Key.CRID + "/sessions/share"},
		{http.MethodPost, "/v2/resources/" + srv.Key.CRID + "/share"},
		{http.MethodGet, sharePath(srv)},
	} {
		status, body := send(t, srv, route.method, route.path, "")
		if status != http.StatusNotFound || problemCode(t, body) != "not_found" {
			t.Errorf("%s %s without a credential = HTTP %d %s, want the mock's 404 for an unknown route", route.method, route.path, status, body)
		}
	}

	var scripted atomic.Int32
	srv.Script(http.MethodPost, "/v1/admin/share", func(w http.ResponseWriter, _ *http.Request) {
		scripted.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	if status, _ := send(t, srv, http.MethodPost, "/v1/admin/share", ""); status != http.StatusNoContent || scripted.Load() != 1 {
		t.Errorf("scripted non-share route = HTTP %d after %d scripted calls, want its 204 without a credential", status, scripted.Load())
	}

	if reports := failures.all(); len(reports) != 0 {
		t.Fatalf("routes that are not the share operator were reported: %q", reports)
	}
}

// TestUnversionedShareRouteIsHeldToTheSameRule covers the share route without
// its version prefix, which the mock serves the same way: it needs a
// credential, and its answer echoes the CRID that path names.
func TestUnversionedShareRouteIsHeldToTheSameRule(t *testing.T) {
	srv := NewServer(t)
	failures := recordFailures(srv)
	requested := DeriveCRID(t, []byte("a-different-resource-key"), VersionTest)

	for _, path := range []string{"/resources/" + requested + "/share", "/v1/resources/" + requested + "/share"} {
		before := len(failures.all())
		status, body := send(t, srv, http.MethodPost, path, "")
		if status != http.StatusUnauthorized || problemCode(t, body) != "unauthorized" {
			t.Errorf("POST %s without a credential = HTTP %d %s, want 401 unauthorized", path, status, body)
		}
		if got := len(failures.all()) - before; got != 1 {
			t.Errorf("POST %s without a credential was reported %d times, want once", path, got)
		}

		status, body = send(t, srv, http.MethodPost, path, "Bearer device-credential")
		if status != http.StatusOK || sharedCRID(t, body) != requested {
			t.Errorf("POST %s with a credential = HTTP %d %s, want a link for the requested CRID", path, status, body)
		}
	}
}
