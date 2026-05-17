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
	"time"
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
	// spinning a real DDB fake. Set to a sentinel (ErrAliasAlreadyBound
	// / ErrAliasNotFound) to exercise the typed-conflict branches.
	errBind   error
	errUnbind error

	// blockBind, when true, makes BindChannelAlias wait on ctx.Done()
	// and return ctx.Err(). Used by the aliasSyncTimeout fence test
	// (a slow DDB write must surface as a write-failed reply, not
	// block past Slack's ack window).
	blockBind bool
}

func newFakeAliasStore() *fakeAliasStore {
	return &fakeAliasStore{rows: map[string]map[string]string{}}
}

func (f *fakeAliasStore) key(teamID, channelID string) string {
	return teamID + "|" + channelID
}

func (f *fakeAliasStore) BindChannelAlias(ctx context.Context, teamID, channelID, aliasName, resourceID string) error {
	// Block before taking the row lock so the timeout test doesn't
	// also have to coordinate with anything else holding f.mu.
	f.mu.Lock()
	block := f.blockBind
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
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
		return ErrAliasAlreadyBound
	}
	row[aliasName] = resourceID
	return nil
}

func (f *fakeAliasStore) UnbindChannelAlias(_ context.Context, teamID, channelID, aliasName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errUnbind != nil {
		return f.errUnbind
	}
	k := f.key(teamID, channelID)
	row, ok := f.rows[k]
	if !ok {
		return ErrAliasNotFound
	}
	if _, exists := row[aliasName]; !exists {
		return ErrAliasNotFound
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
		// Backtick in target would break the Slack inline-code fence
		// the success-copy interpolates the target into. Rejected at
		// parse time so the response renders cleanly.
		{name: "backtick in r_ target rejected", input: "$staging r_abc`bad", wantErr: true},
		{name: "backtick in URL target rejected", input: "$staging https://example.com/`x", wantErr: true},
		// Non-printable runes in target garble the audit log line and
		// the Slack response. Rejected at parse time alongside the
		// backtick guard so the success-copy + slog.Info surfaces
		// remain hygienic.
		{name: "control byte in r_ target rejected", input: "$staging r_abc\x01bad", wantErr: true},
		{name: "control byte in URL target rejected", input: "$staging https://example.com/\x01x", wantErr: true},

		// Fence: http://localhost is currently ACCEPTED. The parser
		// stays scheme-permissive because qurl-service is the
		// authoritative validator on target reachability. The
		// public-host gate follow-up is tracked in #350; when that
		// lands, flip this row to wantErr and the test starts
		// enforcing the new contract.
		{name: "localhost target ACCEPTED (see #350: SSRF gate)", input: "$staging http://localhost:3000", wantAlias: "staging", wantTgt: "http://localhost:3000"},
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
	b := store.bindings(testAliasTeamID, testAliasChannelID)
	if b[testAliasName] != testAliasURL {
		t.Errorf("stored bindings = %v, want {%q: %q}", b, testAliasName, testAliasURL)
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
	b := store.bindings(testAliasTeamID, testAliasChannelID)
	if b[testAliasName] != "r_abc123" {
		t.Errorf("stored bindings = %v, want {%q: r_abc123}", b, testAliasName)
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

func TestSetChannelAlias_SecondAliasOnSameChannelSucceeds(t *testing.T) {
	// Schema decision 2026-05-17: a channel can host many aliases
	// simultaneously. After binding $a, binding $b must succeed; both
	// bindings must coexist in the resulting row.
	h, store := newAliasTestHandler(t)

	body1, sign1 := aliasSlashRequest(t, "setalias $a https://a.example", testAliasTeamID, testAliasChannelID)
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, newSignedRequest(t, "/slack/commands", body1, sign1))
	if got := decodeSlackText(t, w1.Body.Bytes()); !strings.Contains(got, "now points to") {
		t.Fatalf("first bind response = %q, want success copy", got)
	}

	body2, sign2 := aliasSlashRequest(t, "setalias $b https://b.example", testAliasTeamID, testAliasChannelID)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, newSignedRequest(t, "/slack/commands", body2, sign2))
	if got := decodeSlackText(t, w2.Body.Bytes()); !strings.Contains(got, "now points to") || !strings.Contains(got, "$b") {
		t.Errorf("second bind response = %q, want success copy referencing $b", got)
	}

	b := store.bindings(testAliasTeamID, testAliasChannelID)
	if b["a"] != "https://a.example" {
		t.Errorf("$a binding lost: bindings = %v", b)
	}
	if b["b"] != "https://b.example" {
		t.Errorf("$b binding missing: bindings = %v", b)
	}
}

func TestSetChannelAlias_DuplicateAliasIsRefused(t *testing.T) {
	// Binding the same alias name twice in the same channel must
	// surface as a typed conflict (ErrAliasAlreadyBound). A different
	// resource id on the second call does NOT silently overwrite.
	h, store := newAliasTestHandler(t)

	body1, sign1 := aliasSlashRequest(t, "setalias $staging r_first", testAliasTeamID, testAliasChannelID)
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, newSignedRequest(t, "/slack/commands", body1, sign1))
	if got := decodeSlackText(t, w1.Body.Bytes()); !strings.Contains(got, "now points to") {
		t.Fatalf("first bind response = %q, want success copy", got)
	}

	body2, sign2 := aliasSlashRequest(t, "setalias $staging r_second", testAliasTeamID, testAliasChannelID)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, newSignedRequest(t, "/slack/commands", body2, sign2))
	got := decodeSlackText(t, w2.Body.Bytes())
	if !strings.Contains(got, "already bound") || !strings.Contains(got, "$staging") {
		t.Errorf("duplicate bind response = %q, want already-bound copy", got)
	}
	// Refusal copy must NOT leak the bound target (info-disclosure
	// narrowing carried over from the single-alias era).
	if strings.Contains(got, "r_first") {
		t.Errorf("refusal leaked bound target: %q", got)
	}

	// Original binding intact.
	b := store.bindings(testAliasTeamID, testAliasChannelID)
	if b[testAliasName] != "r_first" {
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
	// channel_id empty.
	body, sign := aliasSlashRequest(t, "setalias $staging https://example.com", testAliasTeamID, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))
	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "Could not read your Slack workspace or channel ID") {
		t.Errorf("missing channel_id response = %q, want defensive-guard copy", got)
	}
	// team_id empty.
	body2, sign2 := aliasSlashRequest(t, "setalias $staging https://example.com", "", testAliasChannelID)
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

