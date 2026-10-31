package output

import (
	"cmp"
	"fmt"
	"slices"
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
func (p *Printer) AccessRules(snapshot *accessrules.Snapshot) error {
	if p.format == FormatJSON {
		return p.writeJSON(snapshot)
	}
	// Ready-made choices read lowest to highest protection; ties keep catalog order.
	builtIns := slices.Clone(snapshot.BuiltIns)
	slices.SortStableFunc(builtIns, func(a, b accessrules.Definition) int {
		return cmp.Compare(accessModes[a.Mode].level, accessModes[b.Mode].level)
	})
	for _, items := range [][]accessrules.Definition{builtIns, snapshot.Items} {
		for i := range items {
			if err := p.accessRuleLine(&items[i]); err != nil {
				return err
			}
		}
	}
	if p.quiet {
		return nil
	}
	_, err := fmt.Fprintf(p.out, "Version: %d\nSync: %s\n", snapshot.LocalVersion, snapshot.Sync)
	return err
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
// "Anyone with the link · expires in 1 hour · one browser".
func AccessRuleSummary(d *accessrules.Definition) string {
	parts := []string{accessRuleLabel(d.Mode)}
	if d.Mode == accessrules.ModeEmailDomain && d.EmailDomain != "" {
		parts[0] = "People at " + d.EmailDomain
	}
	if l := d.LinkLimits; l != nil {
		parts = append(parts, "expires in "+ruleDuration(l.ExpiresIn))
		if l.OneTimeUse != nil && *l.OneTimeUse {
			parts = append(parts, "one browser")
		}
		if l.MaxSessions != nil && *l.MaxSessions > 0 {
			parts = append(parts, fmt.Sprintf("up to %d %s", *l.MaxSessions, plural(*l.MaxSessions, "session")))
		}
		if l.SessionDuration != nil {
			parts = append(parts, "sessions last "+ruleDuration(*l.SessionDuration))
		}
	}
	if d.ApprovalExpiresIn != nil {
		parts = append(parts, "approvals last "+ruleDuration(*d.ApprovalExpiresIn))
	}
	if d.AllowDownload != nil && *d.AllowDownload {
		parts = append(parts, "downloads allowed")
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

// accessModes mirrors the catalog fixture's presentation labels and protection
// levels (0 = no level); TestAccessModesMatchCatalog keeps them identical.
var accessModes = map[string]struct {
	label string
	level int
}{
	"public":        {"Anyone with the link", 1},
	"email_capture": {"Anyone with a verified email", 2},
	"email_domain":  {"Work or school email", 3},
	"invite_only":   {"People you invite", 4},
	"review":        {"People you approve", 4},
	"keys":          {"Devices you approve", 5},
	"contract":      {"Custom access", 0},
}

func accessRuleLabel(mode string) string {
	if m, ok := accessModes[mode]; ok {
		return m.label
	}
	return "Unavailable"
}
