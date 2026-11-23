package output

import (
	"errors"
	"fmt"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// SignedIdentity reports only a server-confirmed setting, never an assertion.
func (p *Printer) SignedIdentity(resource *qurlapi.ResourceSummary) error {
	if resource == nil || resource.SignedIdentity == nil {
		return errors.New("qURL signed identity answer is empty")
	}
	if p.format == FormatJSON {
		return p.writeJSON(struct {
			CRID    string `json:"crid"`
			Enabled bool   `json:"signed_identity"`
		}{resource.CRID, *resource.SignedIdentity})
	}
	if p.quiet {
		_, err := fmt.Fprintln(p.out, *resource.SignedIdentity)
		return err
	}
	state := "disabled"
	if *resource.SignedIdentity {
		state = "enabled"
	}
	_, err := fmt.Fprintf(p.out, "CRID: %s\nSigned identity: %s\n", resource.CRID, state)
	return err
}
