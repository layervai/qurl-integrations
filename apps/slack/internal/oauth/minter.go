package oauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	minterTimeout = 15 * time.Second
	// minterBodyLimit caps the qurl-service key-provision response body.
	// The real response is ~few hundred bytes; 8 KiB is generous head-
	// room without leaving an unbounded read on a misbehaving upstream.
	minterBodyLimit = 8 << 10
	// apiKeyListBodyLimit caps the revoked-key list response used only to
	// distinguish "already revoked by this owner" from qurl-service's 404
	// not-found/wrong-owner delete response.
	apiKeyListBodyLimit = 64 << 10
	// apiKeyRevokedMaxPages caps owner-scoped revoked-key pagination so a
	// misbehaving upstream with has_more=true and a non-advancing cursor fails
	// quickly instead of burning the whole request context. qurl-service lists
	// API keys by created_at descending, not revoked_at, so this scan is a
	// bounded best-effort confirmation until qurl-service#946 adds a direct
	// owner-scoped key-status lookup.
	apiKeyRevokedMaxPages = 10

	// errCodeAPIKeyLimit is the qurl-service error-envelope `code` returned
	// when key provisioning is refused because the owner is already at
	// their plan's API-key cap (free tier = 3). Mirrors qurl-service's
	// validation.ErrorCodeAPIKeyLimit. Both that endpoint's qurl:write
	// scope gate AND this quota check surface as HTTP 403, so the status
	// code alone can't disambiguate — the body `code` is the only signal.
	errCodeAPIKeyLimit = "api_key_limit"
	// errCodeAlreadyExists must pair with HTTP 409. It means qurl-service has
	// already bound the workspace identity and the Slack app should show the
	// administrator recovery path instead of minting a second legacy key.
	errCodeAlreadyExists = "already_exists"
	// errCodeBindingsDisabled must pair with HTTP 503. It is the stable dark-
	// launch signal that allows Slack setup to fall back to the legacy API-key
	// endpoint without also masking transient qurl-service outages.
	errCodeBindingsDisabled = "bindings_disabled"

	// structuredErrorEnvelopeCode is an internal sentinel for a bounded
	// qurl-service `{"error":{...}}` body whose concrete code could not be
	// recovered. Treat it as structured so a truncated detail-first envelope
	// cannot be misclassified as a route-missing 404 and fall back to legacy
	// minting.
	structuredErrorEnvelopeCode = "__structured_error_envelope__"
)

// ErrAPIKeyLimitReached is returned when qurl-service refuses provisioning
// because the account holds the maximum number of API keys for its plan. The
// OAuth callback maps this to an actionable "revoke a key" page rather than
// the generic "try again" message — retrying never clears a quota, so the old
// advice was actively misleading.
var ErrAPIKeyLimitReached = errors.New("qurl-service API key limit reached")

// ErrExternalIdentityAlreadyBound is returned when qurl-service reports that
// the Slack workspace already has an external identity binding, but the bot
// cannot recover a usable stored workspace key locally.
var ErrExternalIdentityAlreadyBound = errors.New("qurl-service external identity already bound")

// ErrStoredAPIKeyInvalid is returned by ValidateAPIKey only when the
// workspace's stored qURL API key is empty or rejected by qurl-service as
// unauthenticated. The callback may attempt replacement provisioning for this
// case; other validation errors, including 403, are treated as non-replaceable
// failures because they may indicate a scope/server issue on an otherwise live
// key.
var ErrStoredAPIKeyInvalid = errors.New("stored qURL API key is invalid")

// ErrAPIKeyNotFound is returned by RevokeAPIKey when qurl-service returns 404.
// qurl-service deliberately uses the same status for missing keys and keys
// owned by another account, so callers must not treat it as revoke success
// without an owner-scoped confirmation.
var ErrAPIKeyNotFound = errors.New("qurl-service API key not found")

