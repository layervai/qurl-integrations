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
	"strconv"
	"strings"
	"time"
)

// adminClientUserAgent is the UA the admin client sends to qurl-service.
// Distinct from the customer-style `qurl-go-client/...` UA so internal
// traffic is grep-able in service-side logs.
const adminClientUserAgent = "qurl-slack-admin/dev"

// adminDefaultTimeout bounds a single internal call. Slack's outer
// budget is the 28s `response_url` window — this floor keeps a stuck
// admin endpoint from eating that whole budget.
const adminDefaultTimeout = 10 * time.Second

// JSON field names shared across multiple request bodies. Lifted to
// constants so a server-side rename only edits one site, and
// goconst's repeated-string lint stays satisfied.
const (
	fieldTeamID     = "team_id"
	fieldChannelID  = "channel_id"
	fieldUserID     = "user_id"
	fieldResourceID = "resource_id"
)

// AdminClient is a thin wrapper over qurl-service's `/internal/v1/...`
// admin/policy/redeem/rate-limit endpoints used exclusively by the
// Slack bot. It auths with `QURL_INTERNAL_SERVICE_TOKEN` (a LayerV-side
// internal HMAC, distinct from the customer-style `QURL_API_KEY` that
// the bot uses for `/v1/qurls` mints — see Phase 3c plan).
//
// The methods on this struct return stub-only behavior in PR-3c.1:
// they construct and dispatch the HTTP request, parse the JSON
// envelope, and surface errors. They are NOT wired into the slash
// command handler yet — that lands in PR-3c.3+.
type AdminClient struct {
	baseURL    string
	authToken  string
	httpClient *http.Client
}

// AdminClientOption configures an [AdminClient].
type AdminClientOption func(*AdminClient)

// WithAdminHTTPClient injects a custom HTTP client for tests.
func WithAdminHTTPClient(c *http.Client) AdminClientOption {
	return func(ac *AdminClient) { ac.httpClient = c }
}

// NewAdminClient builds an [AdminClient] for the given qurl-service
// base URL and internal-service token.
func NewAdminClient(baseURL, internalToken string, opts ...AdminClientOption) *AdminClient {
	ac := &AdminClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		authToken: internalToken,
		httpClient: &http.Client{
			Timeout: adminDefaultTimeout,
		},
	}
	for _, opt := range opts {
		opt(ac)
	}
	return ac
}

// WorkspaceMapping describes the workspace -> owner mapping returned
// by `POST /internal/v1/admin/workspace/redeem`. Used by the `admin
// claim` modal flow in PR-3c.3.
type WorkspaceMapping struct {
	TeamID    string    `json:"team_id"`
	OwnerID   string    `json:"owner_id"`
	CreatedAt time.Time `json:"created_at"`
}

