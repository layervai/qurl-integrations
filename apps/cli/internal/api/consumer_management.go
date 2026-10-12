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

// ConsumerManagement exposes the scoped owner API with the configured transport.
func (c *client) ConsumerManagement() ConsumerManagementClient {
	return c.sdk
}

// ConsumerManagementResult is internal; output uses explicit fields to omit secrets.
type ConsumerManagementResult struct {
	Action      string
	CRID        string
	Policy      *qurl.ConsumerAccessPolicy
	Invitation  *qurl.ConsumerInvitation
	Grant       *qurl.ConsumerGrant
	Invitations []qurl.ConsumerInvitation
	Grants      []qurl.ConsumerGrant
	NextCursor  string
}
