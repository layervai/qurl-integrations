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
		}{link, qurlapi.LayerVFeePercent})
	}
	_, err := fmt.Fprintln(p.out, link)
	p.feeNote(err)
	return err
}

// feeNote states LayerV's fee after a successful payments answer.
func (p *Printer) feeNote(err error) {
	if err == nil && !p.quiet {
		p.Notef("LayerV collects %d%% of each CRID payment, including subscription renewals.", qurlapi.LayerVFeePercent)
	}
}

// PublisherStripeAccount prints the connection and whether it can take payments.
func (p *Printer) PublisherStripeAccount(a *qurlapi.PublisherStripeAccount) error {
	if p.format == FormatJSON {
		// Field by field, so a member added to the API type is not printed unreviewed.
		return p.writeJSON(struct {
			AccountID             string `json:"account_id"`
			SetupURL              string `json:"setup_url,omitempty"`
			LayerVFeePercent      int    `json:"layerv_fee_percent"`
			RenewingSubscriptions int    `json:"renewing_subscriptions"`
			ChargesEnabled        bool   `json:"charges_enabled"`
			PayoutsEnabled        bool   `json:"payouts_enabled"`
			Disconnecting         bool   `json:"disconnecting"`
			Revoked               bool   `json:"revoked"`
			StatusUnavailable     bool   `json:"status_unavailable"`
		}{a.AccountID, a.SetupURL, a.LayerVFeePercent, a.RenewingSubscriptions, a.ChargesEnabled, a.PayoutsEnabled, a.Disconnecting, a.Revoked, a.StatusUnavailable})
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

// StripeDisconnectOutcome is what a disconnect did, as far as the CLI knows.
type StripeDisconnectOutcome int

const (
	// StripeNothingConnected means no account was connected, so nothing was sent.
	StripeNothingConnected StripeDisconnectOutcome = iota
	// StripeDisconnected means renewals stopped and LayerV left the account.
	StripeDisconnected
	// StripeReleasedRevoked means LayerV's access was already removed in Stripe, so
	// the account was only released; its billing must still be stopped there.
	StripeReleasedRevoked
	// StripeDisconnectedUnread means --yes read no status, so which of the above
	// happened is unknown; only that nothing is connected any more.
	StripeDisconnectedUnread
)

// PublisherStripeDisconnected reports a disconnect outcome.
func (p *Printer) PublisherStripeDisconnected(outcome StripeDisconnectOutcome) error {
	if p.format == FormatJSON {
		return p.writeJSON(struct {
			Disconnected bool `json:"disconnected"`
		}{outcome != StripeNothingConnected})
	}
	text := map[StripeDisconnectOutcome]string{
		StripeNothingConnected: "No Stripe account is connected.",
		StripeDisconnected:     "Stripe is disconnected. Buyers keep access until their paid period ends and are not charged again. Run qurl payments connect to sell again.",
		StripeReleasedRevoked:  "LayerV is released from the Stripe account. Cancel or refund any remaining subscriptions in Stripe. Run qurl payments connect to sell again.",
		StripeDisconnectedUnread: "Nothing is connected to LayerV any more. If LayerV's access had been removed in Stripe, cancel or refund remaining subscriptions there. " +
			"Run qurl payments connect to sell again.",
	}[outcome]
	_, err := fmt.Fprintln(p.out, text)
	return err
}

func enabledText(on bool) string {
	if on {
		return "enabled"
	}
	return "not enabled"
}
