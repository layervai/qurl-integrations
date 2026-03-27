package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
)

const apiKeysPath = "/v1/api-keys"

// runAuthCmd executes a CLI command without setting QURL_API_KEY,
// so that auth commands can test the unauthenticated state.
func runAuthCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := rootCmd("test")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// newAuth0MockServer creates a mock Auth0 server for device code flow.
// It returns the server and a cleanup function.
func newAuth0MockServer(t *testing.T, tokenResponse map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/oauth/device/code":
			if err := json.NewEncoder(w).Encode(map[string]any{
				"device_code":               "test-device-code",
				"user_code":                 "TEST-CODE",
				"verification_uri":          "https://test.auth0.com/activate",
				"verification_uri_complete": "https://test.auth0.com/activate?user_code=TEST-CODE",
				"expires_in":                900,
				"interval":                  1,
			}); err != nil {
				t.Fatalf("encode device code: %v", err)
			}

		case "/oauth/token":
			if tokenResponse != nil {
				if err := json.NewEncoder(w).Encode(tokenResponse); err != nil {
					t.Fatalf("encode token: %v", err)
				}
			} else {
				if err := json.NewEncoder(w).Encode(map[string]any{
					"access_token": "test-jwt-token",
					"token_type":   "Bearer",
					"expires_in":   86400,
				}); err != nil {
					t.Fatalf("encode token: %v", err)
				}
			}

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// newAPIKeyMockServer creates a mock QURL API server that handles
// POST /v1/api-keys and GET /v1/quota.
func newAPIKeyMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == apiKeysPath:
			authHeader := r.Header.Get("Authorization")
			if authHeader != "Bearer test-jwt-token" {
				w.WriteHeader(http.StatusUnauthorized)
				if err := json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": "unauthorized", "title": "Unauthorized"},
				}); err != nil {
					t.Fatalf("encode: %v", err)
				}
				return
			}
			w.WriteHeader(http.StatusCreated)
			apiEnvelope(t, w, map[string]any{
				"key_id":     "key_test123",
				"api_key":    "lv_live_testkey12345678",
				"key_prefix": "lv_live_test",
				"name":       "CLI (test)",
				"scopes":     []string{"qurl:read", "qurl:write", "qurl:resolve"},
				"status":     "active",
			})

		case r.Method == http.MethodGet && r.URL.Path == "/v1/quota":
			apiEnvelope(t, w, map[string]any{
				"plan":         "growth",
				"period_start": "2026-03-01T00:00:00Z",
				"period_end":   "2026-03-31T00:00:00Z",
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestAuthLoginSuccess(t *testing.T) {
	auth0Srv := newAuth0MockServer(t, nil)
	defer auth0Srv.Close()

	apiSrv := newAPIKeyMockServer(t)
	defer apiSrv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("QURL_API_KEY", "")
	t.Setenv("QURL_AUTH0_CLIENT_ID", "test-client-id")
	t.Setenv("QURL_AUTH0_URL", auth0Srv.URL)

	out, err := runAuthCmd(t,
		"--endpoint", apiSrv.URL,
		"auth", "login",
		"--no-browser",
	)
	if err != nil {
		t.Fatalf("auth login: %v\noutput: %s", err, out)
	}

	if !strings.Contains(out, "Logged in successfully") {
		t.Errorf("expected success message in output:\n%s", out)
	}
	if !strings.Contains(out, "lv_live_test...") {
		t.Errorf("expected key prefix in output:\n%s", out)
	}
	if !strings.Contains(out, "key_test123") {
		t.Errorf("expected key ID in output:\n%s", out)
	}
	if !strings.Contains(out, "TEST-CODE") {
		t.Errorf("expected user code in output:\n%s", out)
	}

	// Verify config was saved.
	cfg, loadErr := config.Load()
	if loadErr != nil {
		t.Fatalf("load config: %v", loadErr)
	}
	if cfg.APIKey != "lv_live_testkey12345678" {
		t.Errorf("config APIKey = %q, want %q", cfg.APIKey, "lv_live_testkey12345678")
	}
	if cfg.KeyID != "key_test123" {
		t.Errorf("config KeyID = %q, want %q", cfg.KeyID, "key_test123")
	}
}

func TestAuthLoginMissingClientID(t *testing.T) {
	t.Setenv("QURL_AUTH0_CLIENT_ID", "")

	_, err := runAuthCmd(t, "auth", "login", "--no-browser")
	if err == nil {
		t.Fatal("expected error for missing client ID")
	}
	if !strings.Contains(err.Error(), "Auth0 client ID") {
		t.Errorf("error should mention client ID: %v", err)
	}
}

func TestAuthLoginDeviceCodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		if err := json.NewEncoder(w).Encode(map[string]string{
			"error":             "unauthorized_client",
			"error_description": "Grant type not allowed",
		}); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	t.Setenv("QURL_AUTH0_CLIENT_ID", "test-client-id")
	t.Setenv("QURL_AUTH0_URL", srv.URL)

	_, err := runAuthCmd(t, "auth", "login", "--no-browser")
	if err == nil {
		t.Fatal("expected error for device code failure")
	}
}

func TestAuthLoginTokenPollExpired(t *testing.T) {
	auth0Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/device/code":
			if err := json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "test-code",
				"user_code":        "ABCD-EFGH",
				"verification_uri": "https://test.auth0.com/activate",
				"expires_in":       900,
				"interval":         1,
			}); err != nil {
				t.Fatalf("encode: %v", err)
			}
		case "/oauth/token":
			w.WriteHeader(http.StatusForbidden)
			if err := json.NewEncoder(w).Encode(map[string]string{
				"error":             "expired_token",
				"error_description": "The device code has expired",
			}); err != nil {
				t.Fatalf("encode: %v", err)
			}
		}
	}))
	defer auth0Srv.Close()

	t.Setenv("QURL_AUTH0_CLIENT_ID", "test-client-id")
	t.Setenv("QURL_AUTH0_URL", auth0Srv.URL)

	_, err := runAuthCmd(t, "auth", "login", "--no-browser")
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	if !strings.Contains(err.Error(), "expired_token") {
		t.Errorf("error should mention expiration: %v", err)
	}
}

