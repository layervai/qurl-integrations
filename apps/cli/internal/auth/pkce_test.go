package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func testPKCEFlow(tokenSrvURL string) *PKCEFlow {
	return NewPKCEFlow(&PKCEConfig{
		Domain:   "test.auth0.com",
		ClientID: "test-client-id",
		Audience: "https://api.test.com",
		Scopes:   []string{"openid", "profile"},
		BaseURL:  tokenSrvURL,
	})
}

func TestStartLoginCreatesSession(t *testing.T) {
	flow := testPKCEFlow("https://mock.auth0.com")
	session, err := flow.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	defer session.Close()

	u, parseErr := url.Parse(session.AuthURL)
	if parseErr != nil {
		t.Fatalf("parse AuthURL: %v", parseErr)
	}

	q := u.Query()
	if got := q.Get("response_type"); got != "code" {
		t.Errorf("response_type = %q, want %q", got, "code")
	}
	if got := q.Get("client_id"); got != "test-client-id" {
		t.Errorf("client_id = %q, want %q", got, "test-client-id")
	}
	if got := q.Get("audience"); got != "https://api.test.com" {
		t.Errorf("audience = %q, want %q", got, "https://api.test.com")
	}
	if got := q.Get("scope"); got != "openid profile" {
		t.Errorf("scope = %q, want %q", got, "openid profile")
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want %q", got, "S256")
	}
	if q.Get("code_challenge") == "" {
		t.Error("code_challenge is empty")
	}
	if q.Get("state") == "" {
		t.Error("state is empty")
	}
	if q.Get("redirect_uri") == "" {
		t.Error("redirect_uri is empty")
	}
	if session.RedirectURI == "" {
		t.Error("RedirectURI is empty")
	}
}

func TestCallbackSuccess(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		form, _ := url.ParseQuery(string(body))

		if got := form.Get("grant_type"); got != "authorization_code" {
			t.Errorf("grant_type = %q, want %q", got, "authorization_code")
		}
		if got := form.Get("code"); got != "test-auth-code" {
			t.Errorf("code = %q, want %q", got, "test-auth-code")
		}
		if form.Get("code_verifier") == "" {
			t.Error("code_verifier is empty")
		}
		if form.Get("redirect_uri") == "" {
			t.Error("redirect_uri is empty")
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(TokenResponse{
			AccessToken: "test-jwt-token",
			TokenType:   "Bearer",
			ExpiresIn:   86400,
		}); err != nil {
			t.Errorf("encode token: %v", err)
		}
	}))
	defer tokenSrv.Close()

	flow := testPKCEFlow(tokenSrv.URL)
	session, err := flow.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	// Extract state from auth URL.
	authURL, _ := url.Parse(session.AuthURL)
	state := authURL.Query().Get("state")

	// Simulate browser callback.
	callbackURL := session.RedirectURI + "?code=test-auth-code&state=" + url.QueryEscape(state)
	resp := httpGet(t, callbackURL)
	_ = resp.Body.Close()

	token, tokenErr := session.WaitForToken(context.Background())
	if tokenErr != nil {
		t.Fatalf("WaitForToken: %v", tokenErr)
	}
	if token.AccessToken != "test-jwt-token" {
		t.Errorf("AccessToken = %q, want %q", token.AccessToken, "test-jwt-token")
	}
}

func TestCallbackStateMismatch(t *testing.T) {
	flow := testPKCEFlow("https://mock.auth0.com")
	session, err := flow.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	// Send callback with wrong state.
	callbackURL := session.RedirectURI + "?code=test-code&state=wrong-state"
	resp := httpGet(t, callbackURL)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	_, tokenErr := session.WaitForToken(context.Background())
	if tokenErr == nil {
		t.Fatal("expected error for state mismatch")
	}
	if !containsString(tokenErr.Error(), "state mismatch") {
		t.Errorf("error = %q, want state mismatch", tokenErr.Error())
	}
}

func TestCallbackMissingCode(t *testing.T) {
	flow := testPKCEFlow("https://mock.auth0.com")
	session, err := flow.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	authURL, _ := url.Parse(session.AuthURL)
	state := authURL.Query().Get("state")

	// Send callback without code.
	callbackURL := session.RedirectURI + "?state=" + url.QueryEscape(state)
	resp := httpGet(t, callbackURL)
	_ = resp.Body.Close()

	_, tokenErr := session.WaitForToken(context.Background())
	if tokenErr == nil {
		t.Fatal("expected error for missing code")
	}
}

