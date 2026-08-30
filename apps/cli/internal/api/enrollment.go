package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
)

const (
	// TODO(upstream-contract): These values mirror qurl-service's Connector
	// enrollment request contract, including its maximum 24-hour lifetime.
	connectorEnrollmentKind     = "enrollment_token"
	connectorEnrollmentTarget   = "connector"
	agentEnrollmentTarget       = "agent"
	connectorEnrollmentStatus   = "active"
	connectorEnrollmentLifetime = "24h"
	minIdempotencyKeyLength     = 32
	maxIdempotencyKeyLength     = 256
)

// MintConnectorEnrollmentTokenOptions identifies the Connector being
// enrolled and the caller-owned idempotency episode. IdempotencyKey is not a
// secret, but it must remain stable across retries of one mint attempt.
type MintConnectorEnrollmentTokenOptions struct {
	ConnectorID    string
	IdempotencyKey string
}

// MintAgentEnrollmentTokenOptions identifies one caller-owned, retryable
// bootstrap episode. The token itself stays unbound; the NHP registration
// binds it to the generated device identity when it is consumed.
type MintAgentEnrollmentTokenOptions struct {
	IdempotencyKey string
}

// ConnectorEnrollmentToken is the one-shot credential returned for native
// Connector enrollment. Token is plaintext secret material and must remain
// in memory; callers must never put it in argv, logs, environment variables,
// or durable state.
type ConnectorEnrollmentToken struct {
	Token     string
	KeyID     string
	ExpiresAt time.Time
}

// AgentEnrollmentToken is the one-shot credential returned for an unbound
// native device registration. Token must remain only in memory until NHP
// consumes it.
type AgentEnrollmentToken struct {
	Token     string
	KeyID     string
	ExpiresAt time.Time
}

type connectorEnrollmentClaim struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type connectorEnrollmentRequest struct {
	Kind      string                     `json:"kind"`
	Name      string                     `json:"name"`
	Target    string                     `json:"target"`
	Claims    []connectorEnrollmentClaim `json:"claims,omitempty"`
	ExpiresIn string                     `json:"expires_in"`
}

type connectorEnrollmentData struct {
	Token     string                     `json:"api_key"`
	KeyID     string                     `json:"key_id"`
	Kind      string                     `json:"kind"`
	Target    string                     `json:"target"`
	Claims    []connectorEnrollmentClaim `json:"claims"`
	Status    string                     `json:"status"`
	ExpiresAt *time.Time                 `json:"expires_at"`
}

