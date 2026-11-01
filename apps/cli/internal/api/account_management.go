package qurlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/layervai/qurl-go/qurl"
)

// AccountManagement uses the signed-in account; linked device owners are unsupported.
type AccountManagement interface {
	Me(context.Context) (*Identity, error)
	ListDomains(context.Context, int, string) (*AccountPage[AccountDomain], error)
	GetAccountDomain(context.Context, string) (*AccountDomain, error)
	RegisterAccountDomain(context.Context, string) (*AccountDomain, error)
	DeleteAccountDomain(context.Context, string) error
	VerifyAccountDomain(context.Context, string) (*DomainVerification, error)
	RegenerateDomainToken(context.Context, string) (*AccountDomain, error)
	ListWebhooks(context.Context, int, string) (*AccountPage[Webhook], error)
	GetWebhook(context.Context, string) (*Webhook, error)
	CreateWebhook(context.Context, WebhookInput) (*WebhookSecret, error)
	UpdateWebhook(context.Context, string, WebhookInput) (*Webhook, error)
	DeleteWebhook(context.Context, string) error
	RotateWebhookSecret(context.Context, string) (*WebhookSecret, error)
	WebhookEvents(context.Context) ([]WebhookEvent, error)
	WebhookDeliveries(context.Context, string, int, string) (*AccountPage[WebhookDelivery], error)
}

// NewAccountManagement shares the CLI transport without device-owner selection.
func NewAccountManagement(cfg *Config) (AccountManagement, error) {
	if cfg == nil || cfg.OwnerID != "" {
		return nil, fmt.Errorf("%w: account administration requires the browser account owner", qurl.ErrInvalidResourceRequest)
	}
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return c.(*client), nil
}

// AccountPage contains one bounded page without automatic cursor traversal.
type AccountPage[T any] struct {
	Data       []T    `json:"data"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
}

// ValidateAccountPage checks the server page bounds before sign-in.
func ValidateAccountPage(limit int) error {
	if limit < 1 || limit > 100 {
		return fmt.Errorf("%w: limit must be 1–100", qurl.ErrInvalidResourceRequest)
	}
	return nil
}

func accountListPath(path string, limit int, cursor string) string {
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	return path + "?" + query.Encode()
}

func decodeAccountPage[T any](reply *restReply, limit int) (*AccountPage[T], error) {
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}
	var wire struct {
		Data []T `json:"data"`
		Meta *struct {
			HasMore    *bool  `json:"has_more"`
			NextCursor string `json:"next_cursor"`
		} `json:"meta"`
	}
	if json.Unmarshal(reply.body, &wire) != nil || wire.Data == nil || len(wire.Data) > limit || wire.Meta == nil || wire.Meta.HasMore == nil || (*wire.Meta.HasMore && wire.Meta.NextCursor == "") {
		return nil, fmt.Errorf("%w: incomplete account page", qurl.ErrInvalidAPIResponse)
	}
	return &AccountPage[T]{Data: wire.Data, HasMore: *wire.Meta.HasMore, NextCursor: wire.Meta.NextCursor}, nil
}
