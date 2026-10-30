package output

import (
	"cmp"
	"fmt"
	"slices"

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
	_, err := fmt.Fprintf(p.out, "%s  %q  %s\n", d.ID, d.Name, accessRuleLabel(d.Mode))
	return err
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
