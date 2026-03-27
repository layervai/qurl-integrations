package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func deviceFlowWithServer(baseURL string) *DeviceFlow {
	return NewDeviceFlow(&DeviceFlowConfig{
		Domain:   "test.auth0.com",
		ClientID: "test-client-id",
		Audience: "https://api.test.com",
		Scopes:   []string{"openid", "profile"},
		BaseURL:  baseURL,
	})
}

func TestRequestDeviceCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/device/code" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != formContentType {
			t.Errorf("Content-Type = %q, want %q", ct, formContentType)
		}

		body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if readErr != nil {
			t.Fatalf("read body: %v", readErr)
		}
		form, parseErr := url.ParseQuery(string(body))
		if parseErr != nil {
			t.Fatalf("parse form: %v", parseErr)
		}
		if got := form.Get("client_id"); got != "test-client-id" {
			t.Errorf("client_id = %q, want %q", got, "test-client-id")
		}
		if got := form.Get("audience"); got != "https://api.test.com" {
			t.Errorf("audience = %q, want %q", got, "https://api.test.com")
		}
		if got := form.Get("scope"); got != "openid profile" {
			t.Errorf("scope = %q, want %q", got, "openid profile")
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(DeviceCodeResponse{
			DeviceCode:              "dev-code-123",
			UserCode:                "ABCD-EFGH",
			VerificationURI:         "https://test.auth0.com/activate",
			VerificationURIComplete: "https://test.auth0.com/activate?user_code=ABCD-EFGH",
			ExpiresIn:               900,
			Interval:                5,
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	flow := deviceFlowWithServer(srv.URL)
	dcr, err := flow.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("RequestDeviceCode: %v", err)
	}

	if dcr.DeviceCode != "dev-code-123" {
		t.Errorf("DeviceCode = %q, want %q", dcr.DeviceCode, "dev-code-123")
	}
	if dcr.UserCode != "ABCD-EFGH" {
		t.Errorf("UserCode = %q, want %q", dcr.UserCode, "ABCD-EFGH")
	}
	if dcr.Interval != 5 {
		t.Errorf("Interval = %d, want %d", dcr.Interval, 5)
	}
}

func TestRequestDeviceCodeError(t *testing.T) {
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

	flow := deviceFlowWithServer(srv.URL)
	_, err := flow.RequestDeviceCode(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}

	var dfe *DeviceFlowError
	if !errors.As(err, &dfe) {
		t.Fatalf("expected DeviceFlowError, got %T: %v", err, err)
	}
	if dfe.Code != "unauthorized_client" {
		t.Errorf("Code = %q, want %q", dfe.Code, "unauthorized_client")
	}
}

func TestRequestDeviceCodeNonJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := w.Write([]byte("internal error")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}))
	defer srv.Close()

	flow := deviceFlowWithServer(srv.URL)
	_, err := flow.RequestDeviceCode(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestPollForTokenImmediateSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if readErr != nil {
			t.Fatalf("read body: %v", readErr)
		}
		form, parseErr := url.ParseQuery(string(body))
		if parseErr != nil {
			t.Fatalf("parse form: %v", parseErr)
		}
		if got := form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Errorf("grant_type = %q", got)
		}
		if got := form.Get("device_code"); got != "dev-code-123" {
			t.Errorf("device_code = %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(TokenResponse{
			AccessToken: "jwt-token-abc",
			TokenType:   "Bearer",
			ExpiresIn:   86400,
		}); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	flow := deviceFlowWithServer(srv.URL)
	token, err := flow.PollForToken(context.Background(), "dev-code-123", 1)
	if err != nil {
		t.Fatalf("PollForToken: %v", err)
	}
	if token.AccessToken != "jwt-token-abc" {
		t.Errorf("AccessToken = %q, want %q", token.AccessToken, "jwt-token-abc")
	}
}

