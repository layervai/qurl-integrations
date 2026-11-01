package qurlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
)

// APIKeyManagement uses one in-memory browser account token, never device auth.
type APIKeyManagement interface {
	ListAPIKeys(context.Context, APIKeyListOptions) (*APIKeyPage, error)
	CreateAccountAPIKey(context.Context, string, []string, string) (*CreatedAPIKey, error)
	UpdateAccountAPIKey(context.Context, string, string) (*APIKeyMetadata, error)
	RevokeAccountAPIKey(context.Context, string) error
}

// NewAPIKeyManagement reuses the CLI transport with explicit account authority.
func NewAPIKeyManagement(cfg *Config) (APIKeyManagement, error) {
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return c.(*client), nil
}

// APIKeyMetadata excludes secrets, including unexpected secrets in list replies.
// TODO(upstream-contract): qurl-service api/openapi.yaml owns credential kinds and pagination.
type APIKeyMetadata struct {
	KeyID      string   `json:"key_id"`
	KeyPrefix  string   `json:"key_prefix,omitempty"`
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	Status     string   `json:"status"`
	CreatedAt  string   `json:"created_at,omitempty"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
}

// CreatedAPIKey carries the secret returned only by creation and its idempotent replay.
type CreatedAPIKey struct {
	APIKeyMetadata
	Secret string `json:"api_key"`
}

// APIKeyListOptions selects one bounded credential page.
type APIKeyListOptions struct {
	Limit          int
	Cursor, Status string
}

// APIKeyPage preserves the service cursor without loading further pages.
type APIKeyPage struct {
	Keys       []APIKeyMetadata `json:"data"`
	NextCursor string           `json:"next_cursor,omitempty"`
	HasMore    bool             `json:"has_more"`
}

var accountKeyIDPattern = regexp.MustCompile(`^key_[A-Za-z0-9]{12}$`)

// ValidateAccountKeyID rejects secrets and path syntax before any account request.
func ValidateAccountKeyID(id string) error {
	if !accountKeyIDPattern.MatchString(id) {
		return fmt.Errorf("%w: key ID must be key_ followed by 12 letters or digits", qurl.ErrInvalidResourceRequest)
	}
	return nil
}

// ValidateAccountKeyName checks the service length bound and printable CLI names.
// TODO(upstream-contract): current APIKeyService counts UTF-8 bytes, despite schema maxLength.
func ValidateAccountKeyName(name string) error {
	if !utf8.ValidString(name) || strings.TrimSpace(name) == "" || len(name) > 100 || strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return fmt.Errorf("%w: key name must contain printable text within 100 UTF-8 bytes", qurl.ErrInvalidResourceRequest)
	}
	return nil
}

// ValidateAccountKeyScopes requires explicit, distinct supported permissions.
func ValidateAccountKeyScopes(scopes []string) error {
	if len(scopes) == 0 {
		return fmt.Errorf("%w: supply at least one scope", qurl.ErrInvalidResourceRequest)
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		switch scope {
		case "qurl:read", "qurl:write", "qurl:resolve", "qurl:agent", "qurl:keys":
		default:
			return fmt.Errorf("%w: unsupported API key scope", qurl.ErrInvalidResourceRequest)
		}
		if seen[scope] {
			return fmt.Errorf("%w: duplicate API key scope", qurl.ErrInvalidResourceRequest)
		}
		seen[scope] = true
	}
	return nil
}

// ListAPIKeys reads one page and discards any unexpected plaintext secrets.
func (c *client) ListAPIKeys(ctx context.Context, opts APIKeyListOptions) (*APIKeyPage, error) {
	if err := ValidateAccountKeyListOptions(opts); err != nil {
		return nil, err
	}
	query := url.Values{"limit": {strconv.Itoa(opts.Limit)}, "status": {opts.Status}}
	if opts.Cursor != "" {
		query.Set("cursor", opts.Cursor)
	}
	reply, err := c.doREST(ctx, http.MethodGet, "/v1/api-keys?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}
	var envelope struct {
		Data []APIKeyMetadata `json:"data"`
		Meta *struct {
			NextCursor string `json:"next_cursor"`
			HasMore    *bool  `json:"has_more"`
		} `json:"meta"`
	}
	if json.Unmarshal(reply.body, &envelope) != nil || envelope.Data == nil || len(envelope.Data) > opts.Limit || envelope.Meta == nil || envelope.Meta.HasMore == nil || (*envelope.Meta.HasMore && envelope.Meta.NextCursor == "") {
		return nil, invalidAPIKeyReply()
	}
	for i := range envelope.Data {
		if !validAccountKeyMetadata(&envelope.Data[i]) {
			return nil, invalidAPIKeyReply()
		}
	}
	return &APIKeyPage{Keys: envelope.Data, NextCursor: envelope.Meta.NextCursor, HasMore: *envelope.Meta.HasMore}, nil
}

// CreateAccountAPIKey binds retries to the same caller-owned creation identity.
func (c *client) CreateAccountAPIKey(ctx context.Context, name string, scopes []string, idempotencyKey string) (*CreatedAPIKey, error) {
	if err := ValidateAccountKeyCreation(name, scopes, idempotencyKey); err != nil {
		return nil, err
	}
	body := struct {
		Kind   string   `json:"kind"`
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}{"api_key", name, scopes}
	reply, err := c.doRESTWithHeaders(ctx, http.MethodPost, "/v1/api-keys", body, http.Header{"Idempotency-Key": {idempotencyKey}})
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusCreated {
		return nil, reply.problem()
	}
	var envelope struct {
		Data *CreatedAPIKey `json:"data"`
	}
	if json.Unmarshal(reply.body, &envelope) != nil || envelope.Data == nil || !validAccountKeyMetadata(&envelope.Data.APIKeyMetadata) || envelope.Data.Kind != "api_key" || auth.ValidateKeyShape(envelope.Data.Secret) != nil || envelope.Data.Status != accountActiveStatus || envelope.Data.Name != name || !sameAccountKeyScopes(envelope.Data.Scopes, scopes) {
		return nil, invalidAPIKeyReply()
	}
	return envelope.Data, nil
}

// UpdateAccountAPIKey renames only the selected credential without transport replay.
func (c *client) UpdateAccountAPIKey(ctx context.Context, id, name string) (*APIKeyMetadata, error) {
	if err := ValidateAccountKeyID(id); err != nil {
		return nil, err
	}
	if err := ValidateAccountKeyName(name); err != nil {
		return nil, err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPatch, "/v1/api-keys/"+id, map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}
	var envelope struct {
		Data *APIKeyMetadata `json:"data"`
	}
	if json.Unmarshal(reply.body, &envelope) != nil || envelope.Data == nil || !validAccountKeyMetadata(envelope.Data) || envelope.Data.KeyID != id || envelope.Data.Name != name {
		return nil, invalidAPIKeyReply()
	}
	return envelope.Data, nil
}

// RevokeAccountAPIKey accepts only the service-confirmed 204 result.
func (c *client) RevokeAccountAPIKey(ctx context.Context, id string) error {
	if err := ValidateAccountKeyID(id); err != nil {
		return err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodDelete, "/v1/api-keys/"+id, nil)
	if err != nil {
		return err
	}
	if reply.status != http.StatusNoContent {
		return reply.problem()
	}
	return nil
}

func validAccountKeyMetadata(key *APIKeyMetadata) bool {
	return accountKeyIDPattern.MatchString(key.KeyID) && key.Kind != "" && key.Status != ""
}
func invalidAPIKeyReply() error {
	return fmt.Errorf("%w: API key response is incomplete or does not match the request", qurl.ErrInvalidAPIResponse)
}

// ValidateAccountKeyCreation checks an invocation before browser sign-in.
func ValidateAccountKeyCreation(name string, scopes []string, idempotencyKey string) error {
	if err := ValidateAccountKeyName(name); err != nil {
		return err
	}
	if err := ValidateAccountKeyScopes(scopes); err != nil {
		return err
	}
	return validateEnrollmentIdempotencyKey(idempotencyKey)
}

// ValidateAccountKeyListOptions checks pagination before browser sign-in.
func ValidateAccountKeyListOptions(opts APIKeyListOptions) error {
	if opts.Limit < 1 || opts.Limit > 100 {
		return fmt.Errorf("%w: limit must be from 1 to 100", qurl.ErrInvalidResourceRequest)
	}
	switch opts.Status {
	case accountActiveStatus, "revoked", "all":
	default:
		return fmt.Errorf("%w: status must be active, revoked or all", qurl.ErrInvalidResourceRequest)
	}

	return nil
}

func sameAccountKeyScopes(a, b []string) bool {
	a = slices.Clone(a)
	b = slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