// HTTPAPIKeyMinter is the production QURLAPIKeyMinter, calling qurl-service
// with the Auth0 access_token as Bearer.
type HTTPAPIKeyMinter struct {
	// BaseURL is the qurl-service origin (e.g. https://api.layerv.ai).
	// Trailing slashes are tolerated — joinAPIKeyURL uses url.JoinPath.
	BaseURL string
	// HTTPClient overrides the default *http.Client (15s timeout). The
	// default carries a request timeout so a hung qurl-service can't
	// pin the request goroutine indefinitely.
	HTTPClient *http.Client

	// defaultClient is the lazily-initialized fallback when HTTPClient
	// is unset. Memoized via defaultOnce so a per-call allocation
	// doesn't churn under load.
	defaultClient *http.Client
	defaultOnce   sync.Once
}

// DependencyAuthFailureError marks an unexpected qurl-service auth-class
// rejection so callers can emit the structured CloudWatch audit event without
// parsing human error strings.
type DependencyAuthFailureError struct {
	Method     string
	Path       string
	StatusCode int
	Code       string
	RequestID  string
}

func (e *DependencyAuthFailureError) Error() string {
	return fmt.Sprintf("qurl-service %s %s returned %d", e.Method, e.Path, e.StatusCode)
}

func dependencyAuthFailureError(method, path string, status int, code, requestID string) error {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return nil
	}
	return &DependencyAuthFailureError{
		Method:     method,
		Path:       path,
		StatusCode: status,
		Code:       code,
		RequestID:  requestID,
	}
}

type mintRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

type bindingRequest struct {
	Provider    string `json:"provider"`
	ExternalID  string `json:"external_id"`
	DisplayName string `json:"display_name"`
}

type mintResponse struct {
	Data struct {
		APIKey    string `json:"api_key"`
		KeyID     string `json:"key_id"`
		KeyPrefix string `json:"key_prefix"`
	} `json:"data"`
}

type listAPIKeysResponse struct {
	Data []struct {
		KeyID  string `json:"key_id"`
		Status string `json:"status"`
	} `json:"data"`
	Meta struct {
		HasMore    bool   `json:"has_more"`
		NextCursor string `json:"next_cursor"`
	} `json:"meta"`
}

// bindingResponse is the POST /v1/external-identity-bindings success shape.
// Unlike the legacy /v1/api-keys response, the API key fields are top-level
// under api_key rather than nested in data.
type bindingResponse struct {
	APIKey struct {
		Plaintext string `json:"plaintext"`
		KeyID     string `json:"key_id"`
		KeyPrefix string `json:"key_prefix"`
	} `json:"api_key"`
}

func (m *HTTPAPIKeyMinter) client() *http.Client {
	if m.HTTPClient != nil {
		return m.HTTPClient
	}
	m.defaultOnce.Do(func() {
		m.defaultClient = &http.Client{Timeout: minterTimeout}
	})
	return m.defaultClient
}

// joinAPIKeyURL composes BaseURL + "/v1/api-keys[/keyID]" so a BaseURL
// that ends with a slash doesn't produce a "//v1/api-keys" path.
func (m *HTTPAPIKeyMinter) joinAPIKeyURL(elem ...string) (string, error) {
	parts := append([]string{"v1", "api-keys"}, elem...)
	u, err := url.JoinPath(m.BaseURL, parts...)
	if err != nil {
		return "", fmt.Errorf("compose qurl-service URL: %w", err)
	}
	return u, nil
}

// joinExternalBindingURL composes BaseURL + "/v1/external-identity-bindings".
func (m *HTTPAPIKeyMinter) joinExternalBindingURL() (string, error) {
	u, err := url.JoinPath(m.BaseURL, "v1", "external-identity-bindings")
	if err != nil {
		return "", fmt.Errorf("compose qurl-service URL: %w", err)
	}
	return u, nil
}

