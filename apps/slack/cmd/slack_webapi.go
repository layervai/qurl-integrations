package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/shared/auth"
)

const slackViewsOpenURL = "https://slack.com/api/views.open"
const defaultSlackAPIUserAgent = "qurl-slack/unknown"

// slackViewsOpenTimeout is the HTTP-client fallback upper bound for every
// views.open request made by this client. Callers can still pass a tighter
// context; the tunnel install handler does that with slackTriggerOpenViewBudget
// so the caller context, not this client timeout, is the primary deadline for
// Slack's short trigger_id window.
const slackViewsOpenTimeout = 2 * time.Second

// Slack echoes the opened view in successful views.open responses. Keep the
// body bounded, but leave room for modal blocks plus Slack-injected state.
const slackViewsOpenResponseBodyLimit = 64 * 1024
const slackAPIMaxErrorSnippetBytes = 200
const slackAPITruncationSuffix = "..."

func newSlackOpenViewFunc(token, userAgent, viewsOpenURL string) func(context.Context, string, string, []byte) error {
	return newSlackOpenViewFuncWithClient(token, userAgent, viewsOpenURL, nil)
}

func newSlackOpenViewFuncWithClient(token, userAgent, viewsOpenURL string, httpClient *http.Client) func(context.Context, string, string, []byte) error {
	return newSlackOpenViewFuncWithTokenLookup(func(context.Context, string) (string, error) {
		return token, nil
	}, userAgent, viewsOpenURL, httpClient)
}

type slackBotTokenLookup func(ctx context.Context, teamID string) (string, error)

func newSlackOpenViewFuncWithTokenLookup(lookup slackBotTokenLookup, userAgent, viewsOpenURL string, httpClient *http.Client) func(context.Context, string, string, []byte) error {
	if httpClient == nil {
		httpClient = defaultSlackViewsOpenClient()
	}
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		userAgent = defaultSlackAPIUserAgent
	}
	// The teamID parameter lets production select the bot token Slack issued
	// for the workspace that invoked the slash command. Tests and single-
	// workspace development still use the static-token wrapper above.
	return func(ctx context.Context, teamID string, triggerID string, viewJSON []byte) error {
		token, err := lookup(ctx, teamID)
		if err != nil {
			return fmt.Errorf("views.open token lookup: %w", err)
		}
		token = strings.TrimSpace(token)
		if token == "" {
			return errors.New("views.open token lookup: empty token")
		}
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
		req.Header.Set("User-Agent", userAgent)

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
		// Prefer Slack's Retry-After hint over any 429 response body; the delay
		// is the operator-actionable part and Slack's body is not more useful
		// than the typed rate-limit error.
		return internal.NewSlackRateLimitError(header.Get("Retry-After"))
	}
	if statusCode >= 300 {
		bodySnippet := slackAPIBodySnippet(raw)
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
		if bodySnippet := slackAPIBodySnippet(raw); bodySnippet != "" {
			return fmt.Errorf("views.open response JSON: %w: %s", err, bodySnippet)
		}
		return fmt.Errorf("views.open response JSON: %w", err)
	}
	if out.OK {
		return nil
	}
	out.Error = slackAPIErrorCode(out.Error)
	if out.Error == "" {
		out.Error = "not_ok"
	}
	return slackOpenViewAPIError(out.Error, header.Get("Retry-After"))
}

