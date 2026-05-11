package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// HTTPAPIKeyMinter is the production QURLAPIKeyMinter, calling
// qurl-service /v1/api-keys with the Auth0 access_token as Bearer.
type HTTPAPIKeyMinter struct {
	// BaseURL is the qurl-service origin (e.g. https://api.layerv.ai).
	BaseURL string
	// HTTPClient overrides the default *http.Client (15s timeout).
	HTTPClient *http.Client
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

// MintAPIKey posts to POST /v1/api-keys with Bearer = accessToken.
// Returns (apiKey, keyID, keyPrefix, err). All three plaintext fields
// must be present for success — a missing keyID would leave us unable
// to revoke an orphan key if the subsequent DDB persist fails.
func (m *HTTPAPIKeyMinter) MintAPIKey(ctx context.Context, accessToken, name string, scopes []string) (apiKey, keyID, keyPrefix string, err error) {
	client := m.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	body, err := json.Marshal(mintRequest{Name: name, Scopes: scopes})
	if err != nil {
		return "", "", "", fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/v1/api-keys", bytes.NewReader(body))
	if err != nil {
		return "", "", "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", "", "", fmt.Errorf("read body: %w", err)
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
	client := m.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, m.BaseURL+"/v1/api-keys/"+keyID, http.NoBody)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("qurl-service DELETE /v1/api-keys returned %d", resp.StatusCode)
	}
	return nil
}
