package internal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

// TestServeHTTP_RejectsOversizedBody fences both the body-cap defense
// AND the cap-before-HMAC ordering invariant. We send a SIGNED oversized
// body and assert 413 — that's load-bearing. The unsigned-oversize case
// would also 401, so unsigned alone can't tell us whether the cap or
// sig-verify fired first; signing it makes the 413 unambiguous evidence
// that the cap runs before signature verification.
func TestServeHTTP_RejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	oversize := string(bytes.Repeat([]byte("a"), maxHTTPBodyBytes+1))
	hdrs := signSlackBody(t, oversize)

	req := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(oversize))
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("signed oversized body: status = %d, want 413 (cap must fire before sig-verify)", rr.Code)
	}
}

// errReader implements io.Reader and returns a non-EOF error on the first
// Read. Models a network read failure mid-stream so we can fence the 400
// branch in the body-read error path of ServeHTTP.
type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, errors.New("synthetic read failure") }

// TestServeHTTP_BodyReadError fences the 400 branch when r.Body's Read
// returns a non-EOF error that isn't an *http.MaxBytesError. Without this
// fence, a future refactor could collapse the read-error branch into the
// oversize branch and silently start returning 413 for transport-level
// failures.
func TestServeHTTP_BodyReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	req := httptest.NewRequest(http.MethodPost, "/slack/commands", io.NopCloser(errReader{}))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ContentLength = 16

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("body read error: status = %d, want 400", rr.Code)
	}
}

// TestServeHTTP_SignatureFromMultiValueHeaders fences the bridge from
// net/http's multi-valued http.Header into the API-Gateway-shaped
// MultiValueHeaders map. Slack sends single-valued headers in practice,
// so happy-path tests only exercise Headers — a regression that dropped
// the MultiValueHeaders population in ServeHTTP would silently fall
// back to a working signature-verify (because Headers still has the
// single value), but proxies that ever appended a duplicate header
// would lose the corresponding fields. We force the multi-value path
// by adding multiple X-Slack-Signature values: signature verification
// must still pass because headerValue() reads MultiValueHeaders when
// Headers' lookup misses, and our adapter populates both.
func TestServeHTTP_SignatureFromMultiValueHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	body := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()
	hdrs := signSlackBody(t, body)

	req := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(body))
	// Add a duplicate X-Forwarded-For so MultiValueHeaders has multiple
	// entries for at least one key (proves the loop captures all values
	// from r.Header, not just the first).
	req.Header.Add("X-Forwarded-For", "10.0.0.1")
	req.Header.Add("X-Forwarded-For", "10.0.0.2")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("multi-value-header signed request: status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// TestServeHTTP_UnknownPath fences two invariants on the catch-all 404
// branch: (1) Handle's default branch is still reachable through the
// HTTP adapter, and (2) the response-header copy from resp.Headers into
// w.Header() round-trips Content-Type. The mux only routes /slack/* and
// /health here, but a future stray wiring would land on this branch —
// and a future handler that emits a custom header (e.g.
// X-Slack-Retry-After) would silently regress if the copy loop ever
// disappeared.
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
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("response Content-Type = %q, want application/json (header round-trip regressed)", got)
	}
}
