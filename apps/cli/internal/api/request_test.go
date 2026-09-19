package qurlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestRegisteredRequestPreservesHTTPErrorAndSafeHeaders(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/account/link", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "01234567-89ab-cdef-0123-456789abcdef" {
			t.Error("idempotency key lost")
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["account_token"] != "secret-account-token" {
			t.Error("request body changed")
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Authorization", "must-not-escape")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict"}}`))
	})
	result, err := Request(context.Background(), newRegisteredTestClient(t, srv), http.MethodPost, "/v1/account/link", json.RawMessage(`{"account_token":"secret-account-token"}`), "01234567-89ab-cdef-0123-456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != 409 || len(result.Headers) != 2 || result.Headers["retry-after"] != "7" || string(result.Body) != `{"error":{"code":"conflict"}}` {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(srv.Requests()) != 1 {
		t.Fatal("request replayed")
	}
}

func TestRegisteredRequestRejectsAuthorityAndDisallowedRoutes(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newRegisteredTestClient(t, srv)
	// TODO(upstream-contract): keep these denied routes aligned with the
	// reviewed qurl-go registered-device transport before updating the SDK.
	for _, path := range []string{"https://evil.test/v1/me", "//evil.test/v1/me", "/v1/me#fragment", "/v1/%6de", "/v1/../v1/me", "/v1/me?x=1", "/v1/quota", "/v1/resources/id/sessions"} {
		if _, err := Request(context.Background(), client, http.MethodGet, path, nil, ""); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("rejected request reached network")
	}
	account, err := New(&Config{BaseURL: srv.URL, APIKey: "account-key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Request(context.Background(), account, http.MethodGet, "/v1/me", nil, ""); err == nil {
		t.Fatal("accepted account authority")
	}
}

func TestRegisteredRequestBodyAndCancellation(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newRegisteredTestClient(t, srv)
	for _, tc := range []struct {
		status     int
		body, want string
	}{{204, "", "null"}, {502, "upstream unavailable", `"upstream unavailable"`}} {
		srv.Script(http.MethodGet, "/v1/me", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		result, err := Request(context.Background(), client, http.MethodGet, "/v1/me", nil, "")
		if err != nil || result.Status != tc.status || string(result.Body) != tc.want {
			t.Fatalf("response = %+v, %v", result, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Request(ctx, client, http.MethodGet, "/v1/me", nil, ""); err == nil {
		t.Fatal("canceled request succeeded")
	}
	if len(srv.Requests()) != 2 {
		t.Fatal("canceled request reached network")
	}
}
