package internal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

// Shared fixture values for alias-handler tests. Lifted to package
// level so goconst (3+ occurrences) stays clean and a change to
// "what's the test workspace?" only happens in one place.
const (
	testAliasTeamID    = "T1"
	testAliasChannelID = "C1"
	testAliasURL       = "https://example.com"
	testAliasName      = "staging"
	testSlashCmd       = "/qurl"
	testOtherAlias     = "other"
	// testResourcesPath is the qurl-service list/lookup endpoint the
	// slug-target set-alias path hits. Lifted so the slug-resolving
	// test servers in this file don't trip goconst on the literal.
	testResourcesPath = "/v1/resources"
)

// fakeAliasStore is an in-memory AliasStore for handler tests. One
// row per (team, channel), each row carrying a `bindings` map keyed
// by alias name — the same shape the channel_policies table's
// alias_bindings Map attribute enforces. Methods are safe under
// t.Parallel since the handler-level tests don't fan out concurrent
// reads on the same row.
type fakeAliasStore struct {
	mu sync.Mutex
	// rows is keyed by team|channel; value is alias→resourceID.
	rows map[string]map[string]string

	// errBind, errUnbind let tests inject deterministic errors without
	// spinning a real DDB fake. Set to a slackdata sentinel to exercise
	// the typed-conflict branches.
	errBind   error
	errUnbind error

	// blockUnbind, when true, makes UnbindChannelAlias wait on ctx.Done()
	// and return ctx.Err(). Fences the aliasSyncTimeout budget on the
	// synchronous unset-alias path (a slow DDB delete must surface as a
	// clear-failed reply, not block past Slack's ack window).
	blockUnbind bool
}

func newFakeAliasStore() *fakeAliasStore {
	return &fakeAliasStore{rows: map[string]map[string]string{}}
}

func (f *fakeAliasStore) key(teamID, channelID string) string {
	return teamID + "|" + channelID
}

func (f *fakeAliasStore) BindChannelAlias(_ context.Context, teamID, channelID, aliasName, resourceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errBind != nil {
		return f.errBind
	}
	k := f.key(teamID, channelID)
	row, ok := f.rows[k]
	if !ok {
		row = map[string]string{}
		f.rows[k] = row
	}
	if _, exists := row[aliasName]; exists {
		return slackdata.ErrAliasAlreadyBound
	}
	row[aliasName] = resourceID
	return nil
}

func (f *fakeAliasStore) UnbindChannelAlias(ctx context.Context, teamID, channelID, aliasName string) error {
	// Read the block flag before taking the row lock so the timeout test
	// doesn't have to coordinate with anything else holding f.mu.
	f.mu.Lock()
	block := f.blockUnbind
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errUnbind != nil {
		return f.errUnbind
	}
	k := f.key(teamID, channelID)
	row, ok := f.rows[k]
	if !ok {
		return slackdata.ErrAliasNotFound
	}
	if _, exists := row[aliasName]; !exists {
		return slackdata.ErrAliasNotFound
	}
	delete(row, aliasName)
	if len(row) == 0 {
		// Mirror the real-store invariant: row is created lazily on
		// first bind and conceptually disappears when emptied, so
		// "no aliases" looks the same as "row never existed."
		delete(f.rows, k)
	}
	return nil
}

// bindings returns a copy of the current alias→resourceID map for
// (teamID, channelID), or nil if the row has no bindings. Test-only
// inspection helper.
func (f *fakeAliasStore) bindings(teamID, channelID string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[f.key(teamID, channelID)]
	if !ok {
		return nil
	}
	out := make(map[string]string, len(row))
	for k, v := range row {
		out[k] = v
	}
	return out
}

// newAliasTestHandler builds a Handler wired with a fakeAliasStore.
// Returns both so individual tests can pre-seed rows or inject error
// states without touching the handler internals.
func newAliasTestHandler(t *testing.T) (*Handler, *fakeAliasStore) {
	t.Helper()
	h := newTestHandler(t, noopQURLServer(t))
	store := newFakeAliasStore()
	h.aliasStore = store
	return h, store
}

