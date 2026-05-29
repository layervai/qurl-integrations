// Package client provides a Go client for the qURL API.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout    = 30 * time.Second
	defaultMaxRetries = 3
	defaultBaseDelay  = 500 * time.Millisecond
	defaultMaxDelay   = 30 * time.Second
)

// HeaderIdempotencyKey is the request header the qURL API reads to dedupe
// retried writes (the partition key on `qurl-service`'s
// idempotency-store table is `hash(owner_id:idempotency_key:method:path)`).
//
// Callers don't set this header directly — pass IdempotencyKey on the
// per-request input struct (currently CreateInput.IdempotencyKey) and the
// client wires the header. The constant is exported only so observability
// code can grep for it consistently across logs and request inspectors.
//
// Key construction guidance (apply to every caller — not just Slack):
//
//   - **Hash identifiers, never pass them raw.** The key transits as a
//     request header and lands in CloudWatch / ALB access logs / any
//     intermediary observer. For Slack the canonical idiom is
//     `sha256("slack:" + team_id + ":" + trigger_id)` hex-encoded — 64
//     ASCII bytes, well under the cap, reveals no PII.
//   - **Stay within the printable-ASCII byte range** (0x20-0x7E + tab) —
//     `Create` rejects anything else with `ErrIdempotencyKeyInvalid` to
//     dodge encoding ambiguity in the upstream key hash.
const HeaderIdempotencyKey = "Idempotency-Key"

// MaxIdempotencyKeyLength is the byte cap mirrored from qurl-service's
// idempotency-store schema (see `idempotency_dynamodb.go`). Since the
// validator rejects ≥0x80, accepted keys are pure ASCII — bytes equal
// runes by construction.
const MaxIdempotencyKeyLength = 256

// ErrIdempotencyKeyTooLong is returned when an idempotency key exceeds
// MaxIdempotencyKeyLength bytes.
var ErrIdempotencyKeyTooLong = errors.New("idempotency key exceeds 256 bytes")

// ErrIdempotencyKeyInvalid is returned by Create when CreateInput.IdempotencyKey
// contains bytes that aren't valid in an HTTP header value: CR/LF/NUL/control
// bytes, non-ASCII bytes, OR leading/trailing whitespace. The first three
// would otherwise cause Go's HTTP transport to reject with an opaque
// `net/http: invalid header field value`; the whitespace case is more
// subtle — RFC 7230's OWS rule trims leading/trailing space and tab on
// the wire, so the upstream dedup hash would silently see a key
// different from what the caller passed. Either way, fail-fast with a
// typed sentinel.
var ErrIdempotencyKeyInvalid = errors.New("idempotency key contains invalid bytes (CR/LF/NUL/control, non-ASCII, or leading/trailing whitespace)")

// --- Input-validation sentinels for Create / *Resource methods.
//
// These are exported so callers can branch with `errors.Is(err, ErrFoo)`
// rather than substring-matching the error text. Pattern matches the
// existing ErrIdempotencyKey* sentinels above.

// ErrCreateRequiresTarget is returned by Create when both TargetURL and
// ResourceID are empty — the qURL has no target to bind to.
var ErrCreateRequiresTarget = errors.New("create: target_url or resource_id required")

// ErrCreateTargetResourceExclusive is returned by Create when both TargetURL
// and ResourceID are populated. The server-side `mutually_exclusive_fields`
// rule rejects this combination; the client fails fast for a typed error.
var ErrCreateTargetResourceExclusive = errors.New("create: target_url and resource_id are mutually exclusive")

// ErrCreateResourceNilInput is returned by CreateResource when input is nil.
var ErrCreateResourceNilInput = errors.New("create resource: input is nil")

// ErrCreateResourceRequiresTargetURL is returned by CreateResource when
// CreateResourceInput.TargetURL is empty — without it the server can't
// compute the `(owner_id, target_url_hash)` idempotency key.
var ErrCreateResourceRequiresTargetURL = errors.New("create resource: target_url required")

// ErrCreateResourceTunnelRejectsTargetURL is returned by CreateResource
// when Type is "tunnel" but TargetURL is non-empty. The server ignores
// TargetURL on tunnel creates, but a non-empty value is almost always
// a stale field from copy-pasted CreateResourceInput literals — fail
// fast for a clearer error than a silent server discard.
var ErrCreateResourceTunnelRejectsTargetURL = errors.New("create resource: type=tunnel must not set target_url (server ignores it)")

// ErrCreateAPIKeyNilInput is returned by CreateAPIKey when input is nil.
var ErrCreateAPIKeyNilInput = errors.New("create api key: input is nil")

// ErrRevokeAPIKeyEmptyID is returned by RevokeAPIKey when keyID is empty.
var ErrRevokeAPIKeyEmptyID = errors.New("revoke api key: key_id is empty")

// ErrUpdateResourceEmptyID is returned by UpdateResource when resourceID
// is the empty string.
var ErrUpdateResourceEmptyID = errors.New("update resource: resource_id is empty")

// ErrUpdateResourceNilInput is returned by UpdateResource when input is nil.
var ErrUpdateResourceNilInput = errors.New("update resource: input is nil")

// ErrUpdateResourceAliasEmpty is returned by UpdateResource when
// UpdateResourceInput.Alias is a pointer to the empty string. The empty
// string is reserved as a footgun guard — the server's
// `^[a-z][a-z0-9-]{1,62}[a-z0-9]$` regex rejects it; use ClearAlias=true.
var ErrUpdateResourceAliasEmpty = errors.New("update resource: alias must not be a pointer to empty string; use ClearAlias=true to clear")

// ErrUpdateResourceAliasClearExclusive is returned by UpdateResource when
// both Alias != nil and ClearAlias=true. Server-side returns 400; client
// fails fast for a typed error.
var ErrUpdateResourceAliasClearExclusive = errors.New("update resource: alias and clear_alias are mutually exclusive")

// ErrUpdateResourceNoFieldsSet is returned by UpdateResource when input
// is non-nil but has no fields populated — the request would PATCH `{}`
// and either no-op or 400 server-side. Symmetric with the no-target
// guard on Create.
var ErrUpdateResourceNoFieldsSet = errors.New("update resource: input has no fields set")

