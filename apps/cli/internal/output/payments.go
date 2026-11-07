package output

import (
	"fmt"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// PublisherPayments prints the Stripe consent URL.
func (p *Printer) PublisherPayments(link string) error {
	if p.format == FormatJSON {
		return p.writeJSON(struct {
			ConnectURL       string `json:"connect_url"`
			LayerVFeePercent int    `json:"layerv_fee_percent"`
		}{link, 3})
	}
	_, err := fmt.Fprintln(p.out, link)
	p.feeNote(err)
	return err
}

// feeNote states LayerV's fee after a successful payments answer.
func (p *Printer) feeNote(err error) {
	if err == nil && !p.quiet {
		p.Notef("LayerV collects 3%% of each CRID payment, including subscription renewals.")
	}
}

// PublisherStripeAccount prints the connection and whether it can take payments.
func (p *Printer) PublisherStripeAccount(a *qurlapi.PublisherStripeAccount) error {
	if p.format == FormatJSON {
		return p.writeJSON(a)
	}
	if a.AccountID == "" {
		_, err := fmt.Fprintln(p.out, "No Stripe account is connected. Run qurl payments connect.")
		p.feeNote(err)
		return err
	}
	if a.Revoked {
		_, err := fmt.Fprintf(p.out, "%s\nLayerV's access was removed in Stripe, so buyers lost access and LayerV cannot stop their billing. Cancel or refund remaining subscriptions in Stripe, then run qurl payments disconnect, and qurl payments connect to use another account.\n", a.AccountID)
		return err
	}
	if a.Disconnecting {
		_, err := fmt.Fprintf(p.out, "%s\nNew purchases are stopped. Run qurl payments disconnect to finish disconnecting.\n", a.AccountID)
		return err
	}
	if a.StatusUnavailable {
		_, err := fmt.Fprintf(p.out, "%s\nStripe could not be reached to check this account. Try again later.\n", a.AccountID)
		return err
	}
	_, err := fmt.Fprintf(p.out, "%s\nCharges: %s\nPayouts: %s\nRenewing subscriptions: %d\n", a.AccountID, enabledText(a.ChargesEnabled), enabledText(a.PayoutsEnabled), a.RenewingSubscriptions)
	if err == nil && a.SetupURL != "" {
		_, err = fmt.Fprintf(p.out, "Finish setup in Stripe: %s\n", a.SetupURL)
	}
	p.feeNote(err)
	return err
}

// PublisherStripeDisconnected reports a disconnect, or that nothing was connected.
// A revoked account was released only: its billing must still be stopped in Stripe.
func (p *Printer) PublisherStripeDisconnected(disconnected, revoked bool) error {
	if p.format == FormatJSON {
		return p.writeJSON(struct {
			Disconnected bool `json:"disconnected"`
		}{disconnected})
	}
	text := "Stripe is disconnected. Buyers keep access until their paid period ends and are not charged again. Run qurl payments connect to sell again."
	switch {
	case !disconnected:
		text = "No Stripe account is connected."
	case revoked:
		text = "LayerV is released from the Stripe account. Cancel or refund any remaining subscriptions in Stripe. Run qurl payments connect to sell again."
	}
	_, err := fmt.Fprintln(p.out, text)
	return err
}

func enabledText(on bool) string {
	if on {
		return "enabled"
	}
	return "not enabled"
}
