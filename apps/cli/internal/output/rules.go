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
		for _, d := range items {
			if p.quiet {
				if _, err := fmt.Fprintln(p.out, d.ID); err != nil {
					return err
				}
				continue
			}
			if _, err := fmt.Fprintf(p.out, "%s  %q  %s\n", d.ID, d.Name, accessRuleLabel(d.Mode)); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintf(p.out, "Version: %d\nSync: %s\n", snapshot.LocalVersion, snapshot.Sync)
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
