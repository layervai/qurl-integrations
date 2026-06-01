package internal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/layervai/qurl-integrations/shared/client"
)

// Repeated test literals, named to satisfy goconst and keep the
// expected-copy assertions in one place.
const (
	testDisplayNameProdAPI      = "Prod API"
	testDisplayNameMissingMsg   = "Missing Display Name"
	testDisplayNameInvalidIDMsg = "valid tunnel id"
)

// --- install-default constructor -----------------------------------------

func TestDefaultTunnelDisplayName(t *testing.T) {
	// Install seeds this and unset-display-name reverts to it. The string
	// must match install's auto-fill exactly, so a fresh install and a
	// post-unset tunnel read identically.
	if got, want := defaultTunnelDisplayName("prod-dashboard"), "Slack tunnel install for prod-dashboard"; got != want {
		t.Errorf("defaultTunnelDisplayName = %q, want %q", got, want)
	}
}

// --- parser unit tests ---------------------------------------------------

func TestParseSetDisplayNameArgs(t *testing.T) {
	const id = "prod-dashboard"
	cases := []struct {
		name     string
		input    string
		wantErr  bool
		wantID   string
		wantName string
		// wantMsgSub, when set on a wantErr case, asserts which rejection copy fired.
		wantMsgSub string
	}{
		{name: "single-word name", input: id + " Prod", wantID: id, wantName: "Prod"},
		{name: "multi-word name", input: id + " Prod API gateway", wantID: id, wantName: "Prod API gateway"},
		{name: "double-quoted name unquoted", input: id + ` "Prod API"`, wantID: id, wantName: testDisplayNameProdAPI},
		{name: "single-quoted name unquoted", input: id + " 'Prod API'", wantID: id, wantName: testDisplayNameProdAPI},
		{name: "surrounding whitespace trimmed", input: id + "    Prod API   ", wantID: id, wantName: testDisplayNameProdAPI},
		{name: "name with internal punctuation kept", input: id + " Staging DB (replica)", wantID: id, wantName: "Staging DB (replica)"},
		{name: "lone quote kept as part of name", input: id + ` it's fine`, wantID: id, wantName: "it's fine"},
		{name: "dollar-prefixed id accepted", input: "$" + id + ` "Prod API"`, wantID: id, wantName: testDisplayNameProdAPI},

		{name: "missing everything", input: "", wantErr: true, wantMsgSub: "Missing tunnel id"},
		{name: "lone dollar id rejected", input: "$ Prod API", wantErr: true, wantMsgSub: "Missing tunnel id"},
		{name: "dollar then invalid id rejected", input: "$Prod foo", wantErr: true, wantMsgSub: testDisplayNameInvalidIDMsg},
		{name: "id only, no name", input: id, wantErr: true, wantMsgSub: testDisplayNameMissingMsg},
		{name: "id then only whitespace", input: id + "    ", wantErr: true, wantMsgSub: testDisplayNameMissingMsg},
		{name: "id then empty quotes", input: id + ` ""`, wantErr: true, wantMsgSub: testDisplayNameMissingMsg},
		{name: "invalid id (uppercase)", input: "Prod foo", wantErr: true, wantMsgSub: testDisplayNameInvalidIDMsg},
		{name: "invalid id (too short)", input: "ab foo", wantErr: true, wantMsgSub: testDisplayNameInvalidIDMsg},
		{name: "name too long rejected", input: id + " " + strings.Repeat("a", displayNameMaxLen+1), wantErr: true, wantMsgSub: "too long"},
		{name: "name at length cap accepted", input: id + " " + strings.Repeat("a", displayNameMaxLen), wantID: id, wantName: strings.Repeat("a", displayNameMaxLen)},
		{name: "control byte in name rejected", input: id + " bad\x01name", wantErr: true, wantMsgSub: "control characters"},
		{name: "backtick in name rejected", input: id + " Prod `code` API", wantErr: true, wantMsgSub: "backticks"},
		{name: "angle-bracket broadcast rejected", input: id + " Prod <!here>", wantErr: true, wantMsgSub: "angle brackets"},
		{name: "angle-bracket disguised link rejected", input: id + " <https://evil|Prod>", wantErr: true, wantMsgSub: "angle brackets"},
		{name: "tab-separated id and name parses", input: id + "\tProd API", wantID: id, wantName: testDisplayNameProdAPI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotName, msg := parseSetDisplayNameArgs(tc.input)
			if tc.wantErr {
				if msg == "" {
					t.Fatalf("expected rejection, got id=%q name=%q", gotID, gotName)
				}
				if tc.wantMsgSub != "" && !strings.Contains(msg, tc.wantMsgSub) {
					t.Errorf("rejection copy = %q, want substring %q", msg, tc.wantMsgSub)
				}
				return
			}
			if msg != "" {
				t.Fatalf("unexpected rejection: %s", msg)
			}
			if gotID != tc.wantID {
				t.Errorf("id = %q, want %q", gotID, tc.wantID)
			}
			if gotName != tc.wantName {
				t.Errorf("name = %q, want %q", gotName, tc.wantName)
			}
		})
	}
}