// PolicyEntry is one row of `GET /internal/v1/admin/policy/list`.
type PolicyEntry struct {
	ChannelID  string    `json:"channel_id"`
	Alias      string    `json:"alias"`
	ResourceID string    `json:"resource_id,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
}

// PolicyList is the paginated response from
// `GET /internal/v1/admin/policy/list`.
type PolicyList struct {
	Entries    []PolicyEntry `json:"entries"`
	NextCursor string        `json:"next_cursor,omitempty"`
	HasMore    bool          `json:"has_more,omitempty"`
}

// adminEnvelope is the response wrapper used by every internal
// endpoint we hit. Mirrors `shared/client.apiResponse` shape so the
// service can use a single envelope codec across customer and
// internal routes.
type adminEnvelope struct {
	Data  json.RawMessage   `json:"data"`
	Error *adminErrorDetail `json:"error,omitempty"`
}

type adminErrorDetail struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
	Status int    `json:"status"`
}

// AdminError is the error type returned for non-2xx responses from
// the internal admin endpoints. Distinct from
// [github.com/layervai/qurl-integrations/shared/client.APIError] so
// callers can route customer-API errors and internal-admin errors
// to different surfacing paths.
type AdminError struct {
	StatusCode int
	Code       string
	Title      string
	Detail     string
}

// Error returns a human-readable message.
func (e *AdminError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s (%d): %s", e.Title, e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("%s (%d)", e.Title, e.StatusCode)
}

// CheckAdmin asks qurl-service whether `slackUserID` is a recognized
// admin for `teamID`. Returns the workspace's owner ID alongside.
//
// PR-3c.1: stub. The HTTP call is wired but the slash-command
// handler does not yet invoke this — that's PR-3c.3+.
func (ac *AdminClient) CheckAdmin(ctx context.Context, teamID, slackUserID string) (bool, string, error) {
	q := url.Values{}
	q.Set(fieldTeamID, teamID)
	q.Set(fieldUserID, slackUserID)
	var out struct {
		IsAdmin bool   `json:"is_admin"`
		OwnerID string `json:"owner_id"`
	}
	if err := ac.do(ctx, http.MethodGet, "/internal/v1/admin/check?"+q.Encode(), nil, &out); err != nil {
		return false, "", err
	}
	return out.IsAdmin, out.OwnerID, nil
}

// ResolvePolicy asks qurl-service whether `resourceID` (or the
// alias-resolved id) is allowed in `channelID` for `teamID`. Used
// by `/qurl get` after alias resolution.
func (ac *AdminClient) ResolvePolicy(ctx context.Context, teamID, channelID, resourceID string) (bool, error) {
	body := map[string]string{
		fieldTeamID:     teamID,
		fieldChannelID:  channelID,
		fieldResourceID: resourceID,
	}
	var out struct {
		Allowed bool `json:"allowed"`
	}
	if err := ac.do(ctx, http.MethodPost, "/internal/v1/policy/resolve", body, &out); err != nil {
		return false, err
	}
	return out.Allowed, nil
}

// RedeemBootstrap exchanges a one-time bootstrap code for a workspace
// mapping row. Called from the `admin claim` modal submit handler.
// The code arrives via the modal's `private_value` field — never via
// slash-command text (Blocker #3).
func (ac *AdminClient) RedeemBootstrap(ctx context.Context, code, teamID, slackUserID string) (*WorkspaceMapping, error) {
	body := map[string]string{
		"code":      code,
		fieldTeamID: teamID,
		fieldUserID: slackUserID,
	}
	var out WorkspaceMapping
	if err := ac.do(ctx, http.MethodPost, "/internal/v1/admin/workspace/redeem", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AllowResource adds `resourceID` (or its alias) to `channelID`'s
// allowed set for `teamID`.
func (ac *AdminClient) AllowResource(ctx context.Context, teamID, channelID, resourceID string) error {
	return ac.do(ctx, http.MethodPost, "/internal/v1/admin/policy/allow", policyMutationBody(teamID, channelID, resourceID), nil)
}

// DisallowResource removes `resourceID` from `channelID`'s allowed
// set.
func (ac *AdminClient) DisallowResource(ctx context.Context, teamID, channelID, resourceID string) error {
	return ac.do(ctx, http.MethodPost, "/internal/v1/admin/policy/disallow", policyMutationBody(teamID, channelID, resourceID), nil)
}

// policyMutationBody is the shared body shape for allow/disallow.
// Centralizing it satisfies dupl and keeps the two methods in sync.
func policyMutationBody(teamID, channelID, resourceID string) map[string]string {
	return map[string]string{
		fieldTeamID:     teamID,
		fieldChannelID:  channelID,
		fieldResourceID: resourceID,
	}
}

// ListPolicies returns one page of policy entries for `teamID`.
// `cursor` and `limit` map to the standard pagination shape used by
// the customer API.
func (ac *AdminClient) ListPolicies(ctx context.Context, teamID, cursor string, limit int) (*PolicyList, error) {
	q := url.Values{}
	q.Set(fieldTeamID, teamID)
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out PolicyList
	if err := ac.do(ctx, http.MethodGet, "/internal/v1/admin/policy/list?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CheckRateLimit asks qurl-service whether a Slack user is currently
// allowed to mint another link, and returns the retry-after window
// when the answer is no. Per-user rate limiting is documented in
// Phase 3b of the plan.
func (ac *AdminClient) CheckRateLimit(ctx context.Context, slackUserID, teamID string) (bool, time.Duration, error) {
	body := map[string]string{
		fieldTeamID: teamID,
		fieldUserID: slackUserID,
	}
	var out struct {
		Allowed         bool `json:"allowed"`
		RetryAfterSecs  int  `json:"retry_after_seconds,omitempty"`
		RetryAfterMilli int  `json:"retry_after_ms,omitempty"`
	}
	if err := ac.do(ctx, http.MethodPost, "/internal/v1/admin/rate-limit/check", body, &out); err != nil {
		return false, 0, err
	}
	var retry time.Duration
	switch {
	case out.RetryAfterMilli > 0:
		retry = time.Duration(out.RetryAfterMilli) * time.Millisecond
	case out.RetryAfterSecs > 0:
		retry = time.Duration(out.RetryAfterSecs) * time.Second
	}
	return out.Allowed, retry, nil
}

// do is the low-level HTTP plumbing shared by every method on this
// client. Centralized here so a future change (mTLS, signed
// requests, distributed tracing headers, …) only edits one site.
func (ac *AdminClient) do(ctx context.Context, method, path string, body, out any) error {
	if ac.baseURL == "" {
		return errors.New("admin client: base URL not configured")
	}
	if ac.authToken == "" {
		return errors.New("admin client: internal service token not configured")
	}

	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, ac.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+ac.authToken)
	req.Header.Set("User-Agent", adminClientUserAgent)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := ac.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("admin request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return parseAdminError(resp.StatusCode, respBody)
	}

	if out == nil || len(respBody) == 0 {
		return nil
	}

	var env adminEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		// Fallback: some internal endpoints return raw JSON without
		// the envelope. Treat the whole body as data.
		if err2 := json.Unmarshal(respBody, out); err2 != nil {
			return fmt.Errorf("unmarshal response: %w", err2)
		}
		return nil
	}
	if env.Error != nil {
		return &AdminError{
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

// parseAdminError turns a non-2xx response into an [AdminError]. Tries
// the envelope shape first, falls back to status-text + raw body.
func parseAdminError(status int, body []byte) error {
	var env adminEnvelope
	if json.Unmarshal(body, &env) == nil && env.Error != nil {
		return &AdminError{
			StatusCode: status,
			Code:       env.Error.Code,
			Title:      env.Error.Title,
			Detail:     env.Error.Detail,
		}
	}
	return &AdminError{
		StatusCode: status,
		Title:      http.StatusText(status),
		Detail:     string(body),
	}
}
