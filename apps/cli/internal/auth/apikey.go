package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	jsonContentType    = "application/json"
	bearerPrefix       = "Bearer "
	defaultHTTPTimeout = 30 * time.Second
)

// CreateKeyRequest is the request body for creating an API key via JWT.
type CreateKeyRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// CreateKeyResponse is the response from creating an API key.
type CreateKeyResponse struct {
	KeyID     string   `json:"key_id"`
	APIKey    string   `json:"api_key"`
	KeyPrefix string   `json:"key_prefix"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
}

type createKeyEnvelope struct {
	Data CreateKeyResponse `json:"data"`
}

// CreateAPIKey creates a new API key using a JWT access token.
// If httpClient is nil, a default client with 30s timeout is used.
func CreateAPIKey(ctx context.Context, httpClient *http.Client, baseURL, jwt string, input CreateKeyRequest) (*CreateKeyResponse, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/api-keys", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", jsonContentType)
	req.Header.Set("Authorization", bearerPrefix+jwt)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create API key: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("create API key: status %d: %s", resp.StatusCode, string(respBody))
	}

	var envelope createKeyEnvelope
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &envelope.Data, nil
}
