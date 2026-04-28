package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
)

// safeBuffer is a thread-safe bytes.Buffer for concurrent read/write in tests.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const apiKeysPath = "/v1/api-keys"
const quotaPath = "/v1/quota"

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

// newTokenMockServer creates a mock Auth0 server that handles only /oauth/token.
func newTokenMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth/token" {
			if err := json.NewEncoder(w).Encode(map[string]any{
				"access_token": "test-jwt-token",
				"token_type":   "Bearer",
				"expires_in":   86400,
			}); err != nil {
				t.Fatalf("encode token: %v", err)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

// newAPIKeyMockServer creates a mock qURL API server that handles
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

		case r.Method == http.MethodGet && r.URL.Path == quotaPath:
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

// runLoginWithCallback runs the auth login command in a goroutine,
// polls stderr to find the auth URL, simulates the browser callback,
// and waits for the command to finish.
func runLoginWithCallback(t *testing.T, auth0URL, apiURL string, extraArgs ...string) (stdout string, cmdErr error) {
	t.Helper()

	cmd := rootCmd("test")
	// stdout uses safeBuffer because we read it from the main goroutine
	// while the command goroutine may still be writing.
	stdoutBuf := &safeBuffer{}
	stderrBuf := &safeBuffer{}
	cmd.SetOut(stdoutBuf)
	cmd.SetErr(stderrBuf)

	args := make([]string, 0, 5+len(extraArgs))
	args = append(args, "--endpoint", apiURL, "auth", "login", "--no-browser")
	args = append(args, extraArgs...)
	cmd.SetArgs(args)

	// Run command in goroutine.
	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Execute()
	}()

	// Poll stderr until we find the auth URL.
	// The command is blocked on WaitForToken, so errCh won't fire until after we
	// send the callback. We only need to wait for stderr output.
	var authURLStr string
	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for authURLStr == "" {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for auth URL in stderr:\n%s", stderrBuf.String())
		case <-ticker.C:
			authURLStr = extractAuthURL(stderrBuf.String())
		}
	}

	// Parse redirect_uri and state from the auth URL.
	u, parseErr := url.Parse(authURLStr)
	if parseErr != nil {
		t.Fatalf("parse auth URL %q: %v", authURLStr, parseErr)
	}
	redirectURI := u.Query().Get("redirect_uri")
	state := u.Query().Get("state")

	if redirectURI == "" || state == "" {
		t.Fatalf("missing redirect_uri or state in auth URL: %s", authURLStr)
	}

	// Simulate the browser callback.
	callbackURL := redirectURI + "?code=test-auth-code&state=" + url.QueryEscape(state)
	callbackReq, reqErr := http.NewRequestWithContext(context.Background(), http.MethodGet, callbackURL, http.NoBody)
	if reqErr != nil {
		t.Fatalf("build callback request: %v", reqErr)
	}
	resp, httpErr := http.DefaultClient.Do(callbackReq)
	if httpErr != nil {
		t.Fatalf("callback request: %v", httpErr)
	}
	_ = resp.Body.Close()

	// Wait for the command to finish before reading stdout.
	// Note: <-errCh must be evaluated BEFORE stdoutBuf.String() —
	// Go evaluates "return a(), b()" left-to-right, so using a temporary.
	cmdErr = <-errCh
	return stdoutBuf.String(), cmdErr
}

// authURLRe matches a line containing an authorization URL (https://…/authorize?…).
// Anchored to the start of a line (after optional whitespace) to prevent false
// positives from log lines that contain "http" before the URL token.
var authURLRe = regexp.MustCompile(`(?m)^\s*(https?://\S+/authorize\?\S+)`)

