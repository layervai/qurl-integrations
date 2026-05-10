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

// adminClientUserAgentName is the product portion of the User-Agent
// the admin client sends to qurl-service. Distinct from the
// customer-style `qurl-go-client/...` UA so internal traffic is
// grep-able in service-side logs. Mirrors the `name/version` shape
// that [shared/client.WithUserAgent] uses; the `version` half is
// plumbed in through [WithAdminUserAgent].
const adminClientUserAgentName = "qurl-slack-admin"

// defaultAdminClientVersion is the placeholder used when no explicit
// version is configured (e.g. tests, local `go run`). Production
// callers wire `runtime/debug.ReadBuildInfo`'s `vcs.revision` (or a
// build-time `-ldflags="-X"` string) into [WithAdminUserAgent].
const defaultAdminClientVersion = "dev"

// adminDefaultTimeout bounds a single internal call. Slack's outer
// budget is the 28s `response_url` window — this floor keeps a stuck
// admin endpoint from eating that whole budget.
const adminDefaultTimeout = 10 * time.Second

// Query-string field names shared across multiple GET requests.
// Lifted to constants so a server-side rename only edits one site,
// and goconst's repeated-string lint stays satisfied. JSON body
// fields are fixed in their struct tags rather than via these
// constants because the compiler will catch a rename in a tag.
const (
	fieldTeamID = "team_id"
	fieldUserID = "user_id"
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
	userAgent  string
	httpClient *http.Client
}

// AdminClientOption configures an [AdminClient].
type AdminClientOption func(*AdminClient)

// WithAdminHTTPClient injects a custom HTTP client for tests.
func WithAdminHTTPClient(c *http.Client) AdminClientOption {
	return func(ac *AdminClient) { ac.httpClient = c }
}

// WithAdminUserAgent overrides the User-Agent's `version` half. The
// product half is fixed at [adminClientUserAgentName] so service-side
// log grep stays simple; the version is whatever the caller passes
// (typically `runtime/debug.ReadBuildInfo`'s `vcs.revision` or a
// build-time ldflags string). Empty version falls back to
// [defaultAdminClientVersion].
func WithAdminUserAgent(version string) AdminClientOption {
	return func(ac *AdminClient) {
		if version == "" {
			version = defaultAdminClientVersion
		}
		ac.userAgent = adminClientUserAgentName + "/" + version
	}
}