func TestCallbackOAuthError(t *testing.T) {
	flow := testPKCEFlow("https://mock.auth0.com")
	session, err := flow.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	authURL, _ := url.Parse(session.AuthURL)
	state := authURL.Query().Get("state")

	// Send callback with error.
	callbackURL := session.RedirectURI + "?error=access_denied&error_description=User+denied&state=" + url.QueryEscape(state)
	resp := httpGet(t, callbackURL)
	_ = resp.Body.Close()

	_, tokenErr := session.WaitForToken(context.Background())
	if tokenErr == nil {
		t.Fatal("expected error for access_denied")
	}
	var oauthErr *OAuthError
	if !errors.As(tokenErr, &oauthErr) {
		t.Fatalf("expected OAuthError, got %T: %v", tokenErr, tokenErr)
	}
	if oauthErr.Code != "access_denied" {
		t.Errorf("Code = %q, want %q", oauthErr.Code, "access_denied")
	}
}

func TestExchangeCodeError(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if err := json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "Invalid authorization code",
		}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer tokenSrv.Close()

	flow := testPKCEFlow(tokenSrv.URL)
	session, err := flow.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	authURL, _ := url.Parse(session.AuthURL)
	state := authURL.Query().Get("state")

	callbackURL := session.RedirectURI + "?code=bad-code&state=" + url.QueryEscape(state)
	resp := httpGet(t, callbackURL)
	_ = resp.Body.Close()

	_, tokenErr := session.WaitForToken(context.Background())
	if tokenErr == nil {
		t.Fatal("expected error for invalid grant")
	}
	var oauthErr *OAuthError
	if !errors.As(tokenErr, &oauthErr) {
		t.Fatalf("expected OAuthError, got %T: %v", tokenErr, tokenErr)
	}
	if oauthErr.Code != "invalid_grant" {
		t.Errorf("Code = %q, want %q", oauthErr.Code, "invalid_grant")
	}
}

func TestWaitForTokenContextCanceled(t *testing.T) {
	flow := testPKCEFlow("https://mock.auth0.com")
	session, err := flow.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, tokenErr := session.WaitForToken(ctx)
	if tokenErr == nil {
		t.Fatal("expected context error")
	}
}

func TestBuildAuthURL(t *testing.T) {
	tests := []struct {
		name    string
		cfg     PKCEConfig
		wantURL string
	}{
		{
			name:    "default from domain",
			cfg:     PKCEConfig{Domain: "auth.example.com"},
			wantURL: "https://auth.example.com/authorize?",
		},
		{
			name:    "override with base URL",
			cfg:     PKCEConfig{Domain: "auth.example.com", BaseURL: "http://localhost:8080"},
			wantURL: "http://localhost:8080/authorize?",
		},
		{
			name:    "trailing slash stripped",
			cfg:     PKCEConfig{BaseURL: "http://localhost:8080/"},
			wantURL: "http://localhost:8080/authorize?",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flow := NewPKCEFlow(&tt.cfg)
			got := flow.buildAuthURL("http://localhost:9999/callback", "test-state", "test-challenge")
			if !containsString(got, tt.wantURL) {
				t.Errorf("buildAuthURL() = %q, want prefix %q", got, tt.wantURL)
			}
		})
	}
}

func TestPKCECodeVerifierAndChallenge(t *testing.T) {
	verifier, err := generateCodeVerifier()
	if err != nil {
		t.Fatalf("generateCodeVerifier: %v", err)
	}
	if len(verifier) < 43 {
		t.Errorf("verifier length = %d, want >= 43", len(verifier))
	}

	challenge := generateCodeChallenge(verifier)
	if challenge == "" {
		t.Fatal("challenge is empty")
	}
	if challenge == verifier {
		t.Error("challenge should differ from verifier")
	}

	// Verify: SHA256(verifier) base64url == challenge.
	h := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(h[:])
	if challenge != expected {
		t.Errorf("challenge = %q, want SHA256 of verifier = %q", challenge, expected)
	}
}

func TestGenerateStateUniqueness(t *testing.T) {
	s1, err := generateState()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := generateState()
	if err != nil {
		t.Fatal(err)
	}
	if s1 == s2 {
		t.Error("two calls to generateState produced the same value")
	}
}

func TestOAuthErrorMessage(t *testing.T) {
	e := &OAuthError{Code: "access_denied", Description: "User denied authorization"}
	if got := e.Error(); got != "access_denied: User denied authorization" {
		t.Errorf("Error() = %q", got)
	}

	e2 := &OAuthError{Code: "server_error"}
	if got := e2.Error(); got != "server_error" {
		t.Errorf("Error() = %q", got)
	}
}

// httpGet is a test helper that makes a GET request with a context.
func httpGet(t *testing.T, rawURL string) *http.Response {
	t.Helper()
	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	return resp
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || s != "" && searchSubstring(s, substr))
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
