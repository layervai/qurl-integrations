package internal

import (
	"context"
	"encoding/json"
	"errors"
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
	testDisplayNameInvalidIDMsg = "valid qURL Connector id"

	// Channel-alias resolution fixtures: an admin targets a connector by a
	// channel `$alias` whose name differs from the connector's own slug.
	testDisplayNameAlias     = "dashboard"         // the channel alias the admin types
	testDisplayNameSlug      = "stats-connector"   // the connector's real (different) slug
	testDisplayNameTunnelRID = "r_stats_connector" // its resource id
	testDisplayNameNewName   = "Stats Dashboard"
)

// --- install-default constructor -----------------------------------------

func TestDefaultTunnelDisplayName(t *testing.T) {
	// Install seeds this and unset-display-name reverts to it. The string
	// must match install's auto-fill exactly, so a fresh install and a
	// post-unset tunnel read identically.
	if got, want := defaultTunnelDisplayName("prod-dashboard"), "Slack qURL Connector install for prod-dashboard"; got != want {
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

		{name: "missing everything", input: "", wantErr: true, wantMsgSub: "Missing qURL Connector id"},
		{name: "lone dollar id rejected", input: "$ Prod API", wantErr: true, wantMsgSub: "Missing qURL Connector id"},
		// Bare `$` with no name: the empty-id check fires before the missing-name
		// check, so this is "Missing qURL Connector id" (not "Missing Display Name").
		{name: "lone dollar no name rejected", input: "$", wantErr: true, wantMsgSub: "Missing qURL Connector id"},
		{name: "dollar then invalid id rejected", input: "$Prod foo", wantErr: true, wantMsgSub: testDisplayNameInvalidIDMsg},
		// TrimPrefix strips exactly one `$`, so `$$prod-dashboard` becomes
		// `$prod-dashboard`, which still fails the slug check (matching
		// parseAliasToken's single-strip). Pins that double-sigil doesn't resolve.
		{name: "double dollar strips one, rejected", input: "$$prod-dashboard foo", wantErr: true, wantMsgSub: testDisplayNameInvalidIDMsg},
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
		// wantMsgSub, when set on a wantErr case, pins which rejection copy
		// fired — the lone-`$` path returns the shared helper's "Missing
		// tunnel id", distinct from the arity path's "Provide exactly one".
		wantMsgSub string
	}{
		{name: "happy", input: id, wantID: id},
		{name: "dollar-prefixed id accepted", input: "$" + id, wantID: id},
		{name: "missing id", input: "", wantErr: true, wantMsgSub: "Provide exactly one qURL Connector id"},
		{name: "lone dollar rejected", input: "$", wantErr: true, wantMsgSub: "Missing qURL Connector id"},
		{name: "trailing args rejected", input: id + " extra", wantErr: true, wantMsgSub: "Provide exactly one qURL Connector id"},
		{name: "invalid id rejected", input: "Prod", wantErr: true, wantMsgSub: testDisplayNameInvalidIDMsg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, msg := parseUnsetDisplayNameArgs(tc.input)
			if tc.wantErr {
				if msg == "" {
					t.Fatalf("expected rejection, got id=%q", gotID)
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
		invokeAdminAsync(`set-display-name `+testTunnelSlug+` "Staging DB *read replica* & failover"`, testAliasTeamID, "U_alias_admin")

	if !strings.Contains(async, "Staging DB ∗read replica∗ &amp; failover") {
		t.Errorf("async reply = %q, want the quoted multi-word name escaped for mrkdwn", async)
	}
	if capPatch.description == nil || *capPatch.description != "Staging DB *read replica* & failover" {
		t.Errorf("PATCH description = %v, want unquoted multi-word name", capPatch.description)
	}
}

// TestSetDisplayName_DollarPrefixedID exercises the `$<id>` sigil form
// end-to-end (parser → admin gate → resolve → PATCH), not just at the parser
// layer: the leading `$` is stripped, the bare slug resolves the tunnel, the
// description is PATCHed, and the success copy echoes the stripped id.
func TestSetDisplayName_DollarPrefixedID(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	capPatch := &capturedPatch{}
	h := newTestHandler(t, displayNameQURLServer(t, testTunnelSlug, capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)

	_, ack, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("set-display-name $"+testTunnelSlug+" Prod API gateway", testAliasTeamID, "U_alias_admin")

	if ack != ackWorkingOnIt {
		t.Fatalf("ack = %q, want async working copy", ack)
	}
	// Success copy echoes the STRIPPED id (no `$`), proving the sigil was
	// removed before the resolve rather than carried into the PATCH target.
	if !strings.Contains(async, "Display Name updated") || !strings.Contains(async, "`"+testTunnelSlug+"`") {
		t.Errorf("async reply = %q, want success copy with stripped id", async)
	}
	if strings.Contains(async, "$"+testTunnelSlug) {
		t.Errorf("async reply = %q, leaked the `$` sigil into the id echo", async)
	}
	if capPatch.calls.Load() != 1 {
		t.Fatalf("PATCH calls = %d, want 1 (the `$<id>` form must resolve end-to-end)", capPatch.calls.Load())
	}
	if capPatch.description == nil || *capPatch.description != "Prod API gateway" {
		t.Errorf("PATCH description = %v, want pointer to %q", capPatch.description, "Prod API gateway")
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

// TestUnsetDisplayName_DollarPrefixedID is the unset counterpart to
// TestSetDisplayName_DollarPrefixedID: `unset-display-name $<id>` strips the
// sigil, resolves the tunnel, and PATCHes the description back to the install
// default end-to-end.
func TestUnsetDisplayName_DollarPrefixedID(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	capPatch := &capturedPatch{}
	h := newTestHandler(t, displayNameQURLServer(t, testTunnelSlug, capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)

	_, _, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("unset-display-name $"+testTunnelSlug, testAliasTeamID, "U_alias_admin")

	if !strings.Contains(async, "Display Name reset") || !strings.Contains(async, "`"+testTunnelSlug+"`") {
		t.Errorf("async reply = %q, want reset copy with stripped id", async)
	}
	if capPatch.calls.Load() != 1 {
		t.Fatalf("PATCH calls = %d, want 1 (the `$<id>` form must resolve end-to-end)", capPatch.calls.Load())
	}
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

	if !strings.Contains(async, "No qURL Connector with id") || !strings.Contains(async, "/qurl list") {
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

// --- channel-alias resolution -------------------------------------------

// displayNameScanQURLServer models qurl-service for the channel-alias path:
// `GET /v1/resources?slug=X` filters to that slug (returning scanResource only
// when X equals its own slug), and a no-slug `GET` returns the first-page list —
// the scan resolveActiveTunnelByResourceID runs after a channel alias resolves
// to a resource_id. PATCH records the description like displayNameQURLServer.
// The alias under test deliberately differs from scanResource's slug, so the
// slug-first lookup misses and the alias fallback drives resolution.
func displayNameScanQURLServer(t *testing.T, scanResource map[string]any, capPatch *capturedPatch) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			// `?slug=` is a server-side filter: return the resource only when the
			// queried slug equals its own. The alias under test never equals the
			// slug, so the slug-first lookup gets an empty page and falls through
			// to the channel-alias binding; the no-slug scan returns the resource.
			if slug := r.URL.Query().Get("slug"); slug != "" {
				if rs, _ := scanResource[testKeySlug].(string); rs != "" && rs == slug {
					respondQURLEnvelope(t, w, []map[string]any{scanResource})
				} else {
					respondQURLEnvelope(t, w, []map[string]any{})
				}
				return
			}
			respondQURLEnvelope(t, w, []map[string]any{scanResource})
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

// seedDisplayNameAliasTunnel wires a handler whose team owns one active tunnel
// (slug testDisplayNameSlug, id testDisplayNameTunnelRID) reachable in
// testAliasChannelID ONLY via the channel alias testDisplayNameAlias — the shape
// that left set/unset-display-name unable to resolve it (slug-only) before this
// path learned to honor channel aliases.
func seedDisplayNameAliasTunnel(t *testing.T, capPatch *capturedPatch) *Handler {
	t.Helper()
	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, displayNameScanQURLServer(t, map[string]any{
		testKeyResourceID: testDisplayNameTunnelRID,
		testKeyType:       client.ResourceTypeTunnel,
		testKeySlug:       testDisplayNameSlug,
		testKeyStatus:     client.StatusActive,
	}, capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)
	if err := h.cfg.AdminStore.BindChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testDisplayNameAlias, testDisplayNameTunnelRID); err != nil {
		t.Fatalf("seed channel alias binding: %v", err)
	}
	return h
}

// TestSetDisplayName_ResolvesByChannelAlias is the fix's core case: the admin
// types a channel `$alias` whose name differs from the connector slug. The
// slug-first lookup misses (no connector is named `dashboard`), then the
// alias_bindings entry resolves to the live tunnel and the PATCH fires — the
// same channel alias `/qurl get` and `/qurl-admin revoke` already honor.
func TestSetDisplayName_ResolvesByChannelAlias(t *testing.T) {
	capPatch := &capturedPatch{}
	h := seedDisplayNameAliasTunnel(t, capPatch)

	_, ack, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("set-display-name "+testDisplayNameAlias+" "+testDisplayNameNewName, testAliasTeamID, "U_alias_admin")

	if ack != ackWorkingOnIt {
		t.Fatalf("ack = %q, want async working copy", ack)
	}
	// Success copy echoes the alias id the admin typed + the new name. (Asserting
	// the name + id rather than the shared "Display Name updated" prefix keeps
	// that literal under goconst's occurrence cap.)
	if !strings.Contains(async, testDisplayNameNewName) || !strings.Contains(async, "`"+testDisplayNameAlias+"`") {
		t.Errorf("async reply = %q, want success copy echoing the alias id + name", async)
	}
	if capPatch.calls.Load() != 1 {
		t.Fatalf("PATCH calls = %d, want 1 (the channel alias must resolve end-to-end)", capPatch.calls.Load())
	}
	if capPatch.description == nil || *capPatch.description != testDisplayNameNewName {
		t.Errorf("PATCH description = %v, want pointer to %q", capPatch.description, testDisplayNameNewName)
	}
}

// TestUnsetDisplayName_ResolvesByChannelAlias is the unset counterpart, and also
// pins that the reset value is built from the connector's REAL slug — not the
// alias name — so it matches what install wrote even when the two differ.
func TestUnsetDisplayName_ResolvesByChannelAlias(t *testing.T) {
	capPatch := &capturedPatch{}
	h := seedDisplayNameAliasTunnel(t, capPatch)

	_, _, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("unset-display-name "+testDisplayNameAlias, testAliasTeamID, "U_alias_admin")

	if !strings.Contains(async, "`"+testDisplayNameAlias+"`") {
		t.Errorf("async reply = %q, want reset copy echoing the alias id", async)
	}
	if capPatch.calls.Load() != 1 {
		t.Fatalf("PATCH calls = %d, want 1", capPatch.calls.Load())
	}
	// Reset to the install default off the connector's REAL slug, not the alias.
	want := defaultTunnelDisplayName(testDisplayNameSlug)
	if capPatch.description == nil || *capPatch.description != want {
		t.Errorf("PATCH description = %v, want pointer to %q (install default off the real slug)", capPatch.description, want)
	}
}

// TestSetDisplayName_ChannelAliasToURLResourceRejected guards the type fence on
// the alias path: a channel alias can point at a URL resource, but Display Names
// apply only to connectors. The alias resolves, the recovered resource is
// type=url, and the verb refuses with an unset-alias hint rather than PATCHing a
// URL resource's description.
func TestSetDisplayName_ChannelAliasToURLResourceRejected(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	const urlResourceID = "r_url_resource"
	capPatch := &capturedPatch{}
	h := newTestHandler(t, displayNameScanQURLServer(t, map[string]any{
		testKeyResourceID: urlResourceID,
		testKeyType:       client.ResourceTypeURL,
		testKeyStatus:     client.StatusActive,
	}, capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)
	if err := h.cfg.AdminStore.BindChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testDisplayNameAlias, urlResourceID); err != nil {
		t.Fatalf("seed channel alias binding: %v", err)
	}

	_, _, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("set-display-name "+testDisplayNameAlias+" "+testDisplayNameNewName, testAliasTeamID, "U_alias_admin")

	if !strings.Contains(async, "no longer points at an active qURL Connector") || !strings.Contains(async, "unset-alias") {
		t.Errorf("async reply = %q, want the alias-not-a-connector hint", async)
	}
	if capPatch.calls.Load() != 0 {
		t.Errorf("PATCH fired against a non-connector resource (calls = %d)", capPatch.calls.Load())
	}
}

// TestSetDisplayName_ChannelAliasToRevokedTunnelRejected is the production
// scenario the fix most resembles: the alias is bound, but its target connector
// has since been revoked (Status != Active). This exercises the Status clause —
// distinct from the Type clause TestSetDisplayName_ChannelAliasToURLResourceRejected
// covers — so no PATCH fires and the admin gets the unset-alias hint.
func TestSetDisplayName_ChannelAliasToRevokedTunnelRejected(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	capPatch := &capturedPatch{}
	h := newTestHandler(t, displayNameScanQURLServer(t, map[string]any{
		testKeyResourceID: testDisplayNameTunnelRID,
		testKeyType:       client.ResourceTypeTunnel,
		testKeySlug:       testDisplayNameSlug,
		testKeyStatus:     client.StatusRevoked,
	}, capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)
	if err := h.cfg.AdminStore.BindChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testDisplayNameAlias, testDisplayNameTunnelRID); err != nil {
		t.Fatalf("seed channel alias binding: %v", err)
	}

	_, _, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("set-display-name "+testDisplayNameAlias+" "+testDisplayNameNewName, testAliasTeamID, "U_alias_admin")

	if !strings.Contains(async, "no longer points at an active qURL Connector") || !strings.Contains(async, "unset-alias") {
		t.Errorf("async reply = %q, want the alias-not-a-connector hint", async)
	}
	if capPatch.calls.Load() != 0 {
		t.Errorf("PATCH fired against a revoked connector (calls = %d)", capPatch.calls.Load())
	}
}

// displayNameScanWindowQURLServer models a >first-page workspace: the no-slug
// scan returns scanResource as the first page WITH has_more=true (more pages
// remain). It drives both scan-window cases — pass an UNRELATED resource for
// "bound id absent from page 1", or the bound-but-dead resource for "id present
// on page 1 but not an active tunnel". The slug-first GET always misses (the
// alias isn't any connector's slug). PATCH should never fire on these paths.
func displayNameScanWindowQURLServer(t *testing.T, scanResource map[string]any, capPatch *capturedPatch) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == testResourcesPath:
			if r.URL.Query().Get("slug") != "" {
				respondQURLEnvelope(t, w, []map[string]any{})
				return
			}
			// First page = scanResource, with has_more=true (respondQURLEnvelope
			// always reports false, so emit the envelope directly).
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{scanResource},
				"meta": map[string]any{"request_id": "req_test", "has_more": true},
			}); err != nil {
				t.Fatalf("encode qurl envelope: %v", err)
			}
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/resources/"):
			// Record so the no-PATCH assertion reports cleanly if this regresses.
			capPatch.calls.Add(1)
			respondQURLEnvelope(t, w, map[string]any{testKeyResourceID: "r_unexpected", testKeyType: client.ResourceTypeTunnel, testKeyStatus: client.StatusActive})
		default:
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSetDisplayName_ChannelAliasPastScanWindow covers the case cr flagged: the
// alias is bound to a connector that exists but sits past the first
// ListResources page (HasMore, and the bound id is ABSENT from page 1). The
// lookup can't confirm the binding is stale, so it must NOT claim the alias is
// dead or recommend unset-alias (which would unbind a live alias) — it surfaces
// a lookup-limit message and issues no PATCH.
func TestSetDisplayName_ChannelAliasPastScanWindow(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	capPatch := &capturedPatch{}
	// First page holds an UNRELATED connector + more pages; the bound id is absent.
	h := newTestHandler(t, displayNameScanWindowQURLServer(t, map[string]any{
		testKeyResourceID: "r_some_other_connector",
		testKeyType:       client.ResourceTypeTunnel,
		testKeySlug:       "other-connector",
		testKeyStatus:     client.StatusActive,
	}, capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)
	if err := h.cfg.AdminStore.BindChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testDisplayNameAlias, testDisplayNameTunnelRID); err != nil {
		t.Fatalf("seed channel alias binding: %v", err)
	}

	_, _, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("set-display-name "+testDisplayNameAlias+" "+testDisplayNameNewName, testAliasTeamID, "U_alias_admin")

	// Must not nudge the admin to destroy a possibly-live binding.
	if strings.Contains(async, "unset-alias") || strings.Contains(async, "no longer points at an active") {
		t.Errorf("async reply = %q, must not recommend unbinding a possibly-live alias", async)
	}
	if !strings.Contains(async, "lookup limit") {
		t.Errorf("async reply = %q, want the non-destructive lookup-limit message", async)
	}
	if capPatch.calls.Load() != 0 {
		t.Errorf("PATCH fired on an unresolved scan-window lookup (calls = %d)", capPatch.calls.Load())
	}
}

// TestSetDisplayName_RevokedTargetOnFirstPageWithMorePages locks in the round-3
// fix: when the bound resource IS on the first page but revoked, a workspace with
// more pages (HasMore) must still get the DEFINITIVE stale-alias hint — a
// seen-but-dead target is not a scan-window ambiguity, so the soft lookup-limit
// copy must not mask it.
func TestSetDisplayName_RevokedTargetOnFirstPageWithMorePages(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	capPatch := &capturedPatch{}
	// Bound resource present on page 1 but revoked, and more pages exist.
	h := newTestHandler(t, displayNameScanWindowQURLServer(t, map[string]any{
		testKeyResourceID: testDisplayNameTunnelRID,
		testKeyType:       client.ResourceTypeTunnel,
		testKeySlug:       testDisplayNameSlug,
		testKeyStatus:     client.StatusRevoked,
	}, capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)
	if err := h.cfg.AdminStore.BindChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testDisplayNameAlias, testDisplayNameTunnelRID); err != nil {
		t.Fatalf("seed channel alias binding: %v", err)
	}

	_, _, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("set-display-name "+testDisplayNameAlias+" "+testDisplayNameNewName, testAliasTeamID, "U_alias_admin")

	// Seen-but-revoked is definitively stale even with HasMore=true: the admin
	// SHOULD get the unset-alias hint, NOT the soft lookup-limit copy.
	if !strings.Contains(async, "no longer points at an active") || !strings.Contains(async, "unset-alias") {
		t.Errorf("async reply = %q, want the definitive stale-alias hint", async)
	}
	if strings.Contains(async, "lookup limit") {
		t.Errorf("async reply = %q, must not use the scan-window copy for a seen-but-dead target", async)
	}
	if capPatch.calls.Load() != 0 {
		t.Errorf("PATCH fired against a revoked connector (calls = %d)", capPatch.calls.Load())
	}
}

// TestSetDisplayName_NoChannelResolvesBySlug pins the channelID=="" degrade: with
// no channel_id the alias fallback is skipped (it needs a channel), but slug
// resolution must still work — channel_id is not required for it.
func TestSetDisplayName_NoChannelResolvesBySlug(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	capPatch := &capturedPatch{}
	h := newTestHandler(t, displayNameQURLServer(t, testTunnelSlug, capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)

	// channelID "" sends a truly-empty channel_id on the wire.
	_, _, async := newAdminSlashInvokerOnChannel(t, h, "").
		invokeAdminAsync("set-display-name "+testTunnelSlug+" "+testDisplayNameNewName, testAliasTeamID, "U_alias_admin")

	if !strings.Contains(async, testDisplayNameNewName) || !strings.Contains(async, "`"+testTunnelSlug+"`") {
		t.Errorf("async reply = %q, want the slug to resolve with no channel_id", async)
	}
	if capPatch.calls.Load() != 1 {
		t.Fatalf("PATCH calls = %d, want 1 (slug must resolve without a channel)", capPatch.calls.Load())
	}
}

// TestSetDisplayName_ChannelAliasLookupErrorFallsThrough pins the soft-fail: when
// the channel-alias GetItem errors, the resolver logs and falls through to the
// not-found copy rather than surfacing a store error or PATCHing. The admin gate
// reads workspace_mappings (a different table), so it still succeeds — only the
// channel_policies read the alias fallback issues is failed.
func TestSetDisplayName_ChannelAliasLookupErrorFallsThrough(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	capPatch := &capturedPatch{}
	// knownSlug differs from the alias, so the slug-first lookup returns empty and
	// the resolver reaches the channel-alias fallback.
	h := newTestHandler(t, displayNameQURLServer(t, "some-other-slug", capPatch))

	// Seed the admin gate inline (like seedAliasAdminGate) but keep the fake so we
	// can fail the channel_policies GetItem.
	names := defaultTestTableNames()
	ddb := newFakeDDB(t, names, nil)
	ddb.seedItem(t, names.workspace, seedWorkspaceAdmins(testAliasTeamID, testAdminOwnerID, []string{"U_admin", "U_alias_admin"}, testWorkspaceConfiguredAt))
	ddb.SetGetItemErr(names.channelPolicy, errors.New("ddb unavailable"))
	h.cfg.AdminStore = newStoreFromFake(t, ddb, names, nil)

	_, _, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("set-display-name "+testDisplayNameAlias+" "+testDisplayNameNewName, testAliasTeamID, "U_alias_admin")

	if !strings.Contains(async, "No qURL Connector with id") {
		t.Errorf("async reply = %q, want the not-found copy after the alias-lookup error", async)
	}
	if capPatch.calls.Load() != 0 {
		t.Errorf("PATCH fired despite the alias-lookup error (calls = %d)", capPatch.calls.Load())
	}
}

// TestSetDisplayName_LegacyURLBindingRefused mirrors /qurl get's legacy guard: a
// channel alias bound to a raw URL (a pre-resource set-alias row, not an `r_`
// id) is refused with the re-bind hint before any resource scan — never a PATCH,
// and never the misleading scan-window copy.
func TestSetDisplayName_LegacyURLBindingRefused(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	capPatch := &capturedPatch{}
	// knownSlug differs from the alias so the slug-first lookup misses; the alias
	// then resolves to a legacy raw-URL binding.
	h := newTestHandler(t, displayNameQURLServer(t, "some-other-slug", capPatch))
	seedAliasAdminGate(t, h, testAliasTeamID)
	if err := h.cfg.AdminStore.BindChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testDisplayNameAlias, "https://legacy.example.com"); err != nil {
		t.Fatalf("seed legacy URL binding: %v", err)
	}

	_, _, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("set-display-name "+testDisplayNameAlias+" "+testDisplayNameNewName, testAliasTeamID, "U_alias_admin")

	if !strings.Contains(async, "no longer supported") || !strings.Contains(async, "set-alias") {
		t.Errorf("async reply = %q, want the legacy re-bind hint", async)
	}
	if strings.Contains(async, "lookup limit") {
		t.Errorf("async reply = %q, legacy binding must not surface the scan-window copy", async)
	}
	if capPatch.calls.Load() != 0 {
		t.Errorf("PATCH fired for a legacy URL binding (calls = %d)", capPatch.calls.Load())
	}
}