func TestAuthLoginAPIKeyCreateFails(t *testing.T) {
	auth0Srv := newAuth0MockServer(t, nil)
	defer auth0Srv.Close()

	// API server that rejects key creation.
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := w.Write([]byte("internal error")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}))
	defer apiSrv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("QURL_API_KEY", "")
	t.Setenv("QURL_AUTH0_CLIENT_ID", "test-client-id")
	t.Setenv("QURL_AUTH0_URL", auth0Srv.URL)

	_, err := runAuthCmd(t,
		"--endpoint", apiSrv.URL,
		"auth", "login",
		"--no-browser",
	)
	if err == nil {
		t.Fatal("expected error for API key creation failure")
	}
}

func TestAuthLoginCustomKeyName(t *testing.T) {
	var receivedName string

	auth0Srv := newAuth0MockServer(t, nil)
	defer auth0Srv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == apiKeysPath {
			var req struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
				receivedName = req.Name
			}
			w.WriteHeader(http.StatusCreated)
			apiEnvelope(t, w, map[string]any{
				"key_id":     "key_custom",
				"api_key":    "lv_live_customkey123456",
				"key_prefix": "lv_live_cust",
				"name":       req.Name,
				"scopes":     []string{"qurl:read", "qurl:write", "qurl:resolve"},
				"status":     "active",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer apiSrv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("QURL_API_KEY", "")
	t.Setenv("QURL_AUTH0_CLIENT_ID", "test-client-id")
	t.Setenv("QURL_AUTH0_URL", auth0Srv.URL)

	_, err := runAuthCmd(t,
		"--endpoint", apiSrv.URL,
		"auth", "login",
		"--no-browser",
		"--key-name", "my-laptop",
	)
	if err != nil {
		t.Fatalf("auth login: %v", err)
	}
	if receivedName != "my-laptop" {
		t.Errorf("key name = %q, want %q", receivedName, "my-laptop")
	}
}

func TestAuthLoginCustomScopes(t *testing.T) {
	var receivedScopes []string

	auth0Srv := newAuth0MockServer(t, nil)
	defer auth0Srv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == apiKeysPath {
			var req struct {
				Scopes []string `json:"scopes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
				receivedScopes = req.Scopes
			}
			w.WriteHeader(http.StatusCreated)
			apiEnvelope(t, w, map[string]any{
				"key_id":     "key_scoped",
				"api_key":    "lv_live_scopedkey12345",
				"key_prefix": "lv_live_scop",
				"scopes":     req.Scopes,
				"status":     "active",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer apiSrv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("QURL_API_KEY", "")
	t.Setenv("QURL_AUTH0_CLIENT_ID", "test-client-id")
	t.Setenv("QURL_AUTH0_URL", auth0Srv.URL)

	_, err := runAuthCmd(t,
		"--endpoint", apiSrv.URL,
		"auth", "login",
		"--no-browser",
		"--scopes", "qurl:read",
	)
	if err != nil {
		t.Fatalf("auth login: %v", err)
	}
	if len(receivedScopes) != 1 || receivedScopes[0] != "qurl:read" {
		t.Errorf("scopes = %v, want [qurl:read]", receivedScopes)
	}
}

func TestAuthLoginInvalidScopes(t *testing.T) {
	t.Setenv("QURL_AUTH0_CLIENT_ID", "test-client-id")

	_, err := runAuthCmd(t, "auth", "login", "--no-browser", "--scopes", "invalid:scope")
	if err == nil {
		t.Fatal("expected error for invalid scope")
	}
	if !strings.Contains(err.Error(), "unknown scope") {
		t.Errorf("error should mention unknown scope: %v", err)
	}
}

func TestAuthLogoutSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("QURL_API_KEY", "")

	// Write a config with credentials.
	cfg := &config.Config{APIKey: "lv_live_test123", KeyID: "key_test"}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	out, err := runAuthCmd(t, "auth", "logout")
	if err != nil {
		t.Fatalf("auth logout: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Logged out") {
		t.Errorf("expected logout message:\n%s", out)
	}

	// Verify credentials cleared.
	loaded, loadErr := config.Load()
	if loadErr != nil {
		t.Fatalf("load config: %v", loadErr)
	}
	if loaded.APIKey != "" {
		t.Errorf("APIKey should be empty after logout, got %q", loaded.APIKey)
	}
	if loaded.KeyID != "" {
		t.Errorf("KeyID should be empty after logout, got %q", loaded.KeyID)
	}
}

func TestAuthLogoutNotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("QURL_API_KEY", "")

	out, err := runAuthCmd(t, "auth", "logout")
	if err != nil {
		t.Fatalf("auth logout: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Not logged in") {
		t.Errorf("expected 'Not logged in' message:\n%s", out)
	}
}

func TestAuthStatusAuthenticated(t *testing.T) {
	apiSrv := newAPIKeyMockServer(t)
	defer apiSrv.Close()

	t.Setenv("QURL_API_KEY", "lv_live_testkey12345678")

	out, err := runAuthCmd(t, "--endpoint", apiSrv.URL, "auth", "status")
	if err != nil {
		t.Fatalf("auth status: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Authenticated") {
		t.Errorf("expected 'Authenticated' in output:\n%s", out)
	}
	if !strings.Contains(out, "lv_live_test...") {
		t.Errorf("expected key prefix in output:\n%s", out)
	}
	if !strings.Contains(out, "QURL_API_KEY") {
		t.Errorf("expected source in output:\n%s", out)
	}
	if !strings.Contains(out, "GROWTH") {
		t.Errorf("expected plan in output:\n%s", out)
	}
}

func TestAuthStatusNotAuthenticated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("QURL_API_KEY", "")

	out, err := runAuthCmd(t, "auth", "status")
	if err != nil {
		t.Fatalf("auth status: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Not authenticated") {
		t.Errorf("expected 'Not authenticated' in output:\n%s", out)
	}
	if !strings.Contains(out, "qurl auth login") {
		t.Errorf("expected login hint in output:\n%s", out)
	}
}

func TestAuthStatusFromConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("QURL_API_KEY", "")

	apiSrv := newAPIKeyMockServer(t)
	defer apiSrv.Close()

	cfg := &config.Config{APIKey: "lv_live_fromcfg12345", KeyID: "key_cfg"}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	out, err := runAuthCmd(t, "--endpoint", apiSrv.URL, "auth", "status")
	if err != nil {
		t.Fatalf("auth status: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Authenticated") {
		t.Errorf("expected 'Authenticated':\n%s", out)
	}
	if !strings.Contains(out, "config file") {
		t.Errorf("expected source 'config file':\n%s", out)
	}
}
