// Package client provides a Go client for the QURL API.
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

// StatusActive indicates the QURL is live and accepting access requests.
const StatusActive = "active"

// StatusExpired indicates the QURL's TTL has elapsed.
const StatusExpired = "expired"

// StatusRevoked indicates the QURL was manually revoked (deleted).
const StatusRevoked = "revoked"

// StatusConsumed indicates a one-time QURL has been used.
const StatusConsumed = "consumed"

// Logger is an optional interface for debug logging.
type Logger interface {
	Printf(format string, args ...any)
}

// Client is a QURL API client.
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

// New creates a QURL API client.
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

// apiResponse is the success response envelope from the QURL API.
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

// --- QURL types (match API schema) ---

// QURL represents a QURL resource as returned by the API.
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

// AccessPolicy defines access restrictions for a QURL.
type AccessPolicy struct {
	IPAllowlist  []string `json:"ip_allowlist,omitempty"`
	IPDenylist   []string `json:"ip_denylist,omitempty"`
	GeoAllowlist []string `json:"geo_allowlist,omitempty"`
	GeoDenylist  []string `json:"geo_denylist,omitempty"`
}

// CreateInput is the input for creating a QURL.
type CreateInput struct {
	TargetURL    string        `json:"target_url"`
	Description  string        `json:"description,omitempty"`
	ExpiresIn    string        `json:"expires_in,omitempty"`
	OneTimeUse   bool          `json:"one_time_use,omitempty"`
	MaxSessions  int           `json:"max_sessions,omitempty"`
	AccessPolicy *AccessPolicy `json:"access_policy,omitempty"`
}

// CreateOutput is the response from creating a QURL.
type CreateOutput struct {
	ResourceID string     `json:"resource_id"`
	QURLLink   string     `json:"qurl_link"`
	QURLSite   string     `json:"qurl_site"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// Create creates a new QURL.
func (c *Client) Create(ctx context.Context, input CreateInput) (*CreateOutput, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal create input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/qurl", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	var out CreateOutput
	if _, err := c.do(req, &out, "POST /v1/qurl"); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get retrieves a QURL by ID.
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

// ListInput is the input for listing QURLs.
type ListInput struct {
	Limit  int
	Cursor string
	Status string
	Query  string
	Sort   string
}

// ListOutput is the output of listing QURLs.
type ListOutput struct {
	QURLs      []QURL `json:"qurls"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more,omitempty"`
}

// List retrieves a paginated list of QURLs.
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

// Delete revokes a QURL by ID.
func (c *Client) Delete(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/qurls/"+url.PathEscape(id), http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	_, err = c.do(req, nil, "DELETE /v1/qurls/:id")
	return err
}

// ExtendInput holds input for extending a QURL.
type ExtendInput struct {
	ExtendBy  string     `json:"extend_by,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Extend extends a QURL's expiration.
// Both Extend and Update use PATCH /v1/qurls/:id — the server differentiates
// by request body fields (extend_by/expires_at vs description).
func (c *Client) Extend(ctx context.Context, id string, input ExtendInput) (*QURL, error) {
	return c.patchQURL(ctx, id, input)
}

// UpdateInput holds input for updating a QURL's mutable properties.
type UpdateInput struct {
	Description *string `json:"description,omitempty"`
}

// Update updates a QURL's mutable properties.
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

// MintLink mints a new access link for a QURL.
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

// --- Resolve ---

// ResolveInput holds input for headless QURL resolution.
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

// Resolve resolves a QURL access token, triggering a network access request
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

// APIError represents an error response from the QURL API.
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