// aliasSlashRequest builds a signed /slack/commands request for the
// given text, posted by a user in (teamID, channelID). Returns the
// body twice: once as the request body, once as the sign-body (the
// pair matches the newSignedRequest contract used elsewhere in
// handler_test.go).
func aliasSlashRequest(t *testing.T, text, teamID, channelID string) (body, signBody string) {
	t.Helper()
	body = url.Values{
		fieldCommand:   {testSlashCmd},
		fieldText:      {text},
		fieldTeamID:    {teamID},
		fieldChannelID: {channelID},
		fieldUserID:    {"U_admin"},
		fieldTriggerID: {"trig-1"},
	}.Encode()
	return body, body
}

// decodeSlackText extracts the ephemeral text from a slash-command
// response body. Centralized so the "response_type+text" envelope
// shape stays asserted in one place.
func decodeSlackText(t *testing.T, raw []byte) string {
	t.Helper()
	var resp map[string]string
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal response body: %v\nraw=%s", err, string(raw))
	}
	if resp[respFieldResponseType] != respTypeEphemeral {
		t.Errorf("response_type = %q, want %q", resp[respFieldResponseType], respTypeEphemeral)
	}
	return resp[respFieldText]
}

// --- parser unit tests ---------------------------------------------------

func TestParseAliasArgs_SetAlias(t *testing.T) {
	// notTunnelSub is the substring unique to msgAliasTargetNotTunnel
	// (the not-a-`$slug` rejection), used to assert that any non-`$`
	// target — a URL, an `r_<id>`, or a sigil-less typo — gets the
	// uniform not-a-tunnel copy rather than the generic usage dump.
	const notTunnelSub = "aren't supported"
	cases := []struct {
		name      string
		input     string
		wantErr   bool
		wantAlias string
		wantTgt   string
		// wantMsgSub, when set on a wantErr case, asserts which rejection
		// copy fired — the not-a-tunnel copy (msgAliasTargetNotTunnel)
		// for any non-`$` target, vs the usage dump
		// (msgAliasTargetInvalid) for a malformed alias or a `$`-prefixed
		// token that fails the slug grammar. Empty skips the copy check.
		wantMsgSub string
	}{
		// Tunnels-only: the sole accepted target shape is a tunnel `$slug`.
		{name: "happy tunnel slug", input: "$staging $prod-dashboard", wantAlias: testAliasName, wantTgt: "$prod-dashboard"},
		{name: "single-char alias allowed with slug target", input: "$a $prod-dashboard", wantAlias: "a", wantTgt: "$prod-dashboard"},
		{name: "internal dashes allowed in alias", input: "$demo-grafana $prod-dashboard", wantAlias: "demo-grafana", wantTgt: "$prod-dashboard"},

		// URL / resource-id targets are well-formed but unsupported now —
		// uniform not-a-tunnel copy (a valid r_<id> and a r_<typo> read
		// the same; the URL too).
		{name: "URL target rejected as not-a-tunnel", input: "$staging https://example.com", wantErr: true, wantMsgSub: notTunnelSub},
		{name: "localhost URL target rejected as not-a-tunnel", input: "$staging http://localhost:3000", wantErr: true, wantMsgSub: notTunnelSub},
		{name: "resource id target rejected as not-a-tunnel", input: "$staging r_abc123", wantErr: true, wantMsgSub: notTunnelSub},
		{name: "bare r_ target rejected as not-a-tunnel", input: "$staging r_", wantErr: true, wantMsgSub: notTunnelSub},
		{name: "garbage non-url target rejected as not-a-tunnel", input: "$staging not-a-url", wantErr: true, wantMsgSub: notTunnelSub},
		{name: "non-http scheme target rejected as not-a-tunnel", input: "$staging ftp://example.com", wantErr: true, wantMsgSub: notTunnelSub},

		// Alias-name / arity errors → usage dump.
		{name: "missing target", input: "$staging", wantErr: true},
		{name: "missing alias", input: testAliasURL, wantErr: true},
		{name: "no sigil", input: "staging $prod-dashboard", wantErr: true},
		{name: "empty alias after sigil", input: "$ $prod-dashboard", wantErr: true},
		{name: "uppercase alias rejected", input: "$Staging $prod-dashboard", wantErr: true},
		{name: "trailing dash alias rejected", input: "$staging- $prod-dashboard", wantErr: true},
		{name: "leading dash alias rejected", input: "$-staging $prod-dashboard", wantErr: true},
		{name: "extra args rejected", input: "$staging $prod-dashboard extra", wantErr: true},
		{name: "alias over cap rejected", input: "$" + strings.Repeat("a", 65) + " $prod-dashboard", wantErr: true},
		// A `$slug` target that fails the tunnel-slug grammar → usage
		// dump (it passed the `$`-prefix gate but isn't a valid slug).
		{name: "slug target too short rejected", input: "$staging $ab", wantErr: true, wantMsgSub: "tunnel slug"},
		{name: "slug target uppercase rejected", input: "$staging $Prod", wantErr: true, wantMsgSub: "tunnel slug"},
		// Backtick / control byte in the target token are rejected before
		// the slug check so the success-copy fence + audit log stay clean.
		{name: "backtick in slug target rejected", input: "$staging $prod`bad", wantErr: true},
		{name: "control byte in slug target rejected", input: "$staging $prod\x01bad", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, userMsg := parseAliasArgs(tc.input, true)
			if tc.wantErr {
				if userMsg == "" {
					t.Fatalf("expected rejection, got %+v", got)
				}
				if tc.wantMsgSub != "" && !strings.Contains(userMsg, tc.wantMsgSub) {
					t.Errorf("rejection copy = %q, want substring %q", userMsg, tc.wantMsgSub)
				}
				return
			}
			if userMsg != "" {
				t.Fatalf("unexpected rejection: %s", userMsg)
			}
			if got.Alias != tc.wantAlias {
				t.Errorf("Alias = %q, want %q", got.Alias, tc.wantAlias)
			}
			if got.Target != tc.wantTgt {
				t.Errorf("Target = %q, want %q", got.Target, tc.wantTgt)
			}
		})
	}
}

