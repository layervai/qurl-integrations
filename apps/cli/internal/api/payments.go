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

// PublisherStripe reads and ends the publisher's Stripe connection.
type PublisherStripe interface {
	PublisherStripeStatus(context.Context) (*PublisherStripeAccount, error)
	DisconnectPublisherStripe(context.Context) error
}

// LayerVFeePercent is LayerV's share of each CRID payment, as this CLI states it.
const LayerVFeePercent = 3

// PublisherStripeAccount is the publisher's Stripe connection. It holds no
// payment credentials.
// TODO(upstream-contract): qurl-service owns GET/DELETE /v1/payments/account:
// these members, 204 on DELETE, and what disconnecting, revoked and
// status_unavailable mean. Change this file with that contract. DELETE names
// no account, so the account a prompt showed is not a precondition; the
// service has no such precondition today.
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
	// StatusUnavailable means Stripe could not be read; the status members are unknown.
	StatusUnavailable bool `json:"status_unavailable"`
}

// stripeStatusMembers must all be present: a service that predates them would
// otherwise read as "not enabled" and "0 subscriptions".
var stripeStatusMembers = []string{"account_id", "layerv_fee_percent", "charges_enabled", "payouts_enabled", "renewing_subscriptions"}

// PublisherStripeStatus reads the connected account, whether Stripe lets it
// take payments, and how many subscriptions a disconnect would stop renewing.
func (c *client) PublisherStripeStatus(ctx context.Context) (*PublisherStripeAccount, error) {
	reply, err := c.doREST(ctx, http.MethodGet, "/v1/payments/account", nil)
	if err != nil {
		return nil, err
	}
	return decodeStripeStatus(reply)
}

func decodeStripeStatus(reply *restReply) (*PublisherStripeAccount, error) {
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	var members map[string]json.RawMessage
	var a PublisherStripeAccount
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode Stripe account: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if json.Unmarshal(env.Data, &members) != nil || json.Unmarshal(env.Data, &a) != nil {
		return nil, fmt.Errorf("%w: invalid Stripe account", qurl.ErrInvalidAPIResponse)
	}
	for _, name := range stripeStatusMembers {
		if _, ok := members[name]; !ok {
			return nil, fmt.Errorf("%w: Stripe account has no %s; the service predates this CLI", qurl.ErrInvalidAPIResponse, name)
		}
	}
	if a.RenewingSubscriptions < 0 || (a.AccountID != "" && !stripeAccountID.MatchString(a.AccountID)) || !stripeSetupURL(a.SetupURL) {
		return nil, fmt.Errorf("%w: invalid Stripe account", qurl.ErrInvalidAPIResponse)
	}
	return &a, nil
}

// stripeSetupURL accepts only an absent link or a Stripe dashboard page, so the
// CLI never tells a publisher to open another site.
// TODO(upstream-contract): qurl-service links OAuth-connected Standard accounts
// to dashboard.stripe.com; an Account Link would live on connect.stripe.com.
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
	switch reply.status {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return fmt.Errorf("%w: this qURL service cannot disconnect Stripe yet (HTTP %d)", qurl.ErrInvalidAPIResponse, reply.status)
	default:
		if reply.status < 300 {
			return fmt.Errorf("%w: Stripe disconnect answered %d, not 204", qurl.ErrInvalidAPIResponse, reply.status)
		}
		return reply.problem()
	}
}
