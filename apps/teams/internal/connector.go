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
	"strings"
	"sync"
	"time"
)

const (
	botTokenEndpoint = "https://login.microsoftonline.com/botframework.com/oauth2/v2.0/token"
	botTokenScope    = "https://api.botframework.com/.default"

	teamsConnectorHostPublic = "smba.trafficmanager.net"
	teamsConnectorHostGCC    = "smba.infra.gcc.teams.microsoft.com"
	teamsConnectorHostGCCH   = "smba.infra.gov.teams.microsoft.us"
	teamsConnectorHostDoD    = "smba.infra.dod.teams.microsoft.us"
)

// MessagePoster posts replies or direct messages back into Teams.
type MessagePoster interface {
	Reply(ctx context.Context, in *Activity, text string) error
	SendText(ctx context.Context, serviceURL, conversationID, text string) error
}

// ConnectorClient posts replies and direct messages through the Teams connector API.
type ConnectorClient struct {
	AppID       string
	AppPassword string
	HTTPClient  *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

type postActivity struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	TextFormat string          `json:"textFormat,omitempty"`
	ReplyToID  string          `json:"replyToId,omitempty"`
	Recipient  *ChannelAccount `json:"recipient,omitempty"`
}

type botTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// Reply posts a reply to the incoming Teams activity.
func (c *ConnectorClient) Reply(ctx context.Context, in *Activity, text string) error {
	if in == nil {
		return errors.New("reply activity is nil")
	}
	body := postActivity{
		Type:       "message",
		Text:       text,
		TextFormat: "markdown",
		ReplyToID:  strings.TrimSpace(in.ID),
		Recipient:  &in.From,
	}
	return c.postActivity(ctx, strings.TrimSpace(in.ServiceURL), strings.TrimSpace(in.Conversation.ID), body)
}

// SendText posts a text message to a Teams conversation.
func (c *ConnectorClient) SendText(ctx context.Context, serviceURL, conversationID, text string) error {
	body := postActivity{
		Type:       "message",
		Text:       text,
		TextFormat: "markdown",
	}
	return c.postActivity(ctx, serviceURL, conversationID, body)
}

func (c *ConnectorClient) postActivity(ctx context.Context, serviceURL, conversationID string, body postActivity) error {
	if strings.TrimSpace(serviceURL) == "" || strings.TrimSpace(conversationID) == "" {
		return errors.New("serviceURL and conversationID are required")
	}
	token, err := c.appToken(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal connector activity: %w", err)
	}
	endpoint, err := connectorActivityURL(serviceURL, conversationID)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build connector request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// The serviceURL host is validated against a strict allowlist in
	// validateConnectorServiceURL (trustedTeamsConnectorHost) before this request.
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("post connector activity: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4097))
		return fmt.Errorf("post connector activity returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func connectorActivityURL(serviceURL, conversationID string) (string, error) {
	base, err := validateConnectorServiceURL(serviceURL)
	if err != nil {
		return "", err
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return "", errors.New("conversationID is required")
	}
	return strings.TrimRight(base.String(), "/") + "/v3/conversations/" + escapeConnectorPathSegment(conversationID) + "/activities", nil
}

func escapeConnectorPathSegment(segment string) string {
	escaped := url.PathEscape(segment)
	// Bot Framework conversation IDs are opaque single-segment identifiers and
	// frequently contain ":" separators. Encode them so intermediaries cannot
	// reinterpret the reserved delimiter while preserving the trusted base path.
	return strings.ReplaceAll(escaped, ":", "%3A")
}

func validateConnectorServiceURL(serviceURL string) (*url.URL, error) {
	serviceURL = strings.TrimSpace(serviceURL)
	if serviceURL == "" {
		return nil, errors.New("serviceURL is required")
	}
	base, err := url.Parse(serviceURL)
	if err != nil {
		return nil, fmt.Errorf("parse serviceURL: %w", err)
	}
	if !strings.EqualFold(base.Scheme, "https") {
		return nil, fmt.Errorf("serviceURL scheme %q is not https", base.Scheme)
	}
	if base.User != nil {
		return nil, errors.New("serviceURL contains userinfo")
	}
	if base.Port() != "" {
		return nil, fmt.Errorf("serviceURL host %q must not include an explicit port", base.Host)
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("serviceURL must not include query or fragment")
	}
	host, err := trustedTeamsConnectorHost(base.Hostname())
	if err != nil {
		return nil, err
	}
	return &url.URL{
		Scheme:  "https",
		Host:    host,
		Path:    base.Path,
		RawPath: base.RawPath,
	}, nil
}

func trustedTeamsConnectorHost(hostname string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(hostname)) {
	case teamsConnectorHostPublic:
		return teamsConnectorHostPublic, nil
	case teamsConnectorHostGCC:
		return teamsConnectorHostGCC, nil
	case teamsConnectorHostGCCH:
		return teamsConnectorHostGCCH, nil
	case teamsConnectorHostDoD:
		return teamsConnectorHostDoD, nil
	default:
		return "", fmt.Errorf("serviceURL host %q is not an allowed Microsoft Teams connector host", hostname)
	}
}

func (c *ConnectorClient) appToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.accessToken != "" && time.Until(c.expiresAt) > time.Minute {
		token := c.accessToken
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", strings.TrimSpace(c.AppID))
	form.Set("client_secret", c.AppPassword)
	form.Set("scope", botTokenScope)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, botTokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build bot token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("request bot token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4097))
		return "", fmt.Errorf("bot token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out botTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode bot token response: %w", err)
	}
	if out.AccessToken == "" || out.ExpiresIn <= 0 {
		return "", fmt.Errorf("bot token response missing access token or expiry")
	}
	c.mu.Lock()
	c.accessToken = out.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	token := c.accessToken
	c.mu.Unlock()
	return token, nil
}

func (c *ConnectorClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}
