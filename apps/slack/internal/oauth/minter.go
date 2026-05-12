package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	minterTimeout = 15 * time.Second
	// minterBodyLimit caps the qurl-service /v1/api-keys response body.
	// The real response is ~few hundred bytes; 8 KiB is generous head-
	// room without leaving an unbounded read on a misbehaving upstream.
	minterBodyLimit = 8 << 10
)

// HTTPAPIKeyMinter is the production QURLAPIKeyMinter, calling
// qurl-service /v1/api-keys with the Auth0 access_token as Bearer.
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

type mintResponse struct {
	Data struct {
		APIKey    string `json:"api_key"`
		KeyID     string `json:"key_id"`
		KeyPrefix string `json:"key_prefix"`
	} `json:"data"`
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

// MintAPIKey posts to POST /v1/api-keys with Bearer = accessToken.
// Returns (apiKey, keyID, keyPrefix, err). All three plaintext fields
// must be present for success — a missing keyID would leave us unable
// to revoke an orphan key if the subsequent DDB persist fails.
func (m *HTTPAPIKeyMinter) MintAPIKey(ctx context.Context, accessToken, name string, scopes []string) (apiKey, keyID, keyPrefix string, err error) {
	body, err := json.Marshal(mintRequest{Name: name, Scopes: scopes})
	if err != nil {
		return "", "", "", fmt.Errorf("marshal: %w", err)
	}
	reqURL, err := m.joinAPIKeyURL()
	if err != nil {
		return "", "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return "", "", "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.client().Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("do request: %w", err)
	}
	defer func() {
		// Bounded drain — see callback.go drainCap rationale.
		_, _ = io.CopyN(io.Discard, resp.Body, drainCap)
		_ = resp.Body.Close()
	}()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, minterBodyLimit))
	if err != nil {
		return "", "", "", fmt.Errorf("read body: %w", err)
	}
	// Distinct error on truncation — see exchangeAuth0Code for the
	// same pattern.
	if len(rb) == minterBodyLimit {
		return "", "", "", fmt.Errorf("qurl-service /v1/api-keys response exceeded %d bytes", minterBodyLimit)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", "", "", fmt.Errorf("qurl-service /v1/api-keys returned %d", resp.StatusCode)
	}
	var mr mintResponse
	if err := json.Unmarshal(rb, &mr); err != nil {
		return "", "", "", fmt.Errorf("parse response: %w", err)
	}
	if mr.Data.APIKey == "" || mr.Data.KeyID == "" {
		return "", "", "", errors.New("qurl-service returned empty api_key or key_id")
	}
	return mr.Data.APIKey, mr.Data.KeyID, mr.Data.KeyPrefix, nil
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
