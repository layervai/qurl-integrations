package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/layervai/qurl-go/qurl"
)

const fixtureAdminKeyID = "key_Ab0123456789"
const fixtureAdminSecret = "lv_test_fixture_admin_key_123456789"

func fixtureAdminMetadata() APIKeyMetadata {
	return APIKeyMetadata{KeyID: fixtureAdminKeyID, Kind: "api_key", Name: "CI reader", Scopes: []string{"qurl:read"}, Status: "active"}
}

func TestAPIKeyManagementWireAndSecretBoundary(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-account-token" || r.Header.Get("X-QURL-Owner") != "auth0|fixture" {
			t.Error("lost explicit browser account authority")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			if r.URL.Path != "/v1/api-keys" || r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("status") != "all" || r.URL.Query().Get("cursor") != "opaque&+/#?" {
				t.Errorf("wrong list route: %s", r.URL)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"key_id": fixtureAdminKeyID, "kind": "future_kind", "name": "CI reader", "status": "revoked", "api_key": fixtureAdminSecret}}, "meta": map[string]any{"has_more": true, "next_cursor": "opaque-next"}})
		case http.MethodPost:
			if r.URL.Path != "/v1/api-keys" || r.Header.Get("Idempotency-Key") != strings.Repeat("a", 64) {
				t.Error("wrong create route or replay key")
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 3 || body["kind"] != "api_key" || body["name"] != "CI reader" {
				t.Errorf("wrong create body fields")
			}
			scopes, ok := body["scopes"].([]any)
			if !ok || len(scopes) != 1 || scopes[0] != "qurl:read" {
				t.Error("wrong explicit scopes")
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": CreatedAPIKey{APIKeyMetadata: fixtureAdminMetadata(), Secret: fixtureAdminSecret}})
		case http.MethodPatch:
			if r.URL.Path != "/v1/api-keys/"+fixtureAdminKeyID {
				t.Error("wrong update route")
			}
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["name"] != "Renamed" {
				t.Error("update changed fields other than name")
			}
			key := fixtureAdminMetadata()
			key.Name = "Renamed"
			_ = json.NewEncoder(w).Encode(map[string]any{"data": key})
		case http.MethodDelete:
			if r.URL.Path != "/v1/api-keys/"+fixtureAdminKeyID {
				t.Error("wrong revoke route")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	client, err := NewAPIKeyManagement(&Config{BaseURL: server.URL, APIKey: "fixture-account-token", OwnerID: "auth0|fixture"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.ListAPIKeys(context.Background(), APIKeyListOptions{Limit: 2, Status: "all", Cursor: "opaque&+/#?"})
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || page.NextCursor != "opaque-next" || len(page.Keys) != 1 || page.Keys[0].Kind != "future_kind" {
		t.Fatal("list lost pagination or an unknown future kind")
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), fixtureAdminSecret) || strings.Contains(string(raw), "api_key") {
		t.Fatal("list exposed an unexpected secret")
	}
	created, err := client.CreateAccountAPIKey(context.Background(), "CI reader", []string{"qurl:read"}, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if created.Secret != fixtureAdminSecret {
		t.Fatal("create lost the one-time secret")
	}
	updated, err := client.UpdateAccountAPIKey(context.Background(), fixtureAdminKeyID, "Renamed")
	if err != nil || updated == nil || updated.Name != "Renamed" {
		t.Fatalf("update failed: %v", err)
	}
	if err := client.RevokeAccountAPIKey(context.Background(), fixtureAdminKeyID); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatalf("got %d requests, want 4", calls.Load())
	}
}

func TestAPIKeyManagementValidationBeforeRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusBadRequest) }))
	defer server.Close()
	client, err := NewAPIKeyManagement(&Config{BaseURL: server.URL, APIKey: "fixture-account-token"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tests := []struct {
		name string
		run  func() error
	}{
		{"invalid list limit", func() error { _, e := client.ListAPIKeys(ctx, APIKeyListOptions{Limit: 101, Status: "all"}); return e }},
		{"invalid list status", func() error { _, e := client.ListAPIKeys(ctx, APIKeyListOptions{Limit: 1, Status: "hidden"}); return e }},
		{"empty create name", func() error {
			_, e := client.CreateAccountAPIKey(ctx, "", []string{"qurl:read"}, strings.Repeat("a", 64))
			return e
		}},
		{"control name", func() error {
			_, e := client.CreateAccountAPIKey(ctx, "CI\nforged", []string{"qurl:read"}, strings.Repeat("a", 64))
			return e
		}},
		{"long name", func() error {
			_, e := client.CreateAccountAPIKey(ctx, strings.Repeat("😀", 101), []string{"qurl:read"}, strings.Repeat("a", 64))
			return e
		}},
		{"empty scopes", func() error { _, e := client.CreateAccountAPIKey(ctx, "CI", nil, strings.Repeat("a", 64)); return e }},
		{"unknown scopes", func() error {
			_, e := client.CreateAccountAPIKey(ctx, "CI", []string{"admin:all"}, strings.Repeat("a", 64))
			return e
		}},
		{"duplicate scopes", func() error {
			_, e := client.CreateAccountAPIKey(ctx, "CI", []string{"qurl:read", "qurl:read"}, strings.Repeat("a", 64))
			return e
		}},
		{"short replay key", func() error { _, e := client.CreateAccountAPIKey(ctx, "CI", []string{"qurl:read"}, "bad"); return e }},
		{"invalid update ID", func() error { _, e := client.UpdateAccountAPIKey(ctx, "../other", "CI"); return e }},
		{"invalid update name", func() error { _, e := client.UpdateAccountAPIKey(ctx, fixtureAdminKeyID, "\x1b[31m"); return e }},
		{"invalid revoke ID", func() error { return client.RevokeAccountAPIKey(ctx, fixtureAdminSecret) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); !errors.Is(err, qurl.ErrInvalidResourceRequest) {
				t.Fatalf("want validation error, got %v", err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input reached the server")
	}
}

func TestAPIKeyCreateRejectsMismatchedReceipts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CreatedAPIKey)
	}{
		{"missing ID", func(k *CreatedAPIKey) { k.KeyID = "" }}, {"wrong kind", func(k *CreatedAPIKey) { k.Kind = "enrollment_token" }},
		{"wrong name", func(k *CreatedAPIKey) { k.Name = "Other" }}, {"extra scope", func(k *CreatedAPIKey) { k.Scopes = []string{"qurl:read", "qurl:write"} }},
		{"revoked", func(k *CreatedAPIKey) { k.Status = "revoked" }}, {"missing secret", func(k *CreatedAPIKey) { k.Secret = "" }},
		{"control secret", func(k *CreatedAPIKey) { k.Secret = fixtureAdminSecret + "\nforged" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := CreatedAPIKey{APIKeyMetadata: fixtureAdminMetadata(), Secret: fixtureAdminSecret}
			tt.mutate(&key)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": key})
			}))
			defer server.Close()
			client, err := NewAPIKeyManagement(&Config{BaseURL: server.URL, APIKey: "fixture-account-token"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.CreateAccountAPIKey(context.Background(), "CI reader", []string{"qurl:read"}, strings.Repeat("a", 64))
			if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatalf("want invalid receipt, got %v", err)
			}
			if strings.Contains(err.Error(), fixtureAdminSecret) {
				t.Fatal("invalid receipt leaked a secret")
			}
		})
	}
}

