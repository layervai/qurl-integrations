package client

import (
	"context"
	"encoding/json"
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
		json.NewEncoder(w).Encode(want)
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

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"message": "invalid api key"})
	}))
	defer srv.Close()

	c := New(srv.URL, "bad-key")
	_, err := c.Get(context.Background(), "qurl_123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 401 {
		t.Errorf("got status %d, want 401", apiErr.StatusCode)
	}
}
