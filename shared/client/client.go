// Package client provides a Go client for the qURL API.
//
// Nil-input conventions:
//   - Create requires non-nil input (TargetURL is mandatory) and returns an error if nil.
//   - List treats nil as &ListInput{} (no required fields, so nil is always safe).
//   - MintLink(ctx, id, nil) sends a bodiless POST — the server mints with the qURL's own
//     defaults. MintLink(ctx, id, &MintLinkInput{…}) sends a JSON body with per-link overrides.
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
	"time"
)

const (
	defaultTimeout    = 30 * time.Second
	defaultMaxRetries = 3
	defaultBaseDelay  = 500 * time.Millisecond
	defaultMaxDelay   = 30 * time.Second
)

// StatusActive indicates the qURL is live and accepting access requests.
const StatusActive = "active"

// StatusRevoked indicates the qURL was manually revoked (deleted).
const StatusRevoked = "revoked"

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
	ResourceID  string     `json:"resource_id"`
	TargetURL   string     `json:"target_url"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Description string     `json:"description,omitempty"`
	Tags        []string   `json:"tags,omitempty"`
	QURLSite    string     `json:"qurl_site,omitempty"`
	// CustomDomain is a pointer to distinguish "not set" (nil) from "explicitly empty" on
	// API responses. CreateInput.CustomDomain is a plain string with omitempty, which is
	// sufficient for write requests where absence and empty mean the same thing.
	CustomDomain *string `json:"custom_domain,omitempty"`
}

// AIAgentPolicy controls access by AI agent categories.
type AIAgentPolicy struct {
	BlockAll        bool     `json:"block_all,omitempty"`
	DenyCategories  []string `json:"deny_categories,omitempty"`
	AllowCategories []string `json:"allow_categories,omitempty"`
}

// AccessPolicy defines access restrictions for a qURL.
type AccessPolicy struct {
	IPAllowlist         []string       `json:"ip_allowlist,omitempty"`
	IPDenylist          []string       `json:"ip_denylist,omitempty"`
	GeoAllowlist        []string       `json:"geo_allowlist,omitempty"`
	GeoDenylist         []string       `json:"geo_denylist,omitempty"`
	UserAgentAllowRegex string         `json:"user_agent_allow_regex,omitempty"`
	UserAgentDenyRegex  string         `json:"user_agent_deny_regex,omitempty"`
	AIAgentPolicy       *AIAgentPolicy `json:"ai_agent_policy,omitempty"`
}

// CreateInput is the input for creating a qURL.
type CreateInput struct {
	TargetURL       string        `json:"target_url"`
	Label           string        `json:"label,omitempty"`
	ExpiresIn       string        `json:"expires_in,omitempty"`
	OneTimeUse      bool          `json:"one_time_use,omitempty"`
	MaxSessions     int           `json:"max_sessions,omitempty"`
	SessionDuration string        `json:"session_duration,omitempty"`
	CustomDomain    string        `json:"custom_domain,omitempty"`
	AccessPolicy    *AccessPolicy `json:"access_policy,omitempty"`
}

// CreateOutput is the response from creating a qURL.
type CreateOutput struct {
	QURLID     string     `json:"qurl_id"`
	ResourceID string     `json:"resource_id"`
	QURLLink   string     `json:"qurl_link"`
	QURLSite   string     `json:"qurl_site"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Label      string     `json:"label,omitempty"`
}

// Create creates a new qURL. input must not be nil.
func (c *Client) Create(ctx context.Context, input *CreateInput) (*CreateOutput, error) {
	if input == nil {
		return nil, errors.New("create input must not be nil")
	}

	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal create input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/qurls", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var out CreateOutput
	if _, err := c.do(req, &out, "POST /v1/qurls"); err != nil {
		return nil, err
	}
	return &out, nil
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
	Limit         int
	Cursor        string
	Status        string
	Query         string
	Sort          string
	CreatedAfter  string
	CreatedBefore string
	ExpiresBefore string
	ExpiresAfter  string
}

// ListOutput is the output of listing qURLs.
type ListOutput struct {
	QURLs      []QURL `json:"qurls"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more,omitempty"`
}