func TestAPIKeyListRejectsIncompletePages(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":null}`, `{"data":[{"key_id":"bad","kind":"api_key","status":"active"}]}`, `{"data":[],"meta":{"has_more":true}}`, `{"data":{}}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			client, err := NewAPIKeyManagement(&Config{BaseURL: server.URL, APIKey: "fixture-account-token"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ListAPIKeys(context.Background(), APIKeyListOptions{Limit: 1, Status: "active"})
			if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatalf("want invalid page, got %v", err)
			}
		})
	}
}

func TestAPIKeyUpdateAndRevokeDoNotReplay(t *testing.T) {
	for _, operation := range []string{"update", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			client, err := NewAPIKeyManagement(&Config{BaseURL: server.URL, APIKey: "fixture-account-token"})
			if err != nil {
				t.Fatal(err)
			}
			if operation == "update" {
				_, err = client.UpdateAccountAPIKey(context.Background(), fixtureAdminKeyID, "CI")
			} else {
				err = client.RevokeAccountAPIKey(context.Background(), fixtureAdminKeyID)
			}
			if err == nil || calls.Load() != 1 {
				t.Fatalf("mutation must return its failure after one attempt; calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestAPIKeyCreationRecoveryPreservesTheOriginalRequest(t *testing.T) {
	recoverable := false
	creates := 0
	var firstBody, firstKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid request JSON")
		}
		key := r.Header.Get("Idempotency-Key")
		if firstKey == "" {
			firstKey = key
			firstBody = string(body)
			creates++
		} else if key != firstKey || string(body) != firstBody {
			t.Error("recovery changed the creation identity or body")
		}
		if !recoverable {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": CreatedAPIKey{APIKeyMetadata: fixtureAdminMetadata(), Secret: fixtureAdminSecret}})
	}))
	defer server.Close()
	client, err := NewAPIKeyManagement(&Config{BaseURL: server.URL, APIKey: "fixture-account-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateAccountAPIKey(context.Background(), "CI reader", []string{"qurl:read"}, strings.Repeat("b", 64)); err == nil {
		t.Fatal("first unavailable result must not look successful")
	}
	recoverable = true
	key, err := client.CreateAccountAPIKey(context.Background(), "CI reader", []string{"qurl:read"}, strings.Repeat("b", 64))
	if err != nil || key == nil || key.Secret != fixtureAdminSecret || creates != 1 {
		t.Fatalf("could not recover original result: %v", err)
	}
}

func TestAccountKeyNamesMatchCurrentServiceByteLimit(t *testing.T) {
	for _, tt := range []struct {
		name  string
		valid bool
	}{{strings.Repeat("x", 100), true}, {strings.Repeat("x", 101), false}, {strings.Repeat("😀", 25), true}, {strings.Repeat("😀", 26), false}} {
		if err := ValidateAccountKeyName(tt.name); (err == nil) != tt.valid {
			t.Errorf("name with %d bytes: valid=%v err=%v", len(tt.name), tt.valid, err)
		}
	}
}

func TestAccountAdminAuthRejectsScopeInjectionBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusBadRequest) }))
	defer server.Close()
	for _, scopes := range [][]string{nil, {"qurl:read qurl:keys"}, {"qurl:read", "qurl:read"}, {"openid"}} {
		_, err := SignInAccountForManagement(context.Background(), &Config{BaseURL: server.URL}, func(context.Context, string) error { t.Error("invalid scopes opened browser"); return nil }, scopes)
		if !errors.Is(err, qurl.ErrInvalidResourceRequest) {
			t.Fatalf("invalid auth scopes accepted: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid scopes reached account settings endpoint")
	}
}
