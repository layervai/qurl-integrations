package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	testDescription   = "updated"
	testAlias         = "dev-dashboard"
	testAliasAlt      = "prod-grafana"
	testResourceID    = "r_existing01"
	testResourceIDAlt = "r_dev_dash01"
	testTargetURL     = "https://internal.example.com"
	testTunnelSlug    = "prod-dashboard"
)

// testClient creates a client with retries disabled for fast unit tests.
func testClient(url, key string) *Client {
	return New(url, key, WithRetry(0))
}

// withDelaysForTest collapses retry/backoff delays to 1ns. Reaches into
// unexported fields, which only works because tests are same-package —
// preferable to a public knob since the values aren't meaningful outside
// of test speed.
func withDelaysForTest() Option {
	return func(c *Client) {
		c.baseDelay = 1
		c.maxDelay = 1
	}
}

// apiEnvelope wraps data in the API response envelope.
func apiEnvelope(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"data": data,
		"meta": map[string]string{"request_id": "req_test"},
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/qurls" {
			t.Errorf("expected /v1/qurls, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %s", r.Header.Get("Authorization"))
		}

		var input CreateInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if input.TargetURL != "https://example.com" {
			t.Errorf("expected target_url https://example.com, got %s", input.TargetURL)
		}

		apiEnvelope(t, w, map[string]any{
			"resource_id": "r_abc123test",
			"qurl_link":   "https://qurl.link/at_abc123",
			"qurl_site":   "https://r_abc123test.qurl.site",
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.Create(context.Background(), CreateInput{TargetURL: "https://example.com"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got.ResourceID != "r_abc123test" {
		t.Errorf("got ResourceID %q, want %q", got.ResourceID, "r_abc123test")
	}
	if got.QURLLink != "https://qurl.link/at_abc123" {
		t.Errorf("got QURLLink %q, want %q", got.QURLLink, "https://qurl.link/at_abc123")
	}
}

func TestUserAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		if ua != "qurl-cli/1.0.0" {
			t.Errorf("expected User-Agent 'qurl-cli/1.0.0', got %q", ua)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": "r_test",
			"target_url":  "https://example.com",
			"status":      "active",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key", WithRetry(0), WithUserAgent("qurl-cli/1.0.0"))
	_, err := c.Get(context.Background(), "r_test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
}

func TestGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/qurls/r_abc123test" {
			t.Errorf("expected /v1/qurls/r_abc123test, got %s", r.URL.Path)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id":  "r_abc123test",
			"target_url":   "https://example.com",
			"status":       "active",
			"one_time_use": false,
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.Get(context.Background(), "r_abc123test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ResourceID != "r_abc123test" {
		t.Errorf("got ResourceID %q, want %q", got.ResourceID, "r_abc123test")
	}
	if got.Status != "active" {
		t.Errorf("got Status %q, want %q", got.Status, "active")
	}
}

func TestList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "5" {
			t.Errorf("expected limit=5, got %s", r.URL.Query().Get("limit"))
		}
		if r.URL.Query().Get("status") != "active" {
			t.Errorf("expected status=active, got %s", r.URL.Query().Get("status"))
		}

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"data": []map[string]any{
				{"resource_id": "r_1", "target_url": "https://a.com", "status": "active"},
				{"resource_id": "r_2", "target_url": "https://b.com", "status": "active"},
			},
			"meta": map[string]any{
				"request_id":  "req_test",
				"page_size":   2,
				"has_more":    true,
				"next_cursor": "cursor_abc",
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.List(context.Background(), ListInput{Limit: 5, Status: "active"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got.QURLs) != 2 {
		t.Fatalf("got %d QURLs, want 2", len(got.QURLs))
	}
	if got.NextCursor != "cursor_abc" {
		t.Errorf("got NextCursor %q, want %q", got.NextCursor, "cursor_abc")
	}
	if !got.HasMore {
		t.Error("expected HasMore=true")
	}
}

func TestListCursorEscaping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		if cursor != "a=b&c=d" {
			t.Errorf("expected cursor 'a=b&c=d', got %q", cursor)
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"data": []map[string]any{},
			"meta": map[string]any{"request_id": "req_test"},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	_, err := c.List(context.Background(), ListInput{Limit: 10, Cursor: "a=b&c=d"})
	if err != nil {
		t.Fatalf("List with special cursor: %v", err)
	}
}

func TestResolve(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/resolve" {
			t.Errorf("expected /v1/resolve, got %s", r.URL.Path)
		}

		var input ResolveInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if input.AccessToken != "at_testtoken123" {
			t.Errorf("expected at_testtoken123, got %s", input.AccessToken)
		}

		apiEnvelope(t, w, map[string]any{
			"target_url":  "https://api.example.com/data",
			"resource_id": "r_abc123test",
			"access_grant": map[string]any{
				"expires_in": 305,
				"granted_at": "2026-03-09T15:30:00Z",
				"src_ip":     "203.0.113.42",
			},
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.Resolve(context.Background(), ResolveInput{AccessToken: "at_testtoken123"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got.TargetURL != "https://api.example.com/data" {
		t.Errorf("got TargetURL %q", got.TargetURL)
	}
	if got.AccessGrant == nil {
		t.Fatal("expected AccessGrant, got nil")
	}
	if got.AccessGrant.ExpiresIn != 305 {
		t.Errorf("got ExpiresIn %d, want 305", got.AccessGrant.ExpiresIn)
	}
}

func TestMintLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/qurls/r_abc123test/mint_link" {
			t.Errorf("expected mint_link path, got %s", r.URL.Path)
		}
		apiEnvelope(t, w, map[string]any{
			"qurl_link": "https://qurl.link/at_newtoken",
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.MintLink(context.Background(), "r_abc123test")
	if err != nil {
		t.Fatalf("MintLink: %v", err)
	}
	if got.QURLLink != "https://qurl.link/at_newtoken" {
		t.Errorf("got QURLLink %q", got.QURLLink)
	}
}

func TestGetQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiEnvelope(t, w, map[string]any{
			"plan":         "growth",
			"period_start": "2026-03-01T00:00:00Z",
			"period_end":   "2026-03-31T23:59:59Z",
			"usage": map[string]any{
				"active_qurls":  45,
				"qurls_created": 150,
			},
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.GetQuota(context.Background())
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if got.Plan != "growth" {
		t.Errorf("got Plan %q, want %q", got.Plan, "growth")
	}
	if got.Usage == nil || got.Usage.ActiveQURLs != 45 {
		t.Errorf("got Usage.ActiveQURLs %v", got.Usage)
	}
}

func TestUpdate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		if r.URL.Path != "/v1/qurls/r_abc123test" {
			t.Errorf("expected /v1/qurls/r_abc123test, got %s", r.URL.Path)
		}

		var input UpdateInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if input.Description == nil || *input.Description != testDescription {
			t.Errorf("expected description 'updated', got %v", input.Description)
		}

		apiEnvelope(t, w, map[string]any{
			"resource_id": "r_abc123test",
			"target_url":  "https://example.com",
			"status":      "active",
			"description": testDescription,
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	desc := testDescription
	got, err := c.Update(context.Background(), "r_abc123test", UpdateInput{Description: &desc})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Description != testDescription {
		t.Errorf("got Description %q, want %q", got.Description, testDescription)
	}
}

func TestAPIErrorRFC7807(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		resp := map[string]any{
			"error": map[string]any{
				"type":   "https://api.qurl.link/problems/invalid_request",
				"title":  "Bad Request",
				"status": 400,
				"detail": "The target_url field must be a valid HTTPS URL",
				"code":   "invalid_request",
				"invalid_fields": map[string]string{
					"target_url": "must be a valid HTTPS URL",
				},
			},
			"meta": map[string]string{"request_id": "req_abc"},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	_, err := c.Get(context.Background(), "r_abc123test")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("got status %d, want 400", apiErr.StatusCode)
	}
	if apiErr.Code != "invalid_request" {
		t.Errorf("got code %q, want %q", apiErr.Code, "invalid_request")
	}
	if apiErr.Detail != "The target_url field must be a valid HTTPS URL" {
		t.Errorf("got detail %q", apiErr.Detail)
	}
	if apiErr.RequestID != "req_abc" {
		t.Errorf("got request_id %q, want %q", apiErr.RequestID, "req_abc")
	}
	if apiErr.InvalidFields["target_url"] != "must be a valid HTTPS URL" {
		t.Errorf("got invalid_fields %v", apiErr.InvalidFields)
	}
}

func TestAPIErrorRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		resp := map[string]any{
			"error": map[string]any{
				"title":  "Too Many Requests",
				"status": 429,
				"detail": "Rate limit exceeded",
				"code":   "rate_limited",
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	_, err := c.Get(context.Background(), "r_abc123test")

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.RetryAfter != 30 {
		t.Errorf("got RetryAfter %d, want 30", apiErr.RetryAfter)
	}
}

func TestRetryOn503(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": "r_test",
			"target_url":  "https://example.com",
			"status":      "active",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key",
		WithRetry(3),
		// Use very short delays for test speed.
		withDelaysForTest(),
	)
	got, err := c.Get(context.Background(), "r_test")
	if err != nil {
		t.Fatalf("Get after retries: %v", err)
	}
	if got.ResourceID != "r_test" {
		t.Errorf("got ResourceID %q, want %q", got.ResourceID, "r_test")
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestRetryExhausted(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key",
		WithRetry(2),
		withDelaysForTest(),
	)
	_, err := c.Get(context.Background(), "r_test")
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 502 {
		t.Errorf("got status %d, want 502", apiErr.StatusCode)
	}
	if attempts.Load() != 3 { // 1 initial + 2 retries
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key",
		WithRetry(3),
		withDelaysForTest(),
	)
	_, err := c.Get(context.Background(), "r_test")
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts.Load() != 1 {
		t.Errorf("expected 1 attempt (no retry on 404), got %d", attempts.Load())
	}
}

func TestDelete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	if err := c.Delete(context.Background(), "r_abc123test"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestCreateIdempotencyKeyHeaderSet(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(HeaderIdempotencyKey)
		apiEnvelope(t, w, map[string]any{
			"resource_id": "r_idem_test",
			"qurl_link":   "https://qurl.link/idem",
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	_, err := c.Create(context.Background(), CreateInput{
		TargetURL:      "https://example.com",
		IdempotencyKey: "slack:T123:trig_456",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if gotHeader != "slack:T123:trig_456" {
		t.Errorf("Idempotency-Key header: got %q, want %q", gotHeader, "slack:T123:trig_456")
	}
}

func TestCreateIdempotencyKeyAbsentWhenEmpty(t *testing.T) {
	var headerSeen, headerValue string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Distinguish absent vs empty-string: net/http treats Get on a
		// missing header as "" — so we check the canonical header map
		// directly to know if it was sent at all.
		if vs, ok := r.Header[http.CanonicalHeaderKey(HeaderIdempotencyKey)]; ok {
			headerSeen = "present"
			if len(vs) > 0 {
				headerValue = vs[0]
			}
		}
		apiEnvelope(t, w, map[string]any{"resource_id": "r_no_idem"})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	_, err := c.Create(context.Background(), CreateInput{TargetURL: "https://example.com"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if headerSeen != "" {
		t.Errorf("Idempotency-Key header should be absent when IdempotencyKey is empty; got value %q", headerValue)
	}
}

func TestCreateBodyByteIdenticalWithAndWithoutIdempotencyKey(t *testing.T) {
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		bodies = append(bodies, b)
		apiEnvelope(t, w, map[string]any{"resource_id": "r_body_test"})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	if _, err := c.Create(context.Background(), CreateInput{TargetURL: "https://example.com"}); err != nil {
		t.Fatalf("Create #1: %v", err)
	}
	if _, err := c.Create(context.Background(), CreateInput{
		TargetURL:      "https://example.com",
		IdempotencyKey: "slack:T999:trig_xyz",
	}); err != nil {
		t.Fatalf("Create #2: %v", err)
	}

	if len(bodies) != 2 {
		t.Fatalf("expected 2 bodies, got %d", len(bodies))
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Errorf("request bodies should be byte-identical regardless of IdempotencyKey (json:\"-\" tag).\n#1: %s\n#2: %s", bodies[0], bodies[1])
	}
}

func TestCreateIdempotencyKeyPreservedAcrossRetry(t *testing.T) {
	var attempts atomic.Int32
	// Synchronize the slice append explicitly even though the client
	// serializes attempts (next httpClient.Do happens-after the previous
	// response is fully read). The mutex preserves correctness if any
	// future refactor breaks that serialization invariant — and keeps
	// `go test -race` clean on platforms where the implicit
	// happens-before is harder for the race detector to see.
	var mu sync.Mutex
	var sawKeyOnAttempts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		mu.Lock()
		sawKeyOnAttempts = append(sawKeyOnAttempts, r.Header.Get(HeaderIdempotencyKey))
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		apiEnvelope(t, w, map[string]any{"resource_id": "r_retry_idem"})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key",
		WithRetry(3),
		withDelaysForTest(),
	)
	_, err := c.Create(context.Background(), CreateInput{
		TargetURL:      "https://example.com",
		IdempotencyKey: "slack:T1:trig_retry",
	})
	if err != nil {
		t.Fatalf("Create after retries: %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	for i, k := range sawKeyOnAttempts {
		if k != "slack:T1:trig_retry" {
			t.Errorf("attempt %d: Idempotency-Key got %q, want %q", i+1, k, "slack:T1:trig_retry")
		}
	}
}

func TestCreateIdempotencyKeyTooLong(t *testing.T) {
	// Server should never be hit — fail-fast is the contract.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	tooLong := strings.Repeat("a", MaxIdempotencyKeyLength+1)
	_, err := c.Create(context.Background(), CreateInput{
		TargetURL:      "https://example.com",
		IdempotencyKey: tooLong,
	})
	if !errors.Is(err, ErrIdempotencyKeyTooLong) {
		t.Fatalf("expected ErrIdempotencyKeyTooLong, got %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("server should not be hit on too-long key; got %d hits", hits.Load())
	}
}

func TestCreateIdempotencyKeyAtMaxBoundary(t *testing.T) {
	// Boundary case split out from the over-cap test so a future
	// regression on the boundary points at this test name directly.
	atMax := strings.Repeat("b", MaxIdempotencyKeyLength)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(HeaderIdempotencyKey); got != atMax {
			t.Errorf("header got len=%d, want %q (len=%d)", len(got), atMax, len(atMax))
		}
		apiEnvelope(t, w, map[string]any{"resource_id": "r_boundary"})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	if _, err := c.Create(context.Background(), CreateInput{
		TargetURL:      "https://example.com",
		IdempotencyKey: atMax,
	}); err != nil {
		t.Errorf("at-max boundary (%d bytes exactly): unexpected error %v", MaxIdempotencyKeyLength, err)
	}
}

func TestValidateIdempotencyKey(t *testing.T) {
	// Direct unit test on the validator — protects the contract from a
	// future refactor (e.g. someone adds a min-length check that breaks
	// the empty-key-is-valid path) without going through the full
	// httptest+Create round-trip every assertion.
	cases := []struct {
		name string
		key  string
		want error
	}{
		{"empty (no header)", "", nil},
		{"single ASCII char", "a", nil},
		{"sha256-hex (Slack canonical)", strings.Repeat("0123456789abcdef", 4), nil}, // 64 chars
		{"max-length boundary", strings.Repeat("x", MaxIdempotencyKeyLength), nil},
		{"internal space accepted", "key with spaces", nil},
		{"leading space rejected (would be trimmed by OWS)", " abc", ErrIdempotencyKeyInvalid},
		{"trailing space rejected (would be trimmed by OWS)", "abc ", ErrIdempotencyKeyInvalid},
		{"leading tab rejected (would be trimmed by OWS)", "\tabc", ErrIdempotencyKeyInvalid},
		{"trailing tab rejected (would be trimmed by OWS)", "abc\t", ErrIdempotencyKeyInvalid},
		{"one byte over cap", strings.Repeat("x", MaxIdempotencyKeyLength+1), ErrIdempotencyKeyTooLong},
		{"CR injection", "abc\rdef", ErrIdempotencyKeyInvalid},
		{"LF injection", "abc\ndef", ErrIdempotencyKeyInvalid},
		{"NUL byte", "abc\x00def", ErrIdempotencyKeyInvalid},
		{"non-ASCII", "abc\xc3\xa9def", ErrIdempotencyKeyInvalid}, // é
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateIdempotencyKey(tc.key)
			if !errors.Is(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCreateIdempotencyKeyRejectsInvalidBytes(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := testClient(srv.URL, "test-key")

	cases := []struct {
		name string
		key  string
	}{
		{"CR injection", "slack:T1:abc\rfoo"},
		{"LF injection", "slack:T1:abc\nfoo"},
		{"CRLF injection", "slack:T1:abc\r\nfoo"},
		{"NUL byte", "slack:T1:abc\x00foo"},
		{"DEL byte", "slack:T1:abc\x7ffoo"},
		{"low control", "slack:T1:abc\x01foo"},
		{"non-ASCII (emoji)", "slack:T1:abc\xf0\x9f\x9a\x80foo"}, // 🚀
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Create(context.Background(), CreateInput{
				TargetURL:      "https://example.com",
				IdempotencyKey: tc.key,
			})
			if !errors.Is(err, ErrIdempotencyKeyInvalid) {
				t.Errorf("got %v, want ErrIdempotencyKeyInvalid", err)
			}
		})
	}
	if hits.Load() != 0 {
		t.Errorf("server should not be hit on invalid-byte keys; got %d hits", hits.Load())
	}
}

func TestCreateIdempotencyKeyAcceptsValidBytes(t *testing.T) {
	// Inverse-contract tests: anything outside the reject set must
	// reach the wire unchanged. Split out from RejectsInvalidBytes so
	// "rejects" actually means rejects (no positive cases hidden in
	// the same parent test).
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get(HeaderIdempotencyKey)
		apiEnvelope(t, w, map[string]any{"resource_id": "r_valid"})
	}))
	defer srv.Close()
	c := testClient(srv.URL, "test-key")

	t.Run("all printable ASCII 0x21-0x7E (excluding boundary space)", func(t *testing.T) {
		// Skip 0x20 (space) here because it's only valid mid-key —
		// validator rejects it as the first or last byte (RFC 7230
		// OWS would trim it on the wire). Mid-key space is covered
		// by the "internal space" subtest below.
		var b strings.Builder
		for byteVal := byte(0x21); byteVal <= 0x7E; byteVal++ {
			b.WriteByte(byteVal)
		}
		want := b.String()
		if _, err := c.Create(context.Background(), CreateInput{
			TargetURL:      "https://example.com",
			IdempotencyKey: want,
		}); err != nil {
			t.Errorf("unexpected error %v", err)
		}
		if gotKey != want {
			t.Errorf("header drift: got %q, want %q", gotKey, want)
		}
	})

	t.Run("internal space accepted and preserved", func(t *testing.T) {
		// Mid-key space is permitted; only boundary whitespace
		// triggers OWS-trim by the wire.
		want := "key with spaces"
		if _, err := c.Create(context.Background(), CreateInput{
			TargetURL:      "https://example.com",
			IdempotencyKey: want,
		}); err != nil {
			t.Errorf("unexpected error %v", err)
		}
		if gotKey != want {
			t.Errorf("header drift: got %q, want %q", gotKey, want)
		}
	})

	t.Run("internal tab accepted and preserved", func(t *testing.T) {
		// Mid-key tab is permitted by the validator; pin it.
		want := "key\twith\ttabs"
		if _, err := c.Create(context.Background(), CreateInput{
			TargetURL:      "https://example.com",
			IdempotencyKey: want,
		}); err != nil {
			t.Errorf("unexpected error %v", err)
		}
		if gotKey != want {
			t.Errorf("header drift: got %q, want %q", gotKey, want)
		}
	})
}

// TestListResourcesLimitClamp fences the defensive client-side clamp:
// a caller passing Limit > 100 (server max) gets the request fired at
// limit=100 rather than letting the server return a 400. The clamp
// is a boundary check, not a transform — Limit=0 still falls back to
// the server default by omitting the param.
func TestListResourcesLimitClamp(t *testing.T) {
	cases := []struct {
		name     string
		inLimit  int
		wantSent string // "" means the param must not be set
	}{
		{name: "below max passes through", inLimit: 50, wantSent: "50"},
		{name: "at max passes through", inLimit: 100, wantSent: "100"},
		{name: "above max clamps to 100", inLimit: 500, wantSent: "100"},
		{name: "zero omits param", inLimit: 0, wantSent: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got := r.URL.Query().Get("limit")
				if got != tc.wantSent {
					t.Errorf("limit query param: got %q, want %q", got, tc.wantSent)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{
					"data": []map[string]any{},
					"meta": map[string]string{"request_id": "req_test"},
				}); err != nil {
					t.Fatalf("encode: %v", err)
				}
			}))
			defer srv.Close()

			c := testClient(srv.URL, "test-key")
			if _, err := c.ListResources(context.Background(), ListResourcesInput{Limit: tc.inLimit}); err != nil {
				t.Fatalf("ListResources: %v", err)
			}
		})
	}
}

func TestListResourcesSlugFilter(t *testing.T) {
	const slug = "prod-dashboard"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("slug"); got != slug {
			t.Errorf("slug query param: got %q, want %q", got, slug)
		}
		if r.URL.Query().Get("limit") != "" || r.URL.Query().Get("cursor") != "" {
			t.Errorf("unexpected pagination params for slug lookup: %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"resource_id": "r_prod_dash01",
				"type":        ResourceTypeTunnel,
				"slug":        slug,
				"status":      StatusActive,
			}},
			"meta": map[string]string{"request_id": "req_test"},
		}); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	out, err := c.ListResources(context.Background(), ListResourcesInput{Slug: slug})
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(out.Resources) != 1 || out.Resources[0].Slug != slug {
		t.Fatalf("resources = %+v, want one slug match", out.Resources)
	}
}

// TestCreatePathIsPlural pins the canonical /v1/qurls path as a named
// regression assertion. TestCreate already asserts the path generically
// at line 57; the value of this test is the explicit name — any future
// "let me just rename it back" change surfaces by the regression-pin
// test name, not by an unrelated TestCreate failure. PR #176 fixed the
// original singular `/v1/qurl` bug in production.
func TestCreatePathIsPlural(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		apiEnvelope(t, w, map[string]any{
			"resource_id": "r_path_test",
			"qurl_link":   "https://qurl.link/path",
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	if _, err := c.Create(context.Background(), CreateInput{TargetURL: "https://example.com"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if gotPath != "/v1/qurls" {
		t.Errorf("expected /v1/qurls (plural), got %q", gotPath)
	}
}

func TestCreateResourceIDFlow(t *testing.T) {
	// When ResourceID is set, Create hits the resource-scoped mint
	// endpoint (POST /v1/resources/{id}/qurls). The id rides in the
	// URL path, so the request body MUST NOT carry resource_id or
	// target_url — qURL service's CreateQurlForResourceRequest schema
	// has neither (the path identifies the resource).
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": testResourceID,
			"qurl_link":   "https://qurl.link/at_existing",
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	if _, err := c.Create(context.Background(), CreateInput{ResourceID: testResourceID}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got, want := gotPath, "/v1/resources/"+testResourceID+"/qurls"; got != want {
		t.Errorf("path: got %q, want %q", got, want)
	}
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("unmarshal body: %v (body=%s)", err, gotBody)
	}
	if _, ok := raw["resource_id"]; ok {
		t.Errorf("resource_id must NOT appear in body (rides in URL path); body=%s", gotBody)
	}
	if _, ok := raw["target_url"]; ok {
		t.Errorf("target_url must NOT appear in body on resource-scoped mint; body=%s", gotBody)
	}
}

func TestCreateTargetURLOnlyOmitsResourceID(t *testing.T) {
	// Mirror of the ResourceID-only test: a TargetURL-only call must
	// not emit `resource_id` on the wire. Pin the omitempty contract
	// in both directions so a future `,omitempty` removal trips here.
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		apiEnvelope(t, w, map[string]any{"resource_id": "r_url_only"})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	if _, err := c.Create(context.Background(), CreateInput{TargetURL: "https://example.com"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got, ok := raw["target_url"]; !ok || got != "https://example.com" {
		t.Errorf("target_url should be present and set; got %v", got)
	}
	if _, ok := raw["resource_id"]; ok {
		t.Errorf("resource_id must be omitted when empty; body=%s", gotBody)
	}
}

// TestCreateSessionDurationOnWire pins that CreateInput.SessionDuration
// serializes as `session_duration` on both create wire shapes — the direct
// `/v1/qurls` body (target_url form) and the resource-scoped
// `/v1/resources/{id}/qurls` body — and that it omits when empty.
func TestCreateSessionDurationOnWire(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input CreateInput
	}{
		{"target_url form", CreateInput{TargetURL: "https://example.com", SessionDuration: "1h"}},
		{"resource_id form", CreateInput{ResourceID: "r_tunnel", SessionDuration: "1h"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				apiEnvelope(t, w, map[string]any{"resource_id": "r_x"})
			}))
			defer srv.Close()
			c := testClient(srv.URL, "test-key")
			if _, err := c.Create(context.Background(), tc.input); err != nil {
				t.Fatalf("Create: %v", err)
			}
			var raw map[string]any
			if err := json.Unmarshal(gotBody, &raw); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			if got, _ := raw["session_duration"].(string); got != "1h" {
				t.Errorf("session_duration = %v, want \"1h\"; body=%s", raw["session_duration"], gotBody)
			}
		})
	}
}

func TestCreateForPublicResourceID(t *testing.T) {
	const publicResourceID = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEN4yvBX3yjAvYl9qagkStIWB1ie2gp_LF2Jy0w5AdxXefsTNLn9nrOlA4umKRiIQeGfvad9OFVoWa3PAIxcy4qg"
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		apiEnvelope(t, w, map[string]any{"resource_id": publicResourceID})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	if _, err := c.Create(context.Background(), CreateInput{ResourceID: publicResourceID}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantPath := "/v1/resources/" + publicResourceID + "/qurls"
	if gotPath != wantPath {
		t.Fatalf("request path = %q, want %q", gotPath, wantPath)
	}
}

// TestCreateSessionDurationOmittedWhenEmpty pins the omitempty contract so a
// zero SessionDuration inherits the server/plan default rather than forcing 0.
func TestCreateSessionDurationOmittedWhenEmpty(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		apiEnvelope(t, w, map[string]any{"resource_id": "r_x"})
	}))
	defer srv.Close()
	c := testClient(srv.URL, "test-key")
	if _, err := c.Create(context.Background(), CreateInput{TargetURL: "https://example.com"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if _, ok := raw["session_duration"]; ok {
		t.Errorf("session_duration must be omitted when empty; body=%s", gotBody)
	}
}

// TestCreateLabelOnWire pins that CreateInput.Label serializes as `label`
// (not `description`) on both create wire shapes — a regression guard for
// sending the wrong create field. See CreateInput.Label for the wire-field
// rationale.
// TODO(upstream-rebrand): keep aligned with qurl-service's openapi.
func TestCreateLabelOnWire(t *testing.T) {
	const wantLabel = "Alice from Acme"
	for _, tc := range []struct {
		name  string
		input CreateInput
	}{
		{"target_url form", CreateInput{TargetURL: "https://example.com", Label: wantLabel}},
		{"resource_id form", CreateInput{ResourceID: "r_tunnel", Label: wantLabel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				apiEnvelope(t, w, map[string]any{"resource_id": testResourceID})
			}))
			defer srv.Close()
			c := testClient(srv.URL, "test-key")
			if _, err := c.Create(context.Background(), tc.input); err != nil {
				t.Fatalf("Create: %v", err)
			}
			var raw map[string]any
			if err := json.Unmarshal(gotBody, &raw); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			if got, _ := raw["label"].(string); got != wantLabel {
				t.Errorf("label = %v, want %q; body=%s", raw["label"], wantLabel, gotBody)
			}
			if _, ok := raw["description"]; ok {
				t.Errorf("create body must not send `description` (use `label`); body=%s", gotBody)
			}
		})
	}
}

// TestCreateTargetURLAndResourceIDMutuallyExclusive pins the client-side
// fail-fast for the both-populated case. Mirror of the symmetric guard in
// UpdateResource for (Alias, ClearAlias). Closes the third leg of the
// omitempty + exclusivity contract — the two valid shapes are pinned by
// TestCreateResourceIDFlow / TestCreateTargetURLOnlyOmitsResourceID, and
// this test pins the invalid combination.
func TestCreateTargetURLAndResourceIDMutuallyExclusive(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	_, err := c.Create(context.Background(), CreateInput{
		TargetURL:  "https://example.com",
		ResourceID: testResourceID,
	})
	if !errors.Is(err, ErrCreateTargetResourceExclusive) {
		t.Fatalf("expected ErrCreateTargetResourceExclusive, got %v", err)
	}
}

// TestCreateNeitherTargetURLNorResourceIDRejected pins the client-side
// fail-fast for the both-empty case (companion to the both-populated test).
// Without a target on either field the server can't bind the qURL to
// anything; failing fast saves a 400 round-trip.
func TestCreateNeitherTargetURLNorResourceIDRejected(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	_, err := c.Create(context.Background(), CreateInput{})
	if !errors.Is(err, ErrCreateRequiresTarget) {
		t.Fatalf("expected ErrCreateRequiresTarget, got %v", err)
	}
}

// TestCreateNoTargetBeatsInvalidIdempotencyKey pins the validation
// order in Create: target presence (a structural error) is checked
// before the idempotency-key bytes (a request-decoration error). A
// caller missing both should learn about the missing target first;
// the error is the more actionable of the two. Symmetric with
// TestUpdateResourceEmptyAliasPlusClearAliasReportsExclusivityFirst.
func TestCreateNoTargetBeatsInvalidIdempotencyKey(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	_, err := c.Create(context.Background(), CreateInput{
		// No TargetURL, no ResourceID, AND a key with a control byte
		// (would also fail validateIdempotencyKey). Target check runs
		// first.
		IdempotencyKey: "bad\nkey",
	})
	if !errors.Is(err, ErrCreateRequiresTarget) {
		t.Fatalf("expected ErrCreateRequiresTarget (target-presence beats idempotency), got %v", err)
	}
}

// TestCreateBothPopulatedBeatsInvalidIdempotencyKey is the both-populated
// counterpart of the both-empty validation-order test. The exclusivity
// error (a structural error) should fire before the idempotency-key
// validation (a request-decoration error). Pinning both directions
// ensures a future reorder of guards trips a test.
func TestCreateBothPopulatedBeatsInvalidIdempotencyKey(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	_, err := c.Create(context.Background(), CreateInput{
		TargetURL:      "https://example.com",
		ResourceID:     testResourceID,
		IdempotencyKey: "bad\nkey",
	})
	if !errors.Is(err, ErrCreateTargetResourceExclusive) {
		t.Fatalf("expected ErrCreateTargetResourceExclusive (exclusivity beats idempotency), got %v", err)
	}
}

// --- Resource methods ---

func TestCreateResource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/resources" {
			t.Errorf("expected /v1/resources, got %s", r.URL.Path)
		}
		var input CreateResourceInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if input.TargetURL != testTargetURL {
			t.Errorf("got TargetURL %q", input.TargetURL)
		}
		if input.Alias != testAlias {
			t.Errorf("got Alias %q, want %q", input.Alias, testAlias)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": testResourceIDAlt,
			"target_url":  testTargetURL,
			"alias":       testAlias,
			"type":        ResourceTypeURL,
			"status":      StatusActive,
			"updated_at":  "2026-05-10T07:30:00Z",
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.CreateResource(context.Background(), &CreateResourceInput{
		TargetURL: testTargetURL,
		Alias:     testAlias,
	})
	if err != nil {
		t.Fatalf("CreateResource: %v", err)
	}
	if got.ResourceID != testResourceIDAlt {
		t.Errorf("got ResourceID %q", got.ResourceID)
	}
	if got.Alias != testAlias {
		t.Errorf("got Alias %q, want %q", got.Alias, testAlias)
	}
	// Pin Status decoding — confirms the response shape from
	// qurl-service/api/openapi.yaml ResourceData.status round-trips.
	if got.Status != StatusActive {
		t.Errorf("got Status %q, want %q", got.Status, StatusActive)
	}
	// Pin UpdatedAt decoding — time.Time + omitzero round-trip. The
	// non-zero check confirms the field decoded; year/month assertion
	// catches a future timezone-parsing regression.
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt should be populated when present on the wire")
	}
	if got.UpdatedAt.Year() != 2026 || got.UpdatedAt.Month() != 5 {
		t.Errorf("UpdatedAt: got %v, want 2026-05-...", got.UpdatedAt)
	}
}

// TestCreateResourceURLTypeExplicit pins the explicit Type=ResourceTypeURL
// branch of the validator switch. The empty-Type and Type=tunnel branches
// have dedicated tests; this fills the middle case.
func TestCreateResourceURLTypeExplicit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input CreateResourceInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if input.Type != ResourceTypeURL {
			t.Errorf("got Type %q, want %q", input.Type, ResourceTypeURL)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": "r_url_explicit",
			"target_url":  testTargetURL,
			"type":        ResourceTypeURL,
			"status":      StatusActive,
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.CreateResource(context.Background(), &CreateResourceInput{
		Type:      ResourceTypeURL,
		TargetURL: testTargetURL,
	})
	if err != nil {
		t.Fatalf("CreateResource: %v", err)
	}
	if got.Type != ResourceTypeURL {
		t.Errorf("got Type %q, want %q", got.Type, ResourceTypeURL)
	}
}

// TestCreateResourceUnknownTypePassesThrough pins the forward-compat
// default branch in the TargetURL validator: an unknown Type value
// should pass through to the server, which is the authority on type
// validation. Keeps the client release-independent of new ResourceType
// values added to the qurl-service OpenAPI surface.
func TestCreateResourceUnknownTypePassesThrough(t *testing.T) {
	const futureType = "webhook"
	var sawRequest bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		var input CreateResourceInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if input.Type != futureType {
			t.Errorf("got Type %q, want %q", input.Type, futureType)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": "r_future",
			"type":        futureType,
			"status":      StatusActive,
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	if _, err := c.CreateResource(context.Background(), &CreateResourceInput{
		Type: futureType,
	}); err != nil {
		t.Fatalf("CreateResource: %v", err)
	}
	if !sawRequest {
		t.Fatal("server should have seen the request (unknown type must pass through)")
	}
}

func TestCreateResourceNilInputRejected(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	_, err := c.CreateResource(context.Background(), nil)
	if !errors.Is(err, ErrCreateResourceNilInput) {
		t.Fatalf("expected ErrCreateResourceNilInput, got %v", err)
	}
}

// TestCreateResourceEmptyTargetURLRejected pins the client-side fail-fast
// when TargetURL is missing — without it the server can't compute the
// `(owner_id, target_url_hash)` idempotency key. Companion to the nil-input
// test.
func TestCreateResourceEmptyTargetURLRejected(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	_, err := c.CreateResource(context.Background(), &CreateResourceInput{
		Alias: testAlias,
	})
	if !errors.Is(err, ErrCreateResourceRequiresTargetURL) {
		t.Fatalf("expected ErrCreateResourceRequiresTargetURL, got %v", err)
	}
}

// TestCreateResourceTunnelTypeAcceptsEmptyTargetURL pins the tunnel
// branch of the TargetURL validator. The doc on
// CreateResourceInput.Type says TargetURL is ignored when type=tunnel;
// this test ensures the client doesn't reject the request before it
// reaches the server.
func TestCreateResourceTunnelTypeAcceptsEmptyTargetURL(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": "r_tunnel01",
			"type":        ResourceTypeTunnel,
			"status":      StatusActive,
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.CreateResource(context.Background(), &CreateResourceInput{
		Type: ResourceTypeTunnel,
	})
	if err != nil {
		t.Fatalf("CreateResource: %v", err)
	}
	if got.Type != ResourceTypeTunnel {
		t.Errorf("got Type %q, want %q", got.Type, ResourceTypeTunnel)
	}
	// Pin wire elision — `target_url` must be absent from the JSON
	// payload entirely (not serialized as `""`). Mirrors the
	// TestCreateResourceIDFlow pattern so a future `,omitempty`
	// removal on TargetURL trips here.
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got := raw["type"]; got != ResourceTypeTunnel {
		t.Errorf("type: got %v, want %q", got, ResourceTypeTunnel)
	}
	if _, ok := raw["target_url"]; ok {
		t.Errorf("target_url must be omitted on tunnel creates; body=%s", gotBody)
	}
}

func TestCreateResourceTunnelFindOrCreateSlug(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id":       "r_tunnel01",
			"type":              ResourceTypeTunnel,
			"slug":              testTunnelSlug,
			"status":            StatusActive,
			"knock_resource_id": "qurl-tunnel-server",
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.CreateResource(context.Background(), &CreateResourceInput{
		Type:         ResourceTypeTunnel,
		Slug:         testTunnelSlug,
		FindOrCreate: true,
	})
	if err != nil {
		t.Fatalf("CreateResource: %v", err)
	}
	if got.Slug != testTunnelSlug {
		t.Errorf("Slug = %q, want %s", got.Slug, testTunnelSlug)
	}
	if got.KnockResourceID != "qurl-tunnel-server" {
		t.Errorf("KnockResourceID = %q, want qurl-tunnel-server", got.KnockResourceID)
	}
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if raw["slug"] != testTunnelSlug || raw["find_or_create"] != true {
		t.Errorf("body = %s, want slug + find_or_create", gotBody)
	}
}

// TestCreateResourceTunnelTypeRejectsTargetURL pins the inverse of
// TestCreateResourceTunnelTypeAcceptsEmptyTargetURL — a tunnel resource
// with a non-empty TargetURL is almost always a stale field from
// copy-pasted literals; failing fast yields a clearer error than a
// silent server-side discard.
func TestCreateResourceTunnelTypeRejectsTargetURL(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	_, err := c.CreateResource(context.Background(), &CreateResourceInput{
		Type:      ResourceTypeTunnel,
		TargetURL: testTargetURL,
	})
	if !errors.Is(err, ErrCreateResourceTunnelRejectsTargetURL) {
		t.Fatalf("expected ErrCreateResourceTunnelRejectsTargetURL, got %v", err)
	}
}

func TestCreateAPIKeyTunnelBootstrap(t *testing.T) {
	var gotHeader string
	var gotWire map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/api-keys" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotHeader = r.Header.Get(HeaderIdempotencyKey)
		if err := json.NewDecoder(r.Body).Decode(&gotWire); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		apiEnvelope(t, w, map[string]any{
			"key_id":      "key_abc123DEF456",
			"api_key":     "lv_live_secret",
			"name":        testTunnelSlug + " bootstrap",
			"scopes":      []string{"qurl:agent", "qurl:write"},
			"status":      StatusActive,
			"key_type":    APIKeyTypeTunnelBootstrap,
			"tunnel_slug": testTunnelSlug,
			"expires_at":  "2026-05-28T00:00:00Z",
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.CreateAPIKey(context.Background(), &CreateAPIKeyInput{
		Name:           testTunnelSlug + " bootstrap",
		Scopes:         []string{"qurl:agent", "qurl:write"},
		KeyType:        APIKeyTypeTunnelBootstrap,
		TunnelSlug:     testTunnelSlug,
		ExpiresIn:      "24h",
		IdempotencyKey: "bootstrap-key-12345678901234567890",
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if gotHeader != "bootstrap-key-12345678901234567890" {
		t.Errorf("Idempotency-Key = %q", gotHeader)
	}
	if gotWire["key_type"] != APIKeyTypeTunnelBootstrap || gotWire["tunnel_slug"] != testTunnelSlug {
		t.Errorf("body = %+v, want tunnel bootstrap fields", gotWire)
	}
	if _, ok := gotWire["purpose"]; ok {
		t.Errorf("body contained deprecated purpose field: %+v", gotWire)
	}
	if got.APIKey != "lv_live_secret" || got.KeyType != APIKeyTypeTunnelBootstrap || got.TunnelSlug != testTunnelSlug {
		t.Errorf("decoded key = %+v", got)
	}
	if got.ExpiresAt == nil {
		t.Fatal("ExpiresAt should decode")
	}
}

func TestRevokeAPIKey(t *testing.T) {
	var gotMethod string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	if err := c.RevokeAPIKey(context.Background(), "key_tunnel_bootstrap"); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/api-keys/key_tunnel_bootstrap" {
		t.Fatalf("request = %s %s, want DELETE /v1/api-keys/key_tunnel_bootstrap", gotMethod, gotPath)
	}
}

func TestRevokeAPIKeyReturnsAPIErrorOnFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/api-keys/key_missing" {
			t.Fatalf("request = %s %s, want DELETE /v1/api-keys/key_missing", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"title":"not found","status":404,"detail":"key already gone"}}`))
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	err := c.RevokeAPIKey(context.Background(), "key_missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("RevokeAPIKey error = %v, want APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound || apiErr.Detail != "key already gone" {
		t.Fatalf("APIError = %+v, want 404 key already gone", apiErr)
	}
}

func TestRevokeAPIKeyRejectsEmptyID(t *testing.T) {
	c := testClient("https://qurl.invalid", "test-key")
	if err := c.RevokeAPIKey(context.Background(), " \t "); !errors.Is(err, ErrRevokeAPIKeyEmptyID) {
		t.Fatalf("RevokeAPIKey empty id error = %v, want ErrRevokeAPIKeyEmptyID", err)
	}
}

func TestDeleteResource(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	if err := c.DeleteResource(context.Background(), "r_abc123test"); err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/resources/r_abc123test" {
		t.Fatalf("request = %s %s, want DELETE /v1/resources/r_abc123test", gotMethod, gotPath)
	}
}

func TestDeleteResourceReturnsAPIErrorOnFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/resources/r_missing0001" {
			t.Fatalf("request = %s %s, want DELETE /v1/resources/r_missing0001", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"title":"not found","status":404,"detail":"resource already revoked"}}`))
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	err := c.DeleteResource(context.Background(), "r_missing0001")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("DeleteResource error = %v, want APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound || apiErr.Detail != "resource already revoked" {
		t.Fatalf("APIError = %+v, want 404 resource already revoked", apiErr)
	}
}

func TestDeleteResourceRejectsEmptyID(t *testing.T) {
	c := testClient("https://qurl.invalid", "test-key")
	if err := c.DeleteResource(context.Background(), " \t "); !errors.Is(err, ErrDeleteResourceEmptyID) {
		t.Fatalf("DeleteResource empty id error = %v, want ErrDeleteResourceEmptyID", err)
	}
}

func TestDeleteResourceEscapesIDPathSegment(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	// Defend the opaque path segment the same way RevokeAPIKey/UpdateResource do:
	// a stray slash must not escape
	// the /v1/resources/ collection.
	if err := c.DeleteResource(context.Background(), "r_a/b"); err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if gotPath != "/v1/resources/r_a%2Fb" {
		t.Fatalf("escaped path = %q, want /v1/resources/r_a%%2Fb", gotPath)
	}
}

// TestCreateResourceAccessPolicyRoundTrip pins the AccessPolicy decoding
// path — if the server schema renames a subfield (ip_allowlist →
// ip_allow, etc.) the round-trip would silently drop it without this
// test.
func TestCreateResourceAccessPolicyRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input CreateResourceInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if input.AccessPolicy == nil {
			t.Fatal("AccessPolicy should not be nil on the wire")
		}
		if len(input.AccessPolicy.IPAllowlist) != 2 {
			t.Errorf("IPAllowlist: got %v", input.AccessPolicy.IPAllowlist)
		}
		if len(input.AccessPolicy.IPDenylist) != 1 {
			t.Errorf("IPDenylist: got %v", input.AccessPolicy.IPDenylist)
		}
		if len(input.AccessPolicy.GeoAllowlist) != 2 {
			t.Errorf("GeoAllowlist: got %v", input.AccessPolicy.GeoAllowlist)
		}
		if len(input.AccessPolicy.GeoDenylist) != 1 {
			t.Errorf("GeoDenylist: got %v", input.AccessPolicy.GeoDenylist)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": "r_policy01",
			"target_url":  testTargetURL,
			"status":      StatusActive,
			"access_policy": map[string]any{
				"ip_allowlist":  []string{"10.0.0.0/8", "192.168.0.0/16"},
				"ip_denylist":   []string{"203.0.113.0/24"},
				"geo_allowlist": []string{"US", "CA"},
				"geo_denylist":  []string{"CN"},
			},
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.CreateResource(context.Background(), &CreateResourceInput{
		TargetURL: testTargetURL,
		AccessPolicy: &AccessPolicy{
			IPAllowlist:  []string{"10.0.0.0/8", "192.168.0.0/16"},
			IPDenylist:   []string{"203.0.113.0/24"},
			GeoAllowlist: []string{"US", "CA"},
			GeoDenylist:  []string{"CN"},
		},
	})
	if err != nil {
		t.Fatalf("CreateResource: %v", err)
	}
	if got.AccessPolicy == nil {
		t.Fatal("response AccessPolicy should not be nil")
	}
	if len(got.AccessPolicy.IPAllowlist) != 2 {
		t.Errorf("response IPAllowlist: got %v", got.AccessPolicy.IPAllowlist)
	}
	if len(got.AccessPolicy.IPDenylist) != 1 {
		t.Errorf("response IPDenylist: got %v", got.AccessPolicy.IPDenylist)
	}
	if len(got.AccessPolicy.GeoAllowlist) != 2 {
		t.Errorf("response GeoAllowlist: got %v", got.AccessPolicy.GeoAllowlist)
	}
	if len(got.AccessPolicy.GeoDenylist) != 1 {
		t.Errorf("response GeoDenylist: got %v", got.AccessPolicy.GeoDenylist)
	}
}

// TestCreateResourceRetryPreservesBody pins the body-buffering invariant
// on the CreateResource retry path, mirroring
// TestUpdateResourceRetryPreservesBody. POST /v1/resources is server-side
// idempotent on (owner_id, target_url_hash), but client-side body bytes
// must stay byte-identical across attempts so the server's hash matches.
func TestCreateResourceRetryPreservesBody(t *testing.T) {
	var attempts atomic.Int32
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("attempt %d: read body: %v", n, err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": testResourceIDAlt,
			"target_url":  testTargetURL,
			"status":      StatusActive,
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key",
		WithRetry(3),
		withDelaysForTest(),
	)
	if _, err := c.CreateResource(context.Background(), &CreateResourceInput{
		TargetURL: testTargetURL,
		Alias:     testAlias,
	}); err != nil {
		t.Fatalf("CreateResource after retries: %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(bodies); i++ {
		if !bytes.Equal(bodies[0], bodies[i]) {
			t.Errorf("attempt %d body diverged from attempt 1.\n#1: %s\n#%d: %s", i+1, bodies[0], i+1, bodies[i])
		}
	}
}

// TestUpdateResourceNoFieldsSetRejected pins the client-side fail-fast
// for the all-nil-pointers, ClearAlias=false case. Symmetric with
// Create's no-target guard — saves a round-trip on a no-op PATCH.
func TestUpdateResourceNoFieldsSetRejected(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	_, err := c.UpdateResource(context.Background(), testResourceID, &UpdateResourceInput{})
	if !errors.Is(err, ErrUpdateResourceNoFieldsSet) {
		t.Fatalf("expected ErrUpdateResourceNoFieldsSet, got %v", err)
	}
}

// TestHasAnyFieldSetCoversAllFields walks UpdateResourceInput's struct
// fields via reflection and asserts that setting *each one alone* makes
// hasAnyFieldSet return true. Catches the failure mode where a future
// contributor adds a new mutable field but forgets to update
// hasAnyFieldSet — without this test, a no-op PATCH that exercises only
// the new field would slip past the guard.
//
// The reflection walk is intentionally narrow: pointer fields get a
// pointer to a zero value of the target type; bool fields flip to true.
// Skip the test (with a t.Skip) for any field whose type isn't covered;
// adding such a field here is a nudge to extend the helper. Today's
// surface (5 fields: Description, Alias, ClearAlias, CustomDomain,
// AccessPolicy) is fully covered.
func TestHasAnyFieldSetCoversAllFields(t *testing.T) {
	emptyStr := ""
	cases := []struct {
		name  string
		input UpdateResourceInput
	}{
		{"Description", UpdateResourceInput{Description: &emptyStr}},
		{"Alias", UpdateResourceInput{Alias: &emptyStr}},
		{"ClearAlias", UpdateResourceInput{ClearAlias: true}},
		{"CustomDomain", UpdateResourceInput{CustomDomain: &emptyStr}},
		{"AccessPolicy", UpdateResourceInput{AccessPolicy: &AccessPolicy{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.input.hasAnyFieldSet() {
				t.Errorf("hasAnyFieldSet should be true with %s set; got false", tc.name)
			}
		})
	}

	// Sanity check: empty struct returns false.
	var empty UpdateResourceInput
	if empty.hasAnyFieldSet() {
		t.Error("hasAnyFieldSet on empty UpdateResourceInput should return false")
	}

	// Coverage tripwire: if a new mutable field is added to
	// UpdateResourceInput without updating both hasAnyFieldSet and
	// the cases above, the reflect-based set-equality check fires.
	// Set-equality (not just count) catches the duplicate-case copy-
	// paste mistake too — two cases named "Alias" and a missing case
	// for a new field would slip past a count-only check.
	// Filters out fields with `json:"-"` so future request-decoration
	// fields (e.g. IdempotencyKey per #148) don't trip the check.
	expected := make(map[string]bool)
	tt := reflect.TypeOf(UpdateResourceInput{})
	for i := range tt.NumField() {
		f := tt.Field(i)
		if f.Tag.Get("json") != "-" {
			expected[f.Name] = true
		}
	}
	got := make(map[string]bool)
	for _, tc := range cases {
		got[tc.name] = true
	}
	for name := range expected {
		if !got[name] {
			t.Errorf("UpdateResourceInput field %q has no test case in TestHasAnyFieldSetCoversAllFields — "+
				"add it to the cases slice (and ensure hasAnyFieldSet covers it)", name)
		}
	}
	for name := range got {
		if !expected[name] {
			t.Errorf("test case %q does not match any UpdateResourceInput field — "+
				"likely a typo or stale rename", name)
		}
	}
}

func TestUpdateResourceSetAlias(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		wantPath := "/v1/resources/" + testResourceID
		if r.URL.Path != wantPath {
			t.Errorf("expected %s, got %s", wantPath, r.URL.Path)
		}
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": testResourceID,
			"alias":       testAliasAlt,
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	alias := testAliasAlt
	got, err := c.UpdateResource(context.Background(), testResourceID, &UpdateResourceInput{
		Alias: &alias,
	})
	if err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	if got.Alias != testAliasAlt {
		t.Errorf("got Alias %q, want %q", got.Alias, testAliasAlt)
	}

	// Pin the wire shape: alias serialized, clear_alias absent (false +
	// omitempty), description / custom_domain absent.
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got := raw["alias"]; got != testAliasAlt {
		t.Errorf("alias: got %v, want %q", got, testAliasAlt)
	}
	if _, ok := raw["clear_alias"]; ok {
		t.Errorf("clear_alias must elide when false; body=%s", gotBody)
	}
	if _, ok := raw["description"]; ok {
		t.Errorf("description must elide when nil pointer; body=%s", gotBody)
	}
}

func TestUpdateResourceClearAlias(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		// Server responds with the cleared resource — alias absent on
		// the wire, decoding to Resource.Alias == "".
		apiEnvelope(t, w, map[string]any{
			"resource_id": testResourceID,
			"status":      StatusActive,
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.UpdateResource(context.Background(), testResourceID, &UpdateResourceInput{
		ClearAlias: true,
	})
	if err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	// Confirm the cleared alias decodes correctly on the response side.
	if got.Alias != "" {
		t.Errorf("cleared alias should decode as empty string; got %q", got.Alias)
	}

	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got, ok := raw["clear_alias"]; !ok || got != true {
		t.Errorf("clear_alias: want true, got %v (ok=%v); body=%s", got, ok, gotBody)
	}
	// Symmetric pin: clearing must NOT also send a stale `alias` key.
	// Mirror of the assertion in TestUpdateResourceSetAlias.
	if _, ok := raw["alias"]; ok {
		t.Errorf("alias must elide when ClearAlias=true; body=%s", gotBody)
	}
}

// TestUpdateResourceRetryPreservesBody pins the retry-safety contract
// documented at [Client.UpdateResource]: do() retries 5xx with the
// buffered body, so a successfully-applied PATCH that returns 502
// will be re-applied with byte-identical request bytes. If a future
// refactor breaks the body-buffering invariant, the per-attempt
// payloads would diverge and this test catches it before any
// non-idempotent PATCH field starts shipping.
func TestUpdateResourceRetryPreservesBody(t *testing.T) {
	var attempts atomic.Int32
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("attempt %d: read body: %v", n, err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		apiEnvelope(t, w, map[string]any{"resource_id": testResourceID})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key",
		WithRetry(3),
		withDelaysForTest(),
	)
	alias := testAliasAlt
	if _, err := c.UpdateResource(context.Background(), testResourceID, &UpdateResourceInput{
		Alias: &alias,
	}); err != nil {
		t.Fatalf("UpdateResource after retries: %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(bodies); i++ {
		if !bytes.Equal(bodies[0], bodies[i]) {
			t.Errorf("attempt %d body diverged from attempt 1.\n#1: %s\n#%d: %s", i+1, bodies[0], i+1, bodies[i])
		}
	}
}

// TestUpdateResourceClearDescriptionByEmptyString pins the documented
// `&""` clear convention on Description (no sentinel-clear sibling). The
// empty string must round-trip on the wire as `"description": ""` so the
// server treats it as a clear; if a future contributor adds `omitempty` to
// `Description` (echoing Alias), this test fails loudly.
func TestUpdateResourceClearDescriptionByEmptyString(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": testResourceID,
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	empty := ""
	if _, err := c.UpdateResource(context.Background(), testResourceID, &UpdateResourceInput{
		Description: &empty,
	}); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	got, ok := raw["description"]
	if !ok {
		t.Fatalf("description must be present (the &\"\" clear semantic); body=%s", gotBody)
	}
	if got != "" {
		t.Errorf("description: got %v, want \"\"", got)
	}
}

// TestUpdateResourceClearCustomDomainByEmptyString pins the same `&""`
// clear convention on CustomDomain — symmetric with
// TestUpdateResourceClearDescriptionByEmptyString. Both fields share
// the convention but only one direction was pinned.
func TestUpdateResourceClearCustomDomainByEmptyString(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": testResourceID,
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	empty := ""
	if _, err := c.UpdateResource(context.Background(), testResourceID, &UpdateResourceInput{
		CustomDomain: &empty,
	}); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	got, ok := raw["custom_domain"]
	if !ok {
		t.Fatalf("custom_domain must be present (the &\"\" clear semantic); body=%s", gotBody)
	}
	if got != "" {
		t.Errorf("custom_domain: got %v, want \"\"", got)
	}
}

// TestUpdateResourceClearAccessPolicyByEmptyStruct pins the documented
// `&AccessPolicy{}` clear convention. AccessPolicy has no sentinel
// because it's a struct (not a scalar), so passing an all-zero pointer
// is the clear signal. The wire shape must round-trip as
// `"access_policy": {}` — if a future contributor adds a non-omitempty
// field to AccessPolicy, the empty literal would no longer marshal as
// `{}` and the clear contract would silently break; this test catches
// that.
func TestUpdateResourceClearAccessPolicyByEmptyStruct(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": testResourceID,
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	if _, err := c.UpdateResource(context.Background(), testResourceID, &UpdateResourceInput{
		AccessPolicy: &AccessPolicy{},
	}); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	got, ok := raw["access_policy"]
	if !ok {
		t.Fatalf("access_policy must be present (the &AccessPolicy{} clear semantic); body=%s", gotBody)
	}
	policy, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("access_policy should decode as object; got %T (%v)", got, got)
	}
	if len(policy) != 0 {
		t.Errorf("access_policy should marshal as `{}`; got %v", policy)
	}
}

func TestUpdateResourceEmptyIDRejected(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	_, err := c.UpdateResource(context.Background(), "", &UpdateResourceInput{})
	if !errors.Is(err, ErrUpdateResourceEmptyID) {
		t.Fatalf("expected ErrUpdateResourceEmptyID, got %v", err)
	}
}

// TestUpdateResourceWhitespaceIDRejected pins the strings.TrimSpace
// guard — a whitespace-only resource ID would otherwise hit the wire
// as `/v1/resources/%20%20` and 400 server-side. The client error
// stays actionable.
func TestUpdateResourceWhitespaceIDRejected(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	_, err := c.UpdateResource(context.Background(), "   ", &UpdateResourceInput{})
	if !errors.Is(err, ErrUpdateResourceEmptyID) {
		t.Fatalf("expected ErrUpdateResourceEmptyID on whitespace ID, got %v", err)
	}
}

// TestUpdateResourceTrimsSurroundingWhitespace pins the "trim then
// validate AND send" contract — `" r_existing01 "` must hit the wire
// as `/v1/resources/r_existing01`, NOT `/v1/resources/%20r_existing01%20`.
// Catches the bug where TrimSpace validated one value but the
// un-trimmed value went into the URL.
func TestUpdateResourceTrimsSurroundingWhitespace(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		apiEnvelope(t, w, map[string]any{"resource_id": testResourceID})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	desc := testDescription
	if _, err := c.UpdateResource(context.Background(), "  "+testResourceID+"  ", &UpdateResourceInput{
		Description: &desc,
	}); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	wantPath := "/v1/resources/" + testResourceID
	if gotPath != wantPath {
		t.Errorf("trimmed value should hit the wire: got %q, want %q", gotPath, wantPath)
	}
}

// TestUpdateResourceTrimsAliasPointer pins symmetry with the path-segment
// trim contract — a caller passing `Alias: &" prod-grafana "` should
// see the trimmed value on the wire (the server regex would 400 on
// surrounding whitespace anyway). Also pins the defensive-copy posture:
// the caller's input struct must NOT be mutated.
func TestUpdateResourceTrimsAliasPointer(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		apiEnvelope(t, w, map[string]any{"resource_id": testResourceID})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	original := "  " + testAliasAlt + "  "
	in := &UpdateResourceInput{Alias: &original}
	if _, err := c.UpdateResource(context.Background(), testResourceID, in); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}

	// Wire: alias is the trimmed value.
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got := raw["alias"]; got != testAliasAlt {
		t.Errorf("alias on wire: got %v, want %q", got, testAliasAlt)
	}

	// Caller's struct: untouched. The defensive copy must not
	// mutate the original *string.
	if *in.Alias != original {
		t.Errorf("caller's input.Alias was mutated; got %q, want %q", *in.Alias, original)
	}
}

// TestUpdateResourceWhitespaceOnlyAliasRejected pins what happens when
// an Alias-only payload trims to "": the empty-pointer footgun guard
// should fire (use ClearAlias=true). Symmetric with the explicit
// `Alias: &""` case but exercises the new trim path.
func TestUpdateResourceWhitespaceOnlyAliasRejected(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	whitespace := "   "
	_, err := c.UpdateResource(context.Background(), testResourceID, &UpdateResourceInput{
		Alias: &whitespace,
	})
	if !errors.Is(err, ErrUpdateResourceAliasEmpty) {
		t.Fatalf("expected ErrUpdateResourceAliasEmpty on whitespace-only alias, got %v", err)
	}
}

func TestUpdateResourceNilInputRejected(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	_, err := c.UpdateResource(context.Background(), testResourceID, nil)
	if !errors.Is(err, ErrUpdateResourceNilInput) {
		t.Fatalf("expected ErrUpdateResourceNilInput, got %v", err)
	}
}

// TestUpdateResourceEscapesIDPathSegment pins the path-escape contract
// for UpdateResource's resource-id path segment.
// Current server-generated IDs contain no reserved bytes; the test remains
// defensive for direct programmatic callers and keeps a future
// `url.PathEscape` removal from tripping silently.
func TestUpdateResourceEscapesIDPathSegment(t *testing.T) {
	var gotEscapedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscapedPath = r.URL.EscapedPath()
		apiEnvelope(t, w, map[string]any{"resource_id": testResourceID})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	desc := testDescription
	if _, err := c.UpdateResource(context.Background(), "weird/id", &UpdateResourceInput{
		Description: &desc,
	}); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}
	const want = "/v1/resources/weird%2Fid"
	if gotEscapedPath != want {
		t.Errorf("resourceID path-escape: got %q, want %q", gotEscapedPath, want)
	}
}

// TestCreateResourceAliasInUse pins the typed *APIError shape on the
// `alias_in_use` 409 path documented in CreateResourceInput's doc comment
// (alias attempted on an already-existing resource missing an alias).
func TestCreateResourceAliasInUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		resp := map[string]any{
			"error": map[string]any{
				"title":  "Conflict",
				"status": 409,
				"detail": "alias already in use",
				"code":   "alias_in_use",
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	_, err := c.CreateResource(context.Background(), &CreateResourceInput{
		TargetURL: testTargetURL,
		Alias:     testAlias,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != http.StatusConflict {
		t.Errorf("got status %d, want 409", apiErr.StatusCode)
	}
	if apiErr.Code != "alias_in_use" {
		t.Errorf("got code %q, want alias_in_use", apiErr.Code)
	}
}

// TestUpdateResourceInvalidAlias pins the typed *APIError shape on a 400
// `invalid_alias` path — symmetric coverage with the other two new methods.
// Also pins the InvalidFields decoding so a regression in either parseError
// or the test fixture would surface here.
func TestUpdateResourceInvalidAlias(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		resp := map[string]any{
			"error": map[string]any{
				"title":  "Bad Request",
				"status": 400,
				"detail": "alias must match ^[a-z][a-z0-9-]{1,62}[a-z0-9]$",
				"code":   "invalid_alias",
				"invalid_fields": map[string]string{
					"alias": "must match ^[a-z][a-z0-9-]{1,62}[a-z0-9]$",
				},
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	alias := "Bad-Alias!"
	_, err := c.UpdateResource(context.Background(), testResourceID, &UpdateResourceInput{
		Alias: &alias,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("got status %d, want 400", apiErr.StatusCode)
	}
	if apiErr.Code != "invalid_alias" {
		t.Errorf("got code %q, want invalid_alias", apiErr.Code)
	}
	if got, ok := apiErr.InvalidFields["alias"]; !ok || got == "" {
		t.Errorf("InvalidFields[\"alias\"] should be populated; got %v (ok=%v)", got, ok)
	}
}

// TestUpdateResourceEmptyAliasPointerRejected pins the client-side
// fail-fast for the "pointer to empty string" footgun documented on
// UpdateResourceInput.Alias — the server's regex would 400 it anyway,
// but the client's error message tells the caller exactly what to do
// (use ClearAlias=true).
func TestUpdateResourceEmptyAliasPointerRejected(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	empty := ""
	_, err := c.UpdateResource(context.Background(), testResourceID, &UpdateResourceInput{
		Alias: &empty,
	})
	if !errors.Is(err, ErrUpdateResourceAliasEmpty) {
		t.Fatalf("expected ErrUpdateResourceAliasEmpty, got %v", err)
	}
}

// TestUpdateResourceAliasAndClearAliasMutuallyExclusive pins the
// client-side rejection of the (Alias != nil, ClearAlias=true) combo
// — the server returns 400 for this combination, and the client
// fails fast to save the round-trip.
func TestUpdateResourceAliasAndClearAliasMutuallyExclusive(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	alias := testAlias
	_, err := c.UpdateResource(context.Background(), testResourceID, &UpdateResourceInput{
		Alias:      &alias,
		ClearAlias: true,
	})
	if !errors.Is(err, ErrUpdateResourceAliasClearExclusive) {
		t.Fatalf("expected ErrUpdateResourceAliasClearExclusive, got %v", err)
	}
}

// TestUpdateResourceEmptyAliasPlusClearAliasReportsExclusivityFirst pins
// the validation-order contract: when a caller passes both `Alias: &""`
// AND `ClearAlias: true`, the structural conflict (mutual exclusion)
// is the more actionable error and fires first. The empty-pointer
// footgun guard fires only when Alias is the only field in conflict.
func TestUpdateResourceEmptyAliasPlusClearAliasReportsExclusivityFirst(t *testing.T) {
	c := testClient("http://example.invalid", "test-key")
	empty := ""
	_, err := c.UpdateResource(context.Background(), testResourceID, &UpdateResourceInput{
		Alias:      &empty,
		ClearAlias: true,
	})
	if !errors.Is(err, ErrUpdateResourceAliasClearExclusive) {
		t.Fatalf("expected ErrUpdateResourceAliasClearExclusive (exclusivity beats empty-pointer), got %v", err)
	}
}
