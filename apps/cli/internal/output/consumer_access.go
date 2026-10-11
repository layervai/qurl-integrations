package output

import (
	"fmt"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// ConsumerManagement renders the decision receipt without claiming pending cleanup succeeded.
func (p *Printer) ConsumerManagement(result *qurlapi.ConsumerManagementResult) error {
	if p.format == FormatJSON {
		return p.writeJSON(result)
	}
	if p.quiet {
		_, err := fmt.Fprintln(p.out, result.CRID)
		return err
	}
	if _, err := fmt.Fprintf(p.out, "Resource: %s\nAction: %s\n", result.CRID, result.Action); err != nil {
		return err
	}
	if result.Policy != nil {
		_, err := fmt.Fprintf(p.out, "Access: private (%s)\nVersion: %d\n", result.Policy.Mode, result.Policy.Version)
		return err
	}
	if result.Invitation != nil {
		_, err := fmt.Fprintf(p.out, "Invitation: %s\nEmail: %q\nStatus: %s\nVersion: %d\nDelivery: %s\nConnection cleanup: %s\n", result.Invitation.ID, result.Invitation.Email, result.Invitation.Status, result.Invitation.Version, result.Invitation.Delivery, result.Invitation.SessionCleanup)
		return err
	}
	if result.Grant != nil {
		_, err := fmt.Fprintf(p.out, "Consumer: %s\nStatus: %s\nVersion: %d\nConnection cleanup: %s\n", result.Grant.ID, result.Grant.Status, result.Grant.Version, result.Grant.SessionCleanup)
		return err
	}
	for _, invitation := range result.Invitations {
		if _, err := fmt.Fprintf(p.out, "%s  %s  version %d  %q\n", invitation.ID, invitation.Status, invitation.Version, invitation.Email); err != nil {
			return err
		}
	}
	for i := range result.Grants {
		grant := &result.Grants[i]
		if _, err := fmt.Fprintf(p.out, "%s  %s  version %d  %q\n", grant.ID, grant.Status, grant.Version, grant.Email); err != nil {
			return err
		}
	}
	if result.NextCursor != "" {
		_, err := fmt.Fprintf(p.out, "Next page: %q\n", result.NextCursor)
		return err
	}
	return nil
}
