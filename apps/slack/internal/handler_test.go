package internal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const testSigningSecret = "test-secret"

// noopQURLServer is a stand-in upstream that 200s every request. Tests
// that exercise routing/auth (not the QURL API contract) use this so the
// handler can construct a *client.Client without making real network calls.
func noopQURLServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fixedNow pins the handler's clock so every signed-request test produces
// a stable timestamp. Arbitrary absolute value — tests inject h.now so the
// wall clock is irrelevant; this constant just needs to be the same in both
// sign-time and verify-time paths for any given test.
var fixedNow = time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

func newTestHandler(t *testing.T, qurlServer *httptest.Server) *Handler {
	t.Helper()
	h := NewHandler(Config{
		AuthProvider:       &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		SlackSigningSecret: testSigningSecret,
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlServer.URL, apiKey)
		},
	})
	h.now = func() time.Time { return fixedNow }
	return h
}

// signSlackBody returns the pair of headers Slack would send to authenticate
// `body` at `fixedNow`. Using the same algorithm as the handler means any
// drift between them gets caught by the verification tests themselves.
func signSlackBody(t *testing.T, body string) (sig, ts string) {
	t.Helper()
	ts = strconv.FormatInt(fixedNow.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(slackSignatureVersion + ":" + ts + ":" + body))
	sig = slackSignatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
	return sig, ts
}

// newSignedRequest builds a request to `path` carrying `body` and the
// matching signature/timestamp headers for `body`. Caller-supplied
// `signBody` (if non-empty) is what gets signed — used by tamper tests
// where the wire body differs from the signed body.
func newSignedRequest(t *testing.T, path, body, signBody string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if signBody != "" {
		sig, ts := signSlackBody(t, signBody)
		r.Header.Set(headerSlackSignature, sig)
		r.Header.Set(headerSlackTimestamp, ts)
	}
	return r
}

func TestHealthEndpoint(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", http.NoBody))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestSlashCommandHelp(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	body := url.Values{
		"command": {"/qurl"},
		"text":    {"help"},
		"team_id": {"T123"},
	}.Encode()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["text"] == "" {
		t.Error("expected non-empty help text")
	}
}

func TestSlashCommandCreate(t *testing.T) {
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"data": map[string]any{
				"resource_id": "r_abc123test",
				"qurl_link":   "https://qurl.link/at_testtoken",
				"qurl_site":   "https://r_abc123test.qurl.site",
			},
			"meta": map[string]string{
				"request_id": "req_test",
			},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer qurlSrv.Close()

	t.Setenv("QURL_API_KEY", "test-key")

	h := newTestHandler(t, qurlSrv)
	body := url.Values{
		"command": {"/qurl"},
		"text":    {"create https://example.com"},
		"team_id": {"T123"},
	}.Encode()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["response_type"] != "ephemeral" {
		t.Errorf("expected ephemeral response, got %q", result["response_type"])
	}
}

func TestURLVerificationChallenge(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	body := `{"type":"url_verification","challenge":"test-challenge-123"}`

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/events", body, body))

	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["challenge"] != "test-challenge-123" {
		t.Errorf("expected challenge echo, got %q", result["challenge"])
	}
}

// TestSlackEndpoints_Reject401 is the main negative-path fence. Every row
// is a request the handler must reject; the three paths (commands / events
// / interactions) ensure a future endpoint addition can't silently skip
// signature verification.
func TestSlackEndpoints_Reject401(t *testing.T) {
	srv := noopQURLServer(t)

	body := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()
	tamperedBody := url.Values{"command": {"/qurl"}, "text": {"create https://evil.example"}, "team_id": {"T999"}}.Encode()
	replayBody := url.Values{"command": {"/qurl"}, "text": {"list"}, "team_id": {"T_attacker"}}.Encode()
	origReplayBody := url.Values{"command": {"/qurl"}, "text": {"list"}, "team_id": {"T_victim"}}.Encode()

	cases := []struct {
		name      string
		path      string
		body      string
		signBody  string // if set, sign this body and send with `body` (tamper cases)
		nowOffset time.Duration
	}{
		{name: "unsigned /slack/commands", path: "/slack/commands", body: body},
		{name: "unsigned /slack/events", path: "/slack/events", body: `{"type":"url_verification","challenge":"attacker-chosen"}`},
		{name: "unsigned /slack/interactions", path: "/slack/interactions", body: `{"type":"block_actions"}`},
		{name: "tampered body (text swap)", path: "/slack/commands", body: tamperedBody, signBody: body},
		{name: "body swap with different team_id", path: "/slack/commands", body: replayBody, signBody: origReplayBody},
		{name: "stale timestamp (10m outside skew)", path: "/slack/commands", body: body, signBody: body, nowOffset: 10 * time.Minute},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t, srv)
			if tc.nowOffset != 0 {
				h.now = func() time.Time { return fixedNow.Add(tc.nowOffset) }
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, tc.path, tc.body, tc.signBody))
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", w.Code)
			}
		})
	}
}

