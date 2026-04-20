package internal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

func base64Encode(t *testing.T, s string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(s))
}

const testSigningSecret = "test-secret"

// fixedNow pins the handler's clock so every signed-request test produces
// a stable timestamp. Arbitrary absolute value — tests inject h.now so the
// wall clock is irrelevant; this constant just needs to be the same in both
// sign-time and verify-time paths for any given test.
var fixedNow = time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

func newTestHandler(t *testing.T, qurlServer *httptest.Server) *Handler {
	t.Helper()
	h := NewHandler(Config{
		QURLEndpoint:       qurlServer.URL,
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
func signSlackBody(t *testing.T, body string) map[string]string {
	t.Helper()
	tsHeader := strconv.FormatInt(fixedNow.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(slackSignatureVersion + ":" + tsHeader + ":" + body))
	sig := slackSignatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
	return map[string]string{
		headerSlackSignature: sig,
		headerSlackTimestamp: tsHeader,
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/health",
		HTTPMethod: "GET",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestSlashCommandHelp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	body := url.Values{
		"command": {"/qurl"},
		"text":    {"help"},
		"team_id": {"T123"},
	}.Encode()

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/commands",
		HTTPMethod: methodPost,
		Body:       body,
		Headers:    signSlackBody(t, body),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	if err := json.Unmarshal([]byte(resp.Body), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["text"] == "" {
		t.Error("expected non-empty help text")
	}
}

func TestSlashCommandCreate(t *testing.T) {
	// Mock server returns API envelope format
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

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/commands",
		HTTPMethod: methodPost,
		Body:       body,
		Headers:    signSlackBody(t, body),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	if err := json.Unmarshal([]byte(resp.Body), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["response_type"] != "ephemeral" {
		t.Errorf("expected ephemeral response, got %q", result["response_type"])
	}
}

func TestURLVerificationChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	body := `{"type":"url_verification","challenge":"test-challenge-123"}`

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/events",
		HTTPMethod: methodPost,
		Body:       body,
		Headers:    signSlackBody(t, body),
	})
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]string
	if err := json.Unmarshal([]byte(resp.Body), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["challenge"] != "test-challenge-123" {
		t.Errorf("expected challenge echo, got %q", result["challenge"])
	}
}

// Negative-path tests: the exact class of request that was reaching the
// handler pre-fix (unsigned, tampered, stale) must now be rejected with 401.
// Together these three cover Slack's three signing failure modes and fence
// the qurl-integrations #71 exposure.

func TestSlashCommand_RejectsUnsigned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	body := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/commands",
		HTTPMethod: methodPost,
		Body:       body,
		// no signature headers — this is the pre-fix reproducer
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unsigned slash command: status = %d, want 401", resp.StatusCode)
	}
}

func TestSlashCommand_RejectsTamperedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	signedBody := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()
	headers := signSlackBody(t, signedBody)

	// Tamper: attacker swaps the body but reuses a legitimate signature.
	tamperedBody := url.Values{"command": {"/qurl"}, "text": {"create https://evil.example"}, "team_id": {"T999"}}.Encode()

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/commands",
		HTTPMethod: methodPost,
		Body:       tamperedBody,
		Headers:    headers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("tampered body: status = %d, want 401", resp.StatusCode)
	}
}

func TestSlashCommand_RejectsStaleTimestamp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	body := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()
	headers := signSlackBody(t, body)

	// Pin "now" 10 minutes after the signed timestamp — outside Slack's 5m skew.
	h.now = func() time.Time { return fixedNow.Add(10 * time.Minute) }

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/commands",
		HTTPMethod: methodPost,
		Body:       body,
		Headers:    headers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("stale timestamp: status = %d, want 401", resp.StatusCode)
	}
}

// TestSlashCommand_ReplayWithDifferentTeamIDRejected documents the
// signature's binding surface — the signature covers body + timestamp, so
// swapping a different team_id into a previously-signed request doesn't
// replay. Complementary to TestSlashCommand_RejectsTamperedBody which
// mutates the text field; this one mutates team_id specifically because
// that's what a resource-scope-escalation replay would target.
func TestSlashCommand_ReplayWithDifferentTeamID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	originalBody := url.Values{"command": {"/qurl"}, "text": {"list"}, "team_id": {"T_victim"}}.Encode()
	headers := signSlackBody(t, originalBody)
	attackerBody := url.Values{"command": {"/qurl"}, "text": {"list"}, "team_id": {"T_attacker"}}.Encode()

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/commands",
		HTTPMethod: methodPost,
		Body:       attackerBody,
		Headers:    headers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("replay with different team_id: status = %d, want 401", resp.StatusCode)
	}
}

// Per-endpoint fences: the three Slack-receiving endpoints must each enforce
// signing. Without this, /slack/events and /slack/interactions could regress
// the fix while tests for /slack/commands stay green.

func TestEvent_RejectsUnsigned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/events",
		HTTPMethod: methodPost,
		Body:       `{"type":"url_verification","challenge":"attacker-chosen"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unsigned event: status = %d, want 401", resp.StatusCode)
	}
}

func TestInteraction_RejectsUnsigned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/interactions",
		HTTPMethod: methodPost,
		Body:       `{"type":"block_actions"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unsigned interaction: status = %d, want 401", resp.StatusCode)
	}
}

