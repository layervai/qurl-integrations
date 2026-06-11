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
	req.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := m.client().Do(req)
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("do request: %w", err)
	}
	rb, err := io.ReadAll(io.LimitReader(resp.Body, minterBodyLimit+1))
	drainAndCloseResponse(resp)
	if err != nil {
		return WorkspaceAPIKeyMint{}, fmt.Errorf("read body: %w", err)
	}
	bodyOversized := len(rb) > minterBodyLimit
	if bodyOversized {
		rb = rb[:minterBodyLimit]
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		code := errorEnvelopeCode(rb)
		if shouldFallbackToLegacyMint(resp.StatusCode, code) {
			return m.mintLegacyAPIKey(ctx, accessToken, displayName, apiKeyScopes(), legacyFallbackIdempotencyKey(teamID))
		}
		if bodyOversized {
			return WorkspaceAPIKeyMint{}, fmt.Errorf("qurl-service /v1/external-identity-bindings response exceeded %d bytes", minterBodyLimit)
		}
		if code == errCodeAPIKeyLimit && resp.StatusCode == http.StatusForbidden {
			return WorkspaceAPIKeyMint{}, fmt.Errorf("%w (status %d)", ErrAPIKeyLimitReached, resp.StatusCode)
		}
		if code == errCodeAlreadyExists && resp.StatusCode == http.StatusConflict {
			return WorkspaceAPIKeyMint{}, fmt.Errorf("%w (status %d)", ErrExternalIdentityAlreadyBound, resp.StatusCode)
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
	if resp.StatusCode >= 400 {
		return fmt.Errorf("qurl-service DELETE /v1/api-keys returned %d", resp.StatusCode)
	}
	return nil
}

func drainAndCloseResponse(resp *http.Response) {
	// Bounded drain — see callback.go drainCap rationale.
	_, _ = io.CopyN(io.Discard, resp.Body, drainCap)
	_ = resp.Body.Close()
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
	sum := sha256.Sum256([]byte(teamID))
	return prefix + hex.EncodeToString(sum[:])
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

func errorEnvelopeCode(body []byte) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil {
		return env.Error.Code
	}
	return partialErrorEnvelopeCode(body)
}

func partialErrorEnvelopeCode(body []byte) string {
	// Fallback classification runs on a bounded body. Keep a streaming parser
	// so a structured qURL error is not mistaken for a route-missing 404 just
	// because the response was truncated before full JSON unmarshalling.
	dec := json.NewDecoder(bytes.NewReader(body))
	if !consumeJSONObjectStart(dec) {
		return ""
	}
	for dec.More() {
		key, ok := nextJSONKey(dec)
		if !ok {
			return ""
		}
		if key == "error" {
			return partialErrorCodeFromObject(dec)
		}
		if err := skipJSONValue(dec); err != nil {
			return ""
		}
	}
	return ""
}

func partialErrorCodeFromObject(dec *json.Decoder) string {
	if !consumeJSONObjectStart(dec) {
		return ""
	}
	for dec.More() {
		key, ok := nextJSONKey(dec)
		if !ok {
			return structuredErrorEnvelopeCode
		}
		if key != "code" {
			if err := skipJSONValue(dec); err != nil {
				return structuredErrorEnvelopeCode
			}
			continue
		}
		var code string
		if err := dec.Decode(&code); err != nil {
			return structuredErrorEnvelopeCode
		}
		return code
	}
	return structuredErrorEnvelopeCode
}

func consumeJSONObjectStart(dec *json.Decoder) bool {
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	delim, ok := tok.(json.Delim)
	return ok && delim == '{'
}

func nextJSONKey(dec *json.Decoder) (string, bool) {
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	key, ok := tok.(string)
	return key, ok
}

func skipJSONValue(dec *json.Decoder) error {
	var raw json.RawMessage
	return dec.Decode(&raw)
}
