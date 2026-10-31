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
	var catalog struct {
		Presentation map[string]struct {
			Label string `json:"label"`
			Level *int   `json:"level"`
		} `json:"presentation"`
	}
	if err = json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Presentation) != len(accessModes) {
		t.Fatalf("catalog has %d modes, CLI labels %d", len(catalog.Presentation), len(accessModes))
	}
	for mode, want := range catalog.Presentation {
		level := 0
		if want.Level != nil {
			level = *want.Level
		}
		if got := accessModes[mode]; got.label != want.Label || got.level != level {
			t.Errorf("%s: CLI has %q level %d, catalog %q level %d", mode, got.label, got.level, want.Label, level)
		}
	}
}

func TestAccessRulesListBuiltInsByProtection(t *testing.T) {
	var out, stderr bytes.Buffer
	snapshot := accessrules.Snapshot{BuiltIns: accessrules.BuiltIns()}
	if err := newTestPrinter(&out, &stderr, FormatText, false, false, false).AccessRules(&snapshot); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, line := range strings.Split(out.String(), "\n") {
		if id, _, ok := strings.Cut(line, "  "); ok && strings.HasPrefix(id, "builtin.") {
			ids = append(ids, id)
		}
	}
	want := "builtin.public builtin.email_capture builtin.invite_only builtin.review builtin.keys"
	if got := strings.Join(ids, " "); got != want {
		t.Fatalf("built-in order %q, want %q", got, want)
	}
}

func TestAccessRuleSummaryUsesContractCopy(t *testing.T) {
	for _, tc := range []struct {
		rule accessrules.Definition
		want string
	}{
		{accessrules.Definition{Mode: "public", LinkLimits: &accessrules.LinkLimits{ExpiresIn: "1h", OneTimeUse: new(true)}}, "Anyone with the link · expires in 1 hour · one browser"},
		{accessrules.Definition{Mode: "review", ApprovalExpiresIn: new("7d")}, "People you approve · approvals last 7 days"},
		{accessrules.Definition{Mode: "email_domain", EmailDomain: "studio.com"}, "People at studio.com"},
		{accessrules.Definition{Mode: "public", LinkLimits: &accessrules.LinkLimits{ExpiresIn: "90m", MaxSessions: new(1), SessionDuration: new("1d")}, AllowDownload: new(true)}, "Anyone with the link · expires in 90 minutes · up to 1 session · sessions last 1 day · downloads allowed"},
		{accessrules.Definition{Mode: "keys", AllowDownload: new(false)}, "Devices you approve"},
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
