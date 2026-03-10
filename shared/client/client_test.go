package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreate(t *testing.T) {
	want := QURL{
		ID:        "qurl_123",
		ShortCode: "abc123",
		TargetURL: "https://example.com",
		LinkURL:   "https://qurl.link.layerv.xyz/abc123",
	}

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

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(want); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key")
	got, err := c.Create(context.Background(), CreateInput{TargetURL: "https://example.com"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if got.ID != want.ID {
		t.Errorf("got ID %q, want %q", got.ID, want.ID)
	}
	if got.ShortCode != want.ShortCode {
		t.Errorf("got ShortCode %q, want %q", got.ShortCode, want.ShortCode)
	}
}

func TestResolve(t *testing.T) {
	want := ResolveOutput{
		TargetURL:  "https://api.example.com/data",
		ResourceID: "r_abc123",
		SessionID:  "s_xyz789",
		AccessGrant: &AccessGrant{
			ExpiresIn: 305,
			GrantedAt: "2026-03-09T15:30:00Z",
			SrcIP:     "203.0.113.42",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/resolve" {
			t.Errorf("expected /v1/resolve, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %s", r.Header.Get("Authorization"))
		}

		var input ResolveInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if input.AccessToken != "at_testtoken123" {
			t.Errorf("expected access_token at_testtoken123, got %s", input.AccessToken)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(want); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key")
	got, err := c.Resolve(context.Background(), ResolveInput{AccessToken: "at_testtoken123"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got.TargetURL != want.TargetURL {
		t.Errorf("got TargetURL %q, want %q", got.TargetURL, want.TargetURL)
	}
	if got.ResourceID != want.ResourceID {
		t.Errorf("got ResourceID %q, want %q", got.ResourceID, want.ResourceID)
	}
	if got.AccessGrant == nil {
		t.Fatal("expected AccessGrant, got nil")
	}
	if got.AccessGrant.ExpiresIn != 305 {
		t.Errorf("got ExpiresIn %d, want 305", got.AccessGrant.ExpiresIn)
	}
}

func TestListCursorEscaping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		if cursor != "a=b&c=d" {
			t.Errorf("expected cursor 'a=b&c=d', got %q", cursor)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(ListOutput{}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key")
	_, err := c.List(context.Background(), ListInput{Limit: 10, Cursor: "a=b&c=d"})
	if err != nil {
		t.Fatalf("List with special cursor: %v", err)
	}
}

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		if err := json.NewEncoder(w).Encode(map[string]string{"message": "invalid api key"}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "bad-key")
	_, err := c.Get(context.Background(), "qurl_123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 401 {
		t.Errorf("got status %d, want 401", apiErr.StatusCode)
	}
}
