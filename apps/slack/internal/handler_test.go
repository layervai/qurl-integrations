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
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
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

// countingQURLServer is like noopQURLServer but exposes the number of
// requests it received. Used by negative-path tests that want to fence
// "no upstream call leaked through" in addition to "401 returned".
func countingQURLServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
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
	// Tests target httptest servers (http://127.0.0.1:NNNNN) that the
	// production validator rejects. Override here so the async path
	// runs end-to-end; the production validator gets its own table
	// test in process_test.go.
	h.validateResponseURLFn = url.Parse
	// LIFO: this drain runs before any httptest server cleanup, so a
	// goroutine still mid-call to qurlServer doesn't race the close.
	t.Cleanup(h.Wait)
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

func TestSlashCommandGetURL_AcksWithWorkingOnIt(t *testing.T) {
	// Ack contract for /qurl get <url>: the synchronous response is
	// the ephemeral working-on-it message. The actual qURL link is
	// delivered later via response_url (covered in process_test.go).
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"data": map[string]any{
				"resource_id": "r_abc123test",
				"qurl_link":   "https://qurl.link/at_testtoken",
				"qurl_site":   "https://r_abc123test.qurl.site",
			},
			"meta": map[string]string{"request_id": "req_test"},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(qurlSrv.Close)

	t.Setenv("QURL_API_KEY", "test-key")

	h := newTestHandler(t, qurlSrv)
	body := getURLCommandBody("https://example.com", "T123", "trig-1", "https://hooks.slack.com/services/x")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result[respFieldResponseType] != respTypeEphemeral {
		t.Errorf("expected ephemeral response, got %q", result["response_type"])
	}
	if result["text"] != ackWorkingOnIt {
		t.Errorf("ack text = %q, want %q", result["text"], ackWorkingOnIt)
	}
}

// TestSlashCommandCreate_DeprecationHint fences the redirect copy
// surfaced when a user types `/qurl create …` after the consolidation
// to `/qurl get`. The reply is synchronous (no ack-then-async needed —
// nothing to mint) and points the user at the new verb. Both the bare
// verb and the with-tail forms route to the same hint — that way an
// existing user typing the old grammar gets the redirect, not an
// "unknown subcommand" reply.
func TestSlashCommandCreate_DeprecationHint(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	const createWithURL = "create https://example.com"
	cases := []string{"create", createWithURL}
	for _, text := range cases {
		body := url.Values{
			fieldCommand:     {"/qurl"},
			fieldText:        {text},
			fieldTeamID:      {"T123"},
			fieldChannelID:   {"C123"},
			fieldTriggerID:   {"trig-deprecation"},
			fieldResponseURL: {"https://hooks.slack.com/services/x"},
		}.Encode()

		w := httptest.NewRecorder()
		h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

		var result map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("text=%q unmarshal: %v", text, err)
		}
		if !strings.Contains(result["text"], "no longer supported") {
			t.Errorf("text=%q: deprecation copy missing: %q", text, result["text"])
		}
		if !strings.Contains(result["text"], "/qurl get") {
			t.Errorf("text=%q: redirect to /qurl get missing: %q", text, result["text"])
		}
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
	srv, hits := countingQURLServer(t)

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

	// Property fence: every 401 above means we rejected before
	// dispatching to the qURL upstream. The status check alone wouldn't
	// catch a regression that 401'd at the wire while leaking the call.
	if got := hits.Load(); got != 0 {
		t.Errorf("upstream qURL hits during auth-failure suite = %d, want 0", got)
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

// Note: an earlier "lowercase signature headers" test was dropped — net/http
// canonicalizes header names on wire parse via textproto.CanonicalMIMEHeaderKey,
// and httptest's Header.Set canonicalizes too, so the path is structurally
// covered by any signed request and a duplicate test added no coverage.

// Body-size cap rejects oversize requests with 413 before any read.
// httptest.NewRequest sets Content-Length from the reader, so this
// exercises the honest-sender pre-allocation guard. The dishonest-sender
// path (no/lying Content-Length) is caught by MaxBytesReader during the
// read; that defense-in-depth is structural rather than unit-testable.
func TestHandle_OversizeBodyReturns413(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	// 2 MiB — twice the 1 MiB cap.
	oversize := strings.Repeat("a", 2<<20)
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(oversize))
	// Headers intentionally absent — the body-size guard runs before
	// signature verification.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize body: status = %d, want 413", w.Code)
	}
}