func TestParseAliasArgs_UnsetAlias(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantErr   bool
		wantAlias string
	}{
		{name: "happy", input: "$staging", wantAlias: testAliasName},
		{name: "trailing args rejected", input: "$staging extra", wantErr: true},
		{name: "missing alias", input: "", wantErr: true},
		{name: "no sigil", input: "staging", wantErr: true},
		{name: "uppercase rejected", input: "$Staging", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, userMsg := parseAliasArgs(tc.input, false)
			if tc.wantErr {
				if userMsg == "" {
					t.Fatalf("expected rejection, got %+v", got)
				}
				return
			}
			if userMsg != "" {
				t.Fatalf("unexpected rejection: %s", userMsg)
			}
			if got.Alias != tc.wantAlias {
				t.Errorf("Alias = %q, want %q", got.Alias, tc.wantAlias)
			}
			if got.Target != "" {
				t.Errorf("Target = %q, want empty (unsetalias has no target)", got.Target)
			}
		})
	}
}

// --- handler-level tests -------------------------------------------------

// TestSetAlias_URLTargetRejected fences the tunnels-only gate: a
// well-formed URL target parses cleanly but is refused synchronously
// with the not-a-tunnel copy, and never touches the store. set-alias
// points an alias at a tunnel `$slug` now.
func TestSetAlias_URLTargetRejected(t *testing.T) {
	h, store := newAliasTestHandler(t)
	body, sign := aliasSlashRequest(t, "setalias $staging https://example.com", testAliasTeamID, testAliasChannelID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "URLs and resource IDs aren't supported") {
		t.Errorf("response = %q, want not-a-tunnel rejection copy", got)
	}
	if b := store.bindings(testAliasTeamID, testAliasChannelID); b != nil {
		t.Errorf("URL-target rejection should not touch the store, got bindings=%v", b)
	}
}

// TestSetAlias_ResourceIDTargetRejected is the resource-id counterpart
// to TestSetAlias_URLTargetRejected: a raw `r_<id>` target is no longer
// an accepted set-alias target either — only a tunnel `$slug` is.
func TestSetAlias_ResourceIDTargetRejected(t *testing.T) {
	h, store := newAliasTestHandler(t)
	body, sign := aliasSlashRequest(t, "setalias $staging r_abc123", testAliasTeamID, testAliasChannelID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "URLs and resource IDs aren't supported") {
		t.Errorf("response = %q, want not-a-tunnel rejection copy", got)
	}
	if b := store.bindings(testAliasTeamID, testAliasChannelID); b != nil {
		t.Errorf("resource-id-target rejection should not touch the store, got bindings=%v", b)
	}
}