func TestParseUnsetDisplayNameArgs(t *testing.T) {
	const id = "prod-dashboard"
	cases := []struct {
		name    string
		input   string
		wantErr bool
		wantID  string
	}{
		{name: "happy", input: id, wantID: id},
		{name: "dollar-prefixed id accepted", input: "$" + id, wantID: id},
		{name: "missing id", input: "", wantErr: true},
		{name: "lone dollar rejected", input: "$", wantErr: true},
		{name: "trailing args rejected", input: id + " extra", wantErr: true},
		{name: "invalid id rejected", input: "Prod", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, msg := parseUnsetDisplayNameArgs(tc.input)
			if tc.wantErr {
				if msg == "" {
					t.Fatalf("expected rejection, got id=%q", gotID)
				}
				return
			}
			if msg != "" {
				t.Fatalf("unexpected rejection: %s", msg)
			}
			if gotID != tc.wantID {
				t.Errorf("id = %q, want %q", gotID, tc.wantID)
			}
		})
	}
}

// --- handler-level tests -------------------------------------------------

// capturedPatch records the description sent on the most recent
// PATCH /v1/resources/{id}. Pointer field distinguishes "no PATCH seen"
// (nil) from "PATCH with empty/clear description" (&"").
type capturedPatch struct {
	description *string
	calls       atomic.Int32
}

