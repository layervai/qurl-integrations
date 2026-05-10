package internal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testInternalToken = "internal-test-token"

// adminFixture spins a fake qurl-service `/internal/v1/...` endpoint
// for a single request. Returns the captured request fields so tests
// can fence wire-shape assertions (auth header, content type, body).
type adminFixture struct {
	srv         *httptest.Server
	gotMethod   string
	gotPath     string
	gotAuth     string
	gotUA       string
	gotContent  string
	gotBody     []byte
	respondCode int
	respondBody string
}

func newAdminFixture(t *testing.T, status int, body string) *adminFixture {
	t.Helper()
	fx := &adminFixture{respondCode: status, respondBody: body}
	fx.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.gotMethod = r.Method
		fx.gotPath = r.URL.RequestURI()
		fx.gotAuth = r.Header.Get("Authorization")
		fx.gotUA = r.Header.Get("User-Agent")
		fx.gotContent = r.Header.Get("Content-Type")
		fx.gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fx.respondCode)
		_, _ = w.Write([]byte(fx.respondBody))
	}))
	t.Cleanup(fx.srv.Close)
	return fx
}

func (fx *adminFixture) client() *AdminClient {
	return NewAdminClient(fx.srv.URL, testInternalToken)
}

// TestAdminClient_AuthHeaderShape fences the auth wire shape: every
// request must carry `Authorization: Bearer <internal-token>` so the
// service can route the call to the internal-only path. A regression
// that dropped the header (or used the customer-key shape) would let
// the slack bot's traffic land on the customer auth path with the
// wrong key and 401 silently.
func TestAdminClient_AuthHeaderShape(t *testing.T) {
	t.Parallel()
	fx := newAdminFixture(t, http.StatusOK, `{"data":{"is_admin":true,"owner_id":"u_1"}}`)
	ac := fx.client()
	_, _, err := ac.CheckAdmin(context.Background(), "T1", "U1")
	if err != nil {
		t.Fatalf("CheckAdmin: %v", err)
	}
	if fx.gotAuth != "Bearer "+testInternalToken {
		t.Errorf("auth = %q, want Bearer %s", fx.gotAuth, testInternalToken)
	}
	if !strings.HasPrefix(fx.gotUA, "qurl-slack-admin/") {
		t.Errorf("user-agent = %q, want qurl-slack-admin/* prefix", fx.gotUA)
	}
}

// TestAdminClient_CheckAdmin_HappyPath fences the GET shape (no body,
// query string carries the params) and the response unwrap.
func TestAdminClient_CheckAdmin_HappyPath(t *testing.T) {
	t.Parallel()
	fx := newAdminFixture(t, http.StatusOK, `{"data":{"is_admin":true,"owner_id":"u_owner"}}`)
	ac := fx.client()
	isAdmin, owner, err := ac.CheckAdmin(context.Background(), "T1", "U1")
	if err != nil {
		t.Fatalf("CheckAdmin: %v", err)
	}
	if !isAdmin || owner != "u_owner" {
		t.Errorf("isAdmin=%v owner=%q, want true / u_owner", isAdmin, owner)
	}
	if fx.gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", fx.gotMethod)
	}
	if !strings.Contains(fx.gotPath, "team_id=T1") || !strings.Contains(fx.gotPath, "user_id=U1") {
		t.Errorf("path = %q, want team_id=T1 and user_id=U1 in query", fx.gotPath)
	}
}

// TestAdminClient_ResolvePolicy_HappyPath fences the POST shape (JSON
// body with the expected fields) and the unwrap on a positive answer.
func TestAdminClient_ResolvePolicy_HappyPath(t *testing.T) {
	t.Parallel()
	fx := newAdminFixture(t, http.StatusOK, `{"data":{"allowed":true}}`)
	ac := fx.client()
	allowed, err := ac.ResolvePolicy(context.Background(), "T1", "C1", "r_1")
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	if !allowed {
		t.Error("allowed = false, want true")
	}
	if fx.gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", fx.gotMethod)
	}
	if fx.gotContent != "application/json" {
		t.Errorf("content-type = %q, want application/json", fx.gotContent)
	}
	var got map[string]string
	if err := json.Unmarshal(fx.gotBody, &got); err != nil {
		t.Fatalf("body: %v", err)
	}
	if got["team_id"] != "T1" || got["channel_id"] != "C1" || got["resource_id"] != "r_1" {
		t.Errorf("body = %v, missing required fields", got)
	}
}