// ValidateAPIKey performs a cheap authenticated read with the stored workspace
// key. This is an auth-liveness check for keys minted by this bot. The
// qurl-service contract is: GET /v1/quota is reachable with qurl:read, and
// missing/expired/revoked API keys fail API-key auth with 401. 403 stays a
// non-replaceable error so scope or service drift can't burn another
// account-level API-key slot on an otherwise live key.
func (m *HTTPAPIKeyMinter) ValidateAPIKey(ctx context.Context, apiKey string) error {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ErrStoredAPIKeyInvalid
	}
	reqURL, err := url.JoinPath(m.BaseURL, "v1", "quota")
	if err != nil {
		return fmt.Errorf("compose qurl-service URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := m.client().Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer drainAndCloseResponse(resp)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w (status %d)", ErrStoredAPIKeyInvalid, resp.StatusCode)
	case resp.StatusCode >= 400:
		return fmt.Errorf("qurl-service GET /v1/quota returned %d", resp.StatusCode)
	default:
		return nil
	}
}

// MintWorkspaceAPIKey creates the Slack workspace external identity binding
// and returns the qURL API key minted for that binding. qurl-service retains
// successful binding responses in its idempotency store for the 24h setup-retry
// window, so identical retries recover the same plaintext key instead of
// returning already_exists. qurl-service scopes those records by authenticated
// owner (pinned in qurl-service's APIKeyIdempotencyPKWithPurpose tests), so
// another qURL principal cannot replay this workspace's plaintext.
// qurl-service also assigns provider scopes server-side; Slack bindings are
// pinned to the same qurl:read/qurl:write set requested by the legacy fallback.
// After that replay window, the existing binding owns recovery: qurl-service
// returns already_exists until the binding is rotated or revoked.
func (m *HTTPAPIKeyMinter) MintWorkspaceAPIKey(ctx context.Context, accessToken, teamID string) (WorkspaceAPIKeyMint, error) {
	teamID = strings.TrimSpace(teamID)
	if teamID == "" {
		return WorkspaceAPIKeyMint{}, errors.New("MintWorkspaceAPIKey: empty teamID")
	}
	displayName := "Slack workspace " + teamID
	idempotencyKey := bindingIdempotencyKey(teamID)

	body, err := json.Marshal(bindingRequest{
		Provider:    "slack",
		ExternalID:  teamID,
		DisplayName: displayName,
	})
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("marshal: %w", err)
	}
	reqURL, err := m.joinExternalBindingURL()
	if err != nil {
		return WorkspaceAPIKeyMint{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := setIdempotencyKeyHeader(req, idempotencyKey); err != nil {
		return WorkspaceAPIKeyMint{}, err
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("do request: %w", err)
	}
	defer drainAndCloseResponse(resp)
	rb, err := io.ReadAll(io.LimitReader(resp.Body, minterBodyLimit+1))
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("read body: %w", err)
	}
	bodyOversized := len(rb) > minterBodyLimit
	if bodyOversized {
		rb = rb[:minterBodyLimit]
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		if bodyOversized {
			return WorkspaceAPIKeyMint{}, fmt.Errorf("qurl-service /v1/external-identity-bindings response exceeded %d bytes", minterBodyLimit)
		}
		fields := errorEnvelopeFields(rb)
		code := fields.Code
		if shouldFallbackToLegacyMint(resp.StatusCode, code) {
			// Legacy fallback keys are revoked on local persist failure, so omit
			// Idempotency-Key and preserve mint-fresh retry behavior.
			return m.mintLegacyAPIKey(ctx, accessToken, displayName, apiKeyScopes(), "")
		}
		if code == errCodeAPIKeyLimit && resp.StatusCode == http.StatusForbidden {
			return WorkspaceAPIKeyMint{}, fmt.Errorf("%w (status %d)", ErrAPIKeyLimitReached, resp.StatusCode)
		}
		if code == errCodeAlreadyExists && resp.StatusCode == http.StatusConflict {
			return WorkspaceAPIKeyMint{}, fmt.Errorf("%w (status %d)", ErrExternalIdentityAlreadyBound, resp.StatusCode)
		}
		if authErr := dependencyAuthFailureError(http.MethodPost, "/v1/external-identity-bindings", resp.StatusCode, code, fields.RequestID); authErr != nil {
			return WorkspaceAPIKeyMint{}, authErr
		}
		return WorkspaceAPIKeyMint{}, fmt.Errorf("qurl-service /v1/external-identity-bindings returned %d", resp.StatusCode)
	}
	// Success bodies never participate in fallback; reject oversized responses
	// before parsing the api_key payload.
	if bodyOversized {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("qurl-service /v1/external-identity-bindings response exceeded %d bytes", minterBodyLimit)
	}
	return bindingMintFromResponse(rb)
}