// Routing fence: GET on a /slack/* path must 405 (the path exists, the
// method doesn't) with an Allow header pointing to POST. 404 would lie
// about the endpoint's existence; 401 would leak that the path is
// gated behind auth.
func TestHandle_GetOnSlackPathReturns405(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/slack/commands", http.NoBody))

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /slack/commands: status = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow header = %q, want %q", got, "POST")
	}
}

// /health must accept GET and HEAD (for ALB probes) and reject other
// methods with 405 + Allow.
func TestHealthEndpoint_RejectsNonGet(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/health", http.NoBody))

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /health: status = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow header = %q, want %q", got, "GET, HEAD")
	}
}

func TestHealthEndpoint_AcceptsHead(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/health", http.NoBody))

	if w.Code != http.StatusOK {
		t.Errorf("HEAD /health: status = %d, want 200", w.Code)
	}
}

// Boundary fence: a body exactly at the cap must succeed end-to-end.
// Off-by-one on MaxBytesReader would silently 400 legitimate large
// payloads — this row catches that regression.
func TestHandle_BodyAtCapAccepted(t *testing.T) {
	// noopQURLServer is required to populate Config.NewClient; this test
	// posts to /slack/events, which never calls out to qURL.
	h := newTestHandler(t, noopQURLServer(t))
	// /slack/events accepts arbitrary bytes; we're fencing read+verify,
	// not the event payload shape.
	body := strings.Repeat("a", maxRequestBodyBytes)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/events", body, body))

	if w.Code != http.StatusOK {
		t.Errorf("body at cap (%d bytes): status = %d, want 200", maxRequestBodyBytes, w.Code)
	}
}

// Pre-allocation fence: a client honestly declaring a too-large
// Content-Length must be rejected with 413 before any body is read.
func TestHandle_DeclaredOversizeReturns413(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader("ignored"))
	r.ContentLength = int64(maxRequestBodyBytes + 1)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("declared oversize: status = %d, want 413", w.Code)
	}
}

// Simulates the chunked-transfer / no-Content-Length path through
// MaxBytesReader — http.Server reads up to declared CL for non-chunked
// bodies, so the real-world dishonest case is chunked encoding (no CL,
// or a CL that doesn't reflect actual body size). The under-declared
// CL here forces ServeHTTP past the pre-allocation pre-check and
// exercises MaxBytesReader-during-read returning *http.MaxBytesError
// — which must surface as 413, not 400, so operator dashboards bucket
// it with the honest-oversize 413s.
func TestHandle_DishonestContentLengthReturns413(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	oversize := strings.Repeat("a", 2<<20)
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(oversize))
	r.ContentLength = 100

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("dishonest CL: status = %d, want 413", w.Code)
	}
}

// A 100 KiB signed body must reach handleEvent intact and 200. Locks
// the contract that no future refactor caps the read short of the body
// — a truncated read would silently 401 (HMAC mismatch on partial
// bytes) and look like a signature-secret-rotation bug.
func TestHandle_LargeSignedBodyAccepted(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	body := strings.Repeat("b", 100*1024)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/events", body, body))

	if w.Code != http.StatusOK {
		t.Errorf("100 KiB signed body: status = %d, want 200", w.Code)
	}
}

// Signed-but-malformed JSON event must 200 with the {"ok":"true"}
// envelope. Slack retries on non-2xx, so a regression to 400 would
// cause retry storms; a regression to 200-with-error-body would mask
// real failures from monitoring.
func TestHandle_MalformedEventJSON_Returns200(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	body := `{not json at all`

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/events", body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("malformed JSON event: status = %d, want 200", w.Code)
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result["ok"] != "true" {
		t.Errorf("malformed JSON event: body = %q, want ok=true", w.Body.String())
	}
}

