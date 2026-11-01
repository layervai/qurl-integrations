package output

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

func TestAccessModesMatchCatalog(t *testing.T) {
	raw, err := os.ReadFile("../accessrules/testdata/catalog.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	type copyText struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		Summary string `json:"summary"`
	}
	var catalog struct {
		Defaults     []copyText          `json:"defaults"`
		Presentation map[string]copyText `json:"presentation"`
	}
	if err = json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Presentation) != len(accessModes) {
		t.Fatalf("catalog has %d modes, CLI %d", len(catalog.Presentation), len(accessModes))
	}
	for _, m := range accessModes {
		if want := catalog.Presentation[m.mode]; m.label != want.Label || m.line != want.Summary {
			t.Errorf("%s: CLI has %q %q, catalog %q %q", m.mode, m.label, m.line, want.Label, want.Summary)
		}
	}
	if len(catalog.Defaults) != len(defaultRules) {
		t.Fatalf("catalog has %d defaults, CLI %d", len(catalog.Defaults), len(defaultRules))
	}
	for i, d := range defaultRules {
		if want := catalog.Defaults[i]; d.id != want.ID || d.label != want.Label || d.line != want.Summary {
			t.Errorf("default %d: CLI has %q %q %q, catalog %q %q %q", i, d.id, d.label, d.line, want.ID, want.Label, want.Summary)
		}
	}
}

func TestAccessRulesListShowsDefaultsThenSavedRules(t *testing.T) {
	var out, stderr bytes.Buffer
	snapshot := accessrules.Snapshot{BuiltIns: accessrules.BuiltIns(), Items: []accessrules.Definition{{ID: "11111111-1111-4111-8111-111111111111", Name: "Acme team", Mode: "email_domain", EmailDomain: "acme.com"}}, LocalVersion: 2, Sync: "local"}
	if err := newTestPrinter(&out, &stderr, FormatText, false, false, false).AccessRules(&snapshot); err != nil {
		t.Fatal(err)
	}
	want := `Default access rules
  Anyone with the link   Anyone who gets the link can open it.
  Only the first person  Works for whoever opens it first. Then it’s used up.
  People you invite      Only the email addresses you add can open it.
  People you approve     They confirm their email and ask. You approve each person.

Your rules
  11111111-1111-4111-8111-111111111111  "Acme team"  People at acme.com

Version: 2
Sync: local
`
	if out.String() != want {
		t.Fatalf("list\n%s\nwant\n%s", out.String(), want)
	}
	for _, hidden := range []string{"Devices you approve", "verified email", "Minimal", "Strong"} {
		if strings.Contains(out.String(), hidden) {
			t.Errorf("list shows %q", hidden)
		}
	}
}

func TestAccessRuleSummaryUsesContractCopy(t *testing.T) {
	for _, tc := range []struct {
		rule accessrules.Definition
		want string
	}{
		{accessrules.Definition{Mode: "public", LinkLimits: &accessrules.LinkLimits{ExpiresIn: "1h", OneTimeUse: new(true)}}, "Anyone with the link · links expire in 1 hour · single-use"},
		{accessrules.Definition{Mode: "review", ApprovalExpiresIn: new("7d")}, "People you approve · approvals last 7 days"},
		{accessrules.Definition{Mode: "email_domain", EmailDomain: "acme.com"}, "People at acme.com"},
		{accessrules.Definition{Mode: "public", LinkLimits: &accessrules.LinkLimits{ExpiresIn: "90m", SessionDuration: new("1d")}}, "Anyone with the link · links expire in 90 minutes · stays open for 1 day"},
		{accessrules.Definition{Mode: "keys"}, "Devices you approve · approvals until you remove them"},
		{accessrules.Definition{Mode: "email_domain", EmailDomain: "acme.com", AllowedEmails: []string{"a@b.com", "c@d.com"}}, "People at acme.com, plus 2 other people"},
	} {
		if got := AccessRuleSummary(&tc.rule); got != tc.want {
			t.Errorf("summary %q, want %q", got, tc.want)
		}
	}
}

func TestGrantListMarksExpiredGrants(t *testing.T) {
	var out, stderr bytes.Buffer
	now := fixedClock()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	result := &qurlapi.ConsumerManagementResult{Action: "list_grants", CRID: "c", Grants: []qurl.ConsumerGrant{
		{ID: "old", Status: "active", Version: 1, ExpiresAt: &past}, {ID: "live", Status: "active", Version: 1, ExpiresAt: &future},
	}}
	if err := newTestPrinter(&out, &stderr, FormatText, false, false, false).ConsumerManagement(result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "old  active  version 1  expired ") || !strings.Contains(out.String(), "live  active  version 1  expires ") {
		t.Fatalf("grant expiry not shown against now:\n%s", out.String())
	}
}