// TestSetAlias_HyphenatedHappyTunnelSlug fences that the hyphenated
// `set-alias` command form resolves a tunnel `$slug` target end-to-end
// (the dispatcher accepts both `setalias` and `set-alias`). Mirrors
// TestSetAlias_HappyTunnelSlug but exercises the hyphenated verb.
func TestSetAlias_HyphenatedHappyTunnelSlug(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != testResourcesPath {
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		respondQURLEnvelope(t, w, []map[string]any{{
			testKeyResourceID: testTunnelResourceID,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testTunnelSlug,
			testKeyStatus:     client.StatusActive,
		}})
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	store := newFakeAliasStore()
	h.aliasStore = store
	_, ack, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("set-alias $staging $"+testTunnelSlug, testAliasTeamID, "U_alias_admin")
	if ack != ackWorkingOnIt {
		t.Fatalf("ack = %q, want async working copy", ack)
	}
	// Success copy echoes the slug the admin typed, not the opaque
	// resolved resource_id.
	if !strings.Contains(async, "$"+testTunnelSlug) {
		t.Errorf("async response = %q, want the typed slug $%s", async, testTunnelSlug)
	}
	if strings.Contains(async, testTunnelResourceID) {
		t.Errorf("async response = %q leaked the opaque resource_id", async)
	}
	// The binding still stores the resolved resource_id.
	b := store.bindings(testAliasTeamID, testAliasChannelID)
	if b[testAliasName] != testTunnelResourceID {
		t.Errorf("stored bindings = %v, want {%q: %s}", b, testAliasName, testTunnelResourceID)
	}
}

func TestSetAlias_HappyTunnelSlug(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/resources" {
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		gotQuery = r.URL.Query()
		respondQURLEnvelope(t, w, []map[string]any{{
			testKeyResourceID: testTunnelResourceID,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testTunnelSlug,
			testKeyStatus:     client.StatusActive,
		}})
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	store := newFakeAliasStore()
	h.aliasStore = store
	status, ack, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("set-alias $staging $"+testTunnelSlug, testAliasTeamID, "U_alias_admin")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if ack != ackWorkingOnIt {
		t.Fatalf("ack = %q, want async working copy", ack)
	}
	// Success copy echoes the typed slug, not the opaque resource_id.
	if !strings.Contains(async, "$"+testTunnelSlug) {
		t.Errorf("async response = %q, want the typed slug $%s", async, testTunnelSlug)
	}
	if strings.Contains(async, testTunnelResourceID) {
		t.Errorf("async response = %q leaked the opaque resource_id", async)
	}
	if gotQuery.Get("slug") != testTunnelSlug {
		t.Errorf("upstream query = %v, want slug=%q", gotQuery, testTunnelSlug)
	}
	if gotQuery.Get("limit") != "" || gotQuery.Get("cursor") != "" {
		t.Errorf("upstream query = %v, want no pagination params", gotQuery)
	}
	b := store.bindings(testAliasTeamID, testAliasChannelID)
	if b[testAliasName] != testTunnelResourceID {
		t.Errorf("stored bindings = %v, want {%q: %s}", b, testAliasName, testTunnelResourceID)
	}
}

func TestSetAlias_TunnelSlugMissingDoesNotCreateResource(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	var postHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postHits.Add(1)
			t.Fatalf("setalias $slug must not create resources; got %s %s", r.Method, r.URL.Path)
		}
		if r.Method != http.MethodGet || r.URL.Path != "/v1/resources" {
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		if gotSlug := r.URL.Query().Get("slug"); gotSlug != testTunnelSlug {
			t.Fatalf("upstream slug query = %q, want %q", gotSlug, testTunnelSlug)
		}
		respondQURLEnvelope(t, w, []map[string]any{})
	}))
	t.Cleanup(srv.Close)

	h := newTestHandler(t, srv)
	store := newFakeAliasStore()
	h.aliasStore = store

	_, ack, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("setalias $staging $"+testTunnelSlug, testAliasTeamID, "U_alias_admin")

	if ack != ackWorkingOnIt {
		t.Fatalf("ack = %q, want async working copy", ack)
	}
	if !strings.Contains(async, "was not found") {
		t.Fatalf("async response = %q, want not-found copy", async)
	}
	if got := postHits.Load(); got != 0 {
		t.Fatalf("POST hits = %d, want 0", got)
	}
	if b := store.bindings(testAliasTeamID, testAliasChannelID); b != nil {
		t.Fatalf("alias store touched on missing slug: %v", b)
	}
}