// TestSlashCommandSetup_RepliesWithStartURL exercises the /qurl setup
// subcommand path: SetOAuthSetup wires the state secret + base URL,
// /qurl setup mints a signed state from the slash-command-verified
// team_id + user_id, and replies ephemerally with a /oauth/qurl/start
// URL carrying that state. The URL-encoded state must round-trip
// through oauth.VerifyState — that's the bridge from Slack's signing
// secret to the OAuth callback's HMAC.
func TestSlashCommandSetup_RepliesWithStartURL(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	secret := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	h.SetOAuthSetup(oauth.SetupConfig{StateSecret: secret, SlackBaseURL: "https://slack-bot.example"})
	// newTestHandler already pins h.now to fixedNow, which is the clock
	// /qurl setup uses to mint state.

	body := url.Values{
		"command": {"/qurl"},
		"text":    {"setup"},
		"team_id": {"T123ABCDEF"},
		"user_id": {"U_ADMIN1"},
	}.Encode()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result[respFieldResponseType] != respTypeEphemeral {
		t.Errorf("response_type: got %q want ephemeral", result["response_type"])
	}
	text := result["text"]
	if !strings.Contains(text, "https://slack-bot.example/oauth/qurl/start?state=") {
		t.Fatalf("missing /start URL in setup reply: %q", text)
	}

	// Extract the state query value and verify it parses through the
	// oauth package — that's the load-bearing property of this whole
	// flow.
	start := strings.Index(text, "state=")
	if start < 0 {
		t.Fatalf("no state= in reply: %q", text)
	}
	rest := text[start+len("state="):]
	end := strings.IndexAny(rest, "|>") // strip Slack mrkdwn link suffix.
	if end >= 0 {
		rest = rest[:end]
	}
	stateRaw, err := url.QueryUnescape(rest)
	if err != nil {
		t.Fatalf("unescape state: %v", err)
	}
	if _, err := oauth.VerifyState(secret, stateRaw, fixedNow.Add(30*time.Second)); err != nil {
		t.Errorf("minted state failed VerifyState: %v", err)
	}
}

// TestSetOAuthSetupPanicsOnDoubleCall locks the documented "called
// exactly once before Serve" contract. The field is read without a
// lock on the request hot path; the panic is the safety net for a
// future refactor that accidentally re-wires it.
func TestSetOAuthSetupPanicsOnDoubleCall(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	secret := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	h.SetOAuthSetup(oauth.SetupConfig{StateSecret: secret, SlackBaseURL: "https://slack-bot.example"})
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on second SetOAuthSetup call")
		}
	}()
	h.SetOAuthSetup(oauth.SetupConfig{StateSecret: secret, SlackBaseURL: "https://slack-bot.example"})
}

func TestSlashCommandSetup_RepliesNotConfiguredWhenOAuthOff(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	// SetOAuthSetup deliberately NOT called → oauthSetup == nil.
	body := url.Values{
		"command": {"/qurl"},
		"text":    {"setup"},
		"team_id": {"T123ABCDEF"},
		"user_id": {"U_ADMIN1"},
	}.Encode()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(result["text"], "not configured") {
		t.Errorf("expected 'not configured' reply, got %q", result["text"])
	}
}

// Empty-body fence: locks the contract so a future ParseQuery
// substitution can't silently change the empty-text help fallback.
func TestSlashCommand_EmptyBodyShowsHelp(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(""))
	sig, ts := signSlackBody(t, "")
	r.Header.Set(headerSlackSignature, sig)
	r.Header.Set(headerSlackTimestamp, ts)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("signed empty body: status = %d, want 200 (help branch); body=%s", w.Code, w.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(result["text"], "/qurl get") {
		t.Errorf("signed empty body did not produce help; got: %q", result["text"])
	}
}
