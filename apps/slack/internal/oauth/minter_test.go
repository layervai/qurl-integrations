package oauth

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

// mintAPIKeyOnlyErr is a test helper that discards the three return values
// the success-path tests assert on. Test error-only branches use it so
// they aren't tripped by dogsled.
func mintAPIKeyOnlyErr(m *HTTPAPIKeyMinter) error {
	_, _, _, err := m.MintAPIKey(context.Background(), "tok", "name", nil) //nolint:dogsled // intentional discard on error-only paths.
	return err
}

func TestHTTPAPIKeyMinterMintHappyPath(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{
				"api_key":    testAPIKey,
				"key_id":     testKeyID,
				"key_prefix": testKeyPrefix,
			},
		})
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	apiKey, keyID, keyPrefix, err := m.MintAPIKey(context.Background(), "tok", "ws T1", []string{"qurl:read", "qurl:write"})
	if err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}
	if apiKey != testAPIKey || keyID != testKeyID || keyPrefix != testKeyPrefix {
		t.Errorf("unexpected fields: %q %q %q", apiKey, keyID, keyPrefix)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q want POST", gotMethod)
	}
	if gotPath != "/v1/api-keys" {
		t.Errorf("path: got %q want /v1/api-keys", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth: got %q want Bearer tok", gotAuth)
	}
}

func TestHTTPAPIKeyMinterTolerateBaseURLTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the path doesn't get the double-slash treatment ("//v1").
		if r.URL.Path != "/v1/api-keys" {
			http.Error(w, "wrong path "+r.URL.Path, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"api_key": "k", "key_id": "id", "key_prefix": "p"},
		})
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL + "/", HTTPClient: srv.Client()}
	if _, _, _, err := m.MintAPIKey(context.Background(), "tok", "name", nil); err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}
}

func TestHTTPAPIKeyMinterMintNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"forbidden"}`)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintAPIKeyOnlyErr(m)
	if err == nil {
		t.Fatal("expected error on 4xx")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected status code in error, got %q", err.Error())
	}
}

func TestHTTPAPIKeyMinterMintMissingKeyID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Note: api_key present, key_id missing → unable-to-revoke later
		// so the minter must refuse rather than return ("lv_…", "", …).
		_, _ = io.WriteString(w, `{"data":{"api_key":"lv_xxx","key_prefix":"lv_x"}}`)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintAPIKeyOnlyErr(m)
	if err == nil {
		t.Fatal("expected error when key_id is missing")
	}
}

func TestHTTPAPIKeyMinterRevokeHappyPath(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	if err := m.RevokeAPIKey(context.Background(), "tok", "k_1"); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method: got %q want DELETE", gotMethod)
	}
	if gotPath != "/v1/api-keys/k_1" {
		t.Errorf("path: got %q want /v1/api-keys/k_1", gotPath)
	}
}

func TestHTTPAPIKeyMinterRevokeEmptyKeyID(t *testing.T) {
	m := &HTTPAPIKeyMinter{BaseURL: "http://example.invalid"}
	err := m.RevokeAPIKey(context.Background(), "tok", "")
	if err == nil {
		t.Fatal("expected error for empty keyID")
	}
}

func TestHTTPAPIKeyMinterRevokeNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	if err := m.RevokeAPIKey(context.Background(), "tok", "k_1"); err == nil {
		t.Fatal("expected error on 5xx")
	}
}

// TestHTTPAPIKeyMinterMintParseFailure exercises the json.Unmarshal
// error path — qurl-service returning non-JSON 200 (e.g. a CDN error
// page) should surface as a wrapped error rather than a zero-value
// quiet success.
func TestHTTPAPIKeyMinterMintParseFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html>not json</html>")
	}))
	t.Cleanup(srv.Close)
	m := &HTTPAPIKeyMinter{BaseURL: srv.URL, HTTPClient: srv.Client()}
	err := mintAPIKeyOnlyErr(m)
	if err == nil {
		t.Fatal("expected error on non-JSON 200")
	}
	// The wrapped error chain must preserve a json.SyntaxError so callers
	// (and future tests) can errors.As on it. Pinning prevents a refactor
	// that swaps json.Unmarshal for a string-only error message.
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Errorf("expected json.SyntaxError in chain, got %v", err)
	}
}
