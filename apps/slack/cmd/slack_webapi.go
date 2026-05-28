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
const slackViewsOpenTimeout = 2 * time.Second

var slackViewsOpenHTTPClient = &http.Client{Timeout: slackViewsOpenTimeout}

func slackOpenViewFunc(token, userAgent string) func(context.Context, string, string, []byte) error {
	return slackOpenViewFuncWithURL(token, userAgent, slackViewsOpenURL)
}

func slackOpenViewFuncWithURL(token, userAgent, viewsOpenURL string) func(context.Context, string, string, []byte) error {
	return func(ctx context.Context, teamID string, triggerID string, viewJSON []byte) error {
		// The teamID parameter is intentionally part of the Config.OpenView
		// seam so the production wiring can move from this single-token
		// deployment shape to per-team OAuth token lookup without changing
		// the handler contract.
		_ = teamID
		if !json.Valid(viewJSON) {
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
		resp, err := slackViewsOpenHTTPClient.Do(req)
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
			return fmt.Errorf("views.open response exceeded %d bytes", bodyCap)
		}
		if resp.StatusCode >= 400 {
			bodySnippet := strings.TrimSpace(string(raw))
			if bodySnippet == "" {
				return fmt.Errorf("views.open returned HTTP %d", resp.StatusCode)
			}
			return fmt.Errorf("views.open returned HTTP %d: %s", resp.StatusCode, bodySnippet)
		}
		var out struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return fmt.Errorf("views.open response JSON: %w", err)
		}
		if !out.OK {
			if out.Error == "" {
				out.Error = "not_ok"
			}
			if out.Error == "invalid_trigger" || out.Error == "trigger_expired" {
				return fmt.Errorf("%w: %s", internal.ErrSlackTriggerExpired, out.Error)
			}
			return fmt.Errorf("views.open: %s", out.Error)
		}
		return nil
	}
}
