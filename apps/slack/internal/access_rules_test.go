package internal

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackaudit"
	"github.com/layervai/qurl-integrations/shared/client"
)

// sharedCatalogPath is the CLI's byte-identical copy of qurl-service's
// api/fixtures/access-rules/catalog.v1.json; the CLI's tests pin its parity.
const sharedCatalogPath = "../../cli/internal/accessrules/testdata/catalog.v1.json"

// TestDefaultAccessRulesMatchCatalog pins Slack's Default access rules to the
// shared catalog: the public and private built-ins by ID and name, and the
// One-time link preset by ID, label and link limits.
func TestDefaultAccessRulesMatchCatalog(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(sharedCatalogPath)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	var catalog struct {
		BuiltIns []qurl.AccessRule `json:"built_ins"`
		Defaults []struct {
			ID         string                     `json:"id"`
			Label      string                     `json:"label"`
			Mode       string                     `json:"mode"`
			LinkLimits *qurl.AccessRuleLinkLimits `json:"link_limits"`
		} `json:"defaults"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	want := map[string]qurl.AccessRule{}
	for _, b := range catalog.BuiltIns {
		want[b.ID] = b
	}
	for _, d := range catalog.Defaults {
		want[d.ID] = qurl.AccessRule{ID: d.ID, Name: d.Label, Mode: d.Mode, LinkLimits: d.LinkLimits}
	}
	// Every ready-made choice is known, so none is reported as missing.
	known := map[string]bool{}
	for _, r := range defaultAccessRules {
		known[r.ID] = true
	}
	for _, b := range catalog.BuiltIns {
		if !known[b.ID] {
			t.Errorf("catalog built-in %q is missing from defaultAccessRules", b.ID)
		}
	}
	for _, got := range defaultAccessRules {
		w, ok := want[got.ID]
		if !ok || got.Name != w.Name || got.Mode != w.Mode || !reflect.DeepEqual(got.LinkLimits, w.LinkLimits) {
			t.Errorf("default rule %+v does not match catalog %+v", got, w)
		}
	}
}

func TestFindAccessRule(t *testing.T) {
	t.Parallel()
	saved := []qurl.AccessRule{
		{ID: "11111111-1111-4111-8111-111111111111", Name: "Team only", Mode: accessRuleModePublic},
		{ID: "22222222-2222-4222-8222-222222222222", Name: "One-Time Link", Mode: accessRuleModePublic},
	}
	for _, tc := range []struct {
		selector, wantID string
		wantErr          bool
	}{
		{selector: "team ONLY", wantID: saved[0].ID},
		{selector: saved[1].ID, wantID: saved[1].ID},
		{selector: "once", wantID: "once"},
		{selector: "Anyone with the link", wantID: "builtin.public"},
		{selector: "one-time link", wantErr: true}, // a saved rule shares the preset's name
		{selector: "Nobody"},
		{selector: "people who pay", wantID: "paid"},
		{selector: "builtin.review", wantID: "builtin.review"},
		{selector: "Once", wantID: "once"},
	} {
		got, err := findAccessRule(tc.selector, saved)
		gotID := ""
		if got != nil {
			gotID = got.ID
		}
		if (err != nil) != tc.wantErr || gotID != tc.wantID {
			t.Errorf("findAccessRule(%q) = %q, %v; want %q (err %v)", tc.selector, gotID, err, tc.wantID, tc.wantErr)
		}
	}
}

func TestApplyAccessRule(t *testing.T) {
	t.Parallel()
	type testCase struct {
		name    string
		rule    qurl.AccessRule
		want    client.CreateInput
		refusal string
	}
	byID := map[string]qurl.AccessRule{}
	var refusedPresets []testCase
	for _, r := range defaultAccessRules {
		byID[r.ID] = r
		if r.Mode != accessRuleModePublic {
			refusedPresets = append(refusedPresets, testCase{name: r.Name, rule: r, refusal: "Slack can't set"})
		}
	}
	for _, tc := range append(refusedPresets, []testCase{
		{name: "anyone with the link", rule: byID["builtin.public"], want: client.CreateInput{}},
		{name: "one-time link", rule: byID["once"], want: client.CreateInput{OneTimeUse: true, ExpiresIn: "30d", SessionDuration: "24h"}},
		{name: "saved public rule", rule: qurl.AccessRule{Name: "Quick", Mode: accessRuleModePublic, LinkLimits: &qurl.AccessRuleLinkLimits{ExpiresIn: "1h", OneTimeUse: new(false)}}, want: client.CreateInput{ExpiresIn: "1h"}},
		{name: "work email", rule: qurl.AccessRule{Name: "Team", Mode: "email_domain", EmailDomain: "example.com"}, refusal: "Slack can't set"},
		{name: "paid", rule: qurl.AccessRule{Name: "Paid", Mode: "contract", Payment: &qurl.AccessRulePayment{PriceID: "price_1"}}, refusal: "Slack can't set"},
		{name: "unknown mode", rule: qurl.AccessRule{Name: "Future", Mode: "future_mode"}, refusal: "Slack can't set"},
		{name: "public with a private setting", rule: qurl.AccessRule{Name: "Odd", Mode: accessRuleModePublic, AllowedEmails: []string{"a@example.com"}}, refusal: "Slack can't set"},
		{name: "unknown field", rule: qurl.AccessRule{Name: "*Newer*", Mode: accessRuleModePublic, Unknown: map[string]json.RawMessage{"geo": []byte(`"us"`)}}, refusal: "newer qURL app"},
		{name: "unknown link-limit field", rule: qurl.AccessRule{Name: "Newer", Mode: accessRuleModePublic, LinkLimits: &qurl.AccessRuleLinkLimits{ExpiresIn: "1h", Unknown: map[string]json.RawMessage{"max_opens": []byte("2")}}}, refusal: "newer qURL app"},
	}...) {
		// Start from Slack's default limits: an applied rule replaces all of them.
		input := client.CreateInput{OneTimeUse: true, ExpiresIn: resourceLinkExpiry, SessionDuration: resourceSessionDuration, MaxSessions: resourceMaxSessions}
		err := applyAccessRule(&input, &tc.rule)
		if tc.refusal != "" {
			// "**Newer**" would mean the name's own markup reached the reply.
			if err == nil || !strings.Contains(err.Error(), tc.refusal) || strings.Contains(err.Error(), "**Newer**") {
				t.Errorf("%s: err = %v, want an escaped refusal %q", tc.name, err, tc.refusal)
			}
			continue
		}
		if err != nil || input != tc.want {
			t.Errorf("%s: input = %+v, err %v; want %+v", tc.name, input, err, tc.want)
		}
	}
}

func TestRuleLinkNotice(t *testing.T) {
	t.Parallel()
	if got := ruleLinkNotice(&client.CreateInput{}, nil); got != "anyone with the link can open it" {
		t.Errorf("reusable notice = %q", got)
	}
	if got := ruleLinkNotice(&client.CreateInput{OneTimeUse: true, ExpiresIn: "30d"}, nil); got != "one-time use · link expires in 30d" {
		t.Errorf("one-time notice = %q", got)
	}
	at := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	if got, want := ruleLinkNotice(&client.CreateInput{}, &at), "anyone with the link can open it · link expires <!date^1794648600^{date_short_pretty} at {time}|2026-11-14 09:30 UTC>"; got != want {
		t.Errorf("service-expiry notice = %q, want %q", got, want)
	}
}

// writeAccessRules serves GET /v1/me/access-rules with items.
func writeAccessRules(t *testing.T, w http.ResponseWriter, items ...map[string]any) {
	t.Helper()
	if items == nil {
		items = []map[string]any{}
	}
	respondRawQURLEnvelope(t, w, map[string]any{"account_id": "acct", "version": 1, "items": items, "built_ins": []any{}})
}

const accessRulesPath = "/v1/me/access-rules"

// ruleGet runs `/qurl get $prod-db <flags>` with the given access-rules
// handler and returns the reply and the mint body (nil when nothing minted).
func ruleGet(t *testing.T, flags string, rules http.HandlerFunc) (reply string, mint map[string]any) {
	t.Helper()
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	if rules != nil {
		ts.addCustomer(http.MethodGet, accessRulesPath, rules)
	}
	ts.addCustomer(http.MethodPost, mintByTestResourcePath, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &mint); err != nil {
			t.Errorf("decode mint body: %v", err)
		}
		writeCreateFixture(t, w, "https://qurl.link/abc", testResourceIDFix)
	})
	inv := newAdminSlashInvoker(t, newAdminTestHandler(t, ts))
	_, _, reply = inv.invokeAdminAsync("get $prod-db "+flags, testAdminTeamID, testAdminUserID)
	return reply, mint
}

func TestHandleGet_RuleAnyoneWithTheLink(t *testing.T) {
	reply, mint := ruleGet(t, `rule:"Anyone with the link"`, func(w http.ResponseWriter, _ *http.Request) { writeAccessRules(t, w) })
	if mint == nil {
		t.Fatalf("no mint; reply %q", reply)
	}
	if got, ok := mint["one_time_use"]; !ok || got != false {
		t.Errorf("one_time_use = %v (sent %v), want an explicit false", got, ok)
	}
	for _, field := range []string{"expires_in", "session_duration", "max_sessions"} {
		if _, ok := mint[field]; ok {
			t.Errorf("%s = %v, want the service default", field, mint[field])
		}
	}
	if !strings.Contains(reply, "anyone with the link can open it") {
		t.Errorf("reply = %q", reply)
	}
}

func TestHandleGet_RuleSaved(t *testing.T) {
	logs := captureDefaultSlog(t)
	reply, mint := ruleGet(t, `rule:"team only"`, func(w http.ResponseWriter, _ *http.Request) {
		writeAccessRules(t, w, map[string]any{"id": "11111111-1111-4111-8111-111111111111", "name": "Team only", "mode": "public", "link_limits": map[string]any{"expires_in": "2h", "one_time_use": true, "session_duration": "30m"}})
	})
	if mint == nil || mint["expires_in"] != "2h" || mint["one_time_use"] != true || mint["session_duration"] != "30m" {
		t.Fatalf("mint = %v; reply %q", mint, reply)
	}
	if !strings.Contains(reply, "one-time use · link expires in 2h") {
		t.Errorf("reply = %q", reply)
	}
	audit := findAuditRecord(logs, slackaudit.QURLMintRule)
	for k, want := range map[string]any{
		"team_id": testAdminTeamID, "channel_id": "C_test", "user_id": testAdminUserID, "resource_id": testResourceIDFix,
		"rule_id": "11111111-1111-4111-8111-111111111111", "one_time_use": true, "expires_in": "2h", "session_duration": "30m",
	} {
		if audit[k] != want {
			t.Errorf("audit[%s] = %#v, want %#v; audit=%#v", k, audit[k], want, audit)
		}
	}
}

// TestHandleGet_RuleNoAccountLibrary covers a workspace whose qURL account has
// no saved rules this key may read: Default access rules still mint, and a
// saved-rule name is not found.
func TestHandleGet_RuleNoAccountLibrary(t *testing.T) {
	forbidden := func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(t, w, http.StatusForbidden, "forbidden", "Forbidden")
	}
	if reply, mint := ruleGet(t, "rule:once", forbidden); mint == nil || mint["one_time_use"] != true || mint["expires_in"] != "30d" {
		t.Fatalf("One-time link mint = %v; reply %q", mint, reply)
	}
	reply, mint := ruleGet(t, `rule:"Team only"`, forbidden)
	if mint != nil || !strings.Contains(reply, "No access rule is named *Team only*") {
		t.Fatalf("mint = %v, reply = %q", mint, reply)
	}
}

func TestHandleGet_RuleReadFailure(t *testing.T) {
	failing := func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(t, w, http.StatusInternalServerError, "internal", "Internal")
	}
	reply, mint := ruleGet(t, `rule:"Team only"`, failing)
	if mint != nil || !strings.Contains(reply, "Couldn't read your saved access rules") {
		t.Fatalf("mint = %v, reply = %q", mint, reply)
	}
	// A Default rule's name could be an unread saved rule's too; only its ID mints.
	if reply, mint := ruleGet(t, `rule:"One-time link"`, failing); mint != nil {
		t.Fatalf("One-time link by name minted without the saved rules; reply %q", reply)
	}
	if reply, mint := ruleGet(t, "rule:once", failing); mint == nil {
		t.Fatalf("rule:once did not mint; reply %q", reply)
	}
}

// TestHandleGet_WithoutRuleSkipsRulesRead keeps the default mint free of the
// extra read.
func TestHandleGet_WithoutRuleSkipsRulesRead(t *testing.T) {
	reply, mint := ruleGet(t, "", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("a get without rule: read the access rules")
	})
	if mint == nil || mint["one_time_use"] != true || mint["expires_in"] != resourceLinkExpiry {
		t.Fatalf("mint = %v; reply %q", mint, reply)
	}
}

// TestHandleGet_RuleRefusedBeforeMint covers each refusal through the
// handler, saved rules decoded end to end.
func TestHandleGet_RuleRefusedBeforeMint(t *testing.T) {
	for _, tc := range []struct {
		flags, want string
		saved       map[string]any
	}{
		{flags: `rule:"People you approve"`, want: "Slack can't set"},
		{flags: "rule:Newer", want: "newer qURL app", saved: map[string]any{"name": "Newer", "mode": "public", "max_opens": 2}},
		{flags: "rule:Quick", want: "newer qURL app", saved: map[string]any{"name": "Quick", "mode": "public", "link_limits": map[string]any{"expires_in": "1h", "max_opens": 3}}},
		{flags: `rule:"one-time link"`, want: "matches more than one access rule", saved: map[string]any{"name": "One-time link", "mode": "invite_only"}},
		{flags: "rule:once", want: "matches more than one access rule", saved: map[string]any{"name": "once", "mode": "invite_only"}},
	} {
		var items []map[string]any
		if tc.saved != nil {
			tc.saved["id"] = "33333333-3333-4333-8333-333333333333"
			items = append(items, tc.saved)
		}
		reply, mint := ruleGet(t, tc.flags, func(w http.ResponseWriter, _ *http.Request) { writeAccessRules(t, w, items...) })
		if mint != nil || !strings.Contains(reply, tc.want) {
			t.Errorf("%s: mint = %v, reply = %q, want %q", tc.flags, mint, reply, tc.want)
		}
	}
}

// TestHandleGet_RuleAndRateLimit pins the quota order: a refused rule uses no
// quota, and an over-quota user is refused before a rule link mints.
func TestHandleGet_RuleAndRateLimit(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer(http.MethodGet, accessRulesPath, func(w http.ResponseWriter, _ *http.Request) { writeAccessRules(t, w) })
	var mints atomic.Int32
	ts.addCustomer(http.MethodPost, mintByTestResourcePath, func(w http.ResponseWriter, _ *http.Request) {
		mints.Add(1)
		writeCreateFixture(t, w, "https://qurl.link/abc", testResourceIDFix)
	})
	h := newAdminTestHandler(t, ts)
	enableAdminStoreRateLimit(t, h, 1)

	for _, step := range []struct{ text, want string }{
		{`get $prod-db rule:"People you approve"`, "Slack can't set"},
		{"get $prod-db rule:once", "https://qurl.link/abc"}, // the refusal left the one allowed mint
		{"get $prod-db rule:once", "Rate limit hit"},
	} {
		if _, _, reply := newAdminSlashInvoker(t, h).invokeAdminAsync(step.text, testAdminTeamID, testAdminUserID); !strings.Contains(reply, step.want) {
			t.Fatalf("%s: reply = %q, want %q", step.text, reply, step.want)
		}
	}
	if got := mints.Load(); got != 1 {
		t.Fatalf("mints = %d, want 1", got)
	}
}
