package apitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
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

// answer is the part of a resource answer these tests read.
type answer struct {
	Data struct {
		Private           *bool    `json:"private"`
		AllowedDeviceKeys []string `json:"allowed_device_keys"`
		Resource          *struct {
			Private           *bool    `json:"private"`
			AllowedDeviceKeys []string `json:"allowed_device_keys"`
		} `json:"resource"`
	} `json:"data"`
	Error struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"error"`
}

// call sends one authenticated JSON request and decodes the answer.
func call(t *testing.T, srv *Server, method, path, body string) (int, answer) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer lv_test_apitest")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded answer
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode %s %s answer: %v", method, path, err)
	}
	return resp.StatusCode, decoded
}

// TestCreateDefaultIsPrivateUnlessTheMockPlaysAnOlderService pins what the mock
// does with a create request that states no privacy: private, as the service
// does now, and public once the mock plays a service from before that. A
// request that states its privacy gets it from both, and the resource reads
// show what was made.
func TestCreateDefaultIsPrivateUnlessTheMockPlaysAnOlderService(t *testing.T) {
	const create = `{"type":"url","target_url":"https://example.com/data"%s}`
	for _, test := range []struct {
		name            string
		publicByDefault bool
		stated          string
		want            bool
	}{
		{name: "nothing stated", want: true},
		{name: "private stated", stated: `,"private":true`, want: true},
		{name: "public stated", stated: `,"private":false`, want: false},
		{name: "older service, nothing stated", publicByDefault: true, want: false},
		{name: "older service, private stated", publicByDefault: true, stated: `,"private":true`, want: true},
		{name: "older service, public stated", publicByDefault: true, stated: `,"private":false`, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := NewServer(t)
			if test.publicByDefault {
				srv.PlayPublicByDefault()
			}
			status, created := call(t, srv, http.MethodPost, "/v1/resources", fmt.Sprintf(create, test.stated))
			if status != http.StatusCreated || created.Data.Private == nil || *created.Data.Private != test.want {
				t.Fatalf("create = %d with private %v, want 201 with %t", status, created.Data.Private, test.want)
			}
			_, detail := call(t, srv, http.MethodGet, "/v1/resources/"+srv.Key.CRID, "")
			if detail.Data.Resource == nil || detail.Data.Resource.Private == nil || *detail.Data.Resource.Private != test.want {
				t.Fatalf("the resource read does not show the privacy that was made: %+v", detail.Data.Resource)
			}
		})
	}
}

// TestCreateThatFindsOtherAccessSettingsIsRefused pins the refusal for a
// create that finds the existing resource with other access settings: the
// privacy-mismatch code and the device-list code now, and before those codes
// the one invalid-input answer for both. A create that agrees with what
// exists is answered with it, and so is one that states no privacy, which
// the service gives the resource as it is.
func TestCreateThatFindsOtherAccessSettingsIsRefused(t *testing.T) {
	const create = `{"type":"url","target_url":"https://example.com/data"%s}`
	for _, test := range []struct {
		name            string
		publicByDefault bool
		existingPrivate bool
		existingKeys    []string
		stated          string
		wantCode        string
	}{
		{name: "same privacy", existingPrivate: true, stated: `,"private":true`},
		{name: "private asked, public exists", stated: `,"private":true`, wantCode: CodePrivacyMismatch},
		{name: "public asked, private exists", existingPrivate: true, stated: `,"private":false`, wantCode: CodePrivacyMismatch},
		// A request that states no privacy is given the resource as it is,
		// whichever privacy it has.
		{name: "nothing stated, public exists"},
		{name: "nothing stated, private exists", existingPrivate: true},
		{name: "another device list", existingPrivate: true, existingKeys: []string{"a"}, stated: `,"private":true,"allowed_device_keys":["b"]`, wantCode: CodeDeviceKeysMismatch},
		{name: "another device list, nothing else stated", existingPrivate: true, existingKeys: []string{"a"}, stated: `,"allowed_device_keys":["b"]`, wantCode: CodeDeviceKeysMismatch},
		{name: "the same device list", existingPrivate: true, existingKeys: []string{"a", "b"}, stated: `,"private":true,"allowed_device_keys":["b","a"]`},
		{name: "no device list stated, one exists", existingPrivate: true, existingKeys: []string{"a"}, stated: `,"private":true`},
		// Privacy is what the refusal names when both differ.
		{name: "another privacy and another device list", existingKeys: []string{"a"}, stated: `,"private":true,"allowed_device_keys":["b"]`, wantCode: CodePrivacyMismatch},
		{name: "older service, same privacy", publicByDefault: true, existingPrivate: true, stated: `,"private":true`},
		{name: "older service, nothing stated, public exists", publicByDefault: true},
		// An older service reads an absent privacy as public, its default.
		{name: "older service, nothing stated, private exists", publicByDefault: true, existingPrivate: true, wantCode: "invalid_input"},
		{name: "older service, private asked, public exists", publicByDefault: true, stated: `,"private":true`, wantCode: "invalid_input"},
		{name: "older service, another device list", publicByDefault: true, existingPrivate: true, existingKeys: []string{"a"}, stated: `,"private":true,"allowed_device_keys":["b"]`, wantCode: "invalid_input"},
		{name: "older service, the same device list", publicByDefault: true, existingPrivate: true, existingKeys: []string{"a", "b"}, stated: `,"private":true,"allowed_device_keys":["b","a"]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := NewServer(t)
			if test.publicByDefault {
				srv.PlayPublicByDefault()
			}
			srv.SetResourceAccess(test.existingPrivate, test.existingKeys...)
			srv.SetPublishFoundExisting(true)
			status, got := call(t, srv, http.MethodPost, "/v1/resources", fmt.Sprintf(create, test.stated))
			if test.wantCode == "" {
				if status != http.StatusCreated || got.Data.Private == nil || *got.Data.Private != test.existingPrivate {
					t.Fatalf("create = %d with private %v, want the existing resource (private %t)", status, got.Data.Private, test.existingPrivate)
				}
				return
			}
			if status != http.StatusBadRequest || got.Error.Code != test.wantCode {
				t.Fatalf("create = %d code %q, want 400 %q", status, got.Error.Code, test.wantCode)
			}
			if legacy := got.Error.Detail == LegacyAccessSettingsDetail; legacy != (test.wantCode == "invalid_input") {
				t.Fatalf("refusal detail %q does not match the service the mock plays", got.Error.Detail)
			}
		})
	}
}

