package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// resourceClientUserAgent identifies the slack bot's resource-API
// traffic against qurl-service so log greps can split it from
// customer-side `qurl-go-client/...` traffic. Distinct from the
// admin-client UA because resource calls go to the customer-API
// surface (`/v1/resources`) authed with the workspace's API key,
// not the LayerV-internal HMAC.
const resourceClientUserAgent = "qurl-slack-resource/dev"

// resourceClientTimeout bounds a single resource call. Same floor
// as the admin client (10s) so a stuck resource endpoint can't burn
// the whole `response_url` window.
const resourceClientTimeout = 10 * time.Second

// Error codes documented in the Phase 3a.2 plan. Lifted to constants
// so handler error-mapping reads as a single switch rather than
// scattered string literals.
const (
	errCodeAliasInUse        = "alias_in_use"
	errCodeAliasReserved     = "alias_reserved"
	errCodeAliasInvalidFmt   = "alias_invalid_format"
	errCodeTunnelDisabled    = "tunnel_disabled"
	errCodeAliasNotFound     = "alias_not_found"
	contentTypeJSON          = "application/json"
	headerContentType        = "Content-Type"
	headerAuthorization      = "Authorization"
	resourceIDPathByAliasFmt = "/v1/resources/by-alias/%s"
)

// ResourceClient is a thin client over `/v1/resources` paths used by
// the setalias/unsetalias handlers (PR-3c.4). PR-3c.2 will fold these
// methods into shared/client; until then this avoids depending on a
// not-yet-merged sibling PR. The methods cover only the surface
// PR-3c.4 needs — Get-by-alias, Create with optional alias, Update
// (alias set or clear).
type ResourceClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	userAgent  string
}

// ResourceClientOption configures a [ResourceClient].
type ResourceClientOption func(*ResourceClient)

// WithResourceHTTPClient injects a custom HTTP client for tests.
func WithResourceHTTPClient(c *http.Client) ResourceClientOption {
	return func(rc *ResourceClient) { rc.httpClient = c }
}

// NewResourceClient builds a [ResourceClient] for the customer-API
// `/v1/resources` surface. The `apiKey` is the workspace's
// customer-style key (`lv_live_…`), not the LayerV-internal HMAC.
func NewResourceClient(baseURL, apiKey string, opts ...ResourceClientOption) *ResourceClient {
	rc := &ResourceClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		userAgent: resourceClientUserAgent,
		httpClient: &http.Client{
			Timeout: resourceClientTimeout,
		},
	}
	for _, opt := range opts {
		opt(rc)
	}
	return rc
}

// Resource is the subset of `ResourceData` (per qurl-service's
// OpenAPI) that the setalias/unsetalias handlers consume. We keep
// the shape minimal so a cosmetic change in the upstream schema
// doesn't churn this client.
type Resource struct {
	ResourceID string `json:"resource_id"`
	Type       string `json:"type,omitempty"`
	TargetURL  string `json:"target_url,omitempty"`
	Alias      string `json:"alias,omitempty"`
	Status     string `json:"status,omitempty"`
}

// CreateResourceInput is the body shape for `POST /v1/resources`.
// `Alias` is the optional alias to bind on creation; per PR-3a.2 the
// stored value never contains the `$` sigil.
type CreateResourceInput struct {
	Type      string `json:"type,omitempty"`
	TargetURL string `json:"target_url,omitempty"`
	Alias     string `json:"alias,omitempty"`
}

// UpdateResourceInput is the body shape for `PATCH /v1/resources/:id`.
// Only the fields the slack bot mutates are modeled. `ClearAlias` is
// the explicit-true flag (per PR-3a.2's OpenAPI) that removes any
// existing alias from the resource — distinct from omitting `Alias`,
// which is a no-op.
type UpdateResourceInput struct {
	Alias      string `json:"alias,omitempty"`
	ClearAlias bool   `json:"clear_alias,omitempty"`
}

