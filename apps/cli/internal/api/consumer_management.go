package qurlapi

import (
	"context"

	"github.com/layervai/qurl-go/qurl"
)

// ConsumerManagementClient is the owner capability for scoped private decisions.
type ConsumerManagementClient interface {
	InviteConsumer(context.Context, string, string) (*qurl.ConsumerInvitation, error)
	ListConsumerInvitations(context.Context, string, string) (*qurl.ConsumerPage[qurl.ConsumerInvitation], error)
	ResendConsumerInvitation(context.Context, string, string, int64) (*qurl.ConsumerInvitation, error)
	RevokeConsumerInvitation(context.Context, string, string, int64) (*qurl.ConsumerInvitation, error)
	ListConsumerGrants(context.Context, string, string) (*qurl.ConsumerPage[qurl.ConsumerGrant], error)
	ApproveConsumerGrant(context.Context, string, string, qurl.ConsumerGrantApproval) (*qurl.ConsumerGrant, error)
	DenyConsumerGrant(context.Context, string, string, int64) (*qurl.ConsumerGrant, error)
	RevokeConsumerGrant(context.Context, string, string, int64) (*qurl.ConsumerGrant, error)
	UpdateConsumerAccess(context.Context, string, int64, qurl.ConsumerAccessPolicy) error
}

type consumerManagement struct{ ConsumerManagementClient }

// ConsumerManagement exposes the scoped owner API with the configured transport.
func (c *client) ConsumerManagement() ConsumerManagementClient {
	return consumerManagement{c.sdk}
}

// ConsumerManagementResult contains owner receipts, never invitation links or codes.
type ConsumerManagementResult struct {
	Action      string                     `json:"action"`
	CRID        string                     `json:"crid"`
	Policy      *qurl.ConsumerAccessPolicy `json:"consumer_access,omitempty"`
	Invitation  *qurl.ConsumerInvitation   `json:"invitation,omitempty"`
	Grant       *qurl.ConsumerGrant        `json:"grant,omitempty"`
	Invitations []qurl.ConsumerInvitation  `json:"invitations,omitempty"`
	Grants      []qurl.ConsumerGrant       `json:"grants,omitempty"`
	NextCursor  string                     `json:"next_cursor,omitempty"`
}