func TestSetAlias_ParserError(t *testing.T) {
	// Fence: a parser-rejection path must not call into the store.
	// Counts as a regression test for "admin-gate before resolution"
	// — the parser sits in front of every store call, so a malformed
	// command can't probe for resource existence.
	h, store := newAliasTestHandler(t)
	body, sign := aliasSlashRequest(t, "setalias staging https://example.com", testAliasTeamID, testAliasChannelID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "must start with `$`") {
		t.Errorf("response = %q, want sigil error", got)
	}
	if b := store.bindings(testAliasTeamID, testAliasChannelID); b != nil {
		t.Errorf("store was touched on parser-rejection path: %v", b)
	}
}

// slugResolvingQURLServer returns an httptest server that answers
// GET /v1/resources?slug=<s> with a single active tunnel whose
// resource_id is "r_"+<s>. Lets a multi-alias test resolve several
// distinct `$slug` targets to distinct resource_ids without
// per-slug fixture bookkeeping.
func slugResolvingQURLServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != testResourcesPath {
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		slug := r.URL.Query().Get("slug")
		respondQURLEnvelope(t, w, []map[string]any{{
			testKeyResourceID: "r_" + slug,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       slug,
			testKeyStatus:     client.StatusActive,
		}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSetChannelAlias_SecondAliasOnSameChannelSucceeds(t *testing.T) {
	// Schema decision 2026-05-17: a channel can host many aliases
	// simultaneously. After binding $a, binding $b must succeed; both
	// bindings must coexist in the resulting row. Tunnels-only: each
	// alias points at a distinct tunnel slug.
	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, slugResolvingQURLServer(t))
	store := newFakeAliasStore()
	h.aliasStore = store

	_, _, async1 := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("setalias $a $tunnel-a", testAliasTeamID, "U_alias_admin")
	if !strings.Contains(async1, "now points to") {
		t.Fatalf("first bind response = %q, want success copy", async1)
	}

	_, _, async2 := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("setalias $b $tunnel-b", testAliasTeamID, "U_alias_admin")
	if !strings.Contains(async2, "now points to") || !strings.Contains(async2, "$b") {
		t.Errorf("second bind response = %q, want success copy referencing $b", async2)
	}

	b := store.bindings(testAliasTeamID, testAliasChannelID)
	if b["a"] != "r_tunnel-a" {
		t.Errorf("$a binding lost: bindings = %v", b)
	}
	if b["b"] != "r_tunnel-b" {
		t.Errorf("$b binding missing: bindings = %v", b)
	}
}

func TestSetChannelAlias_DuplicateAliasIsRefused(t *testing.T) {
	// Binding the same alias name twice in the same channel must
	// surface as a typed conflict (slackdata.ErrAliasAlreadyBound). A
	// different slug (hence resource id) on the second call does NOT
	// silently overwrite. Tunnels-only: both calls target a tunnel slug.
	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, slugResolvingQURLServer(t))
	store := newFakeAliasStore()
	h.aliasStore = store

	_, _, async1 := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("setalias $staging $first-tun", testAliasTeamID, "U_alias_admin")
	if !strings.Contains(async1, "now points to") {
		t.Fatalf("first bind response = %q, want success copy", async1)
	}

	_, _, async2 := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("setalias $staging $second-tun", testAliasTeamID, "U_alias_admin")
	if !strings.Contains(async2, "already bound") || !strings.Contains(async2, "$staging") {
		t.Errorf("duplicate bind response = %q, want already-bound copy", async2)
	}
	// Refusal copy must NOT leak the bound target (info-disclosure
	// narrowing carried over from the single-alias era).
	if strings.Contains(async2, "r_first-tun") {
		t.Errorf("refusal leaked bound target: %q", async2)
	}

	// Original binding intact.
	b := store.bindings(testAliasTeamID, testAliasChannelID)
	if b[testAliasName] != "r_first-tun" {
		t.Errorf("original binding clobbered: bindings = %v", b)
	}
}

// TestSetAlias_MissingTeamOrChannelID pins the defensive guard in
// aliasPreamble: a Slack form payload that arrives without team_id or
// channel_id surfaces the "Could not read your Slack workspace or
// channel ID" copy and never dials the store. Catches a malformed
// fixture or a future regression in form parsing rather than silently
// writing a row keyed on empty strings.
func TestSetAlias_MissingTeamOrChannelID(t *testing.T) {
	h, store := newAliasTestHandler(t)
	// Slug target: the missing-id guard in aliasValidate fires before
	// runAsync, so the noop upstream is never dialed. The missing-id text
	// assertions below are the ordering fence — if the guard regressed
	// below the slug-resolve dial, the reply would become the "Tunnel
	// slug not found" copy and fail the Contains check.
	// channel_id empty.
	body, sign := aliasSlashRequest(t, "setalias $staging $"+testTunnelSlug, testAliasTeamID, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))
	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "Could not read your Slack workspace or channel ID") {
		t.Errorf("missing channel_id response = %q, want defensive-guard copy", got)
	}
	// team_id empty.
	body2, sign2 := aliasSlashRequest(t, "setalias $staging $"+testTunnelSlug, "", testAliasChannelID)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, newSignedRequest(t, "/slack/commands", body2, sign2))
	got2 := decodeSlackText(t, w2.Body.Bytes())
	if !strings.Contains(got2, "Could not read your Slack workspace or channel ID") {
		t.Errorf("missing team_id response = %q, want defensive-guard copy", got2)
	}
	// Store must not have been dialed in either branch.
	if b := store.bindings(testAliasTeamID, testAliasChannelID); b != nil {
		t.Errorf("missing-id guard should have short-circuited before store dial, got bindings=%v", b)
	}
}