// TestAdminClient_RedeemBootstrap fences the redeem call. The code
// MUST go in the body, not the URL — putting it in a query string
// would land in CloudWatch / Vector access logs and defeat
// Blocker #3 (no plaintext bootstrap codes anywhere user-visible).
func TestAdminClient_RedeemBootstrap(t *testing.T) {
	t.Parallel()
	fx := newAdminFixture(t, http.StatusOK, `{"data":{"team_id":"T1","owner_id":"u_1","created_at":"2026-04-20T12:00:00Z"}}`)
	ac := fx.client()
	got, err := ac.RedeemBootstrap(context.Background(), "boot-code-12345", "T1", "U1")
	if err != nil {
		t.Fatalf("RedeemBootstrap: %v", err)
	}
	if got.OwnerID != "u_1" {
		t.Errorf("owner_id = %q, want u_1", got.OwnerID)
	}
	// Code MUST NOT appear in the URL path or query string.
	if strings.Contains(fx.gotPath, "boot-code") {
		t.Errorf("bootstrap code leaked into URL path %q", fx.gotPath)
	}
	if !strings.Contains(string(fx.gotBody), "boot-code-12345") {
		t.Errorf("body = %s, expected to contain code", fx.gotBody)
	}
}

// TestAdminClient_PolicyMutations fences the allow/disallow shape —
// both should hit the right path and POST the same field set.
func TestAdminClient_PolicyMutations(t *testing.T) {
	t.Parallel()
	for _, op := range []struct {
		name string
		path string
		fn   func(ac *AdminClient) error
	}{
		{"allow", "/internal/v1/admin/policy/allow", func(ac *AdminClient) error {
			return ac.AllowResource(context.Background(), "T1", "C1", "r_1")
		}},
		{"disallow", "/internal/v1/admin/policy/disallow", func(ac *AdminClient) error {
			return ac.DisallowResource(context.Background(), "T1", "C1", "r_1")
		}},
	} {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			fx := newAdminFixture(t, http.StatusOK, `{"data":{}}`)
			ac := fx.client()
			if err := op.fn(ac); err != nil {
				t.Fatalf("%s: %v", op.name, err)
			}
			if fx.gotPath != op.path {
				t.Errorf("path = %q, want %q", fx.gotPath, op.path)
			}
			if fx.gotMethod != http.MethodPost {
				t.Errorf("method = %q, want POST", fx.gotMethod)
			}
		})
	}
}

// TestAdminClient_ListPolicies_Pagination fences the cursor + limit
// query-string round-trip.
func TestAdminClient_ListPolicies_Pagination(t *testing.T) {
	t.Parallel()
	fx := newAdminFixture(t, http.StatusOK, `{"data":{"entries":[{"channel_id":"C1","alias":"prod-db"}],"next_cursor":"abc","has_more":true}}`)
	ac := fx.client()
	got, err := ac.ListPolicies(context.Background(), "T1", "prev", 50)
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	if !got.HasMore || got.NextCursor != "abc" || len(got.Entries) != 1 {
		t.Errorf("got = %+v, missing pagination metadata", got)
	}
	if !strings.Contains(fx.gotPath, "cursor=prev") || !strings.Contains(fx.gotPath, "limit=50") {
		t.Errorf("path = %q, want cursor=prev and limit=50", fx.gotPath)
	}
}

// TestAdminClient_CheckRateLimit_RetryAfter fences the retry-after
// surface. The duration helper must accept either seconds or
// milliseconds — the qurl-service team has not committed to one yet
// and the wire response shape on the rate-limit endpoint is the
// least settled part of Phase 3b.
func TestAdminClient_CheckRateLimit_RetryAfter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		respondJSON string
		wantRetryMs int64
	}{
		{"seconds field", `{"data":{"allowed":false,"retry_after_seconds":3}}`, 3000},
		{"milliseconds field", `{"data":{"allowed":false,"retry_after_ms":750}}`, 750},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newAdminFixture(t, http.StatusOK, tc.respondJSON)
			ac := fx.client()
			allowed, retry, err := ac.CheckRateLimit(context.Background(), "U1", "T1")
			if err != nil {
				t.Fatalf("CheckRateLimit: %v", err)
			}
			if allowed {
				t.Error("allowed = true, want false")
			}
			if retry.Milliseconds() != tc.wantRetryMs {
				t.Errorf("retry = %v, want %d ms", retry, tc.wantRetryMs)
			}
		})
	}
}

// TestAdminClient_NonOK_ParsesAdminError fences the error envelope
// path: a 403 response with a body must surface as an [*AdminError]
// with the right status code.
func TestAdminClient_NonOK_ParsesAdminError(t *testing.T) {
	t.Parallel()
	fx := newAdminFixture(t, http.StatusForbidden, `{"error":{"title":"Forbidden","detail":"not admin","code":"not_admin","status":403}}`)
	ac := fx.client()
	_, _, err := ac.CheckAdmin(context.Background(), "T1", "U1")
	if err == nil {
		t.Fatal("CheckAdmin: error = nil, want non-nil")
	}
	var ae *AdminError
	if !errors.As(err, &ae) {
		t.Fatalf("error %T does not unwrap to *AdminError", err)
	}
	if ae.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", ae.StatusCode)
	}
	if ae.Code != "not_admin" {
		t.Errorf("code = %q, want not_admin", ae.Code)
	}
}