// TestRequestsAreRecordedWithTheirBody pins that a recorded request carries
// the bytes that were sent, and that the handler still reads them.
func TestRequestsAreRecordedWithTheirBody(t *testing.T) {
	srv := NewServer(t)
	const body = `{"type":"url","target_url":"https://example.com/data","private":false}`
	status, created := call(t, srv, http.MethodPost, "/v1/resources", body)
	if status != http.StatusCreated || created.Data.Private == nil || *created.Data.Private {
		t.Fatalf("the handler did not read the body it was sent: %d %+v", status, created.Data)
	}
	requests := srv.Requests()
	if len(requests) != 1 || string(requests[0].Body) != body {
		t.Fatalf("recorded requests = %+v, want the one create with its body", requests)
	}
}

// TestDeviceGrantChangesFollowTheServiceContract pins the mock's grant
// update: a replacement sets the complete list; an edit adds and removes
// single keys as one change, leaves a key that is already present or absent
// alone, and is refused, with the list untouched, when one key is on both
// sides or the result would pass 256 keys. Every answer carries the complete
// resulting list.
func TestDeviceGrantChangesFollowTheServiceContract(t *testing.T) {
	full := make([]string, 0, maxAllowedDeviceKeys)
	for index := range maxAllowedDeviceKeys {
		full = append(full, fmt.Sprintf("key-%03d", index))
	}
	for _, test := range []struct {
		name     string
		existing []string
		body     string
		want     []string
		refused  bool
	}{
		{name: "replace", existing: []string{"a", "b"}, body: `{"allowed_device_keys":["c"]}`, want: []string{"c"}},
		{name: "replace with nothing", existing: []string{"a", "b"}, body: `{"allowed_device_keys":[]}`},
		{name: "add", existing: []string{"a"}, body: `{"allowed_device_keys_add":["b"]}`, want: []string{"a", "b"}},
		{name: "add what is there", existing: []string{"a"}, body: `{"allowed_device_keys_add":["a"]}`, want: []string{"a"}},
		{name: "remove", existing: []string{"a", "b"}, body: `{"allowed_device_keys_remove":["a"]}`, want: []string{"b"}},
		{name: "remove what is not there", existing: []string{"a"}, body: `{"allowed_device_keys_remove":["z"]}`, want: []string{"a"}},
		{name: "add and remove", existing: []string{"a", "b"}, body: `{"allowed_device_keys_add":["c"],"allowed_device_keys_remove":["a"]}`, want: []string{"b", "c"}},
		{name: "one key on both sides", existing: []string{"a"}, body: `{"allowed_device_keys_add":["b"],"allowed_device_keys_remove":["b"]}`, want: []string{"a"}, refused: true},
		{name: "replace and edit together", existing: []string{"a"}, body: `{"allowed_device_keys":["c"],"allowed_device_keys_add":["b"]}`, want: []string{"a"}, refused: true},
		{name: "one more than a full list", existing: full, body: `{"allowed_device_keys_add":["one-more"]}`, want: full, refused: true},
		{name: "a swap on a full list", existing: full, body: `{"allowed_device_keys_add":["one-more"],"allowed_device_keys_remove":["key-000"]}`, want: append(append([]string(nil), full[1:]...), "one-more")},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := NewServer(t)
			srv.SetResourceAccess(true, test.existing...)
			status, got := call(t, srv, http.MethodPatch, "/v1/resources/"+srv.Key.CRID, test.body)
			if test.refused {
				if status != http.StatusBadRequest || got.Error.Code != "invalid_input" {
					t.Fatalf("change = %d %q, want 400 invalid_input", status, got.Error.Code)
				}
			} else if status != http.StatusOK || !slices.Equal(got.Data.AllowedDeviceKeys, test.want) {
				t.Fatalf("change = %d with list %v, want 200 with %v", status, got.Data.AllowedDeviceKeys, test.want)
			}
			_, detail := call(t, srv, http.MethodGet, "/v1/resources/"+srv.Key.CRID, "")
			if detail.Data.Resource == nil || !slices.Equal(detail.Data.Resource.AllowedDeviceKeys, test.want) {
				t.Fatalf("the list after the change = %+v, want %v", detail.Data.Resource, test.want)
			}
		})
	}
}

