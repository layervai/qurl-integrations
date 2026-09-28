package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestRequestCommand(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/account/link", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["account_token"] != "private-token" {
			t.Error("stdin body lost")
		}
		if r.Header.Get("Idempotency-Key") != "01234567-89ab-cdef-0123-456789abcdef" {
			t.Error("idempotency key lost")
		}
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit"}}`))
	})
	state := bootstrapRegisteredState(t)
	res := runCLI(t, &runOpts{args: []string{"request", "POST", "/v1/account/link", "-o", "json", "--idempotency-key", "01234567-89ab-cdef-0123-456789abcdef"}, stdin: strings.NewReader(`{"account_token":"private-token"}`), openAPIClient: func(ctx context.Context) (qurlapi.Client, error) {
		return qurlapi.NewRegistered(ctx, &qurlapi.Config{BaseURL: srv.URL, HTTPClient: srv.Client()}, &bootstrapAgentStateStore{state: state})
	}})
	var envelope struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(res.stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if res.code != 0 || envelope.Status != 429 || envelope.Headers["retry-after"] != "3" || len(srv.Requests()) != 1 {
		t.Fatalf("exit %d: %s %s", res.code, res.stdout.String(), res.stderr.String())
	}
	for _, secret := range []string{state.DeviceAPIKey, "private-token"} {
		if strings.Contains(res.stdout.String()+res.stderr.String(), secret) {
			t.Fatal("credential leaked")
		}
	}
}

func TestRequestRejectsInputBeforeOpeningDevice(t *testing.T) {
	for _, tc := range []struct {
		args []string
		body string
	}{
		{[]string{"GET", "https://evil.test/v1/me"}, ""},
		{[]string{"GET", "/v1/me"}, "{}"},
		{[]string{"POST", "/v1/account/link"}, `{"account_token":"private-token"} garbage`},
		{[]string{"POST", "/v1/account/link"}, "{}" + strings.Repeat(" ", 1<<20)},
		{[]string{"GET", "/v1/me", "--idempotency-key", strings.Repeat("a", 32) + "\r\nheader"}, ""},
	} {
		res := runCLI(t, &runOpts{args: append([]string{"request", "-o", "json"}, tc.args...), stdin: strings.NewReader(tc.body), openAPIClient: func(context.Context) (qurlapi.Client, error) {
			t.Error("opened device for invalid input")
			return nil, errors.New("unexpected device open")
		}})
		if res.code == 0 || res.stdout.Len() != 0 || strings.Contains(res.stderr.String(), "private-token") {
			t.Fatalf("invalid input exit %d: %s", res.code, res.stderr.String())
		}
	}
}
