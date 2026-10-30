package output

import (
	"fmt"

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
	for _, items := range [][]accessrules.Definition{snapshot.BuiltIns, snapshot.Items} {
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
func accessRuleLabel(mode string) string {
	switch mode {
	case "public":
		return "Anyone with the link"
	case "keys":
		return "Approved passkeys and devices"
	case "invite_only":
		return "Invite only"
	case "email_capture":
		return "Verify an email"
	case "email_domain":
		return "People at a domain"
	case "review":
		return "Ask first by email"
	case "contract":
		return "Approved by your service"
	default:
		return "Unavailable"
	}
}
