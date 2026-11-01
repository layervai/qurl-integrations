package output

import (
	"fmt"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
)

// AccessRuleCapabilities reports the offline machine contract without authentication.
func (p *Printer) AccessRuleCapabilities() error {
	return p.writeJSON(struct {
		SchemaVersion int      `json:"schema_version"`
		Modes         []string `json:"modes"`
	}{1, accessrules.Modes()})
}

// AccessRules prints only the public bridge snapshot, never the internal account journal.
// Text shows the four default access rules, then the saved ones; other modes
// are offered only when creating a rule.
func (p *Printer) AccessRules(snapshot *accessrules.Snapshot) error {
	if p.format == FormatJSON {
		return p.writeJSON(snapshot)
	}
	ew := &errWriter{w: p.out}
	if p.quiet {
		for _, d := range defaultRules {
			ew.printf("%s\n", d.selector)
		}
		for i := range snapshot.Items {
			ew.printf("%s\n", snapshot.Items[i].ID)
		}
		return ew.flush(nil)
	}
	ew.printf("Default access rules\n")
	for _, d := range defaultRules {
		ew.printf("  %-23s%s\n", d.label, d.line)
	}
	ew.printf("\nYour rules\n")
	if len(snapshot.Items) == 0 {
		ew.printf("  None yet. Create one with qurl rules create.\n")
	}
	for i := range snapshot.Items {
		d := &snapshot.Items[i]
		ew.printf("  %s  %q  %s\n", d.ID, d.Name, AccessRuleSummary(d))
	}
	ew.printf("\nVersion: %d\nSync: %s\n", snapshot.LocalVersion, snapshot.Sync)
	return ew.flush(nil)
}

// defaultRules mirrors the catalog's defaults, easiest to open first; selector
// is what --rule takes. TestDefaultRulesMatchCatalog keeps them identical.
var defaultRules = []struct{ id, selector, label, line string }{
	{"public", "builtin.public", "Anyone with the link", "Anyone who gets the link can open it."},
	{"once", accessrules.OnlyFirstPersonID, accessrules.OnlyFirstPersonLabel, "Works for whoever opens it first. Then it’s used up."},
	{"invite_only", "builtin.invite_only", "People you invite", "Only the email addresses you add can open it."},
	{"review", "builtin.review", "People you approve", "They confirm their email and ask. You approve each person."},
}

// AccessRule prints one rule, or only its ID when quiet.
func (p *Printer) AccessRule(d *accessrules.Definition) error {
	if p.format == FormatJSON {
		return p.writeJSON(d)
	}
	return p.accessRuleLine(d)
}

func (p *Printer) accessRuleLine(d *accessrules.Definition) error {
	if p.quiet {
		_, err := fmt.Fprintln(p.out, d.ID)
		return err
	}
	_, err := fmt.Fprintf(p.out, "%s  %q  %s\n", d.ID, d.Name, AccessRuleSummary(d))
	return err
}

// AccessRuleSummary is the contract's one-line rule summary, such as
// "Anyone with the link · links expire in 1 hour · single-use".
func AccessRuleSummary(d *accessrules.Definition) string {
	parts := []string{accessRuleLabel(d.Mode)}
	if d.Mode == accessrules.ModeEmailDomain && d.EmailDomain != "" {
		parts[0] = "People at " + d.EmailDomain
	}
	if n := len(d.AllowedEmails); d.Mode == accessrules.ModeEmailDomain && n > 0 {
		who := "people"
		if n == 1 {
			who = "person"
		}
		parts[0] += fmt.Sprintf(", plus %d other %s", n, who)
	}
	if l := d.LinkLimits; l != nil {
		parts = append(parts, "links expire in "+ruleDuration(l.ExpiresIn))
		if l.OneTimeUse != nil && *l.OneTimeUse {
			parts = append(parts, "single-use")
		}
		if l.SessionDuration != nil {
			parts = append(parts, "stays open for "+ruleDuration(*l.SessionDuration))
		}
	}
	switch {
	case d.ApprovalExpiresIn != nil:
		parts = append(parts, "approvals last "+ruleDuration(*d.ApprovalExpiresIn))
	case accessrules.ApprovalsApply(d.Mode):
		parts = append(parts, "approvals until you remove them")
	}
	return strings.Join(parts, " · ")
}

// ruleDuration names a validated duration in its largest whole unit.
func ruleDuration(value string) string {
	d, err := accessrules.ParseDuration(value)
	if err != nil {
		return value
	}
	for _, unit := range []struct {
		size time.Duration
		name string
	}{{24 * time.Hour, "day"}, {time.Hour, "hour"}, {time.Minute, "minute"}, {time.Second, "second"}} {
		if d >= unit.size && d%unit.size == 0 {
			n := int(d / unit.size)
			return fmt.Sprintf("%d %s", n, plural(n, unit.name))
		}
	}
	return d.String()
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// accessModes mirrors the catalog's presentation label and one-line summary,
// in the order a rule's modes are offered; TestAccessModesMatchCatalog keeps
// them identical.
var accessModes = []struct{ mode, label, line string }{
	{"public", "Anyone with the link", "Anyone who gets the link can open it."},
	{"email_capture", "Anyone with a verified email", "Anyone can open it after confirming their email."},
	{"email_domain", "Work or school email", "Only people with an email at your domain can open it."},
	{"invite_only", "People you invite", "Only the email addresses you add can open it."},
	{"review", "People you approve", "They confirm their email and ask. You approve each person."},
	{"keys", "Devices you approve", "No email needed. They send you their passkey’s details, and you approve it."},
	{"contract", "Custom access", "Your app or service decides who gets in."},
}

func accessRuleLabel(mode string) string {
	for _, m := range accessModes {
		if m.mode == mode {
			return m.label
		}
	}
	return "Unavailable"
}

// AccessModeHelp lists every mode with its one-line summary, for creating a rule.
func AccessModeHelp() string {
	lines := make([]string, len(accessModes))
	for i, m := range accessModes {
		lines[i] = fmt.Sprintf("  %-15s%s", m.mode, m.line)
	}
	return strings.Join(lines, "\n")
}