// TestSlashCommand_Base64Body_Help fences the API-Gateway-binary-media-type
// trap called out in the #73 review. Two invariants:
//  1. The HMAC check runs against the decoded body (not the base64 blob).
//  2. The decoded body is threaded to the downstream handler so
//     url.ParseQuery sees real form data, not a base64 string. Without (2),
//     a signed /qurl help would verify then fall through to the "unknown
//     subcommand" branch because ParseQuery would find no "command" / "text"
//     keys in the base64 blob.
//
// The help-path text assertion distinguishes (2) from (1) — a broken body
// threading would still return 200 but with different body text.
func TestSlashCommand_Base64Body_Help(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	rawBody := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()
	headers := signSlackBody(t, rawBody)
	encoded := base64Encode(t, rawBody)

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:            "/slack/commands",
		HTTPMethod:      methodPost,
		Body:            encoded,
		Headers:         headers,
		IsBase64Encoded: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("base64 body + valid signature: status = %d, want 200; body=%s", resp.StatusCode, resp.Body)
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(resp.Body), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(result["text"], "qurl create") {
		t.Errorf("base64 body + text=help did not produce help response — body threading regressed. Got: %q", result["text"])
	}
}

// TestSlashCommand_Base64Body_Create strengthens the body-threading fence:
// a signed create command must actually reach handleCreate (which hits the
// mock QURL server) rather than being misrouted through help.
func TestSlashCommand_Base64Body_Create(t *testing.T) {
	var qurlCalled bool
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		qurlCalled = true
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"data": map[string]any{"resource_id": "r_test", "qurl_link": "https://qurl.link/at_t", "qurl_site": "https://r_test.qurl.site"},
			"meta": map[string]string{"request_id": "req_test"},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer qurlSrv.Close()

	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, qurlSrv)
	rawBody := url.Values{"command": {"/qurl"}, "text": {"create https://example.com"}, "team_id": {"T123"}}.Encode()
	headers := signSlackBody(t, rawBody)
	encoded := base64Encode(t, rawBody)

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:            "/slack/commands",
		HTTPMethod:      methodPost,
		Body:            encoded,
		Headers:         headers,
		IsBase64Encoded: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("base64 create: status = %d, want 200; body=%s", resp.StatusCode, resp.Body)
	}
	if !qurlCalled {
		t.Error("QURL backend was never called — handleCreate didn't run; body threading regressed")
	}
}

// TestHandle_EmptySigningSecret fences the deploy-is-open failure mode — a
// handler with no signing secret must 401 every request, not silently accept
// them (the helper-level test covers the algorithm, this one pins the wire).
func TestHandle_EmptySigningSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	h.cfg.SlackSigningSecret = ""

	body := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()
	// Even with "correct-looking" headers — an empty secret means no message
	// can verify. We include them to prove the 401 isn't coming from the
	// "missing headers" path.
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/commands",
		HTTPMethod: methodPost,
		Body:       body,
		Headers: map[string]string{
			headerSlackSignature: "v0=aaaa",
			headerSlackTimestamp: "1761998400",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("empty signing secret: status = %d, want 401", resp.StatusCode)
	}
}

func TestHeaderValue(t *testing.T) {
	// API Gateway v1 preserves caller casing (Slack sends mixed case);
	// v2 lowercases. Some v1 configs populate MultiValueHeaders instead of
	// Headers. The helper handles all four combinations.
	cases := []struct {
		name   string
		hdrs   map[string]string
		multi  map[string][]string
		lookup string
		want   string
	}{
		{"mixed case in Headers", map[string]string{"X-Slack-Signature": "v0=abc"}, nil, "X-Slack-Signature", "v0=abc"},
		{"lowercase in Headers", map[string]string{"x-slack-signature": "v0=abc"}, nil, "X-Slack-Signature", "v0=abc"},
		{"not present returns empty", map[string]string{"other": "val"}, nil, "X-Slack-Signature", ""},
		{"MultiValueHeaders mixed case", nil, map[string][]string{"X-Slack-Signature": {"v0=abc"}}, "X-Slack-Signature", "v0=abc"},
		{"MultiValueHeaders lowercase", nil, map[string][]string{"x-slack-signature": {"v0=abc"}}, "X-Slack-Signature", "v0=abc"},
		{"empty multi-value returns empty", nil, map[string][]string{"X-Slack-Signature": {}}, "X-Slack-Signature", ""},
		{"Headers wins over MultiValueHeaders", map[string]string{"X-Slack-Signature": "v0=fromhdrs"}, map[string][]string{"X-Slack-Signature": {"v0=frommulti"}}, "X-Slack-Signature", "v0=fromhdrs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := headerValue(tc.hdrs, tc.multi, tc.lookup)
			if got != tc.want {
				t.Errorf("headerValue = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSlashCommand_InvalidBase64_Returns401 covers the decode-error branch
// in prepareAndVerifySlackRequest. A future refactor that drops the decode
// would silently let API-Gateway binary-media-type regressions produce
// either 200s or 500s — this test pins the 401 behavior.
func TestSlashCommand_InvalidBase64_Returns401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:            "/slack/commands",
		HTTPMethod:      methodPost,
		Body:            "this is not valid base64 at all!@#$%",
		Headers:         map[string]string{headerSlackSignature: "v0=deadbeef", headerSlackTimestamp: "1761998400"},
		IsBase64Encoded: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("invalid base64 with IsBase64Encoded=true: status = %d, want 401", resp.StatusCode)
	}
}
