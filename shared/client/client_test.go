package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const testDescription = "updated"
const testResourceID = "r_abc123test"

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
		// Verify the Description→Label rename is wired correctly at the wire level.
		if input.Label != "test" {
			t.Errorf("expected label 'test', got %q (Description→Label rename may be broken)", input.Label)
		}

		apiEnvelope(t, w, map[string]any{
			"qurl_id":     "q_abc123test",
			"resource_id": testResourceID,
			"qurl_link":   "https://qurl.link/at_abc123",
			"qurl_site":   "https://r_abc123test.qurl.site",
			"label":       "test",
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.Create(context.Background(), &CreateInput{TargetURL: "https://example.com", Label: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got.ResourceID != testResourceID {
		t.Errorf("got ResourceID %q, want %q", got.ResourceID, testResourceID)
	}
	if got.QURLLink != "https://qurl.link/at_abc123" {
		t.Errorf("got QURLLink %q, want %q", got.QURLLink, "https://qurl.link/at_abc123")
	}
	if got.QURLID != "q_abc123test" {
		t.Errorf("got QURLID %q, want %q", got.QURLID, "q_abc123test")
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
		if r.URL.Path != "/v1/qurls/"+testResourceID {
			t.Errorf("expected /v1/qurls/%s, got %s", testResourceID, r.URL.Path)
		}
		apiEnvelope(t, w, map[string]any{
			"resource_id": testResourceID,
			"target_url":  "https://example.com",
			"status":      "active",
			"tags":        []string{"test", "api"},
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.Get(context.Background(), testResourceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ResourceID != testResourceID {
		t.Errorf("got ResourceID %q, want %q", got.ResourceID, testResourceID)
	}
	if got.Status != "active" {
		t.Errorf("got Status %q, want %q", got.Status, "active")
	}
	if len(got.Tags) != 2 || got.Tags[0] != "test" {
		t.Errorf("got Tags %v, want [test api]", got.Tags)
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
	got, err := c.List(context.Background(), &ListInput{Limit: 5, Status: "active"})
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
	_, err := c.List(context.Background(), &ListInput{Limit: 10, Cursor: "a=b&c=d"})
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
			"resource_id": testResourceID,
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
		if r.URL.Path != "/v1/qurls/"+testResourceID+"/mint_link" {
			t.Errorf("expected mint_link path, got %s", r.URL.Path)
		}
		// nil input must produce a bodiless POST so the server can distinguish
		// "mint with QURL defaults" from "mint with an explicit (empty) override".
		if r.ContentLength != 0 {
			t.Errorf("expected no body for nil MintLinkInput, got ContentLength=%d", r.ContentLength)
		}
		apiEnvelope(t, w, map[string]any{
			"qurl_link": "https://qurl.link/at_newtoken",
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.MintLink(context.Background(), testResourceID, nil)
	if err != nil {
		t.Fatalf("MintLink: %v", err)
	}
	if got.QURLLink != "https://qurl.link/at_newtoken" {
		t.Errorf("got QURLLink %q", got.QURLLink)
	}
}

func TestMintLinkContentType(t *testing.T) {
	t.Run("nil input has no Content-Type", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ct := r.Header.Get("Content-Type"); ct != "" {
				t.Errorf("expected no Content-Type header for bodiless POST, got %q", ct)
			}
			apiEnvelope(t, w, map[string]any{"qurl_link": "https://qurl.link/at_test"})
		}))
		t.Cleanup(srv.Close)

		c := testClient(srv.URL, "test-key")
		if _, err := c.MintLink(context.Background(), testResourceID, nil); err != nil {
			t.Fatalf("MintLink: %v", err)
		}
	})

	t.Run("non-nil input has Content-Type application/json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("expected Content-Type application/json, got %q", ct)
			}
			apiEnvelope(t, w, map[string]any{"qurl_link": "https://qurl.link/at_test"})
		}))
		t.Cleanup(srv.Close)

		c := testClient(srv.URL, "test-key")
		if _, err := c.MintLink(context.Background(), testResourceID, &MintLinkInput{Label: "x"}); err != nil {
			t.Fatalf("MintLink: %v", err)
		}
	})
}

// TestMintLinkOmitEmpty verifies that MintLinkInput fields with zero values are
// stripped by omitempty, so &MintLinkInput{} marshals to {} rather than a struct
// with false/0/null fields. This locks the contract mint.go relies on when
// omitting the Changed() gate for bool/int flags.
func TestMintLinkOmitEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if got := strings.TrimSpace(string(body)); got != "{}" {
			t.Errorf("expected body {}, got %q", got)
		}
		apiEnvelope(t, w, map[string]any{"qurl_link": "https://qurl.link/at_test"})
	}))
	t.Cleanup(srv.Close)

	c := testClient(srv.URL, "test-key")
	if _, err := c.MintLink(context.Background(), testResourceID, &MintLinkInput{}); err != nil {
		t.Fatalf("MintLink: %v", err)
	}
}

func TestBatchCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/qurls/batch" {
			t.Errorf("expected /v1/qurls/batch, got %s", r.URL.Path)
		}

		var payload struct {
			Items []CreateInput `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(payload.Items) != 2 {
			t.Errorf("expected 2 items, got %d", len(payload.Items))
		}

		apiEnvelope(t, w, map[string]any{
			"succeeded": 2,
			"failed":    0,
			"results": []map[string]any{
				{"index": 0, "success": true, "resource_id": "r_1", "qurl_link": "https://qurl.link/at_1"},
				{"index": 1, "success": true, "resource_id": "r_2", "qurl_link": "https://qurl.link/at_2"},
			},
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.BatchCreate(context.Background(), []*CreateInput{
		{TargetURL: "https://a.com"},
		{TargetURL: "https://b.com"},
	})
	if err != nil {
		t.Fatalf("BatchCreate: %v", err)
	}
	if got.Succeeded != 2 {
		t.Errorf("got Succeeded %d, want 2", got.Succeeded)
	}
	if len(got.Results) != 2 {
		t.Errorf("got %d results, want 2", len(got.Results))
	}
}

func TestListWithDateFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("created_after") != "2026-01-01T00:00:00Z" {
			t.Errorf("expected created_after, got %q", r.URL.Query().Get("created_after"))
		}
		if r.URL.Query().Get("created_before") != "2026-06-01T00:00:00Z" {
			t.Errorf("expected created_before, got %q", r.URL.Query().Get("created_before"))
		}
		if r.URL.Query().Get("expires_before") != "2026-12-31T23:59:59Z" {
			t.Errorf("expected expires_before, got %q", r.URL.Query().Get("expires_before"))
		}
		if r.URL.Query().Get("expires_after") != "2026-07-01T00:00:00Z" {
			t.Errorf("expected expires_after, got %q", r.URL.Query().Get("expires_after"))
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
	_, err := c.List(context.Background(), &ListInput{
		CreatedAfter:  "2026-01-01T00:00:00Z",
		CreatedBefore: "2026-06-01T00:00:00Z",
		ExpiresBefore: "2026-12-31T23:59:59Z",
		ExpiresAfter:  "2026-07-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("List with date filters: %v", err)
	}
}

func TestMintLinkWithInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/qurls/"+testResourceID+"/mint_link" {
			t.Errorf("expected mint_link path, got %s", r.URL.Path)
		}

		var input MintLinkInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if input.Label != "test-link" {
			t.Errorf("expected label 'test-link', got %q", input.Label)
		}
		if !input.OneTimeUse {
			t.Error("expected one_time_use=true")
		}

		apiEnvelope(t, w, map[string]any{
			"qurl_link":  "https://qurl.link/at_minted",
			"expires_at": "2026-04-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.MintLink(context.Background(), testResourceID, &MintLinkInput{
		Label:      "test-link",
		OneTimeUse: true,
	})
	if err != nil {
		t.Fatalf("MintLink: %v", err)
	}
	if got.QURLLink != "https://qurl.link/at_minted" {
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
		if r.URL.Path != "/v1/qurls/"+testResourceID {
			t.Errorf("expected /v1/qurls/%s, got %s", testResourceID, r.URL.Path)
		}

		var input UpdateInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if input.Description == nil || *input.Description != testDescription {
			t.Errorf("expected description 'updated', got %v", input.Description)
		}

		apiEnvelope(t, w, map[string]any{
			"resource_id": testResourceID,
			"target_url":  "https://example.com",
			"status":      "active",
			"description": testDescription,
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	desc := testDescription
	got, err := c.Update(context.Background(), testResourceID, UpdateInput{Description: &desc})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Description != testDescription {
		t.Errorf("got Description %q, want %q", got.Description, testDescription)
	}
}

func TestUpdateTags(t *testing.T) {
	tests := []struct {
		name     string
		tags     *[]string
		wantJSON string
	}{
		{
			name:     "set tags",
			tags:     &[]string{"prod", "api"},
			wantJSON: `["prod","api"]`,
		},
		{
			name:     "clear tags",
			tags:     &[]string{},
			wantJSON: `[]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				gotTags, ok := body["tags"]
				if !ok {
					t.Errorf("expected tags field in body, got %v", body)
				} else if string(gotTags) != tt.wantJSON {
					t.Errorf("got tags %s, want %s", gotTags, tt.wantJSON)
				}

				apiEnvelope(t, w, map[string]any{
					"resource_id": testResourceID,
					"target_url":  "https://example.com",
					"status":      "active",
				})
			}))
			t.Cleanup(srv.Close)

			c := testClient(srv.URL, "test-key")
			_, err := c.Update(context.Background(), testResourceID, UpdateInput{Tags: tt.tags})
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
		})
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
	_, err := c.Get(context.Background(), testResourceID)
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
	_, err := c.Get(context.Background(), testResourceID)

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

func TestListNilInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	got, err := c.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List(nil): %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil output")
	}
}

func TestExtend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}

		var input UpdateInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if input.ExtendBy != "24h" {
			t.Errorf("expected extend_by '24h', got %q", input.ExtendBy)
		}

		apiEnvelope(t, w, map[string]any{
			"resource_id": testResourceID,
			"target_url":  "https://example.com",
			"status":      "active",
			"expires_at":  "2026-04-02T00:00:00Z",
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.Extend(context.Background(), testResourceID, "24h")
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if got.ResourceID != testResourceID {
		t.Errorf("got ResourceID %q, want %q", got.ResourceID, testResourceID)
	}
	if got.ExpiresAt == nil {
		t.Error("expected ExpiresAt to be set")
	}
}

func TestBatchCreatePartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiEnvelope(t, w, map[string]any{
			"succeeded": 1,
			"failed":    1,
			"results": []map[string]any{
				{"index": 0, "success": true, "resource_id": "r_1", "qurl_link": "https://qurl.link/at_1"},
				{"index": 1, "success": false, "error": map[string]string{"code": "invalid_url", "message": "target_url is not valid"}},
			},
		})
	}))
	defer srv.Close()

	c := testClient(srv.URL, "test-key")
	got, err := c.BatchCreate(context.Background(), []*CreateInput{
		{TargetURL: "https://valid.com"},
		{TargetURL: "not-a-url"},
	})
	if err != nil {
		t.Fatalf("BatchCreate: %v", err)
	}
	if got.Succeeded != 1 {
		t.Errorf("got Succeeded %d, want 1", got.Succeeded)
	}
	if got.Failed != 1 {
		t.Errorf("got Failed %d, want 1", got.Failed)
	}
	if len(got.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(got.Results))
	}
	if got.Results[1].Error == nil {
		t.Fatal("expected error on second result")
	}
	if got.Results[1].Error.Code != "invalid_url" {
		t.Errorf("got error code %q, want %q", got.Results[1].Error.Code, "invalid_url")
	}
}

func TestBatchCreateValidation(t *testing.T) {
	c := testClient("http://unused", "test-key")

	_, err := c.BatchCreate(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for empty batch")
	}

	_, err = c.BatchCreate(context.Background(), []*CreateInput{})
	if err == nil {
		t.Fatal("expected error for empty batch")
	}

	items := make([]*CreateInput, maxBatchSize+1)
	for i := range items {
		items[i] = &CreateInput{TargetURL: "https://example.com"}
	}
	_, err = c.BatchCreate(context.Background(), items)
	if err == nil {
		t.Fatalf("expected error for batch > %d", maxBatchSize)
	}
}

func TestCreateNilInput(t *testing.T) {
	c := testClient("http://unused", "test-key")
	_, err := c.Create(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil create input")
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
	if err := c.Delete(context.Background(), testResourceID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
