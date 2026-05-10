package internal

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestServeHTTP_Health fences the ALB/ECS probe path. The probe never
// carries a Slack signature, so signature verification must NOT run for
// /health — but the handler still has to return 200 with a JSON body the
// downstream tooling can parse.
//
// Note: cmd/main.go wires /health on the mux directly (so the probe
// bypasses Handle entirely). This test exercises Handle's /health branch
// because that's the safety net if the mux wiring ever regresses.
func TestServeHTTP_Health(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("status field = %q, want ok", got["status"])
	}
}

// TestServeHTTP_SlashCommand_HappyPath fences the body-threading +
// header-threading path: the HTTP adapter must hand Handle a request
// shape that passes signature verification AND survives the slash-
// command form parse + dispatch.
func TestServeHTTP_SlashCommand_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	body := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()
	hdrs := signSlackBody(t, body)

	req := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(body))
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if !strings.Contains(got["text"], "qurl create") {
		t.Errorf("help text missing — body threading regressed: %q", got["text"])
	}
}

// TestServeHTTP_Unsigned_Returns401 fences the negative path: a request
// with no signature headers must hit the same 401 branch the API-Gateway
// path does. If the HTTP adapter ever bypassed prepareAndVerifySlackRequest,
// every endpoint would silently 200 — this test catches that regression.
func TestServeHTTP_Unsigned_Returns401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	body := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()

	req := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("unsigned request: status = %d, want 401", rr.Code)
	}
}

// TestServeHTTP_RejectsOversizedBody fences the body-cap defense. A body
// larger than maxHTTPBodyBytes must be rejected with 413 before signature
// verification — that's the whole point of the cap (a stuck or hostile
// client shouldn't be able to tie up a goroutine streaming an unbounded
// body waiting for an HMAC check that will never run on something
// reasonable).
func TestServeHTTP_RejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	// Cap is 1 MiB; send 1 MiB + 1 byte to land just past it.
	oversize := bytes.Repeat([]byte("a"), maxHTTPBodyBytes+1)

	req := httptest.NewRequest(http.MethodPost, "/slack/commands", bytes.NewReader(oversize))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: status = %d, want 413", rr.Code)
	}
}

// TestServeHTTP_UnknownPath confirms the catch-all 404 still fires
// through the HTTP adapter. The mux only routes /slack/* and /health to
// this handler, but Handle's default branch returning 404 is still
// reachable if someone wires a stray path through.
func TestServeHTTP_UnknownPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	req := httptest.NewRequest(http.MethodGet, "/nope", http.NoBody)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown path: status = %d, want 404", rr.Code)
	}
}