func slackAPIBodySnippet(raw []byte) string {
	bodySnippet := printableLogSnippet(strings.ToValidUTF8(strings.TrimSpace(string(raw)), "?"))
	if len(bodySnippet) <= slackAPIMaxErrorSnippetBytes {
		return bodySnippet
	}
	budget := slackAPIMaxErrorSnippetBytes - len(slackAPITruncationSuffix)
	if budget <= 0 {
		return slackAPITruncationSuffix
	}
	cut := 0
	// Count complete runes so the bounded log snippet uses as much of the byte
	// budget as possible without the truncation suffix exceeding the cap or
	// slicing through a multibyte UTF-8 sequence.
	for _, r := range bodySnippet {
		next := cut + utf8.RuneLen(r)
		if next > budget {
			break
		}
		cut = next
	}
	if cut == 0 {
		// Unreachable with the current 200-byte cap, but keep a defined fallback
		// if a future caller lowers the cap below a single UTF-8 rune width.
		return "..."
	}
	return bodySnippet[:cut] + slackAPITruncationSuffix
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

func slackAPIErrorCode(code string) string {
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

const slackChatPostMessageURL = "https://slack.com/api/chat.postMessage"

// slackChatPostMessageTimeout bounds every chat.postMessage HTTP request. For a
// single post this 4s client timeout is the BINDING deadline: the conversation-
// mode delivery worker's agentDeliveryBudget (15s, handler_agent.go) is a looser
// OUTER envelope spanning the transcript save plus the post, so the 4s timeout
// fires first on a stuck request. Unlike views.open there is no short trigger
// window to race, so 4s leaves comfortable headroom for one round-trip while
// still freeing the worker well inside its budget. The Grid fallback does NOT add
// a second round-trip: it retries only on ErrSlackBotTokenNotConfigured, which
// postBody surfaces solely from the token *lookup* (before any HTTP request is
// built), so the org-token attempt is the first and only HTTP call.
const slackChatPostMessageTimeout = 4 * time.Second

// Slack echoes the posted message back in successful chat.postMessage responses.
// Keep the body bounded; an agent reply plus Slack message metadata stays well
// under this.
const slackChatPostMessageResponseBodyLimit = 64 * 1024

// newSlackPostMessageFuncWithTokenLookup builds the [internal.PostMessageFunc]
// seam: a threaded chat.postMessage using the per-workspace bot token. It mirrors
// newSlackOpenViewFuncWithTokenLookup (same token lookup) and openViewWithGridFallback
// (same Enterprise Grid retry) so conversation-mode replies resolve the workspace
// bot token exactly like the slash-command modals do.
func newSlackPostMessageFuncWithTokenLookup(lookup slackBotTokenLookup, userAgent, postMessageURL string, httpClient *http.Client) internal.PostMessageFunc {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: slackChatPostMessageTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		userAgent = defaultSlackAPIUserAgent
	}

	// postBody resolves the bot token for one owner (workspace team or, on the
	// Grid fallback, the enterprise org) and POSTs the already-marshaled body. The
	// body is identical across both attempts, so the caller marshals it once.
	postBody := func(ctx context.Context, ownerID string, body []byte) error {
		token, err := lookup(ctx, ownerID)
		if err != nil {
			return fmt.Errorf("chat.postMessage token lookup: %w", err)
		}
		token = strings.TrimSpace(token)
		if token == "" {
			return errors.New("chat.postMessage token lookup: empty token")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, postMessageURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("chat.postMessage request build: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)

		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("chat.postMessage request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		raw, err := io.ReadAll(io.LimitReader(resp.Body, slackChatPostMessageResponseBodyLimit+1))
		if err != nil {
			return fmt.Errorf("chat.postMessage response read: %w", err)
		}
		if len(raw) > slackChatPostMessageResponseBodyLimit {
			_, _ = io.Copy(io.Discard, resp.Body)
			return fmt.Errorf("chat.postMessage response exceeded %d bytes", slackChatPostMessageResponseBodyLimit)
		}
		return slackChatPostMessageResponseError(resp.StatusCode, resp.Header, raw)
	}

	// Same Enterprise Grid retry as openViewWithGridFallback: try the workspace
	// token, then retry once with the org-install token when the workspace itself
	// has no bot token and the enterprise is a distinct owner. Every other error
	// returns unchanged. The fallback lives in this seam (not the handler, where
	// OpenView's does) because PostMessageFunc's signature carries enterpriseID —
	// OpenView's seam takes only teamID, so its handler must own the retry.
	return func(ctx context.Context, teamID, enterpriseID, channelID, threadTS, text string) error {
		// Marshal once: the payload is owner-independent, so the Grid retry reuses it.
		body, err := json.Marshal(struct {
			Channel  string `json:"channel"`
			ThreadTS string `json:"thread_ts,omitempty"`
			Text     string `json:"text"`
		}{Channel: channelID, ThreadTS: threadTS, Text: text})
		if err != nil {
			return fmt.Errorf("chat.postMessage request marshal: %w", err)
		}
		err = postBody(ctx, teamID, body)
		if err == nil || !errors.Is(err, auth.ErrSlackBotTokenNotConfigured) {
			return err
		}
		if enterpriseID == "" || enterpriseID == teamID {
			return err
		}
		// Parity with openViewWithGridFallback's warn: leave a breadcrumb so an
		// operator debugging "why is the bot posting as the org install?" can see
		// the workspace token was missing. No *slog.Logger is threaded into this
		// seam, so use the default logger.
		slog.Warn("workspace Slack bot token missing; retrying chat.postMessage with Enterprise Grid install token",
			"team_id", teamID, "enterprise_id", enterpriseID)
		return postBody(ctx, enterpriseID, body)
	}
}

// slackChatPostMessageResponseError maps a chat.postMessage HTTP response to an
// error (or nil on success). Unlike views.open, chat.postMessage returns HTTP 200
// with `ok:false` for most failures (channel_not_found, not_in_channel,
// msg_too_long), so the ok field — not the status code — is the real signal. It
// reuses the shared snippet/error-code helpers but has no trigger_id concept.
func slackChatPostMessageResponseError(statusCode int, header http.Header, raw []byte) error {
	if statusCode == http.StatusTooManyRequests {
		return internal.NewSlackRateLimitError(header.Get("Retry-After"))
	}
	if statusCode >= 300 {
		if snippet := slackAPIBodySnippet(raw); snippet != "" {
			return fmt.Errorf("chat.postMessage returned HTTP %d: %s", statusCode, snippet)
		}
		return fmt.Errorf("chat.postMessage returned HTTP %d", statusCode)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("chat.postMessage: empty response body")
	}

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		if snippet := slackAPIBodySnippet(raw); snippet != "" {
			return fmt.Errorf("chat.postMessage response JSON: %w: %s", err, snippet)
		}
		return fmt.Errorf("chat.postMessage response JSON: %w", err)
	}
	if out.OK {
		return nil
	}
	code := slackAPIErrorCode(out.Error)
	// chat.postMessage-specific: unlike views.open (which surfaces rate limits via
	// the 429 path / slackOpenViewAPIError), chat.postMessage commonly returns a
	// 200 body with ok:false:ratelimited, so map that to the sentinel here. Do not
	// "harmonize" this branch away to match slackOpenViewResponseError.
	if code == "ratelimited" {
		return internal.NewSlackRateLimitError(header.Get("Retry-After"))
	}
	if code == "" {
		code = "not_ok"
	}
	return fmt.Errorf("chat.postMessage: %s", code)
}