// TestRedactURLForLog fences the audit-log redaction contract:
// userinfo (credentials embedded by a setting admin) and raw query
// strings (often carry tokens/keys) are stripped before the target
// lands in operator-visible logs. r_… resource ids and unparseable
// strings pass through unchanged.
func TestRedactURLForLog(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain https", input: "https://example.com/path", want: "https://example.com/path"},
		{name: "userinfo stripped", input: "https://user:token@example.com/path", want: "https://example.com/path"},
		{name: "raw query stripped", input: "https://example.com/path?key=secret", want: "https://example.com/path"},
		{name: "fragment stripped", input: "https://example.com/path#section", want: "https://example.com/path"},
		{name: "userinfo + query stripped", input: "https://u:p@example.com/path?k=v", want: "https://example.com/path"},
		{name: "resource id passthrough", input: "r_abc123", want: "r_abc123"},
		{name: "unparseable passthrough", input: "::not-a-url", want: "::not-a-url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactURLForLog(tc.input)
			if got != tc.want {
				t.Errorf("redactURLForLog(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestSetAlias_SyncTimeoutExceeded fences the aliasSyncTimeout budget:
// a BindChannelAlias that doesn't return inside the budget must
// surface as a write-failed reply (not block past Slack's 3-second
// ack window). Shortens aliasSyncTimeout for the duration of the
// test so the suite doesn't pay a 2.5s real-time tax.
func TestSetAlias_SyncTimeoutExceeded(t *testing.T) {
	orig := aliasSyncTimeout
	aliasSyncTimeout = 25 * time.Millisecond
	t.Cleanup(func() { aliasSyncTimeout = orig })

	h, store := newAliasTestHandler(t)
	store.blockBind = true

	body, sign := aliasSlashRequest(t, "setalias $staging https://example.com", testAliasTeamID, testAliasChannelID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, sign))

	got := decodeSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "Failed to update alias") {
		t.Errorf("response = %q, want update-failed copy (ctx deadline exceeded)", got)
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
	store.errBind = errors.New("ddb conditional failure")

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
	// ErrAliasNotFound branch as "not bound; nothing to clear."
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
	if err := store.BindChannelAlias(context.Background(), "T1", "C_shared", testAliasName, testAliasURL); err != nil {
		t.Fatalf("seed T1 bind: %v", err)
	}

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
	t1 := store.bindings("T1", "C_shared")
	if t1[testAliasName] != testAliasURL {
		t.Errorf("T1 binding was disturbed: got %v, want {%q: %q}", t1, testAliasName, testAliasURL)
	}
	// T2 wrote its own row.
	t2 := store.bindings("T2", "C_shared")
	if t2[testOtherAlias] != "https://t2.example" {
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
