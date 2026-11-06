package output

import (
	"fmt"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// PublisherPayments prints the consent URL without payment credentials.
func (p *Printer) PublisherPayments(account, link string) error {
	if p.format == FormatJSON {
		return p.writeJSON(struct {
			AccountID        string `json:"account_id,omitempty"`
			ConnectURL       string `json:"connect_url,omitempty"`
			LayerVFeePercent int    `json:"layerv_fee_percent"`
		}{account, link, 3})
	}
	value := account
	if link != "" {
		value = link
	}
	if value == "" {
		value = "No Stripe account is connected. Run qurl payments connect."
	}
	_, err := fmt.Fprintln(p.out, value)
	if err == nil && !p.quiet {
		p.Notef("LayerV collects 3%% of each CRID payment, including subscription renewals.")
	}
	return err
}

// PublisherStripeAccount prints the connection and whether it can take payments.
func (p *Printer) PublisherStripeAccount(a *qurlapi.PublisherStripeAccount) error {
	if p.format == FormatJSON {
		return p.writeJSON(a)
	}
	if a.AccountID == "" {
		return p.PublisherPayments("", "")
	}
	if a.Revoked {
		_, err := fmt.Fprintf(p.out, "%s\nLayerV's access was removed in Stripe, so buyers lost access and LayerV cannot stop their billing. Cancel or refund remaining subscriptions in Stripe, then run qurl payments disconnect to connect another account.\n", a.AccountID)
		return err
	}
	if a.Disconnecting {
		_, err := fmt.Fprintf(p.out, "%s\nNew purchases are stopped. Run qurl payments disconnect to finish disconnecting.\n", a.AccountID)
		return err
	}
	enabled := map[bool]string{true: "enabled", false: "not enabled"}
	_, err := fmt.Fprintf(p.out, "%s\nCharges: %s\nPayouts: %s\nRenewing subscriptions: %d\n", a.AccountID, enabled[a.ChargesEnabled], enabled[a.PayoutsEnabled], a.RenewingSubscriptions)
	if err == nil && a.SetupURL != "" {
		_, err = fmt.Fprintf(p.out, "Finish setup in Stripe: %s\n", a.SetupURL)
	}
	if err == nil && !p.quiet {
		p.Notef("LayerV collects 3%% of each CRID payment, including subscription renewals.")
	}
	return err
}

// PublisherStripeDisconnected confirms that sales stopped and LayerV left the account.
func (p *Printer) PublisherStripeDisconnected() error {
	if p.format == FormatJSON {
		return p.writeJSON(struct {
			Disconnected bool `json:"disconnected"`
		}{true})
	}
	_, err := fmt.Fprintln(p.out, "Stripe is disconnected. Buyers keep access until their paid period ends and are not charged again. Run qurl payments connect to sell again.")
	return err
}