func bindingMintFromResponse(body []byte) (WorkspaceAPIKeyMint, error) {
	var br bindingResponse
	if err := json.Unmarshal(body, &br); err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("parse response: %w", err)
	}
	if br.APIKey.Plaintext == "" || br.APIKey.KeyID == "" {
		return WorkspaceAPIKeyMint{}, errors.New("qurl-service returned empty api_key plaintext or key_id")
	}
	keyPrefix := br.APIKey.KeyPrefix
	if keyPrefix == "" {
		keyPrefix = storedAPIKeyPrefix(br.APIKey.Plaintext)
	}
	if keyPrefix == "" {
		return WorkspaceAPIKeyMint{}, errors.New("qurl-service returned empty api_key key_prefix")
	}
	return WorkspaceAPIKeyMint{
		APIKey:        br.APIKey.Plaintext,
		KeyID:         br.APIKey.KeyID,
		KeyPrefix:     keyPrefix,
		BindingBacked: true,
	}, nil
}

// MintWorkspaceReplacementAPIKey mints a fresh workspace key for an explicit
// owner-requested rotation after the previous key has already been revoked.
// It deliberately does not hit the external binding create endpoint: a healthy
// existing binding owns first-setup replay and returns already_exists here.
// qURL request authorization only checks the API key and scopes, so this
// standalone qurl:read/write/resolve key is a valid workspace credential after
// Slack stores it.
func (m *HTTPAPIKeyMinter) MintWorkspaceReplacementAPIKey(ctx context.Context, accessToken, teamID, oldKeyID string) (WorkspaceAPIKeyMint, error) {
	teamID = strings.TrimSpace(teamID)
	if teamID == "" {
		return WorkspaceAPIKeyMint{}, errors.New("MintWorkspaceReplacementAPIKey: empty teamID")
	}
	oldKeyID = strings.TrimSpace(oldKeyID)
	if oldKeyID == "" {
		return WorkspaceAPIKeyMint{}, errors.New("MintWorkspaceReplacementAPIKey: empty oldKeyID")
	}
	return m.mintLegacyAPIKey(ctx, accessToken, "Slack workspace "+teamID, apiKeyScopes(), replacementIdempotencyKey(teamID, oldKeyID))
}

