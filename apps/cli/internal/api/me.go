package qurlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/layervai/qurl-go/qurl"
)

// Identity is the repo-owned result of Me: who the configured credential is,
// as echoed by the platform. It deliberately carries no plan or usage data —
// the identity endpoint answers from authentication state alone, so it is
// cheap enough for login validation and whoami to call freely.
type Identity struct {
	// OwnerID identifies the account the credential belongs to.
	OwnerID string
	// AuthType is how the request authenticated ("api_key" for CLI keys).
	AuthType string
	// Key identifies the API key itself; nil when the platform omitted the
	// block (non-key authentication).
	Key *KeyIdentity
}

// KeyIdentity is the non-secret identity of an API key.
type KeyIdentity struct {
	KeyID string
	// Kind is the credential kind ("api_key" for durable customer keys).
	// Unknown future values pass through untouched.
	Kind string
	// Scopes come back in alphabetical order (a platform contract).
	Scopes []string
	// KeyPrefix is the non-secret leading portion (e.g. "lv_live_a3x9").
	KeyPrefix string
	// ExpiresAt is nil for non-expiring keys.
	ExpiresAt *time.Time
}

// meData mirrors the GET /v1/me response document this CLI consumes.
type meData struct {
	OwnerID  string `json:"owner_id"`
	AuthType string `json:"auth_type"`
	APIKey   *struct {
		KeyID     string     `json:"key_id"`
		Kind      string     `json:"kind"`
		Scopes    []string   `json:"scopes"`
		KeyPrefix string     `json:"key_prefix"`
		ExpiresAt *time.Time `json:"expires_at"`
	} `json:"api_key"`
}

// Me asks the platform who the configured credential is (GET /v1/me). Login
// uses it to validate a key before storing anything; whoami renders it.
func (c *client) Me(ctx context.Context) (*Identity, error) {
	reply, err := c.doREST(ctx, http.MethodGet, "/v1/me", nil)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}
	var env struct {
		Data meData `json:"data"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode identity response: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if strings.TrimSpace(env.Data.OwnerID) == "" {
		return nil, fmt.Errorf("%w: identity response missing owner_id", qurl.ErrInvalidAPIResponse)
	}
	id := &Identity{OwnerID: env.Data.OwnerID, AuthType: env.Data.AuthType}
	if k := env.Data.APIKey; k != nil {
		id.Key = &KeyIdentity{
			KeyID:     k.KeyID,
			Kind:      k.Kind,
			Scopes:    k.Scopes,
			KeyPrefix: k.KeyPrefix,
			ExpiresAt: k.ExpiresAt,
		}
	}
	return id, nil
}