// Empty signing secret must 401 every request — deployment-is-open fence.
func TestHandle_EmptySigningSecret(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	h.cfg.SlackSigningSecret = ""

	body := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()
	// Even with "correct-looking" headers — an empty secret means no message
	// can verify. We include them to prove the 401 isn't coming from the
	// "missing headers" path.
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(body))
	r.Header.Set(headerSlackSignature, "v0=aaaa")
	r.Header.Set(headerSlackTimestamp, "1761998400")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("empty signing secret: status = %d, want 401", w.Code)
	}
}

// classifySlackErr must emit stable, distinct labels for each sentinel —
// ops dashboards page on "secret_empty" distinctly from ordinary 401
// noise. A regression that collapsed labels (or downgraded the
// secret_empty slog.Error) would silently lose the page signal.
func TestClassifySlackErr_SentinelsMapToDistinctLabels(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errSlackSigningSecretEmpty, "secret_empty"},
		{errSlackSignatureMissing, "headers_missing"},
		{errSlackSignatureMalformed, "sig_malformed"},
		{errSlackTimestampMalformed, "ts_malformed"},
		{errSlackTimestampStale, "stale"},
		{errSlackSignatureMismatch, "mismatch"},
	}
	seen := make(map[string]error, len(cases))
	for _, tc := range cases {
		got := classifySlackErr(tc.err)
		if got != tc.want {
			t.Errorf("classifySlackErr(%v) = %q, want %q", tc.err, got, tc.want)
		}
		if prev, ok := seen[got]; ok {
			t.Errorf("label %q is shared by %v and %v — dashboards can't tell them apart", got, prev, tc.err)
		}
		seen[got] = tc.err
	}
}

// Slack signature headers arrive in canonical mixed case; net/http
// canonicalizes header names so r.Header.Get(headerSlackSignature)
// transparently handles whatever casing the proxy delivers. This test
// fences the lowercase shape an ALB/Slack-relay quirk could produce.
func TestSlashCommand_LowercaseSignatureHeaders(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	body := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()
	sig, ts := signSlackBody(t, body)

	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(body))
	r.Header.Set("x-slack-signature", sig)
	r.Header.Set("x-slack-request-timestamp", ts)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("lowercase signature headers: status = %d, want 200", w.Code)
	}
}

// MaxBytesReader caps the request body — anything larger must 400 (and
// must not trigger HMAC over a partial read, which would silently 401 on
// a legitimate truncated payload from a misbehaving proxy and obscure
// the real cause).
func TestHandle_OversizeBodyReturns400(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	// 2 MiB — twice the 1 MiB cap.
	oversize := strings.Repeat("a", 2<<20)
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(oversize))
	// Headers are intentionally absent — the body-size guard runs before
	// signature verification, so the failure mode we're fencing is "body
	// read fails first" not "signature check rejects".
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("oversize body: status = %d, want 400", w.Code)
	}
}

// Routing fence: GET on a /slack/* path must 404 (not fall through to
// signature verification, which would 401 and confuse the operator).
func TestHandle_GetOnSlackPathReturns404(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/slack/commands", http.NoBody))

	if w.Code != http.StatusNotFound {
		t.Errorf("GET /slack/commands: status = %d, want 404", w.Code)
	}
}
