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

	// errCodeAPIKeyLimit is the qurl-service error-envelope `code` returned
	// when key provisioning is refused because the owner is already at
	// their plan's API-key cap (free tier = 3). Mirrors qurl-service's
	// validation.ErrorCodeAPIKeyLimit. Both that endpoint's qurl:write
	// scope gate AND this quota check surface as HTTP 403, so the status
	// code alone can't disambiguate — the body `code` is the only signal.
	errCodeAPIKeyLimit      = "api_key_limit"
	errCodeAlreadyExists    = "already_exists"
	errCodeBindingsDisabled = "bindings_disabled"
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
	defer func() {
		// Bounded drain — see callback.go drainCap rationale.
		_, _ = io.CopyN(io.Discard, resp.Body, drainCap)
		_ = resp.Body.Close()
	}()
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
// and returns the qURL API key minted for that binding.
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
	req.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := m.client().Do(req)
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("do request: %w", err)
	}
	defer func() {
		// Bounded drain — see callback.go drainCap rationale.
		_, _ = io.CopyN(io.Discard, resp.Body, drainCap)
		_ = resp.Body.Close()
	}()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, minterBodyLimit+1))
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("read body: %w", err)
	}
	if len(rb) > minterBodyLimit {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("qurl-service /v1/external-identity-bindings response exceeded %d bytes", minterBodyLimit)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		if shouldFallbackToLegacyMint(resp.StatusCode, rb) {
			return m.mintLegacyAPIKey(ctx, accessToken, displayName, apiKeyScopes(), legacyFallbackIdempotencyKey(teamID))
		}
		code := errorEnvelopeCode(rb)
		if code == errCodeAPIKeyLimit && resp.StatusCode == http.StatusForbidden {
			return WorkspaceAPIKeyMint{}, fmt.Errorf("%w (status %d)", ErrAPIKeyLimitReached, resp.StatusCode)
		}
		if code == errCodeAlreadyExists && resp.StatusCode == http.StatusConflict {
			return WorkspaceAPIKeyMint{}, fmt.Errorf("%w (status %d)", ErrExternalIdentityAlreadyBound, resp.StatusCode)
		}
		return WorkspaceAPIKeyMint{}, fmt.Errorf("qurl-service /v1/external-identity-bindings returned %d", resp.StatusCode)
	}
	var br bindingResponse
	if err := json.Unmarshal(rb, &br); err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("parse response: %w", err)
	}
	if br.APIKey.Plaintext == "" || br.APIKey.KeyID == "" {
		return WorkspaceAPIKeyMint{}, errors.New("qurl-service returned empty api_key plaintext or key_id")
	}
	if br.APIKey.KeyPrefix == "" {
		br.APIKey.KeyPrefix = storedAPIKeyPrefix(br.APIKey.Plaintext)
	}
	return WorkspaceAPIKeyMint{
		APIKey:        br.APIKey.Plaintext,
		KeyID:         br.APIKey.KeyID,
		KeyPrefix:     br.APIKey.KeyPrefix,
		BindingBacked: true,
	}, nil
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
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("do request: %w", err)
	}
	defer func() {
		// Bounded drain — see callback.go drainCap rationale.
		_, _ = io.CopyN(io.Discard, resp.Body, drainCap)
		_ = resp.Body.Close()
	}()
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
		if apiKeyLimitError(rb) {
			return WorkspaceAPIKeyMint{}, fmt.Errorf("%w (status %d)", ErrAPIKeyLimitReached, resp.StatusCode)
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
	defer func() {
		// Bounded drain — see callback.go drainCap rationale.
		_, _ = io.CopyN(io.Discard, resp.Body, drainCap)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("qurl-service DELETE /v1/api-keys returned %d", resp.StatusCode)
	}
	return nil
}

func bindingIdempotencyKey(teamID string) string {
	// qurl-service requires a 32+ character idempotency key. Slack team IDs
	// are shorter, so hash to a stable fixed-width key with a readable prefix.
	return workspaceIdempotencyKey("slack-workspace-binding-v1-", teamID)
}

func legacyFallbackIdempotencyKey(teamID string) string {
	// Keep the legacy fallback idempotency domain separate from the binding
	// domain even if qurl-service stores idempotency keys globally.
	return workspaceIdempotencyKey("slack-workspace-legacy-v1-", teamID)
}

func workspaceIdempotencyKey(prefix, teamID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(teamID)))
	return prefix + hex.EncodeToString(sum[:])
}

func shouldFallbackToLegacyMint(status int, body []byte) bool {
	if status == http.StatusNotFound {
		// During rollout, an older qurl-service has no route and returns an
		// unstructured 404. Intentionally treat that as legacy-compatible
		// while the new endpoint rolls out. TODO(#705): remove this path
		// after rollout. If a deployed route returns a structured qURL error
		// envelope, surface it instead of minting a legacy key.
		return errorEnvelopeCode(body) == ""
	}
	if status != http.StatusServiceUnavailable {
		return false
	}
	return errorEnvelopeCode(body) == errCodeBindingsDisabled
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

func errorEnvelopeCode(body []byte) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Error.Code
}
