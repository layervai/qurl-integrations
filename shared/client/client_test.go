package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

const testDescription = "updated"

// testClient creates a client with retries disabled for fast unit tests.
func testClient(url, key string) *Client {
	return New(url, key, WithRetry(0))
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
		if r.URL.Path != "/v1/qurl" {
			t.Errorf("expected /v1/qurl, got %s", r.URL.Path)
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

func TestAPIErrorRequestIDHeaderFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-ID", "req_from_header")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		resp := map[string]any{
			"error": map[string]any{
				"title":  "Internal Server Error",
				"status": 500,
				"detail": "DynamoDB ValidationException",
				"code":   "internal_error",
			},
			// no "meta" — simulates the missing request_id in body
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
	if apiErr.RequestID != "req_from_header" {
		t.Errorf("got request_id %q, want %q", apiErr.RequestID, "req_from_header")
	}
}

func TestAPIErrorRequestIDBodyTakesPrecedence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-ID", "req_from_header")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		resp := map[string]any{
			"error": map[string]any{
				"title":  "Bad Request",
				"status": 400,
				"code":   "invalid_request",
			},
			"meta": map[string]string{"request_id": "req_from_body"},
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
	// Body value should take precedence over header
	if apiErr.RequestID != "req_from_body" {
		t.Errorf("got request_id %q, want %q (body should take precedence)", apiErr.RequestID, "req_from_body")
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
		func(cl *Client) { cl.baseDelay = 1; cl.maxDelay = 1 },
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
		func(cl *Client) { cl.baseDelay = 1; cl.maxDelay = 1 },
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
		func(cl *Client) { cl.baseDelay = 1; cl.maxDelay = 1 },
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