// displayNameQURLServer answers the two upstream calls the display-name
// verbs make: GET /v1/resources?slug=<id> resolves a single active tunnel
// (resource_id "r_"+id) when the id matches knownSlug (else empty list,
// driving the not-found copy), and PATCH /v1/resources/{id} records the
// body's description and echoes a resource back.
func displayNameQURLServer(t *testing.T, knownSlug string, capPatch *capturedPatch) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			slug := r.URL.Query().Get("slug")
			if slug != knownSlug {
				respondQURLEnvelope(t, w, []map[string]any{})
				return
			}
			respondQURLEnvelope(t, w, []map[string]any{{
				testKeyResourceID: "r_" + slug,
				testKeyType:       client.ResourceTypeTunnel,
				testKeySlug:       slug,
				testKeyStatus:     client.StatusActive,
			}})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/resources/"):
			capPatch.calls.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read PATCH body: %v", err)
			}
			var in struct {
				Description *string `json:"description"`
			}
			if err := json.Unmarshal(body, &in); err != nil {
				t.Fatalf("unmarshal PATCH body: %v", err)
			}
			capPatch.description = in.Description
			id := strings.TrimPrefix(r.URL.Path, "/v1/resources/")
			respondQURLEnvelope(t, w, map[string]any{
				testKeyResourceID: id,
				testKeyType:       client.ResourceTypeTunnel,
				testKeyStatus:     client.StatusActive,
			})
		default:
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSetDisplayName_Happy(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	capPatch := &capturedPatch{}
	h := newTestHandler(t, displayNameQURLServer(t, testTunnelSlug, capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)

	_, ack, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("set-display-name "+testTunnelSlug+" Prod API gateway", testAliasTeamID, "U_alias_admin")

	if ack != ackWorkingOnIt {
		t.Fatalf("ack = %q, want async working copy", ack)
	}
	if !strings.Contains(async, "Display Name updated") || !strings.Contains(async, "Prod API gateway") || !strings.Contains(async, "`"+testTunnelSlug+"`") {
		t.Errorf("async reply = %q, want success copy with id + name", async)
	}
	if capPatch.calls.Load() != 1 {
		t.Fatalf("PATCH calls = %d, want 1", capPatch.calls.Load())
	}
	if capPatch.description == nil || *capPatch.description != "Prod API gateway" {
		t.Errorf("PATCH description = %v, want pointer to %q", capPatch.description, "Prod API gateway")
	}
}

func TestSetDisplayName_MultiWordQuoted(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	capPatch := &capturedPatch{}
	h := newTestHandler(t, displayNameQURLServer(t, testTunnelSlug, capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)

	_, _, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync(`set-display-name `+testTunnelSlug+` "Staging DB (read replica)"`, testAliasTeamID, "U_alias_admin")

	if !strings.Contains(async, "Staging DB (read replica)") {
		t.Errorf("async reply = %q, want the quoted multi-word name", async)
	}
	if capPatch.description == nil || *capPatch.description != "Staging DB (read replica)" {
		t.Errorf("PATCH description = %v, want unquoted multi-word name", capPatch.description)
	}
}

func TestUnsetDisplayName_Happy(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	capPatch := &capturedPatch{}
	h := newTestHandler(t, displayNameQURLServer(t, testTunnelSlug, capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)

	_, _, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("unset-display-name "+testTunnelSlug, testAliasTeamID, "U_alias_admin")

	if !strings.Contains(async, "Display Name reset") || !strings.Contains(async, "`"+testTunnelSlug+"`") {
		t.Errorf("async reply = %q, want reset copy with id", async)
	}
	if capPatch.calls.Load() != 1 {
		t.Fatalf("PATCH calls = %d, want 1", capPatch.calls.Load())
	}
	// Unset REVERTS to the install default, it does not blank: the PATCH
	// carries the same string a fresh install would have written, so the
	// tunnel still has a Display Name.
	want := defaultTunnelDisplayName(testTunnelSlug)
	if capPatch.description == nil || *capPatch.description != want {
		t.Errorf("PATCH description = %v, want pointer to %q (install default)", capPatch.description, want)
	}
}

func TestSetDisplayName_TunnelNotFound(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	capPatch := &capturedPatch{}
	// knownSlug is a DIFFERENT id, so the lookup returns an empty list.
	h := newTestHandler(t, displayNameQURLServer(t, "some-other-tunnel", capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)

	_, _, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("set-display-name "+testTunnelSlug+" Prod", testAliasTeamID, "U_alias_admin")

	if !strings.Contains(async, "No tunnel with id") || !strings.Contains(async, "/qurl list") {
		t.Errorf("async reply = %q, want friendly not-found copy pointing at /qurl list", async)
	}
	if capPatch.calls.Load() != 0 {
		t.Errorf("PATCH fired despite missing tunnel (calls = %d)", capPatch.calls.Load())
	}
}

func TestSetDisplayName_NonAdminDenied(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	var upstreamHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	// AdminStore names U_admin / U_alias_admin; "U_not_admin" is not among them.
	seedAliasAdminGate(t, h, testAliasTeamID)

	status, reply := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdmin("set-display-name "+testTunnelSlug+" Prod", testAliasTeamID, "U_not_admin")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(reply, "admin-only") {
		t.Errorf("reply = %q, want admin-only denial", reply)
	}
	if upstreamHits.Load() != 0 {
		t.Errorf("upstream hit despite non-admin gate (hits = %d)", upstreamHits.Load())
	}
}

func TestUnsetDisplayName_NonAdminDenied(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	var upstreamHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	seedAliasAdminGate(t, h, testAliasTeamID)

	status, reply := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdmin("unset-display-name "+testTunnelSlug, testAliasTeamID, "U_not_admin")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(reply, "admin-only") {
		t.Errorf("reply = %q, want admin-only denial", reply)
	}
	if upstreamHits.Load() != 0 {
		t.Errorf("upstream hit despite non-admin gate (hits = %d)", upstreamHits.Load())
	}
}

func TestSetDisplayName_ParserErrorDoesNotHitUpstream(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	var upstreamHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	h := newTestHandler(t, srv)
	seedAliasAdminGate(t, h, testAliasTeamID)

	// Missing the Display Name argument: the parser rejects synchronously,
	// before the admin gate or any upstream call.
	status, reply := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdmin("set-display-name "+testTunnelSlug, testAliasTeamID, "U_alias_admin")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(reply, testDisplayNameMissingMsg) {
		t.Errorf("reply = %q, want missing-Display-Name usage hint", reply)
	}
	if upstreamHits.Load() != 0 {
		t.Errorf("upstream hit on parser-rejection path (hits = %d)", upstreamHits.Load())
	}
}

func TestSetDisplayName_NoAdminStoreWired(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	// No seedAliasAdminGate: AdminStore stays nil, so the verb soft-fails
	// with the not-configured copy before any upstream call.
	body, sign := aliasSlashRequest(t, "set-display-name "+testTunnelSlug+" Prod", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))
	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "not configured") {
		t.Errorf("response = %q, want not-configured copy", got)
	}
}
