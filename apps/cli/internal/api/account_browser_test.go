package qurlapi

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
	"strings"
	"testing"
)

func TestAccountBrowserPKCEAndState(t *testing.T) {
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
	token, err := SignInAccount(context.Background(), &Config{BaseURL: server.URL, HTTPClient: server.Client()}, func(ctx context.Context, link string) error {
		u, err := url.Parse(link)
		if err != nil {
			return err
		}
		authQuery = u.Query()
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
	for _, query := range []string{"error=access_denied&code=ignored", "code=" + strings.Repeat("a", 4097)} {
		codes := make(chan string, 1)
		handler := accountCallbackHandler("expected", codes)
		request := httptest.NewRequest(http.MethodGet, accountCallback+"?state=expected&"+query, http.NoBody)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), msgAccountCanceled) {
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
