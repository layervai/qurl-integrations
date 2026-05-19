package slackdata

import (
	"context"
)

// ExternalIdentityBindingsClient is the HTTP client surface for the
// `POST /v1/external-identity-bindings` qurl-service endpoint.
//
// This endpoint does NOT EXIST in qurl-service yet — its design is
// drafted in SLACK_QURL_ROLLOUT.md Appendix A; the qurl-service PR
// (#547) is open but not yet merged. The interface lives here so
// the admin-claim redeem flow can compile against the right call
// site today; once the endpoint ships, the production binding-
// client implementation lands in a follow-up PR alongside the
// constructor wire-up in cmd/main.go.
//
// Why this lives as an interface-only seam today:
//   - The Store.ExternalIdentityBindings field is intentionally nil
//     in cmd/main.go — RedeemBootstrap short-circuits the step-2
//     binding call when the field is nil (see bootstrap_codes.go).
//   - Shipping the HTTP implementation here without the endpoint
//     would be dead code; per CLAUDE.md "Don't add features beyond
//     what the task requires." The implementation lands in the
//     same PR that flips the field non-nil.
//
// TODO: implement once qurl-service #547 ships the endpoint.
type ExternalIdentityBindingsClient interface {
	Create(ctx context.Context, req *CreateBindingRequest) (*CreateBindingResponse, error)
}

// CreateBindingRequest is the wire shape designed in Appendix A.
type CreateBindingRequest struct {
	Provider    string   `json:"provider"`    // "slack" | "discord" | "cli" | ...
	ExternalID  string   `json:"external_id"` // Slack team_id, Discord guild_id, ...
	DisplayName string   `json:"display_name,omitempty"`
	Scopes      []string `json:"scopes,omitempty"` // optional; server-derived if omitted

	// IdempotencyKey is set by the caller (client-generated UUIDv7);
	// the bot uses this for replay safety against the 24h window
	// described in Appendix A.
	IdempotencyKey string `json:"-"`

	// Bearer is the Auth0 JWT the bot holds for the admin who
	// completed `/oauth/qurl/start`. Sent as `Authorization: Bearer
	// <jwt>` — NOT in the JSON body. Plumbed via the field rather
	// than ctx so a single call site holds both the body and the
	// auth context.
	Bearer string `json:"-"`
}

// CreateBindingResponse mirrors the Appendix A 201 shape.
type CreateBindingResponse struct {
	BindingID  string                      `json:"binding_id"`
	OwnerID    string                      `json:"owner_id"`
	Provider   string                      `json:"provider"`
	ExternalID string                      `json:"external_id"`
	APIKey     CreateBindingResponseAPIKey `json:"api_key"`
	CreatedAt  string                      `json:"created_at"`
}

// CreateBindingResponseAPIKey is the embedded api_key block.
type CreateBindingResponseAPIKey struct {
	KeyID     string `json:"key_id"`
	KeyPrefix string `json:"key_prefix"`
	Plaintext string `json:"plaintext"` // shown once
}
