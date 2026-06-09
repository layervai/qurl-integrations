package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// slackIncomingWebhookHost is the only host Slack issues incoming-webhook URLs
// on. validateFeedbackWebhookURL pins the scheme + rejects userinfo; the host
// is checked at startup (a warn, not a hard failure) so a legitimate alternate
// relay still works while a typo is surfaced.
const slackIncomingWebhookHost = "hooks.slack.com"

// feedbackWebhookTimeout bounds a single POST to the incoming webhook. Slack's
// hooks ingress responds in well under a second; 5s catches transient blips
// without holding an async-worker slot.
const feedbackWebhookTimeout = 5 * time.Second

// feedbackWebhookResponseBodyLimit caps the response body read. Slack incoming
// webhooks reply with a short "ok"/error string; the cap bounds a misbehaving
// or wrong endpoint.
const feedbackWebhookResponseBodyLimit = 8 * 1024

// newFeedbackWebhookPoster returns an internal.PostFeedbackFunc that delivers a
// pre-built Block Kit payload (from internal.FeedbackMessage) to the Slack
// incoming webhook at webhookURL. webhookURL is operator-configured
// (FEEDBACK_SLACK_WEBHOOK_URL) and validated for shape at startup.
func newFeedbackWebhookPoster(webhookURL, userAgent string, httpClient *http.Client) func(context.Context, []byte) error {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: feedbackWebhookTimeout,
			// Refuse redirects: the webhook host is pinned at validation, and a
			// 30x bounce to another host would otherwise be followed silently.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		userAgent = defaultSlackAPIUserAgent
	}
	return func(ctx context.Context, payload []byte) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("feedback webhook request build: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)

		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("feedback webhook request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		raw, _ := io.ReadAll(io.LimitReader(resp.Body, feedbackWebhookResponseBodyLimit+1))
		if len(raw) > feedbackWebhookResponseBodyLimit {
			// Drain the overflow so the keep-alive transport can recycle the
			// connection, then keep the snippet bounded for the error string.
			_, _ = io.Copy(io.Discard, resp.Body)
			raw = raw[:feedbackWebhookResponseBodyLimit]
		}
		if resp.StatusCode != http.StatusOK {
			snippet := strings.TrimSpace(string(raw))
			if snippet == "" {
				return fmt.Errorf("feedback webhook returned HTTP %d", resp.StatusCode)
			}
			return fmt.Errorf("feedback webhook returned HTTP %d: %s", resp.StatusCode, snippet)
		}
		return nil
	}
}

// validateFeedbackWebhookURL fences FEEDBACK_SLACK_WEBHOOK_URL to an https URL
// with a real host and no embedded userinfo before the poster is wired. The
// value is an operator-set secret, but pinning the scheme keeps a typo from
// turning the bot into an SSRF emitter — the same defense-in-depth posture as
// the response_url fence. The returned host lets the caller warn when it isn't
// Slack's canonical incoming-webhook host.
func validateFeedbackWebhookURL(raw string) (host string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("scheme %q is not https", u.Scheme)
	}
	if u.User != nil {
		return "", errors.New("contains userinfo")
	}
	host = u.Hostname()
	if host == "" {
		return "", errors.New("missing host")
	}
	return host, nil
}
