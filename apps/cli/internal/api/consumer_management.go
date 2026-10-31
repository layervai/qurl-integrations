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
// Its failures go through mapError like every other SDK call, so a 409 exits
// with the conflict code and a 503 with the unavailable code, as the access
// help and the agent instructions say.
func (c *client) ConsumerManagement() ConsumerManagementClient {
	return consumerManagement{sdk: c.sdk}
}

// consumerManagement maps the SDK's API errors to this package's *Error.
type consumerManagement struct {
	sdk *qurl.Client
}

// InviteConsumer mails an invitation, with API errors mapped.
func (m consumerManagement) InviteConsumer(ctx context.Context, crid, email string) (*qurl.ConsumerInvitation, error) {
	invitation, err := m.sdk.InviteConsumer(ctx, crid, email)
	return invitation, mapError(err)
}

// ListConsumerInvitations reads one page of invitations, with API errors mapped.
func (m consumerManagement) ListConsumerInvitations(ctx context.Context, crid, cursor string) (*qurl.ConsumerPage[qurl.ConsumerInvitation], error) {
	page, err := m.sdk.ListConsumerInvitations(ctx, crid, cursor)
	return page, mapError(err)
}

// ResendConsumerInvitation sends an invitation again, with API errors mapped.
func (m consumerManagement) ResendConsumerInvitation(ctx context.Context, crid, id string, version int64) (*qurl.ConsumerInvitation, error) {
	invitation, err := m.sdk.ResendConsumerInvitation(ctx, crid, id, version)
	return invitation, mapError(err)
}

// RevokeConsumerInvitation removes an invitation and the grant it enrolled, with API errors mapped.
func (m consumerManagement) RevokeConsumerInvitation(ctx context.Context, crid, id string, version int64) (*qurl.ConsumerInvitation, error) {
	invitation, err := m.sdk.RevokeConsumerInvitation(ctx, crid, id, version)
	return invitation, mapError(err)
}

// ListConsumerGrants reads one page of grants, with API errors mapped.
func (m consumerManagement) ListConsumerGrants(ctx context.Context, crid, cursor string) (*qurl.ConsumerPage[qurl.ConsumerGrant], error) {
	page, err := m.sdk.ListConsumerGrants(ctx, crid, cursor)
	return page, mapError(err)
}

// ApproveConsumerGrant approves a grant or a key opt-in, with API errors mapped.
func (m consumerManagement) ApproveConsumerGrant(ctx context.Context, crid, id string, approval qurl.ConsumerGrantApproval) (*qurl.ConsumerGrant, error) { //nolint:gocritic // hugeParam: signature is dictated by the ConsumerManagementClient interface and the SDK method.
	grant, err := m.sdk.ApproveConsumerGrant(ctx, crid, id, approval)
	return grant, mapError(err)
}

// DenyConsumerGrant refuses a pending grant, with API errors mapped.
func (m consumerManagement) DenyConsumerGrant(ctx context.Context, crid, id string, version int64) (*qurl.ConsumerGrant, error) {
	grant, err := m.sdk.DenyConsumerGrant(ctx, crid, id, version)
	return grant, mapError(err)
}

// RevokeConsumerGrant removes a grant, with API errors mapped.
func (m consumerManagement) RevokeConsumerGrant(ctx context.Context, crid, id string, version int64) (*qurl.ConsumerGrant, error) {
	grant, err := m.sdk.RevokeConsumerGrant(ctx, crid, id, version)
	return grant, mapError(err)
}

// UpdateConsumerAccess changes the resource policy, with API errors mapped.
func (m consumerManagement) UpdateConsumerAccess(ctx context.Context, crid string, expected int64, policy qurl.ConsumerAccessPolicy) error { //nolint:gocritic // hugeParam: signature is dictated by the ConsumerManagementClient interface and SDK method.
	return mapError(m.sdk.UpdateConsumerAccess(ctx, crid, expected, policy))
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