// ErrGetResourceEmptyID is returned by GetResource when the resource ID
// is the empty string.
var ErrGetResourceEmptyID = errors.New("get resource: resource id is empty")

// StatusActive indicates the qURL is live and accepting access requests.
const StatusActive = "active"

// StatusExpired indicates the qURL's TTL has elapsed.
const StatusExpired = "expired"

// StatusRevoked indicates the qURL was manually revoked (deleted).
const StatusRevoked = "revoked"

// StatusConsumed indicates a one-time qURL has been used.
const StatusConsumed = "consumed"

// --- Resource type constants (mirrors qurl-service/api/openapi.yaml
// `ResourceType` enum).

// ResourceTypeURL is the target-URL proxy type (default).
const ResourceTypeURL = "url"

// ResourceTypeTunnel is the FRP-backed reverse-tunnel type.
const ResourceTypeTunnel = "tunnel"

// APIKeyPurposeTunnelBootstrap is the restricted key purpose used by the
// Docker reverse-tunnel onboarding flow.
const APIKeyPurposeTunnelBootstrap = "tunnel_bootstrap"

// Logger is an optional interface for debug logging.
type Logger interface {
	Printf(format string, args ...any)
}

// Client is a qURL API client.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	userAgent  string
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
	logger     Logger
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.httpClient = c }
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// WithRetry sets the maximum number of retries for transient errors (429, 5xx).
// Set to 0 to disable retries.
func WithRetry(maxRetries int) Option {
	return func(c *Client) { c.maxRetries = maxRetries }
}

// WithLogger enables debug logging of HTTP requests and responses.
func WithLogger(l Logger) Option {
	return func(c *Client) { c.logger = l }
}

// New creates a qURL API client.
func New(baseURL, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		userAgent:  "qurl-go-client/dev",
		maxRetries: defaultMaxRetries,
		baseDelay:  defaultBaseDelay,
		maxDelay:   defaultMaxDelay,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Client) logf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Printf(format, args...)
	}
}

// --- Response envelope ---

// apiResponse is the success response envelope from the qURL API.
type apiResponse struct {
	Data json.RawMessage `json:"data"`
	Meta *ResponseMeta   `json:"meta,omitempty"`
}