// TestAdminClient_NoBaseURL guards the misconfiguration path —
// constructing the client without a base URL should fail every
// method with a clear error rather than calling out to "".
func TestAdminClient_NoBaseURL(t *testing.T) {
	t.Parallel()
	ac := NewAdminClient("", testInternalToken)
	_, _, err := ac.CheckAdmin(context.Background(), "T1", "U1")
	if err == nil {
		t.Error("CheckAdmin with empty base URL: error = nil, want non-nil")
	}
}

// TestAdminClient_NoAuthToken guards the second misconfig: missing
// internal token. Same shape as the no-base-URL case.
func TestAdminClient_NoAuthToken(t *testing.T) {
	t.Parallel()
	ac := NewAdminClient("http://localhost", "")
	_, _, err := ac.CheckAdmin(context.Background(), "T1", "U1")
	if err == nil {
		t.Error("CheckAdmin with empty token: error = nil, want non-nil")
	}
}

// TestAdminClient_WithHTTPClientOption fences the test injection
// hook — without it, every test in this file would have to spin up
// a real httptest server. Verify the option actually swaps the
// client.
func TestAdminClient_WithHTTPClientOption(t *testing.T) {
	t.Parallel()
	called := false
	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"data":{"is_admin":false,"owner_id":""}}`)),
			Header:     make(http.Header),
		}, nil
	})
	ac := NewAdminClient("http://example", testInternalToken, WithAdminHTTPClient(&http.Client{Transport: rt}))
	_, _, err := ac.CheckAdmin(context.Background(), "T1", "U1")
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("custom round-tripper not invoked")
	}
}

// TestAdminClient_WithAdminUserAgent fences the version-plumbing
// option. The product half is fixed; the version half is whatever
// the caller passes. Empty version falls back to the default so
// callers don't have to special-case the build-info-missing path.
func TestAdminClient_WithAdminUserAgent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		version  string
		wantUA   string
		wantStub bool
	}{
		{name: "explicit version", version: "1.2.3-deadbeef", wantUA: "qurl-slack-admin/1.2.3-deadbeef"},
		{name: "empty falls back to default", version: "", wantUA: "qurl-slack-admin/dev", wantStub: true},
		{name: "vcs revision shape", version: "vcs-7f3c9a1", wantUA: "qurl-slack-admin/vcs-7f3c9a1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newAdminFixture(t, http.StatusOK, `{"data":{"is_admin":true,"owner_id":"u_1"}}`)
			ac := NewAdminClient(fx.srv.URL, testInternalToken, WithAdminUserAgent(tc.version))
			if _, _, err := ac.CheckAdmin(context.Background(), "T1", "U1"); err != nil {
				t.Fatalf("CheckAdmin: %v", err)
			}
			if fx.gotUA != tc.wantUA {
				t.Errorf("user-agent = %q, want %q", fx.gotUA, tc.wantUA)
			}
		})
	}
}

// TestAdminClient_EmptyDataSurfacesSentinel fences the empty-envelope
// guard. A buggy server that returns `{"data": null}` or `{}` on a
// 2xx must surface [ErrEmptyAdminResponse] rather than silently
// leaving the caller's `out` at zero values — which would otherwise
// be indistinguishable from a successful response carrying defaults.
func TestAdminClient_EmptyDataSurfacesSentinel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"data is null", `{"data":null}`},
		{"data is missing", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newAdminFixture(t, http.StatusOK, tc.body)
			ac := fx.client()
			_, _, err := ac.CheckAdmin(context.Background(), "T1", "U1")
			if err == nil {
				t.Fatal("CheckAdmin: error = nil, want ErrEmptyAdminResponse")
			}
			if !errors.Is(err, ErrEmptyAdminResponse) {
				t.Errorf("error = %v, want ErrEmptyAdminResponse", err)
			}
		})
	}
}

// TestAdminClient_VoidEndpointAcceptsEmptyData fences the inverse:
// methods that pass `out=nil` (allow/disallow) must NOT surface the
// empty-data sentinel — they don't expect any payload.
func TestAdminClient_VoidEndpointAcceptsEmptyData(t *testing.T) {
	t.Parallel()
	fx := newAdminFixture(t, http.StatusOK, `{"data":null}`)
	ac := fx.client()
	if err := ac.AllowResource(context.Background(), "T1", "C1", "r_1"); err != nil {
		t.Errorf("AllowResource with empty data should succeed (out=nil), got: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
