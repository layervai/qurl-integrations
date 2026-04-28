package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateAPIKeySuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api-keys" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-jwt" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-jwt")
		}
		if got := r.Header.Get("Content-Type"); got != jsonContentType {
			t.Errorf("Content-Type = %q, want %q", got, jsonContentType)
		}

		var req CreateKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Name != "CLI (test)" {
			t.Errorf("Name = %q, want %q", req.Name, "CLI (test)")
		}
		if len(req.Scopes) != 2 || req.Scopes[0] != "qurl:read" {
			t.Errorf("Scopes = %v", req.Scopes)
		}

		w.Header().Set("Content-Type", jsonContentType)
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(createKeyEnvelope{
			Data: CreateKeyResponse{
				KeyID:     "key_abc123",
				APIKey:    "lv_live_testkeyabc123",
				KeyPrefix: "lv_live_test",
				Name:      "CLI (test)",
				Scopes:    []string{"qurl:read", "qurl:write"},
				Status:    "active",
			},
		}); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	resp, err := CreateAPIKey(context.Background(), nil, srv.URL, "test-jwt", CreateKeyRequest{
		Name:   "CLI (test)",
		Scopes: []string{"qurl:read", "qurl:write"},
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if resp.KeyID != "key_abc123" {
		t.Errorf("KeyID = %q, want %q", resp.KeyID, "key_abc123")
	}
	if resp.APIKey != "lv_live_testkeyabc123" {
		t.Errorf("APIKey = %q, want %q", resp.APIKey, "lv_live_testkeyabc123")
	}
	if resp.KeyPrefix != "lv_live_test" {
		t.Errorf("KeyPrefix = %q, want %q", resp.KeyPrefix, "lv_live_test")
	}
}

func TestCreateAPIKeyUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		if _, err := w.Write([]byte(`{"error":"unauthorized"}`)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}))
	defer srv.Close()

	_, err := CreateAPIKey(context.Background(), nil, srv.URL, "bad-jwt", CreateKeyRequest{
		Name:   "test",
		Scopes: []string{"qurl:read"},
	})
	if err == nil {
		t.Fatal("expected error for 401")
	}
}

func TestCreateAPIKeyForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		if _, err := w.Write([]byte(`{"error":"API keys cannot manage API keys"}`)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}))
	defer srv.Close()

	_, err := CreateAPIKey(context.Background(), nil, srv.URL, "some-api-key", CreateKeyRequest{
		Name:   "test",
		Scopes: []string{"qurl:read"},
	})
	if err == nil {
		t.Fatal("expected error for 403")
	}
}

func TestCreateAPIKeyServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := w.Write([]byte("internal error")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}))
	defer srv.Close()

	_, err := CreateAPIKey(context.Background(), nil, srv.URL, "test-jwt", CreateKeyRequest{
		Name:   "test",
		Scopes: []string{"qurl:read"},
	})
	if err == nil {
		t.Fatal("expected error for 500")
	}
}

func TestCreateAPIKeyDefaultHTTPClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", jsonContentType)
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(createKeyEnvelope{
			Data: CreateKeyResponse{
				KeyID:  "key_default",
				APIKey: "lv_live_default",
			},
		}); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	// Pass nil httpClient to verify default is used.
	resp, err := CreateAPIKey(context.Background(), nil, srv.URL, "jwt", CreateKeyRequest{
		Name:   "test",
		Scopes: []string{"qurl:read"},
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if resp.KeyID != "key_default" {
		t.Errorf("KeyID = %q, want %q", resp.KeyID, "key_default")
	}
}