// ResponseMeta holds response metadata.
type ResponseMeta struct {
	RequestID  string `json:"request_id,omitempty"`
	PageSize   int    `json:"page_size,omitempty"`
	HasMore    bool   `json:"has_more,omitempty"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// --- qURL types (match API schema) ---

// QURL represents a qURL resource as returned by the API.
type QURL struct {
	ResourceID   string        `json:"resource_id"`
	TargetURL    string        `json:"target_url"`
	Status       string        `json:"status"`
	CreatedAt    time.Time     `json:"created_at"`
	ExpiresAt    *time.Time    `json:"expires_at,omitempty"`
	OneTimeUse   bool          `json:"one_time_use"`
	MaxSessions  int           `json:"max_sessions,omitempty"`
	Description  string        `json:"description,omitempty"`
	QURLSite     string        `json:"qurl_site,omitempty"`
	QURLLink     string        `json:"qurl_link,omitempty"`
	AccessPolicy *AccessPolicy `json:"access_policy,omitempty"`
}

// AccessPolicy defines access restrictions for a qURL.
//
// All subfields MUST keep `omitempty` — `&AccessPolicy{}` is the
// documented "clear policy" signal in [UpdateResourceInput], and that
// contract relies on every field eliding from the JSON payload when
// zero. Adding a non-omitempty field (or one that doesn't elide on
// zero, like a non-pointer time) silently breaks the clear convention.
type AccessPolicy struct {
	IPAllowlist  []string `json:"ip_allowlist,omitempty"`
	IPDenylist   []string `json:"ip_denylist,omitempty"`
	GeoAllowlist []string `json:"geo_allowlist,omitempty"`
	GeoDenylist  []string `json:"geo_denylist,omitempty"`
}

// CreateInput is the input for creating a qURL.
//
// Either TargetURL or ResourceID must be supplied — never both. Server-side
// validation enforces a `mutually_exclusive_fields` rule (per Phase 3a.3:
// `resource_id` is mutually exclusive with `target_url` and explicit `type`).
// The `,omitempty` JSON tag on TargetURL is load-bearing: it lets the
// ResourceID-only flow ship a request body that omits `target_url` entirely
// rather than serializing the zero value `""`, which would otherwise trip the
// server's exclusivity check.
type CreateInput struct {
	TargetURL string `json:"target_url,omitempty"`
	// ResourceID, when set, mints a qURL bound to an existing resource
	// (e.g. a tunnel resource resolved via GetResource). Mutually
	// exclusive with TargetURL on the wire.
	ResourceID   string        `json:"resource_id,omitempty"`
	Description  string        `json:"description,omitempty"`
	ExpiresIn    string        `json:"expires_in,omitempty"`
	OneTimeUse   bool          `json:"one_time_use,omitempty"`
	MaxSessions  int           `json:"max_sessions,omitempty"`
	AccessPolicy *AccessPolicy `json:"access_policy,omitempty"`
	// Reason is forwarded to the audit log when set (e.g. an
	// operator-supplied "for incident #123" annotation from the
	// `/qurl get $alias reason:"…"` slash-command flag). The server
	// writes this to the audit row only; it is not persisted on the
	// resulting qURL.
	Reason string `json:"reason,omitempty"`

	// IdempotencyKey, when non-empty, is sent as the Idempotency-Key
	// request header so the API dedupes retried writes. See
	// [HeaderIdempotencyKey] for key-construction guidance.
	IdempotencyKey string `json:"-"`
}

// CreateOutput is the response from creating a qURL.
type CreateOutput struct {
	ResourceID string     `json:"resource_id"`
	QURLLink   string     `json:"qurl_link"`
	QURLSite   string     `json:"qurl_site"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// isHeaderSafeASCIIByte reports whether b is safe to ship in a header
// value: tab (0x09) plus visible ASCII (0x20-0x7E). Rejects CR/LF/NUL,
// DEL (0x7F), other control bytes, and all non-ASCII (≥0x80) — the
// upstream qURL API hashes the raw bytes for its dedup partition key, so
// we constrain to the unambiguous-encoding subset.
func isHeaderSafeASCIIByte(b byte) bool {
	return b == '\t' || (b >= 0x20 && b <= 0x7E)
}

// validateIdempotencyKey enforces the constraints documented on
// HeaderIdempotencyKey: ≤MaxIdempotencyKeyLength bytes and header-safe
// ASCII only, with no leading/trailing whitespace (RFC 7230 OWS would
// trim those on the wire, silently changing the upstream dedup hash).
// Empty key is valid (means "no header sent"). Extracted as a helper
// so future write methods that need idempotency (#148) can reuse the
// same contract.
func validateIdempotencyKey(key string) error {
	if len(key) > MaxIdempotencyKeyLength {
		return ErrIdempotencyKeyTooLong
	}
	if key != "" {
		first, last := key[0], key[len(key)-1]
		if first == ' ' || first == '\t' || last == ' ' || last == '\t' {
			return ErrIdempotencyKeyInvalid
		}
	}
	for i := range len(key) {
		if !isHeaderSafeASCIIByte(key[i]) {
			return ErrIdempotencyKeyInvalid
		}
	}
	return nil
}

// Create creates a new qURL.
//
// Exactly one of input.TargetURL or input.ResourceID must be set. The
// client routes to a different qURL service endpoint per case:
//
//   - TargetURL → POST /v1/qurls (CreateQurl). The handler accepts
//     target_url in the request body and auto-creates the underlying
//     resource if one doesn't already exist for that URL.
//   - ResourceID → POST /v1/resources/{id}/qurls (CreateQurlForResource).
//     The resource id rides in the URL path; the body is the policy
//     subset (no target_url, no resource_id). The /v1/qurls handler
//     does NOT accept resource_id in its body — a body-keyed call
//     surfaces as 400 invalid_target_url because target_url is
//     required for that endpoint.
//
// Note: Create takes a value receiver for backward-compat with the existing
// callers in apps/slack and apps/cli; the new methods ([Client.CreateResource],
// [Client.UpdateResource]) take *Input pointers, which is the going-forward
// idiom. Pointer migration tracked at #146.
//
//nolint:gocritic // hugeParam: CreateInput is 104 bytes; *CreateInput migration tracked at #146.
func (c *Client) Create(ctx context.Context, input CreateInput) (*CreateOutput, error) {
	if input.TargetURL == "" && input.ResourceID == "" {
		return nil, ErrCreateRequiresTarget
	}
	if input.TargetURL != "" && input.ResourceID != "" {
		return nil, ErrCreateTargetResourceExclusive
	}
	if err := validateIdempotencyKey(input.IdempotencyKey); err != nil {
		return nil, err
	}

	var (
		endpoint string
		logLabel string
		body     []byte
		err      error
	)
	if input.ResourceID != "" {
		// /v1/resources/{id}/qurls — id rides in the path; the body
		// drops target_url + resource_id and ships the policy subset.
		// Reason ships in the body too (silently dropped by the server
		// if not in its schema; harmless either way and matches the
		// URL-form posture).
		endpoint = c.baseURL + "/v1/resources/" + url.PathEscape(input.ResourceID) + "/qurls"
		logLabel = "POST /v1/resources/:id/qurls"
		body, err = json.Marshal(createForResourceBody{
			Description:  input.Description,
			ExpiresIn:    input.ExpiresIn,
			OneTimeUse:   input.OneTimeUse,
			MaxSessions:  input.MaxSessions,
			AccessPolicy: input.AccessPolicy,
			Reason:       input.Reason,
		})
	} else {
		endpoint = c.baseURL + "/v1/qurls"
		logLabel = "POST /v1/qurls"
		body, err = json.Marshal(input)
	}
	if err != nil {
		return nil, fmt.Errorf("marshal create input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Set Idempotency-Key here (not in do()) because it's request-specific,
	// not transport-wide — Content-Type/Authorization/User-Agent are
	// set in do() because they apply to every request. Either way the
	// header survives retries since do() reuses the same *http.Request.
	if input.IdempotencyKey != "" {
		req.Header.Set(HeaderIdempotencyKey, input.IdempotencyKey)
	}

	var out CreateOutput
	if _, err := c.do(req, &out, logLabel); err != nil {
		return nil, err
	}
	return &out, nil
}

// createForResourceBody is the wire shape for `POST
// /v1/resources/{id}/qurls`. Mirrors the qURL service's
// `CreateQurlForResourceRequest` schema (resource id rides in the URL
// path, so it doesn't repeat in the body; target_url is absent for
// the same reason — the resource already owns it).
type createForResourceBody struct {
	Description  string        `json:"description,omitempty"`
	ExpiresIn    string        `json:"expires_in,omitempty"`
	OneTimeUse   bool          `json:"one_time_use,omitempty"`
	MaxSessions  int           `json:"max_sessions,omitempty"`
	AccessPolicy *AccessPolicy `json:"access_policy,omitempty"`
	Reason       string        `json:"reason,omitempty"`
}

// Get retrieves a qURL by ID.
func (c *Client) Get(ctx context.Context, id string) (*QURL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/qurls/"+url.PathEscape(id), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var qurl QURL
	if _, err := c.do(req, &qurl, "GET /v1/qurls/:id"); err != nil {
		return nil, err
	}
	return &qurl, nil
}

// ListInput is the input for listing qURLs.
type ListInput struct {
	Limit  int
	Cursor string
	Status string
	Query  string
	Sort   string
}

// ListOutput is the output of listing qURLs.
type ListOutput struct {
	QURLs      []QURL `json:"qurls"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more,omitempty"`
}

// List retrieves a paginated list of qURLs.
func (c *Client) List(ctx context.Context, input ListInput) (*ListOutput, error) {
	params := url.Values{}
	if input.Limit > 0 {
		params.Set("limit", strconv.Itoa(input.Limit))
	}
	if input.Cursor != "" {
		params.Set("cursor", input.Cursor)
	}
	if input.Status != "" {
		params.Set("status", input.Status)
	}
	if input.Query != "" {
		params.Set("q", input.Query)
	}
	if input.Sort != "" {
		params.Set("sort", input.Sort)
	}

	u := c.baseURL + "/v1/qurls"
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var qurls []QURL
	meta, err := c.do(req, &qurls, "GET /v1/qurls")
	if err != nil {
		return nil, err
	}

	out := &ListOutput{QURLs: qurls}
	if meta != nil {
		out.NextCursor = meta.NextCursor
		out.HasMore = meta.HasMore
	}
	return out, nil
}

// Delete revokes a qURL by ID.
func (c *Client) Delete(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/qurls/"+url.PathEscape(id), http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	_, err = c.do(req, nil, "DELETE /v1/qurls/:id")
	return err
}

// ExtendInput holds input for extending a qURL.
type ExtendInput struct {
	ExtendBy  string     `json:"extend_by,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Extend extends a qURL's expiration.
// Both Extend and Update use PATCH /v1/qurls/:id — the server differentiates
// by request body fields (extend_by/expires_at vs description).
func (c *Client) Extend(ctx context.Context, id string, input ExtendInput) (*QURL, error) {
	return c.patchQURL(ctx, id, input)
}

// UpdateInput holds input for updating a qURL's mutable properties.
type UpdateInput struct {
	Description *string `json:"description,omitempty"`
}

// Update updates a qURL's mutable properties.
func (c *Client) Update(ctx context.Context, id string, input UpdateInput) (*QURL, error) {
	return c.patchQURL(ctx, id, input)
}

// patchQURL sends a PATCH request to /v1/qurls/:id with the given body.
func (c *Client) patchQURL(ctx context.Context, id string, input any) (*QURL, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal patch input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+"/v1/qurls/"+url.PathEscape(id), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var qurl QURL
	if _, err := c.do(req, &qurl, "PATCH /v1/qurls/:id"); err != nil {
		return nil, err
	}
	return &qurl, nil
}

// MintOutput holds the result of minting an access link.
type MintOutput struct {
	QURLLink  string     `json:"qurl_link"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// MintLink mints a new access link for a qURL.
func (c *Client) MintLink(ctx context.Context, id string) (*MintOutput, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/qurls/"+url.PathEscape(id)+"/mint_link", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var out MintOutput
	if _, err := c.do(req, &out, "POST /v1/qurls/:id/mint_link"); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Resources ---
//
// These methods target the post-PR-3a.2 surface in qurl-service: alias
// fields on `Resource` and `clear_alias` on `PATCH /v1/resources/{id}`.
// The OpenAPI schema for
// PR-3a.2 has not shipped at the time this client method set lands — they
// are added here so the Slack `setalias`/`get` flows in PR-3c.3+ have a
// stable Go surface to call against. Until PR-3a.2 ships in qurl-service,
// these methods will return 404/400 from the API.

// Resource represents a qURL resource (the durable object behind a qURL link).
//
// Field shapes mirror the `ResourceData` schema in
// `qurl-service/api/openapi.yaml` post-PR-3a.2; consumers should treat
// unknown fields as best-effort. Fields not yet on the OpenAPI surface
// (Alias, UpdatedAt, AccessPolicy) are added in anticipation of the PR-3a.2
// schema landing — until then they will be the zero value on the wire.
// `owner_id` is intentionally omitted because the live ResourceData schema
// doesn't expose it (it's derived from auth); add it here only if/when the
// server starts returning it.
type Resource struct {
	ResourceID string `json:"resource_id"`
	TargetURL  string `json:"target_url,omitempty"`
	// Type is one of "url" (target-URL proxy, default) or "tunnel"
	// (FRP-backed reverse tunnel). Mirrors the qurl-service
	// `ResourceType` enum.
	Type string `json:"type,omitempty"`
	// Slug is the owner-scoped immutable tunnel sidecar slug. It is present
	// only for tunnel resources.
	Slug         string `json:"slug,omitempty"`
	Alias        string `json:"alias,omitempty"`
	CustomDomain string `json:"custom_domain,omitempty"`
	Description  string `json:"description,omitempty"`
	// Status is one of [StatusActive] or [StatusRevoked] per the
	// `ResourceData` schema in qurl-service/api/openapi.yaml. The
	// QURL-only [StatusConsumed] / [StatusExpired] don't apply at the
	// resource level — resource state is binary (live or revoked);
	// per-qURL TTL/one-time semantics are tracked on QURL instead.
	Status string `json:"status"`
	// CreatedAt uses `omitzero` (Go 1.24+) — it honors the time.Time
	// zero value, eliding "0001-01-01T00:00:00Z" from the wire when
	// the field is unset on a response. (QURL.CreatedAt still uses
	// `json:"created_at"` without omitzero — that's a separate
	// migration tracked outside this PR.)
	CreatedAt time.Time `json:"created_at,omitzero"`
	// UpdatedAt is also a time.Time + omitzero now that we're on
	// Go 1.24+. The pre-1.24 *time.Time idiom is no longer needed —
	// keeping it as a value avoids the nil-vs-zero ambiguity for a
	// field that's structurally just "missing on the wire".
	UpdatedAt    time.Time     `json:"updated_at,omitzero"`
	AccessPolicy *AccessPolicy `json:"access_policy,omitempty"`
	// KnockResourceID is the client-safe NHP resource_id the tunnel sidecar
	// knocks before dialing FRP. It is surfaced on tunnel resource creates.
	KnockResourceID string `json:"knock_resource_id,omitempty"`
}

// CreateResourceInput is the input for `POST /v1/resources`. Idempotent on
// `(owner_id, target_url_hash)` — repeat calls with the same target return
// the existing resource. Per the PR-3a.2 contract, supplying `Alias` here
// when a matching resource exists *without* an alias yields 409
// `alias_in_use`; callers must use UpdateResource to set alias on an
// already-existing resource.
type CreateResourceInput struct {
	// TargetURL is required when Type is empty or "url"; the client
	// fails fast with ErrCreateResourceRequiresTargetURL otherwise.
	// `omitempty` is load-bearing for the type=tunnel branch where
	// TargetURL is legitimately empty.
	TargetURL string `json:"target_url,omitempty"`
	// Type is one of "url" (default; required for target-URL proxies)
	// or "tunnel". When type=tunnel, TargetURL is ignored server-side.
	// Mirrors the qurl-service `ResourceType` enum.
	Type string `json:"type,omitempty"`
	// Slug is the per-owner stable tunnel handle for type=tunnel resources.
	// When paired with FindOrCreate it lets headless sidecars reserve or
	// recover a tunnel resource without the caller knowing the resource_id.
	Slug string `json:"slug,omitempty"`
	// FindOrCreate requests the server's idempotent resource lookup/create
	// path. For type=tunnel this keys on (owner, slug).
	FindOrCreate bool `json:"find_or_create,omitempty"`
	// Alias: empty string elides via `omitempty` and yields a resource
	// with no alias. Server validates non-empty values against
	// `^[a-z][a-z0-9-]{1,62}[a-z0-9]$`. If a caller passes an empty
	// string from upstream input intending an alias, they'll silently
	// get a no-alias create — pre-validate at the caller if that's a
	// concern. (Note the asymmetry with [UpdateResourceInput.Alias],
	// which is `*string` and rejects `&""` client-side: a `string`
	// can't distinguish "unset" from "empty" without an extra sentinel,
	// and the create surface accepts the silent-no-alias semantic so
	// callers can pass an optional alias literal.)
	Alias        string        `json:"alias,omitempty"`
	CustomDomain string        `json:"custom_domain,omitempty"`
	Description  string        `json:"description,omitempty"`
	AccessPolicy *AccessPolicy `json:"access_policy,omitempty"`
}

// CreateAPIKeyInput is the input for `POST /v1/api-keys`.
type CreateAPIKeyInput struct {
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	ExpiresIn  string   `json:"expires_in,omitempty"`
	Purpose    string   `json:"purpose,omitempty"`
	TunnelSlug string   `json:"tunnel_slug,omitempty"`

	// IdempotencyKey, when non-empty, is sent as the Idempotency-Key
	// request header so retries replay the same plaintext key.
	IdempotencyKey string `json:"-"`
}

// APIKey represents an API key create response. APIKey is populated only on
// create; list/update responses never return plaintext keys.
type APIKey struct {
	KeyID     string     `json:"key_id,omitempty"`
	APIKey    string     `json:"api_key,omitempty"`
	KeyPrefix string     `json:"key_prefix,omitempty"`
	Name      string     `json:"name,omitempty"`
	Scopes    []string   `json:"scopes,omitempty"`
	Status    string     `json:"status,omitempty"`
	CreatedAt time.Time  `json:"created_at,omitzero"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Purpose is the optional constrained-key purpose, such as
	// "tunnel_bootstrap".
	Purpose string `json:"purpose,omitempty"`
	// TunnelSlug is the sidecar slug this constrained key is bound to.
	TunnelSlug string `json:"tunnel_slug,omitempty"`
}

// UpdateResourceInput is the input for `PATCH /v1/resources/{id}`.
//
// Pointer fields distinguish "field absent" (caller didn't provide it,
// server keeps existing value) from "field present with zero value"
// (server should accept the zero).
//
// Clear semantics are asymmetric across fields by design:
//   - Description, CustomDomain — pass `&""` to clear; the empty string
//     is a valid wire value the server accepts as a clear signal.
//   - Alias — has a sentinel `ClearAlias bool`. The server's regex
//     `^[a-z][a-z0-9-]{1,62}[a-z0-9]$` rejects `""`, so the empty-string
//     pointer is reserved as a footgun guard ([Client.UpdateResource]
//     fails fast with [ErrUpdateResourceAliasEmpty]).
//   - AccessPolicy — pass `&AccessPolicy{}` (all zero subfields) to
//     clear; pass nil to leave unchanged. There is no sentinel-clear
//     because AccessPolicy is a struct, not a scalar.
//
// Setting Alias and ClearAlias together is invalid and rejected
// client-side ([ErrUpdateResourceAliasClearExclusive]). An entirely
// empty input (no fields populated) is also rejected
// ([ErrUpdateResourceNoFieldsSet]) — symmetric with Create's
// no-target guard.
//
// Retry-safety invariant: every field on this struct MUST be
// field-idempotent until Idempotency-Key plumbing lands (#148).
// do() retries 5xx/429 with the buffered body, so a successfully-
// applied PATCH that returns 502 will be re-applied on retry — that's
// safe iff a second apply is a no-op (which is true for setting alias
// to a value, clearing alias, setting description, etc., but NOT for
// counters, appends, or audit-log-per-call semantics). Adding a
// non-idempotent field requires #148 first; the [Client.UpdateResource]
// doc-comment carries a TODO marker.
type UpdateResourceInput struct {
	// Description: pass `&""` to clear the field server-side (no
	// `ClearDescription` sentinel — the empty string is the clear
	// semantic). Pass nil to leave unchanged. NOT trimmed by the
	// client — surrounding whitespace round-trips verbatim; callers
	// who care should pre-trim.
	Description *string `json:"description,omitempty"`
	// Alias sets the resource's alias when non-nil. Must NOT be a pointer
	// to the empty string — use ClearAlias=true to clear. The empty-string
	// pointer is reserved as a fail-fast footgun guard because the server
	// regex `^[a-z][a-z0-9-]{1,62}[a-z0-9]$` would 400 on `""` anyway —
	// the client raises a clearer error than the server's generic message.
	Alias *string `json:"alias,omitempty"`
	// ClearAlias=true sends `clear_alias: true` to the server, removing
	// any existing alias. Mutually exclusive with a non-nil Alias.
	// Alias is the only field with a sentinel-clear; Description and
	// CustomDomain use the `&""` convention because their server-side
	// validators accept the empty string as a clear signal.
	ClearAlias bool `json:"clear_alias,omitempty"`
	// CustomDomain: pass `&""` to clear the custom domain mapping (same
	// convention as Description). Pass nil to leave unchanged. NOT
	// trimmed by the client (consistent with Description).
	CustomDomain *string `json:"custom_domain,omitempty"`
	// AccessPolicy: pass a non-nil pointer to update the policy in
	// place. The server treats `&AccessPolicy{}` (all zero subfields)
	// as a clear; pass nil to leave the existing policy unchanged.
	AccessPolicy *AccessPolicy `json:"access_policy,omitempty"`
}

// hasAnyFieldSet reports whether the input has at least one mutable
// field populated. Used by UpdateResource to fail fast on the no-op
// PATCH `{}` case.
//
// Keep in sync with [UpdateResourceInput] fields — adding a new
// mutable field without updating this method silently allows a no-op
// PATCH that exercises only the new field. (No reflection: the
// 5-field surface doesn't justify it.)
func (in *UpdateResourceInput) hasAnyFieldSet() bool {
	return in.Description != nil ||
		in.Alias != nil ||
		in.ClearAlias ||
		in.CustomDomain != nil ||
		in.AccessPolicy != nil
}

// CreateResource creates a (or returns the existing) qURL resource.
//
// CreateResource is server-side idempotent on `(owner_id, target_url_hash)`:
// repeating the same body returns the existing resource. A caller-supplied
// Idempotency-Key would still be useful for layered retry semantics on top
// of side-effecting Slack/Discord triggers; that plumbing is tracked at
// #148 alongside the other write methods.
func (c *Client) CreateResource(ctx context.Context, input *CreateResourceInput) (*Resource, error) {
	if input == nil {
		return nil, ErrCreateResourceNilInput
	}
	// TargetURL handling by Type:
	//   - "" / [ResourceTypeURL] — TargetURL required (uniquely
	//     identifies the resource on `(owner_id, target_url_hash)`).
	//   - [ResourceTypeTunnel]   — TargetURL must be empty (server
	//     ignores it; a stale value usually indicates copy-pasted
	//     literals).
	//   - default                — unknown type; pass through and let
	//     the server be the authority. This keeps the client
	//     forward-compatible with future ResourceType values without
	//     a release dependency. The trade-off is that a typo (e.g.
	//     "urll") skips the TargetURL validator and the caller learns
	//     about it from a server 400 instead of a Go-typed error;
	//     forward-compat won out here because the server's enum
	//     enforcement is authoritative anyway.
	// For known types: server returns 400 either way; failing fast
	// saves a round-trip and yields a typed Go error.
	switch input.Type {
	case "", ResourceTypeURL:
		if input.TargetURL == "" {
			return nil, ErrCreateResourceRequiresTargetURL
		}
	case ResourceTypeTunnel:
		if input.TargetURL != "" {
			return nil, ErrCreateResourceTunnelRejectsTargetURL
		}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal create resource input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/resources", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var out Resource
	if _, err := c.do(req, &out, "POST /v1/resources"); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateAPIKey creates a qURL API key. It is used by Slack onboarding to mint
// short-lived, restricted tunnel bootstrap keys from a workspace API key.
func (c *Client) CreateAPIKey(ctx context.Context, input *CreateAPIKeyInput) (*APIKey, error) {
	if input == nil {
		return nil, ErrCreateAPIKeyNilInput
	}
	if err := validateIdempotencyKey(input.IdempotencyKey); err != nil {
		return nil, err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal create api key input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/api-keys", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if input.IdempotencyKey != "" {
		req.Header.Set(HeaderIdempotencyKey, input.IdempotencyKey)
	}

	var out APIKey
	if _, err := c.do(req, &out, "POST /v1/api-keys"); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeAPIKey revokes a qURL API key by key_id.
func (c *Client) RevokeAPIKey(ctx context.Context, keyID string) error {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return ErrRevokeAPIKeyEmptyID
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/api-keys/"+url.PathEscape(keyID), http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	_, err = c.do(req, nil, "DELETE /v1/api-keys/:key_id")
	return err
}

// UpdateResource updates a resource's mutable properties (alias, description,
// access policy, etc.). resourceID must be a `r_…` ID; alias-keyed updates
// must first resolve the alias to its resource_id.
//
// Validation order (first match wins): trim resourceID → empty
// resourceID → nil input → no-fields-set → trim Alias → exclusivity
// → empty-Alias-pointer. The exclusivity-before-empty-pointer leg is
// pinned by TestUpdateResourceEmptyAliasPlusClearAliasReportsExclusivityFirst;
// the alias-trim leg is pinned by TestUpdateResourceTrimsAliasPointer
// and TestUpdateResourceWhitespaceOnlyAliasRejected.
//
// Retry semantics: do() retries 5xx/429 with the buffered body, so a
// successfully-applied PATCH that returns 502 will be re-applied on retry.
// All currently-supported PATCH fields (alias, description, custom_domain,
// access_policy) are field-idempotent — the second apply is a no-op.
// Adding a non-idempotent field (a counter, an append, etc.) would break
// this contract; callers should plumb Idempotency-Key (tracked at #148)
// before that happens. Until #148 lands, callers needing at-least-once
// retry safety should dedupe on `(owner_id, resource_id)` server-side.
//
// TODO(#148): plumb Idempotency-Key on this method before any
// non-idempotent PATCH field lands.
func (c *Client) UpdateResource(ctx context.Context, resourceID string, input *UpdateResourceInput) (*Resource, error) {
	// Normalize then validate: surrounding whitespace is silently
	// stripped so " r_existing01 " hits the wire as
	// /v1/resources/r_existing01 (not /v1/resources/%20r_existing01%20),
	// and a whitespace-only input is rejected as empty.
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" {
		return nil, ErrUpdateResourceEmptyID
	}
	if input == nil {
		return nil, ErrUpdateResourceNilInput
	}
	if !input.hasAnyFieldSet() {
		return nil, ErrUpdateResourceNoFieldsSet
	}
	// Trim *input.Alias for symmetry with the resourceID and
	// GetResource trim contracts. Shallow-copy input so the
	// trim doesn't mutate the caller's struct, then re-point Alias at
	// the trimmed value. The trimmed string is what hits the wire and
	// what the empty-pointer guard below sees.
	//
	// Note: this is a shallow copy — pointer fields like AccessPolicy
	// still alias the caller's data. Today only Alias gets retargeted;
	// adding more normalization (e.g. trimming Description/CustomDomain)
	// would need either a deeper copy or per-field copy-on-mutate.
	if input.Alias != nil {
		trimmed := strings.TrimSpace(*input.Alias)
		copyInput := *input
		copyInput.Alias = &trimmed
		input = &copyInput
	}
	// Order: the exclusivity check runs before the empty-pointer
	// footgun guard. A caller passing both `Alias: &""` and
	// `ClearAlias: true` learns about the structural conflict first
	// (the actionable fix is "pick one"); the empty-pointer error
	// is the footgun guard for the single-field case.
	if input.Alias != nil && input.ClearAlias {
		return nil, ErrUpdateResourceAliasClearExclusive
	}
	if input.Alias != nil && *input.Alias == "" {
		return nil, ErrUpdateResourceAliasEmpty
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal update resource input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+"/v1/resources/"+url.PathEscape(resourceID), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var out Resource
	if _, err := c.do(req, &out, "PATCH /v1/resources/:id"); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetResource fetches a single resource by its `r_…` ID via
// GET /v1/resources/{id}. The response carries the full Resource,
// including the tunnel Slug for type=tunnel rows. Returns a typed
// APIError with 404 status when the ID is unknown to the caller's owner.
func (c *Client) GetResource(ctx context.Context, resourceID string) (*Resource, error) {
	// Normalize then validate (same posture as UpdateResource on
	// resourceID) — strips surrounding whitespace so the trimmed value
	// is what hits the wire, and rejects whitespace-only as empty.
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" {
		return nil, ErrGetResourceEmptyID
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/resources/"+url.PathEscape(resourceID), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var out Resource
	if _, err := c.do(req, &out, "GET /v1/resources/:id"); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListResourcesInput is the paginated input shape for [Client.ListResources].
// Mirrors the qurl-service `GET /v1/resources` query parameters
// (`cursor`, `limit`, `slug`).
type ListResourcesInput struct {
	// Limit caps the number of items returned in one page. Server
	// accepts 1-100; zero falls back to the server default.
	Limit int
	// Cursor is the opaque next-page handle from a previous response.
	Cursor string
	// Slug filters to a single owner-scoped tunnel resource by immutable
	// sidecar slug. The server returns a 0- or 1-item list and ignores
	// cursor/limit when this is set.
	Slug string
}

// ListResourcesOutput is the response shape from [Client.ListResources].
type ListResourcesOutput struct {
	Resources  []Resource
	NextCursor string
	HasMore    bool
}

// listResourcesMaxLimit is the server-enforced upper bound on the
// `/v1/resources?limit=` query parameter (qurl-service rejects
// `limit > 100` with a 400). Lifted to a constant so the boundary
// clamp in [Client.ListResources] keeps a bad-math caller from ever
// reaching the server.
const listResourcesMaxLimit = 100

// ListResources retrieves a paginated list of resources for the
// authenticated workspace. Used by Slack's `/qurl list` to render
// copy-paste-ready `$<alias>` / `$<resource_id>` rows.
//
// `input.Limit` is clamped to [listResourcesMaxLimit] before the
// request fires — the server returns a 400 on overshoot, but the
// boundary check here means a handler with bad math gets a clean
// page instead of an upstream error.
func (c *Client) ListResources(ctx context.Context, input ListResourcesInput) (*ListResourcesOutput, error) {
	params := url.Values{}
	if input.Limit > listResourcesMaxLimit {
		input.Limit = listResourcesMaxLimit
	}
	if input.Limit > 0 {
		params.Set("limit", strconv.Itoa(input.Limit))
	}
	if input.Cursor != "" {
		params.Set("cursor", input.Cursor)
	}
	if input.Slug != "" {
		params.Set("slug", input.Slug)
	}
	path := c.baseURL + "/v1/resources"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	var resources []Resource
	meta, err := c.do(req, &resources, "GET /v1/resources")
	if err != nil {
		return nil, err
	}
	out := &ListResourcesOutput{Resources: resources}
	if out.Resources == nil {
		// Normalize nil → empty slice so callers can range over it
		// without a nil-guard.
		out.Resources = []Resource{}
	}
	if meta != nil {
		out.NextCursor = meta.NextCursor
		out.HasMore = meta.HasMore
	}
	return out, nil
}

// --- Resolve ---

// ResolveInput holds input for headless qURL resolution.
type ResolveInput struct {
	AccessToken string `json:"access_token"`
}

// ResolveOutput holds the result of a headless resolution.
type ResolveOutput struct {
	TargetURL   string       `json:"target_url"`
	ResourceID  string       `json:"resource_id"`
	AccessGrant *AccessGrant `json:"access_grant,omitempty"`
}

// AccessGrant describes the firewall access that was granted.
type AccessGrant struct {
	ExpiresIn int    `json:"expires_in"`
	GrantedAt string `json:"granted_at"`
	SrcIP     string `json:"src_ip"`
}

// Resolve resolves a qURL access token, triggering a network access request
// to open the firewall for the caller's IP.
func (c *Client) Resolve(ctx context.Context, input ResolveInput) (*ResolveOutput, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal resolve input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/resolve", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var out ResolveOutput
	if _, err := c.do(req, &out, "POST /v1/resolve"); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Quota ---

// QuotaOutput holds quota information.
type QuotaOutput struct {
	Plan        string      `json:"plan"`
	PeriodStart time.Time   `json:"period_start"`
	PeriodEnd   time.Time   `json:"period_end"`
	RateLimits  *RateLimits `json:"rate_limits,omitempty"`
	Usage       *UsageInfo  `json:"usage,omitempty"`
}

// RateLimits holds rate limit configuration.
type RateLimits struct {
	CreatePerMinute  int `json:"create_per_minute"`
	CreatePerHour    int `json:"create_per_hour"`
	ListPerMinute    int `json:"list_per_minute"`
	ResolvePerMinute int `json:"resolve_per_minute"`
	MaxActiveQURLs   int `json:"max_active_qurls"`
	MaxTokensPerQURL int `json:"max_tokens_per_qurl"`
}

// UsageInfo holds usage statistics.
type UsageInfo struct {
	QURLsCreated       int     `json:"qurls_created"`
	ActiveQURLs        int     `json:"active_qurls"`
	ActiveQURLsPercent float64 `json:"active_qurls_percent"`
	TotalAccesses      int     `json:"total_accesses"`
}

// GetQuota retrieves quota information.
func (c *Client) GetQuota(ctx context.Context) (*QuotaOutput, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/quota", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var out QuotaOutput
	if _, err := c.do(req, &out, "GET /v1/quota"); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Error types (RFC 7807) ---

// APIError represents an error response from the qURL API.
type APIError struct {
	StatusCode    int
	Code          string
	Title         string
	Detail        string
	InvalidFields map[string]string
	RequestID     string
	RetryAfter    int // seconds, from Retry-After header (429)
}

// Error returns a human-readable error message.
func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s (%d): %s", e.Title, e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("%s (%d)", e.Title, e.StatusCode)
}

// apiErrorEnvelope is the wire format for API error responses.
type apiErrorEnvelope struct {
	Error *apiErrorDetail `json:"error"`
	Meta  *ResponseMeta   `json:"meta,omitempty"`
}

type apiErrorDetail struct {
	Type          string            `json:"type"`
	Title         string            `json:"title"`
	Status        int               `json:"status"`
	Detail        string            `json:"detail"`
	Code          string            `json:"code"`
	InvalidFields map[string]string `json:"invalid_fields,omitempty"`
}

// --- HTTP plumbing ---

func (c *Client) do(req *http.Request, out any, endpoint string) (*ResponseMeta, error) {
	req.Header.Set("Content-Type", "application/json")
	// NOTE: If you add header logging, redact the Authorization value to avoid leaking API keys.
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	// Buffer body for potential retries. Always done even when maxRetries=0
	// because the cost is negligible for the small JSON payloads this client sends.
	var bodyBytes []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("buffer request body: %w", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.waitForRetry(req.Context(), attempt, lastErr); err != nil {
				return nil, err
			}
			// Reset body for retry.
			if bodyBytes != nil {
				req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
		}

		c.logf("--> %s %s", req.Method, endpoint)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http request: %w", err)
			if attempt < c.maxRetries {
				continue
			}
			return nil, lastErr
		}

		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB max response
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		c.logf("<-- %d %s (%d bytes)", resp.StatusCode, http.StatusText(resp.StatusCode), len(respBody))

		if resp.StatusCode >= 400 {
			apiErr := c.parseError(resp, respBody)
			if isRetryable(resp.StatusCode) && attempt < c.maxRetries {
				lastErr = apiErr
				continue
			}
			return nil, apiErr
		}

		return c.parseSuccess(respBody, out)
	}

	return nil, lastErr
}

func (c *Client) parseSuccess(respBody []byte, out any) (*ResponseMeta, error) {
	// 204 No Content or empty body — nothing to parse.
	if len(respBody) == 0 {
		return &ResponseMeta{}, nil
	}

	// Unwrap response envelope.
	var envelope apiResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		// Fallback: try direct unmarshal (non-envelope response).
		if out != nil {
			if err2 := json.Unmarshal(respBody, out); err2 != nil {
				return nil, fmt.Errorf("unmarshal response: %w", err2)
			}
		}
		return &ResponseMeta{}, nil
	}

	if out != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return nil, fmt.Errorf("unmarshal response data: %w", err)
		}
	}

	return envelope.Meta, nil
}

// isRetryable returns true for status codes that warrant a retry.
func isRetryable(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests ||
		statusCode == http.StatusBadGateway ||
		statusCode == http.StatusServiceUnavailable ||
		statusCode == http.StatusGatewayTimeout
}

// waitForRetry sleeps for the computed backoff delay or returns early if the context is canceled.
func (c *Client) waitForRetry(ctx context.Context, attempt int, lastErr error) error {
	delay := c.retryDelay(attempt, lastErr)
	c.logf("retry %d/%d after %s", attempt, c.maxRetries, delay)
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryDelay computes the backoff delay for a retry attempt.
func (c *Client) retryDelay(attempt int, lastErr error) time.Duration {
	// Use Retry-After header if the last error was a rate limit.
	var apiErr *APIError
	if errors.As(lastErr, &apiErr) && apiErr.RetryAfter > 0 {
		return time.Duration(apiErr.RetryAfter) * time.Second
	}

	// Exponential backoff with jitter.
	delay := c.baseDelay * (1 << uint(attempt-1))
	if delay > c.maxDelay {
		delay = c.maxDelay
	}
	// Jitter: 50-100% of computed delay.
	half := int64(delay / 2)
	if half > 0 {
		n, err := rand.Int(rand.Reader, big.NewInt(half))
		if err == nil {
			delay = time.Duration(half + n.Int64())
		}
	}
	return delay
}

func (c *Client) parseError(resp *http.Response, body []byte) error {
	apiErr := &APIError{StatusCode: resp.StatusCode}

	// Try RFC 7807 envelope
	var envelope apiErrorEnvelope
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil {
		apiErr.Code = envelope.Error.Code
		apiErr.Title = envelope.Error.Title
		apiErr.Detail = envelope.Error.Detail
		apiErr.InvalidFields = envelope.Error.InvalidFields
		if envelope.Meta != nil {
			apiErr.RequestID = envelope.Meta.RequestID
		}
	} else {
		// Fallback for non-standard errors
		apiErr.Title = http.StatusText(resp.StatusCode)
		apiErr.Detail = string(body)
	}

	// Ensure title is set
	if apiErr.Title == "" {
		apiErr.Title = http.StatusText(resp.StatusCode)
	}

	// Parse Retry-After for rate limiting
	if resp.StatusCode == http.StatusTooManyRequests {
		apiErr.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	}

	return apiErr
}

// parseRetryAfter parses the Retry-After header value as seconds, returning 0 if absent or invalid.
func parseRetryAfter(header string) int {
	secs, err := strconv.Atoi(header)
	if err != nil {
		return 0
	}
	return secs
}