func TestSetAlias_NoStoreWired(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	// Slug target: the not-configured guard in aliasPreamble fires
	// before any qURL lookup, so this stays a synchronous reply.
	body, sign := aliasSlashRequest(t, "setalias $staging $"+testTunnelSlug, testAliasTeamID, testAliasChannelID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "not configured") {
		t.Errorf("response = %q, want not-configured copy", got)
	}
}

func TestSetAlias_StoreWriteError(t *testing.T) {
	// Slug resolves upstream, then the DDB bind fails — the async
	// follow-up surfaces the update-failed copy.
	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, slugResolvingQURLServer(t))
	store := newFakeAliasStore()
	store.errBind = errors.New("ddb conditional failure")
	h.aliasStore = store

	_, _, async := newAdminSlashInvokerOnChannel(t, h, testAliasChannelID).
		invokeAdminAsync("setalias $staging $"+testTunnelSlug, testAliasTeamID, "U_alias_admin")
	if !strings.Contains(async, "Failed to update alias") {
		t.Errorf("response = %q, want update-failed copy", async)
	}
}

func TestUnsetAlias_Happy(t *testing.T) {
	h, store := newAliasTestHandler(t)
	if err := store.BindChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testAliasName, testAliasURL); err != nil {
		t.Fatalf("seed bind: %v", err)
	}

	body, sign := aliasSlashRequest(t, "unsetalias $staging", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "no longer bound") {
		t.Errorf("response = %q, want unset success copy", got)
	}
	if b := store.bindings(testAliasTeamID, testAliasChannelID); b != nil {
		t.Errorf("expected row cleared, got bindings=%v", b)
	}
}

func TestClearChannelAlias_OneOfManyLeavesOthers(t *testing.T) {
	// Multi-alias semantics: clearing $a leaves $b on the same row.
	h, store := newAliasTestHandler(t)
	ctx := context.Background()
	if err := store.BindChannelAlias(ctx, testAliasTeamID, testAliasChannelID, "a", "https://a.example"); err != nil {
		t.Fatalf("seed $a: %v", err)
	}
	if err := store.BindChannelAlias(ctx, testAliasTeamID, testAliasChannelID, "b", "https://b.example"); err != nil {
		t.Fatalf("seed $b: %v", err)
	}

	body, sign := aliasSlashRequest(t, "unsetalias $a", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "no longer bound") || !strings.Contains(got, "$a") {
		t.Errorf("response = %q, want unset success copy referencing $a", got)
	}

	b := store.bindings(testAliasTeamID, testAliasChannelID)
	if _, ok := b["a"]; ok {
		t.Errorf("$a was not cleared: bindings = %v", b)
	}
	if b["b"] != "https://b.example" {
		t.Errorf("$b was disturbed: bindings = %v", b)
	}
}

