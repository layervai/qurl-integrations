package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type FeedbackPoster interface {
	Post(ctx context.Context, submitter, tenantID, message string) error
}

type WebhookFeedbackPoster struct {
	URL        string
	UserAgent  string
	HTTPClient *http.Client
}

func (p *WebhookFeedbackPoster) Post(ctx context.Context, submitter, tenantID, message string) error {
	text := "New qURL Teams feedback"
	if tenantID != "" {
		text += " tenant=" + tenantID
	}
	if submitter != "" {
		text += " submitter=" + submitter
	}
	text += "\n" + message
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("marshal feedback payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build feedback request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.UserAgent != "" {
		req.Header.Set("User-Agent", p.UserAgent)
	}
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("post feedback webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4097))
		return fmt.Errorf("feedback webhook returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (p *WebhookFeedbackPoster) httpClient() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func ValidateFeedbackWebhookURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("feedback webhook url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse feedback webhook url: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") || strings.TrimSpace(u.Host) == "" {
		return "", fmt.Errorf("feedback webhook must be https")
	}
	return u.Host, nil
}