// MintAgentEnrollmentToken mints target=agent with zero claims and no
// caller-selected scopes. It is the only account-key operation needed before
// the registered client takes over steady-state resource access.
func (c *client) MintAgentEnrollmentToken(ctx context.Context, opts MintAgentEnrollmentTokenOptions) (*AgentEnrollmentToken, error) {
	if err := validateEnrollmentIdempotencyKey(opts.IdempotencyKey); err != nil {
		return nil, err
	}
	body := connectorEnrollmentRequest{
		Kind: connectorEnrollmentKind, Name: "qURL CLI registered device",
		Target: agentEnrollmentTarget, ExpiresIn: connectorEnrollmentLifetime,
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", opts.IdempotencyKey)
	reply, err := c.doRESTWithHeaders(ctx, http.MethodPost, "/v1/api-keys", body, headers)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusCreated {
		problem := reply.problem()
		var apiErr *Error
		if errors.As(problem, &apiErr) && strings.EqualFold(apiErr.Code, "insufficient_scope") {
			apiErr.enrollmentScopeRequired = true
		}
		return nil, problem
	}
	var env struct {
		Data connectorEnrollmentData `json:"data"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode agent enrollment response: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if err := validateAgentEnrollmentResponse(&env.Data, time.Now()); err != nil {
		return nil, err
	}
	return &AgentEnrollmentToken{Token: env.Data.Token, KeyID: env.Data.KeyID, ExpiresAt: *env.Data.ExpiresAt}, nil
}

// MintConnectorEnrollmentToken mints the least-privilege enrollment shape:
// target=connector, one exact connector claim, no caller-selected scopes, and
// the service's maximum 24-hour lifetime. The returned secret is never
// included in validation errors.
func (c *client) MintConnectorEnrollmentToken(ctx context.Context, opts MintConnectorEnrollmentTokenOptions) (*ConnectorEnrollmentToken, error) {
	if err := validateConnectorEnrollmentOptions(opts); err != nil {
		return nil, err
	}

	body := connectorEnrollmentRequest{
		Kind:   connectorEnrollmentKind,
		Name:   "qURL CLI Connector " + opts.ConnectorID,
		Target: connectorEnrollmentTarget,
		Claims: []connectorEnrollmentClaim{{
			Type: connectorEnrollmentTarget,
			ID:   opts.ConnectorID,
		}},
		ExpiresIn: connectorEnrollmentLifetime,
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", opts.IdempotencyKey)
	reply, err := c.doRESTWithHeaders(ctx, http.MethodPost, "/v1/api-keys", body, headers)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusCreated {
		problem := reply.problem()
		var apiErr *Error
		if errors.As(problem, &apiErr) && strings.EqualFold(apiErr.Code, "insufficient_scope") {
			apiErr.enrollmentScopeRequired = true
		}
		return nil, problem
	}

	var env struct {
		Data connectorEnrollmentData `json:"data"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode connector enrollment response: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if err := validateConnectorEnrollmentResponse(&env.Data, opts.ConnectorID, time.Now()); err != nil {
		return nil, err
	}
	return &ConnectorEnrollmentToken{
		Token:     env.Data.Token,
		KeyID:     env.Data.KeyID,
		ExpiresAt: *env.Data.ExpiresAt,
	}, nil
}

func validateConnectorEnrollmentOptions(opts MintConnectorEnrollmentTokenOptions) error {
	if strings.TrimSpace(opts.ConnectorID) == "" {
		return fmt.Errorf("%w: connector ID must not be empty", qurl.ErrInvalidResourceRequest)
	}
	return validateEnrollmentIdempotencyKey(opts.IdempotencyKey)
}

func validateEnrollmentIdempotencyKey(value string) error {
	if len(value) < minIdempotencyKeyLength || len(value) > maxIdempotencyKeyLength {
		return fmt.Errorf("%w: idempotency key must be between %d and %d characters", qurl.ErrInvalidResourceRequest, minIdempotencyKeyLength, maxIdempotencyKeyLength)
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%w: idempotency key must be a valid single-line HTTP header value", qurl.ErrInvalidResourceRequest)
	}
	return nil
}

func validateAgentEnrollmentResponse(data *connectorEnrollmentData, now time.Time) error {
	invalid := func(detail string) error {
		return fmt.Errorf("%w: agent enrollment response %s", qurl.ErrInvalidAPIResponse, detail)
	}
	if auth.ValidateKeyShape(data.Token) != nil {
		return invalid("has a missing or malformed token")
	}
	if strings.TrimSpace(data.KeyID) == "" || strings.TrimSpace(data.KeyID) != data.KeyID {
		return invalid("has a missing or malformed key ID")
	}
	if data.Kind != connectorEnrollmentKind || data.Target != agentEnrollmentTarget || len(data.Claims) != 0 {
		return invalid("does not carry the exact unbound agent authority")
	}
	if data.Status != connectorEnrollmentStatus {
		return invalid("is not active")
	}
	// This checks only that the one-shot credential is still usable; it does
	// not infer or cap producer lifetime from the client clock. The service's
	// 24-hour enrollment lifetime is the current clock-skew budget. Revisit the
	// policy explicitly if that lifetime is shortened.
	if data.ExpiresAt == nil || data.ExpiresAt.IsZero() || !data.ExpiresAt.After(now) {
		return invalid("has no live expiry")
	}
	return nil
}

func validateConnectorEnrollmentResponse(data *connectorEnrollmentData, connectorID string, now time.Time) error {
	// TODO(upstream-contract): Keep these response-envelope checks in lockstep
	// with qurl-service's Connector enrollment response contract.
	invalid := func(detail string) error {
		return fmt.Errorf("%w: connector enrollment response %s", qurl.ErrInvalidAPIResponse, detail)
	}
	if auth.ValidateKeyShape(data.Token) != nil {
		return invalid("has a missing or malformed token")
	}
	if strings.TrimSpace(data.KeyID) == "" || strings.TrimSpace(data.KeyID) != data.KeyID {
		return invalid("has a missing or malformed key ID")
	}
	if data.Kind != connectorEnrollmentKind {
		return invalid("has an unexpected kind")
	}
	if data.Target != connectorEnrollmentTarget {
		return invalid("has an unexpected target")
	}
	if len(data.Claims) != 1 || data.Claims[0].Type != connectorEnrollmentTarget || data.Claims[0].ID != connectorID {
		return invalid("does not carry the exact Connector claim")
	}
	if data.Status != connectorEnrollmentStatus {
		return invalid("is not active")
	}
	// As above, do not infer producer lifetime from the client clock. The
	// service's 24-hour enrollment lifetime is the current skew budget.
	if data.ExpiresAt == nil || data.ExpiresAt.IsZero() {
		return invalid("missing expiry")
	}
	if !data.ExpiresAt.After(now) {
		return invalid("is already expired")
	}
	return nil
}