func TestClearChannelAlias_NotPresentIsNoop(t *testing.T) {
	// Clearing an alias that isn't bound surfaces the typed
	// slackdata.ErrAliasNotFound branch as "not bound; nothing to clear."
	h, _ := newAliasTestHandler(t)

	body, sign := aliasSlashRequest(t, "unsetalias $staging", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "not bound") || !strings.Contains(got, "$staging") {
		t.Errorf("response = %q, want not-bound copy referencing $staging", got)
	}
}

func TestClearChannelAlias_NotPresentWithOtherAliasBound(t *testing.T) {
	// Clearing $foo when only $other is bound must return the
	// typed not-found refusal AND leave $other intact. This is the
	// multi-alias analog of the old "mismatch refuses" test.
	h, store := newAliasTestHandler(t)
	if err := store.BindChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testOtherAlias, "r_existing"); err != nil {
		t.Fatalf("seed bind: %v", err)
	}

	body, sign := aliasSlashRequest(t, "unsetalias $foo", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "not bound") || !strings.Contains(got, "$foo") {
		t.Errorf("response = %q, want not-bound refusal referencing $foo", got)
	}
	b := store.bindings(testAliasTeamID, testAliasChannelID)
	if b[testOtherAlias] != "r_existing" {
		t.Errorf("$other was disturbed: bindings = %v", b)
	}
}

func TestUnsetAlias_StoreWriteError(t *testing.T) {
	h, store := newAliasTestHandler(t)
	store.errUnbind = errors.New("ddb transient failure")

	body, sign := aliasSlashRequest(t, "unsetalias $staging", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "Failed to clear alias") {
		t.Errorf("response = %q, want clear-failed copy", got)
	}
}

// TestUnsetAlias_SyncTimeoutExceeded fences the aliasSyncTimeout budget
// on the synchronous unset-alias path: an UnbindChannelAlias that
// doesn't return inside the budget must surface as a clear-failed reply
// (ctx deadline exceeded) rather than block past Slack's 3-second ack
// window. Shortens aliasSyncTimeout for the test so the suite doesn't
// pay a 2.5s real-time tax. (set-alias has no sync path to fence — it
// runs async via runAsync's own ctx.)
func TestUnsetAlias_SyncTimeoutExceeded(t *testing.T) {
	orig := aliasSyncTimeout
	aliasSyncTimeout = 25 * time.Millisecond
	t.Cleanup(func() { aliasSyncTimeout = orig })

	h, store := newAliasTestHandler(t)
	store.blockUnbind = true

	body, sign := aliasSlashRequest(t, "unsetalias $staging", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "Failed to clear alias") {
		t.Errorf("response = %q, want clear-failed copy (ctx deadline exceeded)", got)
	}
}

func TestUnsetAlias_NoStoreWired(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	body, sign := aliasSlashRequest(t, "unsetalias $staging", testAliasTeamID, testAliasChannelID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "not configured") {
		t.Errorf("response = %q, want not-configured copy", got)
	}
}

func TestUnsetAlias_HyphenatedForm(t *testing.T) {
	h, store := newAliasTestHandler(t)
	if err := store.BindChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testAliasName, testAliasURL); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	body, sign := aliasSlashRequest(t, "unset-alias $staging", testAliasTeamID, testAliasChannelID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "no longer bound") {
		t.Fatalf("response = %q, want cleared copy", got)
	}
	if b := store.bindings(testAliasTeamID, testAliasChannelID); b != nil {
		t.Fatalf("bindings after unset-alias = %v, want none", b)
	}
}

func TestHelpListsNewVerbs(t *testing.T) {
	// CR feedback fence from old #230: /qurl help must surface
	// setalias and unsetalias when the alias store is wired. The
	// old PR had a stale helpMessage() that listed only
	// create/list/help even after the alias verbs landed.
	h, _ := newAliasTestHandler(t)
	body, sign := aliasSlashRequest(t, "help", testAliasTeamID, testAliasChannelID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "set-alias") {
		t.Errorf("/qurl help = %q, missing set-alias", got)
	}
	if !strings.Contains(got, "unset-alias") {
		t.Errorf("/qurl help = %q, missing unset-alias", got)
	}
	if strings.Contains(got, "tunnel install") {
		t.Errorf("/qurl help = %q, advertised tunnel install without AdminStore", got)
	}
}