// extractAuthURL finds and returns the authorization URL from command output.
func extractAuthURL(output string) string {
	m := authURLRe.FindStringSubmatch(output)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func TestAuthLoginSuccess(t *testing.T) {
	tokenSrv := newTokenMockServer(t)
	defer tokenSrv.Close()
	apiSrv := newAPIKeyMockServer(t)
	defer apiSrv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("QURL_API_KEY", "")
	t.Setenv("QURL_AUTH0_CLIENT_ID", "test-client-id")
	t.Setenv("QURL_AUTH0_URL", tokenSrv.URL)

	out, err := runLoginWithCallback(t, tokenSrv.URL, apiSrv.URL)
	if err != nil {
		t.Fatalf("auth login: %v\noutput: %s", err, out)
	}

	if !strings.Contains(out, "lv_live_test...") {
		t.Errorf("expected key prefix in output:\n%s", out)
	}
	if !strings.Contains(out, "key_test123") {
		t.Errorf("expected key ID in output:\n%s", out)
	}
	if !strings.Contains(out, "qurl:read") {
		t.Errorf("expected scopes in output:\n%s", out)
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
	if !strings.Contains(err.Error(), "auth0 client ID") {
		t.Errorf("error should mention client ID: %v", err)
	}
}

func TestAuthLoginAPIKeyCreateFails(t *testing.T) {
	tokenSrv := newTokenMockServer(t)
	defer tokenSrv.Close()

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
	t.Setenv("QURL_AUTH0_URL", tokenSrv.URL)

	_, err := runLoginWithCallback(t, tokenSrv.URL, apiSrv.URL)
	if err == nil {
		t.Fatal("expected error for API key creation failure")
	}
}

func TestAuthLoginCustomKeyName(t *testing.T) {
	// mu guards receivedName against concurrent access from the HTTP handler
	// goroutine (writer) and the test goroutine (reader).
	var mu sync.Mutex
	var receivedName string

	tokenSrv := newTokenMockServer(t)
	defer tokenSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == apiKeysPath {
			var req struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
				mu.Lock()
				receivedName = req.Name
				mu.Unlock()
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
	t.Setenv("QURL_AUTH0_URL", tokenSrv.URL)

	_, err := runLoginWithCallback(t, tokenSrv.URL, apiSrv.URL, "--key-name", "my-laptop")
	if err != nil {
		t.Fatalf("auth login: %v", err)
	}
	mu.Lock()
	name := receivedName
	mu.Unlock()
	if name != "my-laptop" {
		t.Errorf("key name = %q, want %q", name, "my-laptop")
	}
}

func TestAuthLoginCustomScopes(t *testing.T) {
	// mu guards receivedScopes against concurrent access from the HTTP handler
	// goroutine (writer) and the test goroutine (reader).
	var mu sync.Mutex
	var receivedScopes []string

	tokenSrv := newTokenMockServer(t)
	defer tokenSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == apiKeysPath {
			var req struct {
				Scopes []string `json:"scopes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
				mu.Lock()
				receivedScopes = req.Scopes
				mu.Unlock()
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
	t.Setenv("QURL_AUTH0_URL", tokenSrv.URL)

	_, err := runLoginWithCallback(t, tokenSrv.URL, apiSrv.URL, "--scopes", "qurl:read")
	if err != nil {
		t.Fatalf("auth login: %v", err)
	}
	mu.Lock()
	scopes := receivedScopes
	mu.Unlock()
	if len(scopes) != 1 || scopes[0] != "qurl:read" {
		t.Errorf("scopes = %v, want [qurl:read]", scopes)
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
	t.Setenv("HOME", t.TempDir())
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

func TestAuthStatusQuotaUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Mock server that returns 401 on /v1/quota to simulate a revoked key.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == quotaPath {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","title":"Unauthorized"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	t.Setenv("QURL_API_KEY", "lv_live_testkey12345678")

	out, err := runAuthCmd(t, "--endpoint", srv.URL, "auth", "status")
	if err != nil {
		t.Fatalf("auth status: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Authenticated") {
		t.Errorf("expected 'Authenticated' in output:\n%s", out)
	}
	if !strings.Contains(out, "unavailable") {
		t.Errorf("expected 'unavailable' in output when quota fetch fails:\n%s", out)
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

func TestAuthLoginMalformedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("QURL_API_KEY", "")
	t.Setenv("QURL_AUTH0_CLIENT_ID", "test-client-id")

	// Write a syntactically invalid config file. The preflight config check in
	// runAuthLogin should fail fast — before the OAuth flow starts — so the user
	// gets an error immediately rather than after completing the browser flow.
	cfgDir := filepath.Join(home, ".config", "qurl")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("api_key: [broken yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := runAuthCmd(t, "auth", "login", "--no-browser")
	if err == nil {
		t.Fatal("expected error for malformed config")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Errorf("error should mention 'load config': %v", err)
	}
}

func TestAuthLogoutMalformedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("QURL_API_KEY", "")

	// Write a syntactically invalid config file.
	cfgDir := filepath.Join(home, ".config", "qurl")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("api_key: [broken yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := runAuthCmd(t, "auth", "logout")
	if err == nil {
		t.Fatal("expected error for malformed config")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Errorf("error should mention 'load config': %v", err)
	}
}

func TestAuthStatusMalformedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("QURL_API_KEY", "")

	// Write a syntactically invalid config file.
	cfgDir := filepath.Join(home, ".config", "qurl")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("api_key: [broken yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := runAuthCmd(t, "auth", "status")
	if err == nil {
		t.Fatal("expected error for malformed config")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Errorf("error should mention 'load config': %v", err)
	}
}
