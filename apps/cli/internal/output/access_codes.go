package output

import (
	"fmt"
	"strconv"
	"text/tabwriter"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// AccessCodePage never emits a plaintext code, even if an old server sends one.
func (p *Printer) AccessCodePage(page *qurlapi.AccountPage[qurlapi.AccessCode]) error {
	if p.format == FormatJSON {
		return p.writeJSON(page)
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	if !p.quiet {
		ew.printf("ID\tNAME\tSTATUS\tUSES\tMAX USES\n")
	}
	for _, item := range page.Data {
		if p.quiet {
			ew.printf("%s\n", item.ID)
		} else {
			ew.printf("%s\t%s\t%s\t%d\t%d\n", item.ID, strconv.Quote(item.Name), item.Status, item.UseCount, item.MaxUses)
		}
	}
	if err := ew.flush(tw); err != nil {
		return err
	}
	if page.HasMore && !p.quiet {
		p.Notef("More codes are available; use next_cursor from --output json.")
	}
	return nil
}

// CreatedAccessCode prints the secret only on the caller's selected stdout format.
func (p *Printer) CreatedAccessCode(item *qurlapi.CreatedAccessCode) error {
	if p.format == FormatJSON {
		return p.writeJSON(item)
	}
	if p.quiet {
		_, err := fmt.Fprintln(p.out, item.Code)
		return err
	}
	_, err := fmt.Fprintf(p.out, "ID: %s\nCode: %s\nSave this code securely. It is shown only once.\n", item.ID, item.Code)
	return err
}

// AccessCodeDeleted confirms a completed revocation.
func (p *Printer) AccessCodeDeleted(id string) error {
	if p.format == FormatJSON {
		return p.writeJSON(map[string]string{"access_code_id": id, accountStatusField: "revoked"})
	}
	if p.quiet {
		_, err := fmt.Fprintln(p.out, id)
		return err
	}
	_, err := fmt.Fprintf(p.out, "Revoked %s\n", id)
	return err
}

// RedeemedAccessCode returns the link without navigating to it.
func (p *Printer) RedeemedAccessCode(link string) error {
	if p.format == FormatJSON {
		return p.writeJSON(map[string]string{"redirect_url": link})
	}
	_, err := fmt.Fprintln(p.out, link)
	return err
}