// CreateResource registers a new resource with optional alias. If a
// resource for the same `target_url`+owner already exists, the
// server returns it (idempotent) — the slack handler relies on that
// to keep `setalias` simple even when the URL was previously used.
func (rc *ResourceClient) CreateResource(ctx context.Context, input CreateResourceInput) (*Resource, error) {
	var out Resource
	if err := rc.do(ctx, http.MethodPost, "/v1/resources", input, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateResource updates a resource's mutable fields. The slack bot
// uses this for two paths: (1) `setalias` rebind (set Alias), and
// (2) `unsetalias` (set ClearAlias=true).
func (rc *ResourceClient) UpdateResource(ctx context.Context, resourceID string, input UpdateResourceInput) (*Resource, error) {
	var out Resource
	if err := rc.do(ctx, http.MethodPatch, "/v1/resources/"+url.PathEscape(resourceID), input, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetResourceByAlias resolves an alias to its bound resource. Returns
// a 404 [*ResourceError] when the alias is unbound — handlers route
// that to a friendly "alias not found" message.
func (rc *ResourceClient) GetResourceByAlias(ctx context.Context, alias string) (*Resource, error) {
	var out Resource
	if err := rc.do(ctx, http.MethodGet, fmt.Sprintf(resourceIDPathByAliasFmt, url.PathEscape(alias)), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResourceError is the typed error returned for non-2xx responses
// from the resource API. Carries the qurl-service error code so
// handlers can map known codes (e.g. `alias_in_use`, `alias_reserved`)
// to friendly messages without string-matching titles.
type ResourceError struct {
	StatusCode int
	Code       string
	Title      string
	Detail     string
}

// Error returns a human-readable message.
func (e *ResourceError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s (%d): %s", e.Title, e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("%s (%d)", e.Title, e.StatusCode)
}

// resourceEnvelope mirrors qurl-service's `apiResponse` for the
// fields this client cares about. We don't reuse `shared/client.apiResponse`
// because that package keeps the type unexported.
type resourceEnvelope struct {
	Data  json.RawMessage      `json:"data,omitempty"`
	Error *resourceErrorDetail `json:"error,omitempty"`
}

type resourceErrorDetail struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
	Status int    `json:"status"`
}

// do is the low-level HTTP plumbing. Mirrors the shape of
// AdminClient.do — centralized so a future cross-cutting concern
// (tracing, mTLS, etc.) edits one site.
func (rc *ResourceClient) do(ctx context.Context, method, path string, body, out any) error {
	if rc.baseURL == "" {
		return errors.New("resource client: base URL not configured")
	}
	if rc.apiKey == "" {
		return errors.New("resource client: API key not configured")
	}

	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, rc.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set(headerAuthorization, "Bearer "+rc.apiKey)
	req.Header.Set("User-Agent", rc.userAgent)
	if reader != nil {
		req.Header.Set(headerContentType, contentTypeJSON)
	}
	req.Header.Set("Accept", contentTypeJSON)

	resp, err := rc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("resource request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return parseResourceError(resp.StatusCode, respBody)
	}

	if out == nil || len(respBody) == 0 {
		return nil
	}

	var env resourceEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		// Fallback: some endpoints return raw JSON without the envelope.
		if err2 := json.Unmarshal(respBody, out); err2 != nil {
			return fmt.Errorf("unmarshal response: %w", err2)
		}
		return nil
	}
	if env.Error != nil {
		return &ResourceError{
			StatusCode: env.Error.Status,
			Code:       env.Error.Code,
			Title:      env.Error.Title,
			Detail:     env.Error.Detail,
		}
	}
	if len(env.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("unmarshal data: %w", err)
	}
	return nil
}

// parseResourceError unpacks a non-2xx response into a [*ResourceError].
// Tries the qurl-service envelope first, falls back to status-text +
// raw body so unexpected error shapes still surface cleanly.
func parseResourceError(status int, body []byte) error {
	var env resourceEnvelope
	if json.Unmarshal(body, &env) == nil && env.Error != nil {
		return &ResourceError{
			StatusCode: status,
			Code:       env.Error.Code,
			Title:      env.Error.Title,
			Detail:     env.Error.Detail,
		}
	}
	return &ResourceError{
		StatusCode: status,
		Title:      http.StatusText(status),
		Detail:     string(body),
	}
}