// TestOlderServiceIgnoresSingleGrantEdits pins the mock of a service from
// before single grants: an edit is answered with a success status and the
// list as it was, and a replacement still works.
func TestOlderServiceIgnoresSingleGrantEdits(t *testing.T) {
	srv := NewServer(t)
	srv.SetResourceAccess(true, "a")
	srv.PlayNoSingleGrantEdits()
	status, got := call(t, srv, http.MethodPatch, "/v1/resources/"+srv.Key.CRID, `{"allowed_device_keys_add":["b"],"allowed_device_keys_remove":["a"]}`)
	if status != http.StatusOK || !slices.Equal(got.Data.AllowedDeviceKeys, []string{"a"}) {
		t.Fatalf("ignored edit = %d with list %v, want 200 with the list as it was", status, got.Data.AllowedDeviceKeys)
	}
	status, got = call(t, srv, http.MethodPatch, "/v1/resources/"+srv.Key.CRID, `{"allowed_device_keys":["b"]}`)
	if status != http.StatusOK || !slices.Equal(got.Data.AllowedDeviceKeys, []string{"b"}) {
		t.Fatalf("replacement = %d with list %v, want 200 with the new list", status, got.Data.AllowedDeviceKeys)
	}
}

// TestDeviceGrantChangeNeedsACredential pins that the mock refuses a grant
// change with no credential, as the service does, and changes nothing.
func TestDeviceGrantChangeNeedsACredential(t *testing.T) {
	srv := NewServer(t)
	srv.SetResourceAccess(true, "a")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, srv.URL+"/v1/resources/"+srv.Key.CRID, strings.NewReader(`{"allowed_device_keys":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("grant change without a credential = %d, want 401", resp.StatusCode)
	}
	_, detail := call(t, srv, http.MethodGet, "/v1/resources/"+srv.Key.CRID, "")
	if detail.Data.Resource == nil || !slices.Equal(detail.Data.Resource.AllowedDeviceKeys, []string{"a"}) {
		t.Fatalf("a refused change altered the list: %+v", detail.Data.Resource)
	}
}
