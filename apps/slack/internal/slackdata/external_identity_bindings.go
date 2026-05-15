package slackdata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ExternalIdentityBindingsClient is the HTTP client surface for the
// new `POST /v1/external-identity-bindings` qurl-service endpoint.
//
// This endpoint does NOT EXIST in qurl-service yet — its design is
// drafted in SLACK_QURL_ROLLOUT.md Appendix A, polished 2026-05-14
// for Justin's async sign-off. The interface lives here so the
// admin-claim redeem flow can compile against the right call site
// today; once the endpoint ships, the production binding-client
// constructor wires a non-nil instance into the Store via
// WithExternalIdentityBindings.
//
// TODO: implement once qurl-service ships the endpoint.
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

// HTTPBindingsClient is the production implementation of
// ExternalIdentityBindingsClient. It POSTs JSON to
// `<baseURL>/v1/external-identity-bindings` with an Auth0 Bearer.
//
// Today this client is wired but NOT INVOKED — the
// `ExternalIdentityBindings` field on Store is left nil in
// cmd/main.go until the qurl-service endpoint is live (see
// SLACK_QURL_ROLLOUT.md Appendix A "Status" line). Surfacing it
// here means the call-site signature stays stable; the only thing
// that changes when the endpoint ships is the constructor in
// cmd/main.go.
//
// TODO: implement once qurl-service ships the endpoint.
type HTTPBindingsClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewHTTPBindingsClient builds a [HTTPBindingsClient]. Empty BaseURL
// is rejected at call time (the Create method short-circuits on it
// rather than panicking on a malformed Request.URL).
func NewHTTPBindingsClient(baseURL string) *HTTPBindingsClient {
	return &HTTPBindingsClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Create POSTs the binding request to qurl-service. Until the
// endpoint exists this will return a 404 from the service — the
// caller (RedeemBootstrap) is responsible for treating that as
// "endpoint not deployed yet" and surfacing a user-facing message
// rather than the raw error.
func (c *HTTPBindingsClient) Create(ctx context.Context, req *CreateBindingRequest) (*CreateBindingResponse, error) {
	if c.BaseURL == "" {
		return nil, errors.New("HTTPBindingsClient.Create: BaseURL is empty")
	}
	if req == nil {
		return nil, errors.New("HTTPBindingsClient.Create: request is nil")
	}
	if req.Bearer == "" {
		return nil, errors.New("HTTPBindingsClient.Create: Auth0 Bearer is required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("HTTPBindingsClient.Create: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/external-identity-bindings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("HTTPBindingsClient.Create: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+req.Bearer)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if req.IdempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTPBindingsClient.Create: do: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("HTTPBindingsClient.Create: read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &Error{
			StatusCode: resp.StatusCode,
			Code:       "binding_create_failed",
			Title:      "external_identity_bindings: non-2xx",
			Detail:     string(respBody),
		}
	}
	var out CreateBindingResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("HTTPBindingsClient.Create: unmarshal: %w", err)
	}
	return &out, nil
}
