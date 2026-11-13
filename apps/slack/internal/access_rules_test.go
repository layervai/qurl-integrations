package internal

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"

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
		if d.ID == "once" {
			want[d.ID] = qurl.AccessRule{ID: d.ID, Name: d.Label, Mode: d.Mode, LinkLimits: d.LinkLimits}
		}
	}
	for _, got := range defaultAccessRules {
		w, ok := want[got.ID]
		if !ok || got.Name != w.Name || got.Mode != w.Mode {
			t.Errorf("default rule %+v does not match catalog %+v", got, w)
			continue
		}
		if (got.LinkLimits == nil) != (w.LinkLimits == nil) {
			t.Errorf("%s link limits = %+v, catalog %+v", got.ID, got.LinkLimits, w.LinkLimits)
		} else if l := got.LinkLimits; l != nil && (l.ExpiresIn != w.LinkLimits.ExpiresIn || l.SessionDuration != w.LinkLimits.SessionDuration || *l.OneTimeUse != *w.LinkLimits.OneTimeUse) {
			t.Errorf("%s link limits = %+v, catalog %+v", got.ID, l, w.LinkLimits)
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
		{selector: "team ONLY ", wantID: saved[0].ID},
		{selector: saved[1].ID, wantID: saved[1].ID},
		{selector: "once", wantID: "once"},
		{selector: "Anyone with the link", wantID: "builtin.public"},
		{selector: "one-time link", wantErr: true}, // a saved rule shares the preset's name
		{selector: "Nobody"},
	} {
		got, found, err := findAccessRule(tc.selector, saved)
		if (err != nil) != tc.wantErr || (tc.wantID != "") != found || got.ID != tc.wantID {
			t.Errorf("findAccessRule(%q) = %q, %v, %v; want %q (err %v)", tc.selector, got.ID, found, err, tc.wantID, tc.wantErr)
		}
	}
}

func TestApplyAccessRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rule    qurl.AccessRule
		want    client.CreateInput
		refusal string
	}{
		{name: "anyone with the link", rule: defaultAccessRules[0], want: client.CreateInput{}},
		{name: "one-time link", rule: defaultAccessRules[1], want: client.CreateInput{OneTimeUse: true, ExpiresIn: "30d", SessionDuration: "24h"}},
		{name: "saved public rule", rule: qurl.AccessRule{Name: "Quick", Mode: accessRuleModePublic, LinkLimits: &qurl.AccessRuleLinkLimits{ExpiresIn: "1h", OneTimeUse: new(false)}}, want: client.CreateInput{ExpiresIn: "1h"}},
		{name: "people you invite", rule: defaultAccessRules[2], refusal: "Slack can't set"},
		{name: "people you approve", rule: defaultAccessRules[3], refusal: "Slack can't set"},
		{name: "work email", rule: qurl.AccessRule{Name: "Team", Mode: "email_domain", EmailDomain: "example.com"}, refusal: "Slack can't set"},
		{name: "paid", rule: qurl.AccessRule{Name: "Paid", Mode: "contract", Payment: &qurl.AccessRulePayment{PriceID: "price_1"}}, refusal: "Slack can't set"},
		{name: "unknown mode", rule: qurl.AccessRule{Name: "Future", Mode: "future_mode"}, refusal: "Slack can't set"},
		{name: "public with a private setting", rule: qurl.AccessRule{Name: "Odd", Mode: accessRuleModePublic, AllowedEmails: []string{"a@example.com"}}, refusal: "Slack can't set"},
		{name: "unknown field", rule: qurl.AccessRule{Name: "*Newer*", Mode: accessRuleModePublic, Unknown: map[string]json.RawMessage{"geo": []byte(`"us"`)}}, refusal: "newer qURL app"},
		{name: "unknown link-limit field", rule: qurl.AccessRule{Name: "Newer", Mode: accessRuleModePublic, LinkLimits: &qurl.AccessRuleLinkLimits{ExpiresIn: "1h", Unknown: map[string]json.RawMessage{"max_opens": []byte("2")}}}, refusal: "newer qURL app"},
	} {
		// Start from Slack's default limits: an applied rule replaces all of them.
		input := client.CreateInput{OneTimeUse: true, ExpiresIn: resourceLinkExpiry, SessionDuration: resourceSessionDuration, MaxSessions: resourceMaxSessions}
		err := applyAccessRule(&input, &tc.rule)
		if tc.refusal != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Errorf("%s: err = %v, want refusal %q", tc.name, err, tc.refusal)
			}
			if strings.Contains(err.Error(), "**Newer**") {
				t.Errorf("%s: rule name not escaped: %v", tc.name, err)
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
	if got := ruleLinkNotice(&client.CreateInput{}); got != "anyone with the link can open it" {
		t.Errorf("reusable notice = %q", got)
	}
	if got := ruleLinkNotice(&client.CreateInput{OneTimeUse: true, ExpiresIn: "30d"}); got != "one-time use · link expires in 30d" {
		t.Errorf("one-time notice = %q", got)
	}
}

// writeAccessRules serves GET /v1/me/access-rules with items.
func writeAccessRules(t *testing.T, w http.ResponseWriter, items ...map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if items == nil {
		items = []map[string]any{}
	}
	body := map[string]any{testKeyData: map[string]any{"account_id": "acct", "version": 1, "items": items, "built_ins": []any{}}}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
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
	reply, mint := ruleGet(t, `rule:"team only"`, func(w http.ResponseWriter, _ *http.Request) {
		writeAccessRules(t, w, map[string]any{"id": "11111111-1111-4111-8111-111111111111", "name": "Team only", "mode": "public", "link_limits": map[string]any{"expires_in": "2h", "one_time_use": true, "session_duration": "30m"}})
	})
	if mint == nil || mint["expires_in"] != "2h" || mint["one_time_use"] != true || mint["session_duration"] != "30m" {
		t.Fatalf("mint = %v; reply %q", mint, reply)
	}
	if !strings.Contains(reply, "one-time use · link expires in 2h") {
		t.Errorf("reply = %q", reply)
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
	// A Default access rule never depends on the read.
	if reply, mint := ruleGet(t, `rule:"One-time link"`, failing); mint == nil {
		t.Fatalf("One-time link did not mint; reply %q", reply)
	}
}

func TestHandleGet_RuleRefusedBeforeMint(t *testing.T) {
	reply, mint := ruleGet(t, `rule:"People you approve"`, func(w http.ResponseWriter, _ *http.Request) { writeAccessRules(t, w) })
	if mint != nil || !strings.Contains(reply, "Slack can't set") {
		t.Fatalf("mint = %v, reply = %q", mint, reply)
	}
	reply, mint = ruleGet(t, `rule:Newer`, func(w http.ResponseWriter, _ *http.Request) {
		writeAccessRules(t, w, map[string]any{"id": "33333333-3333-4333-8333-333333333333", "name": "Newer", "mode": "public", "max_opens": 2})
	})
	if mint != nil || !strings.Contains(reply, "newer qURL app") {
		t.Fatalf("mint = %v, reply = %q", mint, reply)
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
