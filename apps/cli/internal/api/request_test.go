package qurlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestRegisteredRequestPreservesHTTPErrorAndSafeHeaders(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/account/link", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "01234567-89ab-cdef-0123-456789abcdef" {
			t.Error("idempotency key lost")
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["account_token"] != "secret-account-token" {
			t.Error("request body changed")
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Authorization", "must-not-escape")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict"}}`))
	})
	result, err := Request(context.Background(), newRegisteredTestClient(t, srv), http.MethodPost, "/v1/account/link", json.RawMessage(`{"account_token":"secret-account-token"}`), "01234567-89ab-cdef-0123-456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != 409 || len(result.Headers) != 2 || result.Headers["retry-after"] != "7" || result.Headers["content-type"] != "application/problem+json" || string(result.Body) != `{"error":{"code":"conflict"}}` {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(srv.Requests()) != 1 {
		t.Fatal("request replayed")
	}
}

func TestRegisteredRequestRejectsAuthorityAndDisallowedRoutes(t *testing.T) {
	srv := apitest.NewServer(t)
	registered := newRegisteredTestClient(t, srv)
	// TODO(upstream-contract): keep these denied routes aligned with the
	// reviewed qurl-go registered-device transport before updating the SDK.
	for _, path := range []string{"https://evil.test/v1/me", "//evil.test/v1/me", "/v1/me#fragment", "/v1/%6de", "/v1/../v1/me", "/v1/me?x=1", "/v1/quota", "/v1/resources/id/sessions/session_id"} {
		if _, err := Request(context.Background(), registered, http.MethodGet, path, nil, ""); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("rejected request reached network")
	}
	account, err := New(&Config{BaseURL: srv.URL, APIKey: "account-key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Request(context.Background(), account, http.MethodGet, "/v1/me", nil, ""); err == nil {
		t.Fatal("accepted account authority")
	}
	if _, err := Request(context.Background(), &registeredClient{Client: &client{}}, http.MethodGet, "/v1/me", nil, ""); err == nil || !strings.Contains(err.Error(), "registered transport is unavailable") {
		t.Fatalf("nil registered transport error = %v", err)
	}
}

// TestValidateRequestTargetEnforcesMethodAndQueryLocally pins the policy
// independently of the SDK allowlist.
func TestValidateRequestTargetEnforcesMethodAndQueryLocally(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/resources?limit=1"},
		{http.MethodGet, "/v1/resources/id/qurls?limit=100&cursor=next"},
		{http.MethodPost, "/v1/resources"},
		{http.MethodDelete, "/v1/resources/id/sessions"},
	} {
		if err := ValidateRequestTarget(tc.method, tc.path); err != nil {
			t.Errorf("%s %s rejected: %v", tc.method, tc.path, err)
		}
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodHead, "/v1/me"},
		{http.MethodOptions, "/v1/me"},
		{"get", "/v1/me"},
		{"", "/v1/me"},
		{http.MethodGet, "/v1/me?x=1"},
		{http.MethodGet, "/v1/me?"},
		{http.MethodGet, "/v1/resources/id/sessions?limit=1"},
		{http.MethodGet, "/v1/resources//qurls?limit=1"},
		{http.MethodGet, "/v1/resources/a/b/qurls?limit=1"},
		{http.MethodGet, "/v1/resources/id/qurls/q_token?x=1"},
		{http.MethodPost, "/v1/resources?limit=1"},
		{http.MethodPatch, "/v1/resources/id/qurls?x=1"},
	} {
		if err := ValidateRequestTarget(tc.method, tc.path); !errors.Is(err, qurl.ErrRegisteredAgentResourceRequestDenied) {
			t.Errorf("%q %s error = %v", tc.method, tc.path, err)
		}
	}
	srv := apitest.NewServer(t)
	if _, err := Request(context.Background(), newRegisteredTestClient(t, srv), "TRACE", "/v1/me", nil, ""); err == nil || len(srv.Requests()) != 0 {
		t.Fatalf("library accepted unsupported method: %v", err)
	}
}

func TestRegisteredRequestEnforcesBodyCaps(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newRegisteredTestClient(t, srv)
	oversized := json.RawMessage(`"` + strings.Repeat("a", MaxRequestBody-1) + `"`)
	if _, err := Request(context.Background(), client, http.MethodPost, "/v1/resources", oversized, ""); err == nil || len(srv.Requests()) != 0 {
		t.Fatalf("library accepted an oversized request body: %v", err)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if _, err := Request(context.Background(), client, method, "/v1/resources/id/sessions", json.RawMessage(`{}`), ""); err == nil || len(srv.Requests()) != 0 {
			t.Fatalf("library accepted a %s body: %v", method, err)
		}
	}
	srv.Script(http.MethodGet, "/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`"` + strings.Repeat("a", maxResponseBody) + `"`))
	})
	if _, err := Request(context.Background(), client, http.MethodGet, "/v1/me", nil, ""); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestRegisteredRequestBodyAndCancellation(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newRegisteredTestClient(t, srv)
	for _, tc := range []struct {
		status     int
		body, want string
	}{{204, "", "null"}, {502, "upstream unavailable", `"upstream unavailable"`}} {
		srv.Script(http.MethodGet, "/v1/me", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		result, err := Request(context.Background(), client, http.MethodGet, "/v1/me", nil, "")
		if err != nil || result.Status != tc.status || string(result.Body) != tc.want || result.Headers == nil || (tc.status == 204 && len(result.Headers) != 0) {
			t.Fatalf("response = %+v, %v", result, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Request(ctx, client, http.MethodGet, "/v1/me", nil, ""); err == nil {
		t.Fatal("canceled request succeeded")
	}
	if len(srv.Requests()) != 2 {
		t.Fatal("canceled request reached network")
	}
}

func TestRegisteredRequestResourceManagement(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newRegisteredTestClient(t, srv)
	routes := []struct {
		method, path string
		body         json.RawMessage
	}{
		{http.MethodGet, "/v1/resources/id/qurls?limit=100&cursor=next", nil},
		{http.MethodPatch, "/v1/resources/id/qurls/q_token", json.RawMessage(`{"label":"a<b>&c"}`)},
		{http.MethodPut, "/v1/resources/id/sharing", json.RawMessage(`{"enabled":true}`)},
		{http.MethodDelete, "/v1/resources/id/qurls/q_token", nil},
		{http.MethodGet, "/v1/resources/id/sessions", nil},
		{http.MethodDelete, "/v1/resources/id/sessions", nil},
		{http.MethodDelete, "/v1/resources/id/sessions/s_session", nil},
	}
	for _, route := range routes {
		srv.Script(route.method, strings.SplitN(route.path, "?", 2)[0], func(w http.ResponseWriter, r *http.Request) {
			if got, _ := io.ReadAll(r.Body); !bytes.Equal(got, route.body) {
				t.Errorf("%s %s body = %q, want %q", route.method, route.path, got, route.body)
			}
			w.WriteHeader(http.StatusNoContent)
		})
		result, err := Request(context.Background(), client, route.method, route.path, route.body, "")
		if err != nil || result.Status != http.StatusNoContent {
			t.Fatalf("%s %s: response=%+v error=%v", route.method, route.path, result, err)
		}
	}
	if len(srv.Requests()) != len(routes) {
		t.Fatal("management requests were dropped or replayed")
	}
}