// mintLegacyAPIKey posts to POST /v1/api-keys. apiKey and keyID must be
// present for success — a missing keyID would leave us unable to revoke an
// orphan key if the subsequent DDB persist fails.
func (m *HTTPAPIKeyMinter) mintLegacyAPIKey(ctx context.Context, accessToken, name string, scopes []string, idempotencyKey string) (WorkspaceAPIKeyMint, error) {
	body, err := json.Marshal(mintRequest{Name: name, Scopes: scopes})
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("marshal: %w", err)
	}
	reqURL, err := m.joinAPIKeyURL()
	if err != nil {
		return WorkspaceAPIKeyMint{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := setIdempotencyKeyHeader(req, idempotencyKey); err != nil {
		return WorkspaceAPIKeyMint{}, err
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("do request: %w", err)
	}
	defer drainAndCloseResponse(resp)
	// Read limit+1 so an exact-cap legitimate body isn't misclassified
	// as truncated — see exchangeAuth0Code for the rationale.
	rb, err := io.ReadAll(io.LimitReader(resp.Body, minterBodyLimit+1))
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("read body: %w", err)
	}
	if len(rb) > minterBodyLimit {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("qurl-service /v1/api-keys response exceeded %d bytes", minterBodyLimit)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		// Preserve the legacy endpoint's historical code-only limit
		// classification; the binding endpoint uses stricter status+code
		// pairing because its error contract is new and controlled.
		if apiKeyLimitError(rb) {
			return WorkspaceAPIKeyMint{}, fmt.Errorf("%w (status %d)", ErrAPIKeyLimitReached, resp.StatusCode)
		}
		fields := errorEnvelopeFields(rb)
		if authErr := dependencyAuthFailureError(http.MethodPost, "/v1/api-keys", resp.StatusCode, fields.Code, fields.RequestID); authErr != nil {
			return WorkspaceAPIKeyMint{}, authErr
		}
		return WorkspaceAPIKeyMint{}, fmt.Errorf("qurl-service /v1/api-keys returned %d", resp.StatusCode)
	}
	var mr mintResponse
	if err := json.Unmarshal(rb, &mr); err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("parse response: %w", err)
	}
	if mr.Data.APIKey == "" || mr.Data.KeyID == "" {
		return WorkspaceAPIKeyMint{}, errors.New("qurl-service returned empty api_key or key_id")
	}
	if mr.Data.KeyPrefix == "" {
		mr.Data.KeyPrefix = storedAPIKeyPrefix(mr.Data.APIKey)
	}
	if mr.Data.KeyPrefix == "" {
		return WorkspaceAPIKeyMint{}, errors.New("qurl-service returned empty key_prefix")
	}
	return WorkspaceAPIKeyMint{
		APIKey:    mr.Data.APIKey,
		KeyID:     mr.Data.KeyID,
		KeyPrefix: mr.Data.KeyPrefix,
	}, nil
}

// RevokeAPIKey best-effort deletes the minted key. Matches the Discord
// orphan-cleanup pattern.
func (m *HTTPAPIKeyMinter) RevokeAPIKey(ctx context.Context, accessToken, keyID string) error {
	if keyID == "" {
		return errors.New("RevokeAPIKey: empty keyID")
	}
	reqURL, err := m.joinAPIKeyURL(keyID)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := m.client().Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer drainAndCloseResponse(resp)
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w (status %d)", ErrAPIKeyNotFound, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		if authErr := dependencyAuthFailureError(http.MethodDelete, "/v1/api-keys/:id", resp.StatusCode, "", ""); authErr != nil {
			return authErr
		}
		return fmt.Errorf("qurl-service DELETE /v1/api-keys returned %d", resp.StatusCode)
	}
	return nil
}

