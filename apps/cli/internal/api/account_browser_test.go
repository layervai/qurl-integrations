package qurlapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestAccountBrowserPKCEAndState(t *testing.T) {
	testAccountBrowserPKCEAndState(t, SignInAccount, "openid email qurl:read qurl:write qurl:agent")
}

func TestAccountManagementBrowserScopes(t *testing.T) {
	for _, scopes := range [][]string{{"qurl:read"}, {"qurl:read", "qurl:write"}, {"qurl:read", "qurl:write", "qurl:agent", "qurl:keys"}} {
		t.Run(strings.Join(scopes, " "), func(t *testing.T) {
			testAccountBrowserPKCEAndState(t, func(ctx context.Context, cfg *Config, open func(context.Context, string) error) (string, error) {
				return SignInAccountForManagement(ctx, cfg, open, scopes)
			}, "openid email "+strings.Join(scopes, " "))
		})
	}
}

func testAccountBrowserPKCEAndState(t *testing.T, signIn func(context.Context, *Config, func(context.Context, string) error) (string, error), wantScope string) {
	var authQuery url.Values
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/account/auth":
			u, _ := url.Parse(server.URL)
			_ = json.NewEncoder(w).Encode(map[string]string{"domain": u.Host, "client_id": "native-client", "audience": "qurl-api"})
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(digest[:]) != authQuery.Get("code_challenge") || r.Form.Get("code") != "verified-code" || r.Form.Get("redirect_uri") != accountCallback {
				t.Error("PKCE exchange does not match browser request")
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "account-token", "token_type": "Bearer"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	token, err := signIn(context.Background(), &Config{BaseURL: server.URL, HTTPClient: server.Client()}, func(ctx context.Context, link string) error {
		u, err := url.Parse(link)
		if err != nil {
			return err
		}
		if u.Host != accountCallbackAddress || u.Path != "/" || u.Query().Get("state") == "" {
			t.Fatalf("browser did not open the nonce-bound local method chooser: %s", link)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, link, http.NoBody)
		if err != nil {
			return err
		}
		page, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(page.Body)
		_ = page.Body.Close()
		if readErr != nil {
			return readErr
		}
		links := accountTestMethodLinks(t, string(body))
		if len(links) != 3 {
			t.Fatalf("method links = %d, want email, Google and password", len(links))
		}
		for _, method := range []string{"email", "google-oauth2", "Username-Password-Authentication"} {
			chosen := links[method]
			if chosen == nil || chosen.Host != strings.TrimPrefix(server.URL, "https://") || chosen.Query().Get("state") != u.Query().Get("state") || chosen.Query().Get("redirect_uri") != accountCallback || chosen.Query().Get("code_challenge") != links["email"].Query().Get("code_challenge") || chosen.Query().Get("code_challenge_method") != "S256" || chosen.Query().Get("scope") != wantScope {
				t.Fatalf("method %s changed the OAuth transaction", method)
			}
		}
		authQuery = links["email"].Query()
		if authQuery.Get("prompt") != "login consent" {
			t.Fatal("permanent account link must require visible sign-in and consent")
		}
		for _, state := range []string{"attacker-state", authQuery.Get("state")} {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, accountCallback+"?"+url.Values{"state": {state}, "code": {"verified-code"}}.Encode(), http.NoBody)
			if err != nil {
				return err
			}
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if state == "attacker-state" && response.StatusCode != 400 {
				t.Error("foreign callback state accepted")
			}
		}
		return nil
	})
	if err != nil || token != "account-token" {
		t.Fatalf("sign-in = %q, %v", token, err)
	}
}

func TestAccountCallbackDenial(t *testing.T) {
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"error=access_denied&code=ignored", http.StatusOK},
		{"error=server_error", http.StatusBadRequest},
		{"code=", http.StatusBadRequest},
		{"code=" + strings.Repeat("a", 4097), http.StatusBadRequest},
		{"error=access_denied&code=" + strings.Repeat("a", 4097), http.StatusBadRequest},
	} {
		codes := make(chan string, 1)
		handler := accountBrowserHandler("expected", &url.URL{Scheme: "https", Host: "auth.example.test", Path: "/authorize"}, codes)
		request := httptest.NewRequest(http.MethodGet, accountCallback+"?state=expected&"+tc.query, http.NoBody)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		wantBody := msgAccountCanceled + "\n"
		if tc.status == http.StatusOK {
			wantBody = msgAccountCanceledPage
		}
		if response.Code != tc.status || response.Body.String() != wantBody {
			t.Fatalf("denial = %d %q", response.Code, response.Body.String())
		}
		select {
		case code := <-codes:
			if code != "" {
				t.Fatal("denial accepted an authorization code")
			}
		default:
			t.Fatal("denial did not end sign-in")
		}
	}
}

func TestAccountBrowserPreservesCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"domain": "auth.example.test", "client_id": "native-client", "audience": "qurl-api"})
	}))
	defer server.Close()
	for _, before := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		if before {
			cancel()
		}
		_, err := SignInAccount(ctx, &Config{BaseURL: server.URL}, func(context.Context, string) error { cancel(); return nil })
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel before browser=%v: %v", before, err)
		}
	}
}

func TestAccountSignInRejectsUntrustedSettings(t *testing.T) {
	for _, endpoint := range []string{"http://example.com", "ftp://localhost", "://bad", "https:///missing-host"} {
		if err := validateAccountEndpoint(endpoint); !errors.Is(err, ErrAccountEndpoint) {
			t.Fatalf("endpoint %q: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{"https://example.com", "http://127.0.0.1:8765", "http://[::1]:8765", "http://localhost"} {
		if err := validateAccountEndpoint(endpoint); err != nil {
			t.Fatalf("endpoint %q: %v", endpoint, err)
		}
	}
	for _, domain := range []string{"", "host/path", "user@host", "host?query", "host#fragment", `host\path`} {
		t.Run(domain, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"domain": domain, "client_id": "client", "audience": "audience"})
			}))
			defer server.Close()
			_, err := SignInAccount(context.Background(), &Config{BaseURL: server.URL}, func(context.Context, string) error { t.Fatal("browser opened for invalid settings"); return nil })
			if !errors.Is(err, ErrAccountUnavailable) {
				t.Fatalf("settings error: %v", err)
			}
		})
	}
}

func accountTestMethodLinks(t *testing.T, body string) map[string]*url.URL {
	t.Helper()
	links := make(map[string]*url.URL)
	for _, match := range regexp.MustCompile(`href="([^"]+)"`).FindAllStringSubmatch(body, -1) {
		target, err := url.Parse(html.UnescapeString(match[1]))
		if err != nil {
			t.Fatal(err)
		}
		if method := target.Query().Get("connection"); method != "" {
			links[method] = target
		}
	}
	return links
}

func TestAccountMethodChooserRejectsForeignRequests(t *testing.T) {
	codes := make(chan string, 1)
	authorizeURL := url.URL{Scheme: "https", Host: "auth.example.test", Path: "/authorize", RawQuery: url.Values{"state": {"expected"}, "redirect_uri": {accountCallback}}.Encode()}
	handler := accountBrowserHandler("expected", &authorizeURL, codes)
	for _, tc := range []struct{ method, host, state string }{
		{http.MethodPost, accountCallbackAddress, "expected"},
		{http.MethodGet, "attacker.example", "expected"},
		{http.MethodGet, accountCallbackAddress, "attacker-state"},
	} {
		request := httptest.NewRequest(tc.method, "http://"+accountCallbackAddress+"/?state="+tc.state, http.NoBody)
		request.Host = tc.host
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "auth.example.test") {
			t.Fatalf("foreign request exposed sign-in links: %d", response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "http://"+accountCallbackAddress+"/?state=expected&redirect_uri=https://attacker.example&connection=attacker", http.NoBody)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Frame-Options") != "DENY" || response.Header().Get("Referrer-Policy") != "no-referrer" || !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || !strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action 'none'") {
		t.Fatalf("chooser status/headers = %d %v", response.Code, response.Header())
	}
	for _, target := range accountTestMethodLinks(t, response.Body.String()) {
		if target.Host != authorizeURL.Host || target.Query().Get("redirect_uri") != accountCallback || target.Query().Get("state") != "expected" {
			t.Fatal("request parameters changed the server-held OAuth transaction")
		}
	}
	select {
	case <-codes:
		t.Fatal("viewing the chooser ended sign-in")
	default:
	}
}
