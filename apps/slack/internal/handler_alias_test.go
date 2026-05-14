package internal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
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
)

// fakeAliasStore is an in-memory AliasStore for handler tests. One
// record per (team, channel) — the same shape the post-pivot
// channel_policies table enforces. Methods are safe under t.Parallel
// since the handler-level tests don't fan out concurrent reads on
// the same row.
type fakeAliasStore struct {
	mu sync.Mutex
	// rows is keyed by team|channel; value is "alias|resourceID".
	rows map[string]aliasRow

	// errLookup, errSet, errClear let tests inject deterministic
	// errors without spinning a real DDB fake.
	errLookup error
	errSet    error
	errClear  error
}

type aliasRow struct {
	alias      string
	resourceID string
}

func newFakeAliasStore() *fakeAliasStore {
	return &fakeAliasStore{rows: map[string]aliasRow{}}
}

func (f *fakeAliasStore) key(teamID, channelID string) string {
	return teamID + "|" + channelID
}

func (f *fakeAliasStore) LookupChannelAlias(_ context.Context, teamID, channelID string) (alias, resourceID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errLookup != nil {
		return "", "", f.errLookup
	}
	r, ok := f.rows[f.key(teamID, channelID)]
	if !ok {
		return "", "", ErrAliasNotFound
	}
	return r.alias, r.resourceID, nil
}

func (f *fakeAliasStore) SetChannelAlias(_ context.Context, teamID, channelID, alias, resourceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errSet != nil {
		return f.errSet
	}
	f.rows[f.key(teamID, channelID)] = aliasRow{alias: alias, resourceID: resourceID}
	return nil
}

func (f *fakeAliasStore) ClearChannelAlias(_ context.Context, teamID, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errClear != nil {
		return f.errClear
	}
	k := f.key(teamID, channelID)
	if _, ok := f.rows[k]; !ok {
		return ErrAliasNotFound
	}
	delete(f.rows, k)
	return nil
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
		t.Errorf("response_type = %q, want %q", resp["response_type"], respTypeEphemeral)
	}
	return resp[respFieldText]
}

// --- parser unit tests ---------------------------------------------------

func TestParseAliasArgs_SetAlias(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantErr   bool
		wantAlias string
		wantTgt   string
	}{
		{name: "happy URL", input: "$staging https://example.com", wantAlias: testAliasName, wantTgt: testAliasURL},
		{name: "happy resource id", input: "$staging r_abc123", wantAlias: testAliasName, wantTgt: "r_abc123"},
		{name: "single-char alias allowed", input: "$a https://x.example", wantAlias: "a", wantTgt: "https://x.example"},
		{name: "internal dashes allowed", input: "$demo-grafana https://x.example", wantAlias: "demo-grafana", wantTgt: "https://x.example"},

		{name: "missing target", input: "$staging", wantErr: true},
		{name: "missing alias", input: testAliasURL, wantErr: true},
		{name: "no sigil", input: "staging https://example.com", wantErr: true},
		{name: "empty alias after sigil", input: "$ https://example.com", wantErr: true},
		{name: "uppercase rejected", input: "$Staging https://example.com", wantErr: true},
		{name: "trailing dash rejected", input: "$staging- https://example.com", wantErr: true},
		{name: "leading dash rejected", input: "$-staging https://example.com", wantErr: true},
		{name: "extra args rejected", input: "$staging https://example.com extra", wantErr: true},
		{name: "non-http target rejected", input: "$staging ftp://example.com", wantErr: true},
		{name: "garbage target rejected", input: "$staging not-a-url", wantErr: true},
		{name: "alias over cap rejected", input: "$" + strings.Repeat("a", 65) + " https://x.example", wantErr: true},
		{name: "bare r_ sigil rejected", input: "$staging r_", wantErr: true},

		// Fence: http://localhost is currently ACCEPTED. The parser
		// stays scheme-permissive because qurl-service is the
		// authoritative validator on target reachability. If/when
		// the parser grows a public-host gate (claude-bot review #3
		// SSRF-adjacent follow-up), flip this row to wantErr and
		// the test starts enforcing the new contract.
		{name: "localhost target ACCEPTED (TODO: SSRF gate)", input: "$staging http://localhost:3000", wantAlias: "staging", wantTgt: "http://localhost:3000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, userMsg := parseAliasArgs(tc.input, true)
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

func TestSetAlias_HappyURL(t *testing.T) {
	h, store := newAliasTestHandler(t)
	body, sign := aliasSlashRequest(t, "setalias $staging https://example.com", testAliasTeamID, testAliasChannelID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "now points to") || !strings.Contains(got, "$staging") {
		t.Errorf("response = %q, missing success copy", got)
	}
	a, rid, err := store.LookupChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID)
	if err != nil {
		t.Fatalf("post-setalias lookup: %v", err)
	}
	if a != testAliasName || rid != testAliasURL {
		t.Errorf("stored row = (%q, %q), want (staging, https://example.com)", a, rid)
	}
}

func TestSetAlias_HappyResourceID(t *testing.T) {
	h, store := newAliasTestHandler(t)
	body, sign := aliasSlashRequest(t, "setalias $staging r_abc123", testAliasTeamID, testAliasChannelID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	_, rid, _ := store.LookupChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID)
	if rid != "r_abc123" {
		t.Errorf("stored resourceID = %q, want r_abc123", rid)
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
	if _, _, err := store.LookupChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID); !errors.Is(err, ErrAliasNotFound) {
		t.Errorf("store was touched on parser-rejection path: %v", err)
	}
}

func TestSetAlias_SameTargetNoOp(t *testing.T) {
	h, store := newAliasTestHandler(t)
	_ = store.SetChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testAliasName, testAliasURL)

	body, sign := aliasSlashRequest(t, "setalias $staging https://example.com", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "No change.") {
		t.Errorf("response = %q, missing no-change copy", got)
	}
}