// APIKeyRevoked reports whether keyID appears in the authenticated owner's
// revoked-key list. It is intentionally separate from DELETE 404 handling:
// qurl-service returns 404 for both already-revoked and wrong-owner keys, and
// only this owner-scoped list check can distinguish those cases safely.
// TODO(upstream-contract): prefer an owner-scoped GET /v1/api-keys/{key_id}
// status endpoint when qurl-service exposes one (tracked at
// layervai/qurl-service#946). Until then, the owner-scoped list is the current
// safe contract, but it is ordered by key creation time rather than revoke time,
// so old workspace keys can exceed this bounded scan and require operator
// verification instead of another admin retry.
func (m *HTTPAPIKeyMinter) APIKeyRevoked(ctx context.Context, accessToken, keyID string) (bool, error) {
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return false, errors.New("APIKeyRevoked: empty keyID")
	}
	var cursor string
	for page := 0; page < apiKeyRevokedMaxPages; page++ {
		reqURL, err := m.joinAPIKeyURL()
		if err != nil {
			return false, err
		}
		u, err := url.Parse(reqURL)
		if err != nil {
			return false, fmt.Errorf("parse qurl-service URL: %w", err)
		}
		q := u.Query()
		// Ask qurl-service for revoked rows, but keep the per-item status check
		// in readRevokedAPIKeyList as the correctness guard if the filter is
		// ignored or broadened upstream.
		q.Set("status", "revoked")
		q.Set("limit", "100")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
		if err != nil {
			return false, fmt.Errorf("new request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Accept", "application/json")
		resp, err := m.client().Do(req)
		if err != nil {
			return false, fmt.Errorf("do request: %w", err)
		}
		found, nextCursor, hasMore, err := func() (bool, string, bool, error) {
			defer drainAndCloseResponse(resp)
			return readRevokedAPIKeyList(resp, keyID)
		}()
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
		if !hasMore || nextCursor == "" {
			return false, nil
		}
		if nextCursor == cursor {
			return false, errors.New("qurl-service revoked API key pagination did not advance")
		}
		cursor = nextCursor
	}
	slog.Warn("oauth/minter revoked API key scan page cap exceeded",
		"key_id", keyID,
		"max_pages", apiKeyRevokedMaxPages,
		"operator_action", "confirm qurl-service revoked-key ordering or use owner-scoped key lookup")
	return false, fmt.Errorf("qurl-service revoked API key pagination exceeded %d pages", apiKeyRevokedMaxPages)
}

func readRevokedAPIKeyList(resp *http.Response, keyID string) (found bool, nextCursor string, hasMore bool, err error) {
	if resp.StatusCode >= 400 {
		return false, "", false, fmt.Errorf("qurl-service GET /v1/api-keys returned %d", resp.StatusCode)
	}
	rb, err := io.ReadAll(io.LimitReader(resp.Body, apiKeyListBodyLimit+1))
	if err != nil {
		return false, "", false, fmt.Errorf("read body: %w", err)
	}
	if len(rb) > apiKeyListBodyLimit {
		return false, "", false, fmt.Errorf("qurl-service /v1/api-keys response exceeded %d bytes", apiKeyListBodyLimit)
	}
	var lr listAPIKeysResponse
	if err := json.Unmarshal(rb, &lr); err != nil {
		return false, "", false, fmt.Errorf("parse response: %w", err)
	}
	for _, key := range lr.Data {
		if key.KeyID == keyID && strings.EqualFold(key.Status, "revoked") {
			return true, "", false, nil
		}
	}
	return false, lr.Meta.NextCursor, lr.Meta.HasMore, nil
}

func drainAndCloseResponse(resp *http.Response) {
	// Bounded drain — see callback.go drainCap rationale.
	_, _ = io.CopyN(io.Discard, resp.Body, drainCap)
	_ = resp.Body.Close()
}

func bindingIdempotencyKey(teamID string) string {
	// qurl-service requires a 32+ character idempotency key. Slack team IDs
	// are shorter, so hash to a stable fixed-width key with a readable prefix.
	sum := sha256.Sum256([]byte(teamID))
	return "slack-workspace-binding-v1-" + hex.EncodeToString(sum[:])
}

func replacementIdempotencyKey(teamID, oldKeyID string) string {
	// Stable across --rotate retries while DDB still stores oldKeyID. If a
	// replacement is minted but Slack fails to persist it, callers must keep
	// this replay recoverable; revoking the replacement would make qurl-service
	// replay a revoked key for the idempotency TTL. The raw IDs are non-secret
	// Slack/qURL identifiers; length-prefixing keeps the unhashed value
	// unambiguous without reintroducing weak-hash scanner noise. If upstream ID
	// lengths ever exceed qurl-service's idempotency-header cap, validation
	// fails closed before revoke/mint side effects.
	return fmt.Sprintf("slack-workspace-rotate-replacement-v1-t%d-%s-k%d-%s", len(teamID), teamID, len(oldKeyID), oldKeyID)
}