func TestPollForTokenPendingThenSuccess(t *testing.T) {
	var pollCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		count := pollCount.Add(1)
		if count < 3 {
			w.WriteHeader(http.StatusForbidden)
			if err := json.NewEncoder(w).Encode(map[string]string{
				"error": "authorization_pending",
			}); err != nil {
				t.Errorf("encode: %v", err)
			}
			return
		}
		if err := json.NewEncoder(w).Encode(TokenResponse{
			AccessToken: "jwt-after-wait",
			TokenType:   "Bearer",
			ExpiresIn:   86400,
		}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer srv.Close()

	flow := deviceFlowWithServer(srv.URL)
	token, err := flow.PollForToken(context.Background(), "dev-code-123", 1)
	if err != nil {
		t.Fatalf("PollForToken: %v", err)
	}
	if token.AccessToken != "jwt-after-wait" {
		t.Errorf("AccessToken = %q, want %q", token.AccessToken, "jwt-after-wait")
	}
	if got := pollCount.Load(); got != 3 {
		t.Errorf("poll count = %d, want 3", got)
	}
}

func TestPollForTokenSlowDown(t *testing.T) {
	var pollCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		count := pollCount.Add(1)
		if count == 1 {
			w.WriteHeader(http.StatusForbidden)
			if err := json.NewEncoder(w).Encode(map[string]string{
				"error": "slow_down",
			}); err != nil {
				t.Errorf("encode: %v", err)
			}
			return
		}
		if err := json.NewEncoder(w).Encode(TokenResponse{
			AccessToken: "jwt-slow",
			TokenType:   "Bearer",
			ExpiresIn:   86400,
		}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer srv.Close()

	flow := deviceFlowWithServer(srv.URL)
	start := time.Now()
	token, err := flow.PollForToken(context.Background(), "dev-code-123", 1)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("PollForToken: %v", err)
	}
	if token.AccessToken != "jwt-slow" {
		t.Errorf("AccessToken = %q, want %q", token.AccessToken, "jwt-slow")
	}
	// After slow_down, interval increases from 1s to 1s+5s = 6s per RFC 8628 section 3.5.
	// First poll at 1s (returns slow_down), second poll at 6s. Total >= 5s.
	if elapsed < 5*time.Second {
		t.Errorf("elapsed %v too short — slow_down should increase poll interval", elapsed)
	}
}

func TestPollForTokenExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		if err := json.NewEncoder(w).Encode(map[string]string{
			"error":             "expired_token",
			"error_description": "The device code has expired",
		}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer srv.Close()

	flow := deviceFlowWithServer(srv.URL)
	_, err := flow.PollForToken(context.Background(), "dev-code-123", 1)
	if err == nil {
		t.Fatal("expected error for expired token")
	}

	var dfe *DeviceFlowError
	if !errors.As(err, &dfe) {
		t.Fatalf("expected DeviceFlowError, got %T: %v", err, err)
	}
	if !dfe.IsExpired() {
		t.Error("expected IsExpired() to be true")
	}
}

func TestPollForTokenDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		if err := json.NewEncoder(w).Encode(map[string]string{
			"error":             "access_denied",
			"error_description": "The user denied authorization",
		}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer srv.Close()

	flow := deviceFlowWithServer(srv.URL)
	_, err := flow.PollForToken(context.Background(), "dev-code-123", 1)
	if err == nil {
		t.Fatal("expected error for access denied")
	}

	var dfe *DeviceFlowError
	if !errors.As(err, &dfe) {
		t.Fatalf("expected DeviceFlowError, got %T: %v", err, err)
	}
	if !dfe.IsDenied() {
		t.Error("expected IsDenied() to be true")
	}
}

func TestPollForTokenContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		if err := json.NewEncoder(w).Encode(map[string]string{
			"error": "authorization_pending",
		}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	flow := deviceFlowWithServer(srv.URL)
	_, err := flow.PollForToken(ctx, "dev-code-123", 10)
	if err == nil {
		t.Fatal("expected context error")
	}
}

func TestDeviceFlowErrorMessages(t *testing.T) {
	e := &DeviceFlowError{Code: "expired_token", Description: "The device code has expired"}
	if got := e.Error(); got != "expired_token: The device code has expired" {
		t.Errorf("Error() = %q", got)
	}

	e2 := &DeviceFlowError{Code: "access_denied"}
	if got := e2.Error(); got != "access_denied" {
		t.Errorf("Error() = %q", got)
	}
}

func TestAuthBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		cfg     DeviceFlowConfig
		wantURL string
	}{
		{
			name:    "default from domain",
			cfg:     DeviceFlowConfig{Domain: "auth.example.com"},
			wantURL: "https://auth.example.com",
		},
		{
			name:    "override with base URL",
			cfg:     DeviceFlowConfig{Domain: "auth.example.com", BaseURL: "http://localhost:8080"},
			wantURL: "http://localhost:8080",
		},
		{
			name:    "trailing slash stripped",
			cfg:     DeviceFlowConfig{BaseURL: "http://localhost:8080/"},
			wantURL: "http://localhost:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flow := NewDeviceFlow(&tt.cfg)
			if got := flow.authBaseURL(); got != tt.wantURL {
				t.Errorf("authBaseURL() = %q, want %q", got, tt.wantURL)
			}
		})
	}
}
