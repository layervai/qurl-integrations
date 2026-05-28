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

// slackViewsOpenTimeout is the HTTP-client upper bound for every views.open
// request made by this client. Callers can still pass a tighter context; the
// tunnel install handler does that with slackTriggerOpenViewBudget so it stays
// inside Slack's short trigger_id window.
const slackViewsOpenTimeout = 2 * time.Second

// Slack echoes the opened view in successful views.open responses. Keep the
// body bounded, but leave room for modal blocks plus Slack-injected state.
const slackViewsOpenResponseBodyLimit = 64 * 1024
const slackOpenViewMaxErrorSnippetBytes = 200

func slackOpenViewFunc(token, userAgent string) func(context.Context, string, string, []byte) error {
	return newSlackOpenViewFunc(token, userAgent, slackViewsOpenURL)
}

func newSlackOpenViewFunc(token, userAgent, viewsOpenURL string) func(context.Context, string, string, []byte) error {
	return newSlackOpenViewFuncWithClient(token, userAgent, viewsOpenURL, nil)
}

func newSlackOpenViewFuncWithClient(token, userAgent, viewsOpenURL string, httpClient *http.Client) func(context.Context, string, string, []byte) error {
	if httpClient == nil {
		httpClient = defaultSlackViewsOpenClient()
	}
	// The teamID parameter is intentionally part of the Config.OpenView seam
	// so production wiring can move from this single-token deployment shape to
	// per-team OAuth token lookup without changing the handler contract. The
	// workspace install row written by internal/oauth is the future lookup
	// authority for this token.
	// TODO(slack-oauth): look up the workspace bot token by teamID once the
	// per-workspace OAuth token store is the only production path.
	return func(ctx context.Context, _ string, triggerID string, viewJSON []byte) error {
		viewJSON = bytes.TrimSpace(viewJSON)
		// json.Valid accepts arrays and scalars; views.open requires a view
		// object, so reject non-object roots before sending the request.
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

		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("views.open request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		raw, err := io.ReadAll(io.LimitReader(resp.Body, slackViewsOpenResponseBodyLimit+1))
		if err != nil {
			return fmt.Errorf("views.open response read: %w", err)
		}
		if len(raw) > slackViewsOpenResponseBodyLimit {
			// LimitReader has already consumed limit+1 bytes from the original
			// body. Drain any bytes after that point before Close so a
			// keep-alive transport can still reuse the connection after an
			// oversized response.
			_, _ = io.Copy(io.Discard, resp.Body)
			return fmt.Errorf("views.open response exceeded %d bytes", slackViewsOpenResponseBodyLimit)
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
	if statusCode >= 300 {
		bodySnippet := slackOpenViewBodySnippet(raw)
		if bodySnippet == "" {
			return fmt.Errorf("views.open returned HTTP %d", statusCode)
		}
		return fmt.Errorf("views.open returned HTTP %d: %s", statusCode, bodySnippet)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("views.open: empty response body")
	}

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		if bodySnippet := slackOpenViewBodySnippet(raw); bodySnippet != "" {
			return fmt.Errorf("views.open response JSON: %w: %s", err, bodySnippet)
		}
		return fmt.Errorf("views.open response JSON: %w", err)
	}
	if out.OK {
		return nil
	}
	out.Error = slackOpenViewAPIErrorCode(out.Error)
	if out.Error == "" {
		out.Error = "not_ok"
	}
	return slackOpenViewAPIError(out.Error, header.Get("Retry-After"))
}

func slackOpenViewBodySnippet(raw []byte) string {
	bodySnippet := printableLogSnippet(strings.ToValidUTF8(strings.TrimSpace(string(raw)), "?"))
	if len(bodySnippet) <= slackOpenViewMaxErrorSnippetBytes {
		return bodySnippet
	}
	cut := 0
	// Walk rune starts so the bounded log snippet never slices through a
	// multibyte UTF-8 sequence.
	for i := range bodySnippet {
		if i > slackOpenViewMaxErrorSnippetBytes {
			break
		}
		cut = i
	}
	if cut == 0 {
		return "..."
	}
	return bodySnippet[:cut] + "..."
}

func printableLogSnippet(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || r == '\u2028' || r == '\u2029':
			return ' '
		case r < ' ' || r == 0x7f:
			return '?'
		default:
			return r
		}
	}, s)
}

func slackOpenViewAPIErrorCode(code string) string {
	return printableLogSnippet(strings.ToValidUTF8(strings.TrimSpace(code), "?"))
}

func slackOpenViewAPIError(code, retryAfter string) error {
	switch code {
	case "invalid_trigger", "trigger_expired":
		return fmt.Errorf("%w: %s", internal.ErrSlackTriggerExpired, code)
	case "ratelimited":
		return internal.NewSlackRateLimitError(retryAfter)
	default:
		return fmt.Errorf("views.open: %s", code)
	}
}