// List retrieves a paginated list of qURLs.
// A nil input is treated as &ListInput{} (all fields at defaults). Unlike Create,
// List has no required fields so nil is silently accepted.
func (c *Client) List(ctx context.Context, input *ListInput) (*ListOutput, error) {
	if input == nil {
		input = &ListInput{}
	}

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
	if input.CreatedAfter != "" {
		params.Set("created_after", input.CreatedAfter)
	}
	if input.CreatedBefore != "" {
		params.Set("created_before", input.CreatedBefore)
	}
	if input.ExpiresBefore != "" {
		params.Set("expires_before", input.ExpiresBefore)
	}
	if input.ExpiresAfter != "" {
		params.Set("expires_after", input.ExpiresAfter)
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

// UpdateInput holds input for updating a qURL.
// The UpdateQurlRequest schema defines exactly four mutable fields: extend_by, expires_at,
// tags, and description. session_duration, one_time_use, access_policy, and custom_domain
// are intentionally absent — the API does not support updating them after creation.
type UpdateInput struct {
	ExtendBy  string     `json:"extend_by,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Tags uses a pointer-to-slice to encode three states:
	//   nil           → omitted from PATCH (leave tags unchanged)
	//   &[]string{}   → sends "tags":[] (clear all tags)
	//   &[]string{…}  → sends "tags":[…] (replace tags with this set)
	Tags *[]string `json:"tags,omitempty"`
	// Description updates the resource-level description. The API uses "description"
	// for updates (not "label") per the UpdateQurlRequest schema — this is an
	// intentional divergence from CreateInput.Label which maps to the QURL token label.
	Description *string `json:"description,omitempty"`
}

// Extend extends a qURL's expiration. It is a convenience wrapper around Update
// with ExtendBy set; callers who need combined updates should use Update directly.
func (c *Client) Extend(ctx context.Context, id, duration string) (*QURL, error) {
	return c.Update(ctx, id, UpdateInput{ExtendBy: duration})
}

// Update updates a qURL's mutable properties.
func (c *Client) Update(ctx context.Context, id string, input UpdateInput) (*QURL, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal update input: %w", err)
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

// MintLinkInput holds optional input for minting an access link.
//
// OneTimeUse and MaxSessions use pointer types so callers can explicitly override a
// qURL's defaults to false/0 (e.g., "allow multiple sessions on a normally one-time
// qURL"). A nil pointer omits the field from the JSON body, letting the server apply the
// qURL-level default. The same logic applies to MintLink(ctx, id, nil): a nil
// MintLinkInput sends a bodiless POST (server uses qURL defaults), while a non-nil
// MintLinkInput (even &MintLinkInput{}) sends a JSON body.
type MintLinkInput struct {
	ExpiresIn       string        `json:"expires_in,omitempty"`
	ExpiresAt       *time.Time    `json:"expires_at,omitempty"`
	Label           string        `json:"label,omitempty"`
	OneTimeUse      *bool         `json:"one_time_use,omitempty"`
	MaxSessions     *int          `json:"max_sessions,omitempty"`
	SessionDuration string        `json:"session_duration,omitempty"`
	AccessPolicy    *AccessPolicy `json:"access_policy,omitempty"`
}

// MintOutput holds the result of minting an access link.
type MintOutput struct {
	QURLLink  string     `json:"qurl_link"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// MintLink mints a new access link for a qURL.
// Pass nil to mint with the qURL's own defaults (server uses bodiless POST).
// Pass a non-nil MintLinkInput to override expiry, one-time-use, and other per-link settings.
func (c *Client) MintLink(ctx context.Context, id string, input *MintLinkInput) (*MintOutput, error) {
	var bodyReader io.Reader = http.NoBody
	if input != nil {
		body, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("marshal mint input: %w", err)
		}
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/qurls/"+url.PathEscape(id)+"/mint_link", bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Content-Type is managed centrally by do(): set when body is present, omitted
	// for bodiless requests (input==nil path). See TestMintLink* for wire-level coverage.

	var out MintOutput
	if _, err := c.do(req, &out, "POST /v1/qurls/:id/mint_link"); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Batch ---

// BatchCreateOutput holds the response from batch creating qURLs.
type BatchCreateOutput struct {
	Succeeded int               `json:"succeeded"`
	Failed    int               `json:"failed"`
	Results   []BatchItemResult `json:"results"`
}

// BatchItemResult holds the result for a single item in a batch.
type BatchItemResult struct {
	Index      int             `json:"index"`
	Success    bool            `json:"success"`
	ResourceID string          `json:"resource_id,omitempty"`
	QURLLink   string          `json:"qurl_link,omitempty"`
	QURLSite   string          `json:"qurl_site,omitempty"`
	ExpiresAt  *time.Time      `json:"expires_at,omitempty"`
	Error      *BatchItemError `json:"error,omitempty"`
}

// BatchItemError holds error details for a failed batch item.
type BatchItemError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// MaxBatchSize is the maximum number of items in a single batch create request.
// Matches the BatchCreateRequest schema constraint (api/openapi.yaml: items.maxItems=100).
const MaxBatchSize = 100

// BatchCreate creates multiple qURLs at once (1-100 items).
func (c *Client) BatchCreate(ctx context.Context, items []*CreateInput) (*BatchCreateOutput, error) {
	if len(items) == 0 || len(items) > MaxBatchSize {
		return nil, fmt.Errorf("batch size must be 1-%d, got %d", MaxBatchSize, len(items))
	}
	for i, item := range items {
		if item == nil {
			return nil, fmt.Errorf("batch item at index %d must not be nil", i)
		}
		if item.TargetURL == "" {
			return nil, fmt.Errorf("batch item at index %d has empty target_url", i)
		}
	}

	payload := struct {
		Items []*CreateInput `json:"items"`
	}{Items: items}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal batch input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/qurls/batch", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var out BatchCreateOutput
	if _, err := c.do(req, &out, "POST /v1/qurls/batch"); err != nil {
		return nil, err
	}
	return &out, nil
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
	MaxExpirySeconds int `json:"max_expiry_seconds"`
}

// UsageInfo holds usage statistics.
type UsageInfo struct {
	QURLsCreated       int      `json:"qurls_created"`
	ActiveQURLs        int      `json:"active_qurls"`
	ActiveQURLsPercent *float64 `json:"active_qurls_percent,omitempty"`
	TotalAccesses      int      `json:"total_accesses"`
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
		req.Header.Set("Content-Type", "application/json")
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