// TestHelpHidesAliasVerbsWhenAliasStoreNil fences round-19 cr #2:
// on a sandbox deploy without an aliasStore the setalias / unsetalias
// verbs reply "not configured" — help text must not advertise them.
// Mirrors the PostDM / OpenView gates above. Without this gate, a
// regression that re-advertised the verbs unconditionally would
// confuse users into running a command whose only reply is the
// not-configured error.
func TestHelpHidesAliasVerbsWhenAliasStoreNil(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	// newTestHandler doesn't wire an aliasStore — the not-configured
	// state we're fencing.
	body, sign := aliasSlashRequest(t, "help", testAliasTeamID, testAliasChannelID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if strings.Contains(got, "set-alias") {
		t.Errorf("/qurl help = %q, leaked set-alias verb on unwired aliasStore", got)
	}
	if strings.Contains(got, "unset-alias") {
		t.Errorf("/qurl help = %q, leaked unset-alias verb on unwired aliasStore", got)
	}
	// help itself MUST still appear — the gate only hides the
	// alias-bearing verbs, not the help line.
	if !strings.Contains(got, "/qurl help") {
		t.Errorf("/qurl help = %q, missing help line", got)
	}
}

// TestSetAlias_CrossTenancyIsolation fences that an alias bound in
// (T1, C1) is invisible to (T2, C1) and vice versa. The
// channel_policies PK is `slack_team_id` so a T2 admin's setalias on
// the same channel-ID must observe "no alias" — not T1's binding.
// fakeAliasStore keys on `team|channel` so this is structurally
// covered, but a regression that switched key construction (e.g. PK
// = channel only) would break tenancy hard, so pin the fence here.
func TestSetAlias_CrossTenancyIsolation(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, slugResolvingQURLServer(t))
	store := newFakeAliasStore()
	h.aliasStore = store
	// Seed T1's binding directly (the stored value is opaque; T1 is the
	// tenant we're fencing isolation against).
	if err := store.BindChannelAlias(context.Background(), "T1", "C_shared", testAliasName, testAliasURL); err != nil {
		t.Fatalf("seed T1 bind: %v", err)
	}

	// T2 admin in the same channel ID — should see "no alias" and
	// be allowed to set their own (a tunnel slug target).
	_, _, async := newAdminSlashInvokerOnChannel(t, h, "C_shared").
		invokeAdminAsync("setalias $other $t2-tun", "T2", "U_alias_admin")
	if !strings.Contains(async, "now points to") {
		t.Errorf("T2 setalias should succeed; response = %q", async)
	}

	// T1's binding should be intact.
	t1 := store.bindings("T1", "C_shared")
	if t1[testAliasName] != testAliasURL {
		t.Errorf("T1 binding was disturbed: got %v, want {%q: %q}", t1, testAliasName, testAliasURL)
	}
	// T2 wrote its own row.
	t2 := store.bindings("T2", "C_shared")
	if t2[testOtherAlias] != "r_t2-tun" {
		t.Errorf("T2 binding missing: got %v", t2)
	}
}

func TestSetAliasStore_DoubleSetPanics(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	store := newFakeAliasStore()
	h.SetAliasStore(store)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on second SetAliasStore, got none")
		}
	}()
	h.SetAliasStore(store)
}

// TestSetAliasStore_NilPassthrough pins the documented contract that
// a nil argument is a no-op (the verbs reply with the "not configured"
// copy via the aliasPreamble guard) and that a defensive
// SetAliasStore(nil) doesn't block a later real wiring. This is the
// shape cmd/main.go uses on sandbox deploys.
func TestSetAliasStore_NilPassthrough(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	// nil is a no-op; field stays nil.
	h.SetAliasStore(nil)
	if h.aliasStore != nil {
		t.Fatal("SetAliasStore(nil) should leave aliasStore unset")
	}
	// Real wiring afterward still works.
	store := newFakeAliasStore()
	h.SetAliasStore(store)
	if h.aliasStore == nil {
		t.Fatal("SetAliasStore(store) should wire the field after a prior nil call")
	}
	// A second nil call after a real store is still a no-op (doesn't
	// swap or panic).
	h.SetAliasStore(nil)
	if h.aliasStore == nil {
		t.Fatal("SetAliasStore(nil) after wiring should not clear the field")
	}
}
