package output

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// APIKeyPage prints metadata only; quiet output contains validated key IDs.
func (p *Printer) APIKeyPage(owner string, page *qurlapi.APIKeyPage) error {
	if p.format == FormatJSON {
		return p.writeJSON(struct {
			OwnerID string `json:"owner_id"`
			*qurlapi.APIKeyPage
		}{owner, page})
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	if !p.quiet {
		ew.printf("KEY ID\tKIND\tSTATUS\tNAME\tSCOPES\n")
	}
	for i := range page.Keys {
		key := &page.Keys[i]
		if p.quiet {
			ew.printf("%s\n", key.KeyID)
		} else {
			ew.printf("%s\t%s\t%s\t%s\t%s\n", key.KeyID, strconv.Quote(key.Kind), strconv.Quote(key.Status), strconv.Quote(key.Name), strconv.Quote(strings.Join(key.Scopes, ",")))
		}
	}
	if err := ew.flush(tw); err != nil {
		return err
	}
	if page.HasMore && !p.quiet {
		p.Notef("More credentials are available; use next_cursor from --output json.")
	}
	return nil
}

// APIKey includes a secret only for an explicit creation result.
func (p *Printer) APIKey(owner string, key *qurlapi.APIKeyMetadata, secret string) error {
	if p.format == FormatJSON {
		return p.writeJSON(struct {
			OwnerID string `json:"owner_id"`
			qurlapi.APIKeyMetadata
			Secret string `json:"api_key,omitempty"`
		}{owner, *key, secret})
	}
	if p.quiet {
		value := key.KeyID
		if secret != "" {
			value = secret
		}
		_, err := fmt.Fprintln(p.out, value)
		return err
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	ew.printf("Key ID:\t%s\nOwner:\t%s\nName:\t%s\nKind:\t%s\nStatus:\t%s\nScopes:\t%s\n", key.KeyID, strconv.Quote(owner), strconv.Quote(key.Name), strconv.Quote(key.Kind), strconv.Quote(key.Status), strconv.Quote(strings.Join(key.Scopes, ",")))
	if secret != "" {
		ew.printf("API key:\t%s\n", secret)
	}
	return ew.flush(tw)
}

// APIKeyRevoked reports the selected credential and confirmed revoke result.
func (p *Printer) APIKeyRevoked(owner, id string) error {
	if p.format == FormatJSON {
		return p.writeJSON(map[string]string{"owner_id": owner, "key_id": id, "status": "revoked"})
	}
	if p.quiet {
		_, err := fmt.Fprintln(p.out, id)
		return err
	}
	_, err := fmt.Fprintf(p.out, "Revoked %s\n", id)
	return err
}