func setIdempotencyKeyHeader(req *http.Request, key string) error {
	if key == "" {
		return nil
	}
	if err := validateIdempotencyKey(key); err != nil {
		return err
	}
	req.Header.Set("Idempotency-Key", key)
	return nil
}

func validateIdempotencyKey(key string) error {
	if len(key) < 32 || len(key) > 256 {
		return fmt.Errorf("idempotency key length %d outside qurl-service range 32-256", len(key))
	}
	for i, r := range key {
		if r <= ' ' || r >= 0x7f {
			return fmt.Errorf("idempotency key contains non-header-safe byte at offset %d", i)
		}
	}
	return nil
}

func shouldFallbackToLegacyMint(status int, errorCode string) bool {
	if status == http.StatusNotFound {
		// During rollout, an older qurl-service has no route and returns a
		// 404 that is not a qURL error envelope. Intentionally treat any 404
		// without a qURL envelope code as legacy-compatible while the new
		// endpoint rolls out, even though that can also catch an infra 404.
		// TODO(#705): remove this path as soon as the binding route is live
		// everywhere. A rollback during a persist-failure retry can otherwise
		// fall back to a legacy key instead of replaying the binding. If a
		// deployed route returns a structured qURL error envelope, surface it
		// instead of minting a legacy key.
		return errorCode == ""
	}
	if status != http.StatusServiceUnavailable {
		return false
	}
	return errorCode == errCodeBindingsDisabled
}

// apiKeyLimitError reports whether body is a qurl-service error envelope
// carrying the api-key-limit code. The envelope shape is
// {"error":{"code":"...", ...}} — qurl-service's own nested form, NOT RFC
// 7807 problem+json (where code/title/detail are top-level, not nested under
// "error"), so the match is driven by the parsed JSON shape, not the
// response Content-Type. A body that doesn't parse to that shape (e.g. a
// bare {"error":"forbidden"} string, or non-JSON) returns false so the
// caller falls back to the generic status-code error.
func apiKeyLimitError(body []byte) bool {
	return errorEnvelopeCode(body) == errCodeAPIKeyLimit
}

type errorEnvelopeFieldsResult struct {
	Code      string
	RequestID string
}

// errorEnvelopeCode returns qurl-service's nested error.code when present.
// It returns structuredErrorEnvelopeCode for qURL-like shapes that must fail
// closed (JSON strings and code-less error objects), and "" for generic
// route-missing bodies that may use the rollout fallback.
func errorEnvelopeCode(body []byte) string {
	return errorEnvelopeFields(body).Code
}

// errorEnvelopeFields returns the nested qurl-service error code plus the
// request_id correlation handle when the response body carries one.
func errorEnvelopeFields(body []byte) errorEnvelopeFieldsResult {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return errorEnvelopeFieldsResult{}
	}
	var message string
	if err := json.Unmarshal(trimmed, &message); err == nil {
		return errorEnvelopeFieldsResult{Code: structuredErrorEnvelopeCode}
	}
	var env struct {
		Error json.RawMessage `json:"error"`
		Meta  struct {
			RequestID string `json:"request_id"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(trimmed, &env); err == nil {
		out := errorEnvelopeFieldsResult{RequestID: env.Meta.RequestID}
		if len(env.Error) == 0 {
			return out
		}
		var problem struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(env.Error, &problem); err != nil {
			// Treat {"error":"..."} as a generic, non-qURL-envelope 404 so
			// the rollout bridge still covers old route-missing JSON bodies.
			return out
		}
		if problem.Code != "" {
			out.Code = problem.Code
			return out
		}
		if bytes.HasPrefix(bytes.TrimSpace(env.Error), []byte("{")) {
			out.Code = structuredErrorEnvelopeCode
			return out
		}
		return out
	}
	return errorEnvelopeFieldsResult{}
}
