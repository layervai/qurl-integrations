package output

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

const webhookStatusField = "status"

// WebhookPage prints one page without secret fields or terminal control sequences.
func (p *Printer) WebhookPage(page *qurlapi.WebhookPage[qurlapi.Webhook]) error {
	if p.format == FormatJSON {
		return p.writeJSON(page)
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	if !p.quiet {
		ew.printf("WEBHOOK ID\tSTATUS\tURL\tEVENTS\n")
	}
	for i := range page.Data {
		item := &page.Data[i]
		if p.quiet {
			ew.printf("%s\n", item.ID)
		} else {
			ew.printf("%s\t%s\t%s\t%s\n", item.ID, strconv.Quote(item.Status), strconv.Quote(item.URL), strconv.Quote(strings.Join(item.Events, ",")))
		}
	}
	if err := ew.flush(tw); err != nil {
		return err
	}
	if page.HasMore && !p.quiet {
		p.Notef("More webhooks are available; use next_cursor from --output json.")
	}
	return nil
}

// Webhook prints a secret only for the explicit create and rotate commands.
func (p *Printer) Webhook(item *qurlapi.Webhook, secret string) error {
	if p.format == FormatJSON {
		if secret != "" {
			return p.writeJSON(qurlapi.WebhookSecret{Webhook: *item, Secret: secret})
		}
		return p.writeJSON(item)
	}
	if p.quiet {
		value := item.ID
		if secret != "" {
			value = secret
		}
		_, err := fmt.Fprintln(p.out, value)
		return err
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	ew.printf("Webhook ID:\t%s\nOwner:\t%s\nURL:\t%s\nStatus:\t%s\nEvents:\t%s\nDescription:\t%s\n", item.ID, strconv.Quote(item.OwnerID), strconv.Quote(item.URL), strconv.Quote(item.Status), strconv.Quote(strings.Join(item.Events, ",")), strconv.Quote(item.Description))
	if secret != "" {
		ew.printf("Signing secret:\t%s\n", secret)
	}
	return ew.flush(tw)
}

// WebhookDeleted confirms the selected subscription was removed.
func (p *Printer) WebhookDeleted(id string) error {
	if p.format == FormatJSON {
		return p.writeJSON(map[string]string{"webhook_id": id, webhookStatusField: "deleted"})
	}
	if p.quiet {
		_, err := fmt.Fprintln(p.out, id)
		return err
	}
	_, err := fmt.Fprintf(p.out, "Deleted %s\n", id)
	return err
}

// WebhookEvents preserves the catalog text without promising event delivery.
func (p *Printer) WebhookEvents(items []qurlapi.WebhookEvent) error {
	if p.format == FormatJSON {
		return p.writeJSON(items)
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	if !p.quiet {
		ew.printf("EVENT\tCATEGORY\tDESCRIPTION\n")
	}
	for _, item := range items {
		if p.quiet {
			ew.printf("%s\n", item.Type)
		} else {
			ew.printf("%s\t%s\t%s\n", strconv.Quote(item.Type), strconv.Quote(item.Category), strconv.Quote(item.Description))
		}
	}
	return ew.flush(tw)
}

// WebhookDeliveries omits endpoint response text in every output format.
func (p *Printer) WebhookDeliveries(page *qurlapi.WebhookPage[qurlapi.WebhookDelivery]) error {
	if p.format == FormatJSON {
		return p.writeJSON(page)
	}
	tw := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	if !p.quiet {
		ew.printf("DELIVERY ID\tEVENT\tSTATUS\tHTTP STATUS\tATTEMPTS\n")
	}
	for i := range page.Data {
		item := &page.Data[i]
		if p.quiet {
			ew.printf("%s\n", item.ID)
		} else {
			ew.printf("%s\t%s\t%s\t%d\t%d\n", item.ID, strconv.Quote(item.EventType), strconv.Quote(item.Status), item.ResponseCode, item.RetryCount)
		}
	}
	if err := ew.flush(tw); err != nil {
		return err
	}
	if page.HasMore && !p.quiet {
		p.Notef("More deliveries are available; use next_cursor from --output json.")
	}
	return nil
}
