package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
)

const slackViewsOpenURL = "https://slack.com/api/views.open"

// Handler callers use a tighter Slack trigger_id budget; this client timeout
// is only a final guard for future callers that forget to pass a deadline.
const slackViewsOpenTimeout = 2 * time.Second

func slackOpenViewFunc(token, userAgent string) func(context.Context, string, string, []byte) error {
	return slackOpenViewFuncWithURL(token, userAgent, slackViewsOpenURL)
}

func slackOpenViewFuncWithURL(token, userAgent, viewsOpenURL string) func(context.Context, string, string, []byte) error {
	return slackOpenViewFuncWithHTTPClient(token, userAgent, viewsOpenURL, defaultSlackViewsOpenClient())
}

func slackOpenViewFuncWithHTTPClient(token, userAgent, viewsOpenURL string, httpClient *http.Client) func(context.Context, string, string, []byte) error {
	if httpClient == nil {
		httpClient = defaultSlackViewsOpenClient()
	}
	// The teamID parameter is intentionally part of the Config.OpenView seam
	// so production wiring can move from this single-token deployment shape to
	// per-team OAuth token lookup without changing the handler contract.
	// TODO(slack-oauth): look up the workspace bot token by teamID once the
	// per-workspace OAuth token store is the only production path.
	return func(ctx context.Context, _ string, triggerID string, viewJSON []byte) error {
		viewJSON = bytes.TrimSpace(viewJSON)
		if !json.Valid(viewJSON) || !bytes.HasPrefix(viewJSON, []byte("{")) {
			return errors.New("views.open: invalid view JSON")
		}
		body, err := json.Marshal(struct {
			TriggerID string          `json:"trigger_id"`
			View      json.RawMessage `json:"view"`
		}{
			TriggerID: triggerID,
			View:      json.RawMessage(viewJSON),
		})
		if err != nil {
			return fmt.Errorf("views.open request marshal: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, viewsOpenURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("views.open request build: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if userAgent != "" {
			req.Header.Set("User-Agent", userAgent)
		}

		// Callers normally pass a tighter, Slack-ack-bound context; this
		// timeout is a fallback for any future caller that forgets to do so.
		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("views.open request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		const bodyCap = 4096
		raw, err := io.ReadAll(io.LimitReader(resp.Body, bodyCap+1))
		if err != nil {
			return fmt.Errorf("views.open response read: %w", err)
		}
		if len(raw) > bodyCap {
			_, _ = io.Copy(io.Discard, resp.Body)
			return fmt.Errorf("views.open response exceeded %d bytes", bodyCap)
		}
		return slackOpenViewResponseError(resp.StatusCode, resp.Header, raw)
	}
}

func defaultSlackViewsOpenClient() *http.Client {
	return &http.Client{
		Timeout: slackViewsOpenTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func slackOpenViewResponseError(statusCode int, header http.Header, raw []byte) error {
	if statusCode == http.StatusTooManyRequests {
		return internal.NewSlackRateLimitError(header.Get("Retry-After"))
	}
	if statusCode >= 400 {
		bodySnippet := slackOpenViewBodySnippet(raw)
		if bodySnippet == "" {
			return fmt.Errorf("views.open returned HTTP %d", statusCode)
		}
		return fmt.Errorf("views.open returned HTTP %d: %s", statusCode, bodySnippet)
	}

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("views.open response JSON: %w", err)
	}
	if out.OK {
		return nil
	}
	if out.Error == "" {
		out.Error = "not_ok"
	}
	return slackOpenViewAPIError(out.Error)
}

func slackOpenViewBodySnippet(raw []byte) string {
	const maxSnippetBytes = 200
	bodySnippet := strings.TrimSpace(string(raw))
	if len(bodySnippet) <= maxSnippetBytes {
		return bodySnippet
	}
	return bodySnippet[:maxSnippetBytes] + "..."
}

func slackOpenViewAPIError(code string) error {
	switch code {
	case "invalid_trigger", "trigger_expired":
		return fmt.Errorf("%w: %s", internal.ErrSlackTriggerExpired, code)
	case "ratelimited":
		return internal.ErrSlackRateLimited
	default:
		return fmt.Errorf("views.open: %s", code)
	}
}
