// Package auth provides authentication helpers for the QURL CLI,
// including the OAuth 2.0 Device Authorization Grant (RFC 8628).
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultDomain is the Auth0 tenant domain for QURL.
	DefaultDomain = "auth.layerv.ai"

	// DefaultAudience is the API audience identifier.
	DefaultAudience = "https://api.layerv.ai"

	// DefaultClientID is the Auth0 client ID for the CLI device flow.
	// Set to the Auth0 Native application client ID before shipping.
	// Override via QURL_AUTH0_CLIENT_ID environment variable.
	DefaultClientID = ""

	defaultPollInterval = 5 * time.Second
	maxResponseBytes    = 1 << 20 // 1 MiB

	// OAuth 2.0 device flow error codes.
	errCodePending  = "authorization_pending"
	errCodeSlowDown = "slow_down"

	// slowDownIncrement is the polling interval increase per RFC 8628 section 3.5.
	slowDownIncrement = 5 * time.Second

	formContentType = "application/x-www-form-urlencoded"
)

// DeviceFlowConfig holds Auth0 configuration for the device code flow.
type DeviceFlowConfig struct {
	Domain   string   // Auth0 tenant domain (e.g., "auth.layerv.ai").
	ClientID string   // Auth0 Native application client ID.
	Audience string   // API audience identifier.
	Scopes   []string // OAuth scopes to request.
	BaseURL  string   // Override base URL for testing (default: "https://{Domain}").
}

// DeviceCodeResponse contains the Auth0 device authorization response.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// TokenResponse contains a successful OAuth token response.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// DeviceFlowError represents an OAuth error during the device authorization flow.
type DeviceFlowError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

// Error returns the error message.
func (e *DeviceFlowError) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	return e.Code
}

// IsExpired reports whether the device code has expired.
func (e *DeviceFlowError) IsExpired() bool {
	return e.Code == "expired_token"
}

// IsDenied reports whether the user denied authorization.
func (e *DeviceFlowError) IsDenied() bool {
	return e.Code == "access_denied"
}

// DeviceFlow implements the OAuth 2.0 Device Authorization Grant (RFC 8628).
type DeviceFlow struct {
	config     DeviceFlowConfig
	httpClient *http.Client
}

// DeviceFlowOption configures a DeviceFlow instance.
type DeviceFlowOption func(*DeviceFlow)

// WithHTTPClient sets a custom HTTP client for the device flow.
func WithHTTPClient(c *http.Client) DeviceFlowOption {
	return func(d *DeviceFlow) { d.httpClient = c }
}

// NewDeviceFlow creates a new device authorization flow handler.
func NewDeviceFlow(cfg *DeviceFlowConfig, opts ...DeviceFlowOption) *DeviceFlow {
	d := &DeviceFlow{
		config:     *cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

func (d *DeviceFlow) authBaseURL() string {
	if d.config.BaseURL != "" {
		return strings.TrimRight(d.config.BaseURL, "/")
	}
	return "https://" + d.config.Domain
}

// RequestDeviceCode initiates the device authorization flow and returns
// the device code, user code, and verification URI.
func (d *DeviceFlow) RequestDeviceCode(ctx context.Context) (*DeviceCodeResponse, error) {
	endpoint := d.authBaseURL() + "/oauth/device/code"

	form := url.Values{
		"client_id": {d.config.ClientID},
		"audience":  {d.config.Audience},
	}
	if len(d.config.Scopes) > 0 {
		form.Set("scope", strings.Join(d.config.Scopes, " "))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", formContentType)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var oauthErr DeviceFlowError
		if jsonErr := json.Unmarshal(body, &oauthErr); jsonErr == nil && oauthErr.Code != "" {
			return nil, &oauthErr
		}
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var dcr DeviceCodeResponse
	if err := json.Unmarshal(body, &dcr); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &dcr, nil
}

// PollForToken polls the token endpoint until the user completes authorization,
// the device code expires, or the context is canceled.
func (d *DeviceFlow) PollForToken(ctx context.Context, deviceCode string, interval int) (*TokenResponse, error) {
	endpoint := d.authBaseURL() + "/oauth/token"

	pollInterval := time.Duration(interval) * time.Second
	if pollInterval < time.Second {
		pollInterval = defaultPollInterval
	}

	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {d.config.ClientID},
	}
	encodedForm := form.Encode()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}

		token, retry, err := d.pollOnce(ctx, endpoint, encodedForm)
		if err != nil {
			return nil, err
		}
		if token != nil {
			return token, nil
		}
		if retry > 0 {
			pollInterval = retry
		}
	}
}

// pollOnce makes a single token poll request. It returns the token on success,
// a new poll interval if the server requested slow-down, or an error.
func (d *DeviceFlow) pollOnce(ctx context.Context, endpoint, encodedForm string) (*TokenResponse, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(encodedForm))
	if err != nil {
		return nil, 0, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", formContentType)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("poll for token: %w", err)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		var token TokenResponse
		if err := json.Unmarshal(body, &token); err != nil {
			return nil, 0, fmt.Errorf("parse token response: %w", err)
		}
		return &token, 0, nil
	}

	var oauthErr DeviceFlowError
	if err := json.Unmarshal(body, &oauthErr); err != nil {
		return nil, 0, fmt.Errorf("parse error response (status %d): %w", resp.StatusCode, err)
	}

	switch oauthErr.Code {
	case errCodePending:
		return nil, 0, nil
	case errCodeSlowDown:
		return nil, defaultPollInterval + slowDownIncrement, nil
	default:
		return nil, 0, &oauthErr
	}
}
