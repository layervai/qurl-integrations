package qurlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"

	"github.com/layervai/qurl-go/qurl"
)

var stripeAccountID = regexp.MustCompile(`^acct_[A-Za-z0-9]{1,128}$`)

// PublisherStripeAccount is the publisher's Stripe connection. It holds no
// payment credentials.
type PublisherStripeAccount struct {
	AccountID             string `json:"account_id"`
	SetupURL              string `json:"setup_url,omitempty"`
	LayerVFeePercent      int    `json:"layerv_fee_percent"`
	RenewingSubscriptions int    `json:"renewing_subscriptions"`
	ChargesEnabled        bool   `json:"charges_enabled"`
	PayoutsEnabled        bool   `json:"payouts_enabled"`
	// Disconnecting means sales stopped and a disconnect must be run again to finish.
	Disconnecting bool `json:"disconnecting"`
	// Revoked means LayerV's access was removed in Stripe; billing must be canceled there.
	Revoked bool `json:"revoked"`
}

// PublisherStripeStatus reads the connected account, whether Stripe lets it
// take payments, and how many subscriptions a disconnect would stop renewing.
func (c *client) PublisherStripeStatus(ctx context.Context) (*PublisherStripeAccount, error) {
	reply, err := c.doREST(ctx, http.MethodGet, "/v1/payments/account", nil)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}
	var env struct {
		Data *PublisherStripeAccount `json:"data"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode Stripe account: %w", qurl.ErrInvalidAPIResponse, err)
	}
	a := env.Data
	if a == nil || a.LayerVFeePercent != 3 || a.RenewingSubscriptions < 0 || (a.AccountID != "" && !stripeAccountID.MatchString(a.AccountID)) || !stripeSetupURL(a.SetupURL) {
		return nil, fmt.Errorf("%w: invalid Stripe account", qurl.ErrInvalidAPIResponse)
	}
	return a, nil
}

// stripeSetupURL accepts only an absent link or a Stripe dashboard page, so the
// CLI never tells a publisher to open another site.
func stripeSetupURL(value string) bool {
	if value == "" {
		return true
	}
	u, err := url.Parse(value)
	return err == nil && u.Scheme == schemeHTTPS && u.Host == "dashboard.stripe.com" && u.User == nil
}

// DisconnectPublisherStripe stops renewals and removes LayerV from the
// publisher's Stripe account. It succeeds when nothing is connected.
func (c *client) DisconnectPublisherStripe(ctx context.Context) error {
	reply, err := c.doREST(ctx, http.MethodDelete, "/v1/payments/account", nil)
	if err != nil {
		return err
	}
	if reply.status != http.StatusNoContent {
		return reply.problem()
	}
	return nil
}
