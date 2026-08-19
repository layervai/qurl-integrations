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
)

const (
	connectorEnrollmentKind     = "enrollment_token"
	connectorEnrollmentTarget   = "connector"
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

// ConnectorEnrollmentToken is the one-shot credential returned for native
// Connector enrollment. Token is plaintext secret material and must remain
// in memory; callers must never put it in argv, logs, environment variables,
// or durable state.
type ConnectorEnrollmentToken struct {
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
	Claims    []connectorEnrollmentClaim `json:"claims"`
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
			apiErr.connectorEnrollmentScopeRequired = true
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
	if len(opts.IdempotencyKey) < minIdempotencyKeyLength || len(opts.IdempotencyKey) > maxIdempotencyKeyLength {
		return fmt.Errorf("%w: idempotency key must be between %d and %d characters", qurl.ErrInvalidResourceRequest, minIdempotencyKeyLength, maxIdempotencyKeyLength)
	}
	if strings.TrimSpace(opts.IdempotencyKey) != opts.IdempotencyKey || strings.ContainsAny(opts.IdempotencyKey, "\r\n") {
		return fmt.Errorf("%w: idempotency key must be a valid single-line HTTP header value", qurl.ErrInvalidResourceRequest)
	}
	return nil
}

func validateConnectorEnrollmentResponse(data *connectorEnrollmentData, connectorID string, now time.Time) error {
	invalid := func(detail string) error {
		return fmt.Errorf("%w: connector enrollment response %s", qurl.ErrInvalidAPIResponse, detail)
	}
	if strings.TrimSpace(data.Token) == "" || strings.TrimSpace(data.Token) != data.Token {
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
	if data.ExpiresAt == nil || data.ExpiresAt.IsZero() {
		return invalid("missing expiry")
	}
	if !data.ExpiresAt.After(now) {
		return invalid("is already expired")
	}
	return nil
}