// NewAdminClient builds an [AdminClient] for the given qurl-service
// base URL and internal-service token.
func NewAdminClient(baseURL, internalToken string, opts ...AdminClientOption) *AdminClient {
	ac := &AdminClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		authToken: internalToken,
		userAgent: adminClientUserAgentName + "/" + defaultAdminClientVersion,
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

// ErrEmptyAdminResponse is returned when a 2xx admin response carries
// an empty `data` field but the caller expected a populated payload.
// Distinguishes "endpoint shipped a buggy empty envelope" from "this
// endpoint legitimately returns no data" — the latter is signaled by
// passing `out=nil` to [AdminClient.do].
var ErrEmptyAdminResponse = errors.New("admin client: empty data field in 2xx response")

// Error returns a human-readable message. Includes the service-side
// `Code` field (e.g. "not_admin", "rate_limited") so log lines can
// be cross-referenced against qurl-service logs by error code rather
// than only by status text.
func (e *AdminError) Error() string {
	codeSuffix := ""
	if e.Code != "" {
		codeSuffix = " [" + e.Code + "]"
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s%s (%d): %s", e.Title, codeSuffix, e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("%s%s (%d)", e.Title, codeSuffix, e.StatusCode)
}

// CheckAdmin asks qurl-service whether `slackUserID` is a recognized
// admin for `teamID`. Returns the workspace's owner ID alongside.
//
// PR-3c.1: stub. The HTTP call is wired but the slash-command
// handler does not yet invoke this — that's PR-3c.3+.
func (ac *AdminClient) CheckAdmin(ctx context.Context, teamID, slackUserID string) (isAdmin bool, ownerID string, err error) {
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
	body := policyMutationBody(teamID, channelID, resourceID)
	var out struct {
		Allowed bool `json:"allowed"`
	}
	if err := ac.do(ctx, http.MethodPost, "/internal/v1/policy/resolve", body, &out); err != nil {
		return false, err
	}
	return out.Allowed, nil
}

// redeemBootstrapRequest is the typed wire shape for the redeem call.
// A struct (not `map[string]string`) so a server-side rename surfaces
// at compile time when the JSON tag here is updated in lockstep.
type redeemBootstrapRequest struct {
	Code   string `json:"code"`
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
}

// RedeemBootstrap exchanges a one-time bootstrap code for a workspace
// mapping row. Called from the `admin claim` modal submit handler.
// The code arrives via the modal's `plain_text_input` block (see
// [AdminClaimModal]) — never via slash-command text (Blocker #3) and
// never logged at the bot's logging boundary (the block_id is in
// [RedactedSubmissionBlockIDs]).
func (ac *AdminClient) RedeemBootstrap(ctx context.Context, code, teamID, slackUserID string) (*WorkspaceMapping, error) {
	body := redeemBootstrapRequest{
		Code:   code,
		TeamID: teamID,
		UserID: slackUserID,
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

// policyMutationRequest is the typed wire shape for allow/disallow.
// A struct lets the compiler catch a server-side field rename at the
// build step rather than runtime.
type policyMutationRequest struct {
	TeamID     string `json:"team_id"`
	ChannelID  string `json:"channel_id"`
	ResourceID string `json:"resource_id"`
}

// policyMutationBody is the shared body shape for allow/disallow.
// Centralizing it satisfies dupl and keeps the two methods in sync.
func policyMutationBody(teamID, channelID, resourceID string) policyMutationRequest {
	return policyMutationRequest{
		TeamID:     teamID,
		ChannelID:  channelID,
		ResourceID: resourceID,
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

// rateLimitCheckRequest is the typed body for the rate-limit endpoint.
type rateLimitCheckRequest struct {
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
}

// CheckRateLimit asks qurl-service whether a Slack user is currently
// allowed to mint another link, and returns the retry-after window
// when the answer is no. Per-user rate limiting is documented in
// Phase 3b of the plan.
func (ac *AdminClient) CheckRateLimit(ctx context.Context, slackUserID, teamID string) (bool, time.Duration, error) {
	body := rateLimitCheckRequest{
		TeamID: teamID,
		UserID: slackUserID,
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
	req.Header.Set("User-Agent", ac.userAgent)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := ac.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("admin request: %w", err)
	}
	defer func() {
		// Drain residue past the LimitReader cap so the connection
		// can be reused by HTTP keep-alive instead of dropped on
		// close. Low-impact at slash-command volumes but cheap
		// hygiene against future high-frequency endpoints.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

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

	// All admin endpoints ship enveloped JSON — `{"data": ...}` on
	// success, `{"error": {...}}` on failure. The envelope is the
	// load-bearing contract; a previous version had a "raw JSON
	// fallback" branch but that branch was unreachable for any
	// valid JSON object (adminEnvelope's fields are all optional
	// and would unmarshal cleanly from `{"is_admin":true}`,
	// silently masking a misshapen response). The contract is now
	// explicit: enveloped or [ErrEmptyAdminResponse].
	var env adminEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return fmt.Errorf("unmarshal envelope: %w", err)
	}
	if env.Error != nil {
		// Prefer the HTTP status code over `env.Error.Status` so the
		// surfaced status is consistent with [parseAdminError]
		// (which uses the HTTP status verbatim). If the body's
		// status disagrees with the HTTP status, the wire-level
		// HTTP code is the authoritative one. The body's status
		// field is otherwise unused — we keep it as part of the
		// envelope shape only because it mirrors what
		// shared/client emits for customer-facing errors.
		return &AdminError{
			StatusCode: resp.StatusCode,
			Code:       env.Error.Code,
			Title:      env.Error.Title,
			Detail:     env.Error.Detail,
		}
	}
	// `out != nil` (checked above) means the caller is expecting a
	// populated payload — surface a sentinel rather than silently
	// leaving zero values when the server returns `{"data": null}`
	// or `{}`. A buggy internal endpoint that ships an empty
	// envelope must not be confused with a successful zero-value
	// response.
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return ErrEmptyAdminResponse
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