func TestSetAlias_DifferentExistingAliasBlocks(t *testing.T) {
	// Schema-gap fence: the channel already has $other bound. Refuse
	// to overwrite a teammate's alias without explicit
	// /qurl unsetalias first. The test row pins the refusal copy so
	// a future regression that silently overwrites lights this up.
	h, store := newAliasTestHandler(t)
	_ = store.SetChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testOtherAlias, "r_existing")

	body, sign := aliasSlashRequest(t, "setalias $staging https://example.com", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "$other") || !strings.Contains(got, "unsetalias") {
		t.Errorf("response = %q, want refusal copy referencing $other and unsetalias", got)
	}
	// Refusal copy must NOT leak the bound target (claude-bot review #5
	// — info-disclosure narrowing). The test row pins this.
	if strings.Contains(got, "r_existing") {
		t.Errorf("refusal leaked bound target: %q", got)
	}
	a, _, _ := store.LookupChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID)
	if a != testOtherAlias {
		t.Errorf("alias was overwritten: stored = %q, want %q", a, testOtherAlias)
	}
}

func TestSetAlias_NoStoreWired(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	body, sign := aliasSlashRequest(t, "setalias $staging https://example.com", testAliasTeamID, testAliasChannelID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "not configured") {
		t.Errorf("response = %q, want not-configured copy", got)
	}
}

func TestSetAlias_StoreWriteError(t *testing.T) {
	h, store := newAliasTestHandler(t)
	store.errSet = errors.New("ddb conditional failure")

	body, sign := aliasSlashRequest(t, "setalias $staging https://example.com", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "Failed to update alias") {
		t.Errorf("response = %q, want update-failed copy", got)
	}
}

func TestUnsetAlias_Happy(t *testing.T) {
	h, store := newAliasTestHandler(t)
	_ = store.SetChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testAliasName, testAliasURL)

	body, sign := aliasSlashRequest(t, "unsetalias $staging", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "no longer bound") {
		t.Errorf("response = %q, want unset success copy", got)
	}
	if _, _, err := store.LookupChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID); !errors.Is(err, ErrAliasNotFound) {
		t.Errorf("expected row cleared, got err=%v", err)
	}
}

func TestUnsetAlias_NotSet(t *testing.T) {
	h, _ := newAliasTestHandler(t)

	body, sign := aliasSlashRequest(t, "unsetalias $staging", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "No alias is set") {
		t.Errorf("response = %q, want not-set copy", got)
	}
}

func TestUnsetAlias_MismatchRefuses(t *testing.T) {
	// The channel has $other bound; user runs unsetalias $foo. The
	// least-surprise posture is "refuse and surface the mismatch"
	// rather than silently nuke the wrong alias.
	h, store := newAliasTestHandler(t)
	_ = store.SetChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testOtherAlias, "r_existing")

	body, sign := aliasSlashRequest(t, "unsetalias $foo", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "not `$foo`") {
		t.Errorf("response = %q, want mismatch refusal copy", got)
	}
	a, _, _ := store.LookupChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID)
	if a != testOtherAlias {
		t.Errorf("alias was cleared on mismatch: stored = %q, want %q", a, testOtherAlias)
	}
}

func TestUnsetAlias_TOCTOUClearedRace(t *testing.T) {
	// Race: lookup returns the expected alias, but ClearChannelAlias
	// returns ErrAliasNotFound (another admin cleared it between the
	// two calls). The user's intent is satisfied — render the
	// success copy rather than confusing them with a 404.
	h, store := newAliasTestHandler(t)
	_ = store.SetChannelAlias(context.Background(), testAliasTeamID, testAliasChannelID, testAliasName, "r_x")
	store.errClear = ErrAliasNotFound

	body, sign := aliasSlashRequest(t, "unsetalias $staging", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "no longer bound") {
		t.Errorf("response = %q, want TOCTOU success copy", got)
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

func TestHelpListsNewVerbs(t *testing.T) {
	// CR feedback fence from old #230: /qurl help must surface
	// setalias and unsetalias. The old PR had a stale helpMessage()
	// that listed only create/list/help even after the alias verbs
	// landed. This test pins the help copy includes both.
	h := newTestHandler(t, noopQURLServer(t))
	body, sign := aliasSlashRequest(t, "help", testAliasTeamID, testAliasChannelID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "setalias") {
		t.Errorf("/qurl help = %q, missing setalias", got)
	}
	if !strings.Contains(got, "unsetalias") {
		t.Errorf("/qurl help = %q, missing unsetalias", got)
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
	h, store := newAliasTestHandler(t)
	_ = store.SetChannelAlias(context.Background(), "T1", "C_shared", testAliasName, testAliasURL)

	// T2 admin in the same channel ID — should see "no alias" and
	// be allowed to set their own.
	body, sign := aliasSlashRequest(t, "setalias $other https://t2.example", "T2", "C_shared")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "now points to") {
		t.Errorf("T2 setalias should succeed; response = %q", got)
	}

	// T1's binding should be intact.
	t1Alias, t1RID, _ := store.LookupChannelAlias(context.Background(), "T1", "C_shared")
	if t1Alias != testAliasName || t1RID != testAliasURL {
		t.Errorf("T1 binding was disturbed: got (%q, %q), want (%q, %q)", t1Alias, t1RID, testAliasName, testAliasURL)
	}
	// T2 wrote its own row.
	t2Alias, _, _ := store.LookupChannelAlias(context.Background(), "T2", "C_shared")
	if t2Alias != testOtherAlias {
		t.Errorf("T2 binding missing: got %q, want %q", t2Alias, testOtherAlias)
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
