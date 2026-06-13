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
	neturl "net/url"
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

const (
	slackAPIInvalidArguments    = "invalid_arguments"
	slackAPIInvalidBlockType    = "invalid_block_type"
	slackAPIInvalidBlocks       = "invalid_blocks"
	slackAPIInvalidBlocksFormat = "invalid_blocks_format"
)

type slackWebAPIError struct {
	op   string
	code string
}

func (e *slackWebAPIError) Error() string {
	return e.op + ": " + e.code
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

const slackChatPostEphemeralURL = "https://slack.com/api/chat.postEphemeral"

const (
	slackReactionsAddURL    = "https://slack.com/api/reactions.add"
	slackReactionsRemoveURL = "https://slack.com/api/reactions.remove"
)

const slackConversationsInfoURL = "https://slack.com/api/conversations.info"
const slackConversationsMembersURL = "https://slack.com/api/conversations.members"
const slackConversationsOpenURL = "https://slack.com/api/conversations.open"

const (
	// maxMembershipPages bounds the conversations.members page scan for one membership check.
	// Coverage is up to maxMembershipPages × the EFFECTIVE per-page size — and Slack may return
	// fewer than membershipPageLimit per page (its docs recommend ≤200 and warn a page can be
	// short even when the list isn't exhausted), so don't assume the full 2×1000. A member
	// beyond whatever the scan reaches reads as not-confirmed and the pane stays un-scoped — an
	// acceptable degradation for a best-effort access guard, NOT a correctness bug (the bound is
	// the cap, fail-closed is the safety); it also bounds the non-member case, which would
	// otherwise scan every page. The effective page size should be confirmed against a large
	// channel before enablement (qurl-integrations-infra#1004) and the bound tuned if it's small.
	maxMembershipPages = 2
	// membershipPageLimit is the per-page member count REQUESTED (member ids are a light
	// payload). Slack may cap the returned page below this (see maxMembershipPages); requesting
	// the practical max just maximizes coverage where Slack honors it.
	membershipPageLimit = 1000
)

const (
	slackAssistantSetTitleURL            = "https://slack.com/api/assistant.threads.setTitle"
	slackAssistantSetSuggestedPromptsURL = "https://slack.com/api/assistant.threads.setSuggestedPrompts"
	slackAssistantSetStatusURL           = "https://slack.com/api/assistant.threads.setStatus"
)

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

// slackWebAPIResponseBodyLimit bounds any Slack web API response the shared poster
// reads (an echoed chat.postMessage body, a reactions.add confirmation) — all stay
// well under this.
const slackWebAPIResponseBodyLimit = 64 * 1024

// slackWebAPIPoster posts a pre-marshaled JSON body to one Slack web API method using
// the per-workspace bot token with the Enterprise Grid fallback. It's the shared
// transport behind the conversation-mode seams — chat.postMessage (text + Block Kit)
// and reactions.add/remove — which differ only in the URL, the marshaled body, the
// op label (for error strings), and the response parser (respErr). Each seam
// constructs its own poster (so its own http.Client + timeout); share one if
// connection pooling ever matters.
type slackWebAPIPoster struct {
	lookup     slackBotTokenLookup
	userAgent  string
	url        string
	op         string // method label for error strings, e.g. "chat.postMessage"
	respErr    func(statusCode int, header http.Header, raw []byte) error
	httpClient *http.Client
}

func newSlackWebAPIPoster(lookup slackBotTokenLookup, userAgent, url, op string, respErr func(int, http.Header, []byte) error, httpClient *http.Client) *slackWebAPIPoster {
	if httpClient == nil {
		httpClient = defaultSlackPostMessageClient()
	}
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		userAgent = defaultSlackAPIUserAgent
	}
	return &slackWebAPIPoster{lookup: lookup, userAgent: userAgent, url: url, op: op, respErr: respErr, httpClient: httpClient}
}

// postOnce resolves one owner's bot token (workspace team or, on the Grid fallback,
// the enterprise org) and POSTs the already-marshaled body, returning the (size-bounded)
// response body alongside the parsed error. Most seams ignore the body (via post); only
// chat.startStream reads it, for the stream ts (see slackAgentStreamPort.StartStream).
func (p *slackWebAPIPoster) postOnce(ctx context.Context, ownerID string, body []byte) ([]byte, error) {
	token, err := p.lookup(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("%s token lookup: %w", p.op, err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		// A ("", nil) lookup returns a plain error, NOT the sentinel — so it does not
		// trigger the Grid fallback. That's fine: the real workspaceTokenLookup returns
		// ErrSlackBotTokenNotConfigured (which does fall back), never an empty-but-nil
		// token. A future lookup returning ("", nil) on a miss would need the sentinel.
		return nil, fmt.Errorf("%s token lookup: empty token", p.op)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s request build: %w", p.op, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", p.userAgent)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", p.op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, slackWebAPIResponseBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("%s response read: %w", p.op, err)
	}
	if len(raw) > slackWebAPIResponseBodyLimit {
		// LimitReader already consumed limit+1 bytes; drain the rest before Close so a
		// keep-alive transport can reuse the connection after an oversized response
		// (mirrors the views.open drain).
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("%s response exceeded %d bytes", p.op, slackWebAPIResponseBodyLimit)
	}
	return raw, p.respErr(resp.StatusCode, resp.Header, raw)
}

// gridPost sends body with the workspace token, then retries once with the Enterprise
// Grid org-install token when the workspace itself has no bot token and the enterprise
// is a distinct owner. Same retry as openViewWithGridFallback; it lives in this seam
// because PostMessage*Func carry enterpriseID. body is reused across both attempts.
// Returns the successful (or last) attempt's response body.
func (p *slackWebAPIPoster) gridPost(ctx context.Context, teamID, enterpriseID string, body []byte) ([]byte, error) {
	raw, err := p.postOnce(ctx, teamID, body)
	if err == nil || !errors.Is(err, auth.ErrSlackBotTokenNotConfigured) {
		return raw, err
	}
	if enterpriseID == "" || enterpriseID == teamID {
		return raw, err
	}
	// Parity with openViewWithGridFallback's warn: a breadcrumb so an operator
	// debugging "why is the bot posting as the org install?" can see the workspace
	// token was missing. No *slog.Logger is threaded into this seam; use the default.
	slog.Warn("workspace Slack bot token missing; retrying with Enterprise Grid install token",
		"op", p.op, "team_id", teamID, "enterprise_id", enterpriseID)
	return p.postOnce(ctx, enterpriseID, body)
}

// post sends body with the Grid fallback, discarding the response body — the shape the
// fire-and-forget seams (chat.postMessage, reactions, assistant.threads.*, views.publish,
// chat.appendStream/stopStream) use.
func (p *slackWebAPIPoster) post(ctx context.Context, teamID, enterpriseID string, body []byte) error {
	_, err := p.gridPost(ctx, teamID, enterpriseID, body)
	return err
}

// newSlackPostMessageFuncWithTokenLookup builds the [internal.PostMessageFunc] seam:
// a threaded text chat.postMessage using the per-workspace bot token (the same token
// resolution as the slash-command modals).
func newSlackPostMessageFuncWithTokenLookup(lookup slackBotTokenLookup, userAgent, postMessageURL string, httpClient *http.Client) internal.PostMessageFunc {
	poster := newSlackWebAPIPoster(lookup, userAgent, postMessageURL, "chat.postMessage", slackChatPostMessageResponseError, httpClient)
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
		return poster.post(ctx, teamID, enterpriseID, body)
	}
}

// newSlackPostDMFuncWithTokenLookup builds the [internal.PostDMFunc] seam: open
// or resume a 1:1 IM with conversations.open, then post into that DM channel.
// Both calls use the same per-workspace token lookup and Enterprise Grid
// fallback as channel posts.
func newSlackPostDMFuncWithTokenLookup(lookup slackBotTokenLookup, userAgent, conversationsOpenURL, postMessageURL string, httpClient *http.Client) internal.PostDMFunc {
	open := newSlackWebAPIPoster(lookup, userAgent, conversationsOpenURL, "conversations.open", slackConversationsOpenResponseError, httpClient)
	post := newSlackPostMessageFuncWithTokenLookup(lookup, userAgent, postMessageURL, httpClient)
	return func(ctx context.Context, teamID, enterpriseID, slackUserID, text string) error {
		body, err := json.Marshal(struct {
			Users string `json:"users"`
		}{Users: slackUserID})
		if err != nil {
			return fmt.Errorf("conversations.open request marshal: %w", err)
		}
		raw, err := open.gridPost(ctx, teamID, enterpriseID, body)
		if err != nil {
			return err
		}
		channelID, err := slackConversationsOpenChannelID(raw)
		if err != nil {
			return err
		}
		return post(ctx, teamID, enterpriseID, channelID, "", text)
	}
}

// newSlackPostEphemeralFuncWithTokenLookup builds the [internal.PostEphemeralFunc] seam:
// a threaded chat.postEphemeral visible only to userID — used to deliver a get's one-time
// link privately in a (multi-party) channel, as a standalone message the click's
// response_url card-replace can't overwrite. Shares the poster (token lookup + Grid
// fallback + ok:false parse) with the text seams; only the JSON body (which adds `user`)
// differs.
func newSlackPostEphemeralFuncWithTokenLookup(lookup slackBotTokenLookup, userAgent, postEphemeralURL string, httpClient *http.Client) internal.PostEphemeralFunc {
	poster := newSlackWebAPIPoster(lookup, userAgent, postEphemeralURL, "chat.postEphemeral", slackChatPostEphemeralResponseError, httpClient)
	return func(ctx context.Context, teamID, enterpriseID, channelID, threadTS, userID, text string) error {
		body, err := json.Marshal(struct {
			Channel  string `json:"channel"`
			User     string `json:"user"`
			ThreadTS string `json:"thread_ts,omitempty"`
			Text     string `json:"text"`
		}{Channel: channelID, User: userID, ThreadTS: threadTS, Text: text})
		if err != nil {
			return fmt.Errorf("chat.postEphemeral request marshal: %w", err)
		}
		return poster.post(ctx, teamID, enterpriseID, body)
	}
}

// newSlackPostMarkdownMessageFuncWithTokenLookup builds the
// [internal.PostMarkdownMessage] seam: a threaded chat.postMessage whose visible
// body is a Slack markdown block, so Slack renders standard Markdown (the dialect
// the streaming pane's chat.appendStream also takes) instead of the text field's
// mrkdwn. The top-level text is a literal notification/screen-reader fallback; Slack
// rejects text alongside markdown_text, so the normal path uses blocks. If Slack
// rejects the markdown block shape in an older/limited surface, retry once with the
// older markdown_text-only body so the answer still delivers.
func newSlackPostMarkdownMessageFuncWithTokenLookup(lookup slackBotTokenLookup, userAgent, postMessageURL string, httpClient *http.Client) internal.PostMessageFunc {
	poster := newSlackWebAPIPoster(lookup, userAgent, postMessageURL, "chat.postMessage", slackChatPostMessageResponseError, httpClient)
	return func(ctx context.Context, teamID, enterpriseID, channelID, threadTS, markdownText string) error {
		// Marshal once: the payload is owner-independent, so the Grid retry reuses it.
		body, err := slackMarkdownBlockMessageBody(channelID, threadTS, markdownText)
		if err != nil {
			return fmt.Errorf("chat.postMessage request marshal: %w", err)
		}
		err = poster.post(ctx, teamID, enterpriseID, body)
		if !isSlackMarkdownBlockFallbackError(err) {
			return err
		}
		errorCode := slackChatPostMessageErrorCode(err)
		if errorCode == slackAPIInvalidArguments {
			slog.Debug("Slack rejected markdown block; retrying with markdown_text", "error_code", errorCode, "error", err)
		} else {
			slog.Info("Slack rejected markdown block; retrying with markdown_text", "error_code", errorCode, "error", err)
		}
		body, err = slackMarkdownTextMessageBody(channelID, threadTS, markdownText)
		if err != nil {
			return fmt.Errorf("chat.postMessage request marshal: %w", err)
		}
		return poster.post(ctx, teamID, enterpriseID, body)
	}
}

func slackMarkdownBlockMessageBody(channelID, threadTS, markdownText string) ([]byte, error) {
	return json.Marshal(struct {
		Channel  string               `json:"channel"`
		ThreadTS string               `json:"thread_ts,omitempty"`
		Text     string               `json:"text"`
		Blocks   []slackMarkdownBlock `json:"blocks"`
		// Keep Slack from reparsing the notification/screen-reader fallback as mrkdwn.
		Mrkdwn bool `json:"mrkdwn"`
	}{Channel: channelID, ThreadTS: threadTS, Text: slackMarkdownFallbackText(markdownText), Blocks: []slackMarkdownBlock{{Type: "markdown", Text: markdownText}}, Mrkdwn: false})
}

type slackMarkdownBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// slackMarkdownFallbackText is a lossy notification/screen-reader fallback, not
// a second Markdown renderer; the markdown block remains the visible body.
// It reuses the agent markdown hardener as defense-in-depth in case a future
// caller passes raw Markdown rather than the already-hardened agent reply.
// Escaped hardening artifacts may remain here so the fallback never hides text
// that was made literal for safety.
func slackMarkdownFallbackText(markdownText string) string {
	markdownText = internal.HardenAgentMarkdown(markdownText)
	var lines []string
	for _, line := range strings.Split(markdownText, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(line, ">"))
		line = strings.TrimLeft(line, "#")
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "- "), "* "))
		line = strings.TrimSpace(trimMarkdownFallbackOrderedListMarker(line))
		if line != "" {
			lines = append(lines, line)
		}
	}
	text := strings.Join(lines, " ")
	text = slackMarkdownFallbackReplacer.Replace(text)
	fields := strings.Fields(text)
	for i := range fields {
		fields[i] = trimMarkdownFallbackUnderscoreEmphasis(fields[i])
	}
	if fallback := strings.Join(fields, " "); fallback != "" {
		return fallback
	}
	return strings.TrimSpace(markdownText)
}

var slackMarkdownFallbackReplacer = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "", "*", "")

func trimMarkdownFallbackUnderscoreEmphasis(field string) string {
	if !strings.HasPrefix(field, "_") || strings.Contains(field, "://") {
		return field
	}
	last := strings.LastIndexByte(field[1:], '_')
	if last < 0 {
		return field
	}
	last++
	return field[1:last] + field[last+1:]
}

func trimMarkdownFallbackOrderedListMarker(line string) string {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i+1 >= len(line) || line[i] != '.' || line[i+1] != ' ' {
		return line
	}
	return line[i+2:]
}

// Slack rejects markdown_text paired with text/blocks, so this compatibility
// retry intentionally omits the literal notification/screen-reader fallback.
func slackMarkdownTextMessageBody(channelID, threadTS, markdownText string) ([]byte, error) {
	return json.Marshal(struct {
		Channel      string `json:"channel"`
		ThreadTS     string `json:"thread_ts,omitempty"`
		MarkdownText string `json:"markdown_text"`
	}{Channel: channelID, ThreadTS: threadTS, MarkdownText: markdownText})
}

func isSlackMarkdownBlockFallbackError(err error) bool {
	switch slackChatPostMessageErrorCode(err) {
	// invalid_arguments is broader than the block-specific codes, but this helper
	// only runs after the markdown-block attempt. A real argument error costs one
	// doomed markdown_text retry, while older Slack surfaces still get delivery.
	case slackAPIInvalidArguments, slackAPIInvalidBlockType, slackAPIInvalidBlocks, slackAPIInvalidBlocksFormat:
		return true
	default:
		return false
	}
}

func slackChatPostMessageErrorCode(err error) string {
	var apiErr *slackWebAPIError
	if !errors.As(err, &apiErr) || apiErr.op != "chat.postMessage" {
		return ""
	}
	return apiErr.code
}

// newSlackPostMessageBlocksFuncWithTokenLookup builds the
// [internal.PostMessageBlocksFunc] seam: a threaded Block Kit chat.postMessage (the
// conversation-mode Approve/Reject confirm card). text is the notification /
// non-block-client fallback. It shares the poster (token lookup + Grid fallback +
// response parsing) with the text seam — only the JSON body differs.
func newSlackPostMessageBlocksFuncWithTokenLookup(lookup slackBotTokenLookup, userAgent, postMessageURL string, httpClient *http.Client) internal.PostMessageBlocksFunc {
	poster := newSlackWebAPIPoster(lookup, userAgent, postMessageURL, "chat.postMessage", slackChatPostMessageResponseError, httpClient)
	return func(ctx context.Context, teamID, enterpriseID, channelID, threadTS string, blocks []any, fallbackText string) error {
		body, err := json.Marshal(struct {
			Channel  string `json:"channel"`
			ThreadTS string `json:"thread_ts,omitempty"`
			Text     string `json:"text"`
			Blocks   []any  `json:"blocks"`
			// mrkdwn:false renders the top-level fallback text LITERALLY, fulfilling the
			// PostMessageBlocksFunc seam's defense-in-depth contract: a prompt-injected,
			// LLM-distilled summary in the fallback can't surface markup (e.g. a masked
			// link) in the notification / non-block-client preview, regardless of whether
			// the caller also escaped it. The card itself renders the summary as plain_text.
			Mrkdwn bool `json:"mrkdwn"`
		}{Channel: channelID, ThreadTS: threadTS, Text: fallbackText, Blocks: blocks, Mrkdwn: false})
		if err != nil {
			return fmt.Errorf("chat.postMessage request marshal: %w", err)
		}
		return poster.post(ctx, teamID, enterpriseID, body)
	}
}

const slackViewsPublishURL = "https://slack.com/api/views.publish"

// slackViewsPublishResponseError preserves the views.publish error shape via the shared
// mapper (no benign codes).
func slackViewsPublishResponseError(statusCode int, header http.Header, raw []byte) error {
	return slackWebAPIResponseError("views.publish", nil, statusCode, header, raw)
}

// slackHomeView is the {"type":"home", "blocks":[...]} view object views.publish wraps
// the App Home content in.
type slackHomeView struct {
	Type   string `json:"type"`
	Blocks []any  `json:"blocks"`
}

// newSlackAppHomePublishFuncWithTokenLookup builds the [internal.AppHomePublishFunc]
// seam: a views.publish that sets a user's App Home tab to the agent's review surface
// (their own recent confirmed actions). It shares the poster (token lookup + Grid
// fallback + response parsing) with the chat seams — only the JSON body differs. No
// extra OAuth scope is needed beyond the bot token, but the manifest's App Home tab
// must be enabled for Slack to render the published view.
func newSlackAppHomePublishFuncWithTokenLookup(lookup slackBotTokenLookup, userAgent, viewsPublishURL string, httpClient *http.Client) internal.AppHomePublishFunc {
	poster := newSlackWebAPIPoster(lookup, userAgent, viewsPublishURL, "views.publish", slackViewsPublishResponseError, httpClient)
	return func(ctx context.Context, teamID, enterpriseID, userID string, blocks []any) error {
		body, err := json.Marshal(struct {
			UserID string        `json:"user_id"`
			View   slackHomeView `json:"view"`
		}{UserID: userID, View: slackHomeView{Type: "home", Blocks: blocks}})
		if err != nil {
			return fmt.Errorf("views.publish request marshal: %w", err)
		}
		return poster.post(ctx, teamID, enterpriseID, body)
	}
}

const (
	slackChatStartStreamURL  = "https://slack.com/api/chat.startStream"
	slackChatAppendStreamURL = "https://slack.com/api/chat.appendStream"
	slackChatStopStreamURL   = "https://slack.com/api/chat.stopStream"
)

// slackAgentStreamPort implements [internal.AgentStreamPort] over Slack's native AI-app
// streaming. startStream reads the response for the stream ts; appendStream/stopStream
// are fire-and-forget. All three share the per-team token lookup + Grid fallback poster.
type slackAgentStreamPort struct {
	start *slackWebAPIPoster
	appnd *slackWebAPIPoster
	stop  *slackWebAPIPoster
}

func newSlackAgentStreamPortWithTokenLookup(lookup slackBotTokenLookup, userAgent, startURL, appendURL, stopURL string, httpClient *http.Client) internal.AgentStreamPort {
	if httpClient == nil {
		// One shared client across start/append×N/stop so a streamed turn's verb sequence
		// reuses keep-alive connections (parity with the assistant/reactions ports).
		httpClient = defaultSlackPostMessageClient()
	}
	mk := func(url, op string) *slackWebAPIPoster {
		return newSlackWebAPIPoster(lookup, userAgent, url, op, func(s int, h http.Header, raw []byte) error {
			return slackWebAPIResponseError(op, nil, s, h, raw)
		}, httpClient)
	}
	return &slackAgentStreamPort{
		start: mk(startURL, "chat.startStream"),
		appnd: mk(appendURL, "chat.appendStream"),
		stop:  mk(stopURL, "chat.stopStream"),
	}
}

func (p *slackAgentStreamPort) StartStream(ctx context.Context, start *internal.AgentStreamStart) (string, error) {
	if start == nil {
		return "", errors.New("chat.startStream: missing start request")
	}
	body, err := json.Marshal(struct {
		Channel         string `json:"channel"`
		ThreadTS        string `json:"thread_ts,omitempty"`
		RecipientTeamID string `json:"recipient_team_id,omitempty"`
		RecipientUserID string `json:"recipient_user_id,omitempty"`
	}{
		Channel:         start.ChannelID,
		ThreadTS:        start.ThreadTS,
		RecipientTeamID: start.RecipientTeamID,
		RecipientUserID: start.RecipientUserID,
	})
	if err != nil {
		return "", fmt.Errorf("chat.startStream request marshal: %w", err)
	}
	raw, err := p.start.gridPost(ctx, start.TeamID, start.EnterpriseID, body)
	if err != nil {
		return "", err
	}
	var r struct {
		TS string `json:"ts"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("chat.startStream: decode response: %w", err)
	}
	if r.TS == "" {
		return "", errors.New("chat.startStream: response carried no stream ts")
	}
	return r.TS, nil
}

func (p *slackAgentStreamPort) AppendStream(ctx context.Context, teamID, enterpriseID, channelID, streamTS, markdownText string) error {
	body, err := json.Marshal(struct {
		Channel      string `json:"channel"`
		TS           string `json:"ts"`
		MarkdownText string `json:"markdown_text"`
	}{Channel: channelID, TS: streamTS, MarkdownText: markdownText})
	if err != nil {
		return fmt.Errorf("chat.appendStream request marshal: %w", err)
	}
	return p.appnd.post(ctx, teamID, enterpriseID, body)
}

func (p *slackAgentStreamPort) StopStream(ctx context.Context, teamID, enterpriseID, channelID, streamTS string) error {
	body, err := json.Marshal(struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}{Channel: channelID, TS: streamTS})
	if err != nil {
		return fmt.Errorf("chat.stopStream request marshal: %w", err)
	}
	return p.stop.post(ctx, teamID, enterpriseID, body)
}

// newSlackResolveChannelNameFuncWithTokenLookup builds the
// [internal.ResolveChannelNameFunc] seam: a conversations.info read on the
// per-workspace bot token returning the channel's name. Unlike the POST seams
// (which discard the body), this parses the response, so it doesn't use the shared
// poster; it replicates the same token lookup + Enterprise Grid org-token fallback.
// Requires the channels:read / groups:read scopes — without them Slack answers
// missing_scope and the agent falls back to the channel id.
func newSlackResolveChannelNameFuncWithTokenLookup(lookup slackBotTokenLookup, userAgent, conversationsInfoURL string, httpClient *http.Client) internal.ResolveChannelNameFunc {
	if httpClient == nil {
		httpClient = defaultSlackPostMessageClient()
	}
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		userAgent = defaultSlackAPIUserAgent
	}
	// get resolves one owner's token and reads conversations.info. A token-lookup
	// failure is wrapped with %w so the caller's errors.Is(ErrSlackBotTokenNotConfigured)
	// Grid fallback fires, mirroring slackWebAPIPoster.post.
	get := func(ctx context.Context, ownerID, channelID string) (string, error) {
		token, err := lookup(ctx, ownerID)
		if err != nil {
			return "", fmt.Errorf("conversations.info token lookup: %w", err)
		}
		if token = strings.TrimSpace(token); token == "" {
			return "", errors.New("conversations.info token lookup: empty token")
		}
		// The channel id is a Slack object id (C/G/D + base32) from the
		// signature-verified Events payload; its charset can't contain a query-reserved
		// character, so interpolating it raw is equivalent to escaping it. (The only
		// caller passes that trusted id — a future untrusted source would need escaping.)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, conversationsInfoURL+"?channel="+channelID, http.NoBody)
		if err != nil {
			return "", fmt.Errorf("conversations.info request build: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", userAgent)
		resp, err := httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("conversations.info request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, slackWebAPIResponseBodyLimit+1))
		if err != nil {
			return "", fmt.Errorf("conversations.info response read: %w", err)
		}
		if len(raw) > slackWebAPIResponseBodyLimit {
			_, _ = io.Copy(io.Discard, resp.Body)
			return "", fmt.Errorf("conversations.info response exceeded %d bytes", slackWebAPIResponseBodyLimit)
		}
		var out struct {
			OK      bool   `json:"ok"`
			Error   string `json:"error"`
			Channel struct {
				Name string `json:"name"`
			} `json:"channel"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", fmt.Errorf("conversations.info decode: %w", err)
		}
		if !out.OK {
			slackErr := out.Error
			if slackErr == "" {
				slackErr = fmt.Sprintf("status %d", resp.StatusCode)
			}
			return "", fmt.Errorf("conversations.info: %s", slackErr)
		}
		return out.Channel.Name, nil
	}
	return func(ctx context.Context, teamID, enterpriseID, channelID string) (string, error) {
		name, err := get(ctx, teamID, channelID)
		if err == nil || !errors.Is(err, auth.ErrSlackBotTokenNotConfigured) {
			return name, err
		}
		if enterpriseID == "" || enterpriseID == teamID {
			return name, err
		}
		return get(ctx, enterpriseID, channelID)
	}
}

// fetchConversationsMembersPage GETs one conversations.members page on token and returns its
// member ids + the next_cursor (empty when the membership is exhausted). The body is bounded
// by slackWebAPIResponseBodyLimit; a Slack ok:false surfaces as an error.
func fetchConversationsMembersPage(ctx context.Context, httpClient *http.Client, baseURL, userAgent, token, channelID, cursor string) (members []string, nextCursor string, err error) {
	// channelID is a trusted Slack object id (its charset has no query-reserved character —
	// see the conversations.info seam); the cursor is Slack-issued and opaque (can carry
	// '=' / '/' / '+'), so it MUST be escaped.
	reqURL := fmt.Sprintf("%s?channel=%s&limit=%d", baseURL, channelID, membershipPageLimit)
	if cursor != "" {
		reqURL += "&cursor=" + neturl.QueryEscape(cursor)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return nil, "", fmt.Errorf("conversations.members request build: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("conversations.members request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, slackWebAPIResponseBodyLimit+1))
	if err != nil {
		return nil, "", fmt.Errorf("conversations.members response read: %w", err)
	}
	if len(raw) > slackWebAPIResponseBodyLimit {
		_, _ = io.Copy(io.Discard, resp.Body) // drain so keep-alive can reuse the connection
		return nil, "", fmt.Errorf("conversations.members response exceeded %d bytes", slackWebAPIResponseBodyLimit)
	}
	var out struct {
		OK               bool     `json:"ok"`
		Error            string   `json:"error"`
		Members          []string `json:"members"`
		ResponseMetadata struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "", fmt.Errorf("conversations.members decode: %w", err)
	}
	if !out.OK {
		slackErr := out.Error
		if slackErr == "" {
			slackErr = fmt.Sprintf("status %d", resp.StatusCode)
		}
		return nil, "", fmt.Errorf("conversations.members: %s", slackErr)
	}
	return out.Members, out.ResponseMetadata.NextCursor, nil
}

// newSlackChannelMembershipFuncWithTokenLookup builds the [internal.ChannelMembershipFunc]
// seam: a bounded conversations.members scan on the per-workspace bot token reporting
// whether userID is a member of channelID. Like the conversations.info seam it parses the
// response and replicates the token lookup + Enterprise Grid org-token fallback. Requires
// the channels:read / groups:read scopes (same as ResolveChannelName) — without them Slack
// answers missing_scope and the caller treats the error as "not confirmed" (no scope). It
// scans at most maxMembershipPages pages, so a member beyond that bound reads as
// not-confirmed; that's an acceptable degradation for a best-effort access guard.
func newSlackChannelMembershipFuncWithTokenLookup(lookup slackBotTokenLookup, userAgent, conversationsMembersURL string, httpClient *http.Client) internal.ChannelMembershipFunc {
	if httpClient == nil {
		httpClient = defaultSlackPostMessageClient()
	}
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		userAgent = defaultSlackAPIUserAgent
	}
	// isMember runs the bounded page scan + member match on one owner's token; a token-lookup
	// failure wraps %w so the outer Grid fallback can retry on the org token.
	isMember := func(ctx context.Context, ownerID, channelID, userID string) (bool, error) {
		token, err := lookup(ctx, ownerID)
		if err != nil {
			return false, fmt.Errorf("conversations.members token lookup: %w", err)
		}
		if token = strings.TrimSpace(token); token == "" {
			return false, errors.New("conversations.members token lookup: empty token")
		}
		// Scan up to maxMembershipPages, following next_cursor: a hit returns true; an
		// exhausted-within-bound scan (or a member beyond the bound) returns false — the
		// fail-closed degradation.
		cursor := ""
		for page := 0; page < maxMembershipPages; page++ {
			members, next, err := fetchConversationsMembersPage(ctx, httpClient, conversationsMembersURL, userAgent, token, channelID, cursor)
			if err != nil {
				return false, err
			}
			for _, m := range members {
				if m == userID {
					return true, nil
				}
			}
			if cursor = next; cursor == "" {
				break
			}
		}
		return false, nil
	}
	return func(ctx context.Context, teamID, enterpriseID, channelID, userID string) (bool, error) {
		member, err := isMember(ctx, teamID, channelID, userID)
		if err == nil || !errors.Is(err, auth.ErrSlackBotTokenNotConfigured) {
			return member, err
		}
		if enterpriseID == "" || enterpriseID == teamID {
			return member, err
		}
		return isMember(ctx, enterpriseID, channelID, userID)
	}
}

// slackAssistantThreadsPort implements [internal.AssistantThreadsPort] over
// assistant.threads.setTitle / setSuggestedPrompts / setStatus. Each verb has its own
// shared-transport poster (token lookup + Grid fallback + parse), like the reactions
// port — it drives the Assistants-container UX (first-run title/prompts + per-turn status).
type slackAssistantThreadsPort struct {
	setTitle   *slackWebAPIPoster
	setPrompts *slackWebAPIPoster
	setStatus  *slackWebAPIPoster
}

func newSlackAssistantThreadsPortWithTokenLookup(lookup slackBotTokenLookup, userAgent, setTitleURL, setSuggestedPromptsURL, setStatusURL string, httpClient *http.Client) internal.AssistantThreadsPort {
	if httpClient == nil {
		httpClient = defaultSlackPostMessageClient()
	}
	setTitle := newSlackWebAPIPoster(lookup, userAgent, setTitleURL, "assistant.threads.setTitle",
		func(s int, h http.Header, raw []byte) error {
			return slackWebAPIResponseError("assistant.threads.setTitle", nil, s, h, raw)
		}, httpClient)
	setPrompts := newSlackWebAPIPoster(lookup, userAgent, setSuggestedPromptsURL, "assistant.threads.setSuggestedPrompts",
		func(s int, h http.Header, raw []byte) error {
			return slackWebAPIResponseError("assistant.threads.setSuggestedPrompts", nil, s, h, raw)
		}, httpClient)
	setStatus := newSlackWebAPIPoster(lookup, userAgent, setStatusURL, "assistant.threads.setStatus",
		func(s int, h http.Header, raw []byte) error {
			return slackWebAPIResponseError("assistant.threads.setStatus", nil, s, h, raw)
		}, httpClient)
	return &slackAssistantThreadsPort{setTitle: setTitle, setPrompts: setPrompts, setStatus: setStatus}
}

func (p *slackAssistantThreadsPort) SetTitle(ctx context.Context, teamID, enterpriseID, channelID, threadTS, title string) error {
	body, err := json.Marshal(struct {
		ChannelID string `json:"channel_id"`
		ThreadTS  string `json:"thread_ts"`
		Title     string `json:"title"`
	}{ChannelID: channelID, ThreadTS: threadTS, Title: title})
	if err != nil {
		return fmt.Errorf("assistant.threads.setTitle request marshal: %w", err)
	}
	return p.setTitle.post(ctx, teamID, enterpriseID, body)
}

func (p *slackAssistantThreadsPort) SetSuggestedPrompts(ctx context.Context, teamID, enterpriseID, channelID, threadTS string, prompts []internal.SuggestedPrompt) error {
	apiPrompts := make([]assistantPromptBody, len(prompts))
	for i := range prompts {
		apiPrompts[i] = assistantPromptBody{Title: prompts[i].Title, Message: prompts[i].Message}
	}
	body, err := json.Marshal(struct {
		ChannelID string                `json:"channel_id"`
		ThreadTS  string                `json:"thread_ts"`
		Prompts   []assistantPromptBody `json:"prompts"`
	}{ChannelID: channelID, ThreadTS: threadTS, Prompts: apiPrompts})
	if err != nil {
		return fmt.Errorf("assistant.threads.setSuggestedPrompts request marshal: %w", err)
	}
	return p.setPrompts.post(ctx, teamID, enterpriseID, body)
}

func (p *slackAssistantThreadsPort) SetStatus(ctx context.Context, teamID, enterpriseID, channelID, threadTS, status string) error {
	body, err := json.Marshal(struct {
		ChannelID string `json:"channel_id"`
		ThreadTS  string `json:"thread_ts"`
		Status    string `json:"status"`
	}{ChannelID: channelID, ThreadTS: threadTS, Status: status})
	if err != nil {
		return fmt.Errorf("assistant.threads.setStatus request marshal: %w", err)
	}
	return p.setStatus.post(ctx, teamID, enterpriseID, body)
}

// assistantPromptBody is the Slack assistant.threads.setSuggestedPrompts prompt
// shape ({title, message}).
type assistantPromptBody struct {
	Title   string `json:"title"`
	Message string `json:"message"`
}

// defaultSlackPostMessageClient builds the seam's HTTP client. Mirrors
// defaultSlackViewsOpenClient (CheckRedirect → ErrUseLastResponse so a redirect
// is surfaced as a response, not followed) but with the chat.postMessage timeout.
func defaultSlackPostMessageClient() *http.Client {
	return &http.Client{
		Timeout: slackChatPostMessageTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// slackWebAPIResponseError maps a Slack web API HTTP response to an error (or nil on
// success). op prefixes the error strings; benign holds ok:false error codes treated
// as SUCCESS (an idempotent no-op already at the desired end state — e.g. reactions.add
// of an existing reaction, reactions.remove of an absent one). Unlike views.open, most
// methods return HTTP 200 with `ok:false` for failures, so the ok field — not the
// status code — is the real signal; both the 429 path and a 200-body
// `ok:false:ratelimited` map to the rate-limit sentinel (load-bearing for the
// chat.postMessage delivery worker — do not "harmonize" the ratelimited branch away).
func slackWebAPIResponseError(op string, benign map[string]struct{}, statusCode int, header http.Header, raw []byte) error {
	if statusCode == http.StatusTooManyRequests {
		return internal.NewSlackRateLimitError(header.Get("Retry-After"))
	}
	if statusCode >= 300 {
		if snippet := slackAPIBodySnippet(raw); snippet != "" {
			return fmt.Errorf("%s returned HTTP %d: %s", op, statusCode, snippet)
		}
		return fmt.Errorf("%s returned HTTP %d", op, statusCode)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("%s: empty response body", op)
	}

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		if snippet := slackAPIBodySnippet(raw); snippet != "" {
			return fmt.Errorf("%s response JSON: %w: %s", op, err, snippet)
		}
		return fmt.Errorf("%s response JSON: %w", op, err)
	}
	if out.OK {
		return nil
	}
	code := slackAPIErrorCode(out.Error)
	if code == "ratelimited" {
		return internal.NewSlackRateLimitError(header.Get("Retry-After"))
	}
	if _, ok := benign[code]; ok {
		return nil
	}
	if code == "" {
		code = "not_ok"
	}
	if code == "missing_scope" {
		return fmt.Errorf("%s: %w", op, internal.ErrSlackMissingScope)
	}
	return &slackWebAPIError{op: op, code: code}
}

// slackChatPostMessageResponseError preserves the chat.postMessage error shape: no
// ok:false code is benign (a post that didn't happen is a real failure the delivery
// worker must surface).
func slackChatPostMessageResponseError(statusCode int, header http.Header, raw []byte) error {
	return slackWebAPIResponseError("chat.postMessage", nil, statusCode, header, raw)
}

func slackConversationsOpenResponseError(statusCode int, header http.Header, raw []byte) error {
	return slackWebAPIResponseError("conversations.open", nil, statusCode, header, raw)
}

func slackConversationsOpenChannelID(raw []byte) (string, error) {
	var out struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		if snippet := slackAPIBodySnippet(raw); snippet != "" {
			return "", fmt.Errorf("conversations.open response JSON: %w: %s", err, snippet)
		}
		return "", fmt.Errorf("conversations.open response JSON: %w", err)
	}
	channelID := strings.TrimSpace(out.Channel.ID)
	if channelID == "" {
		return "", errors.New("conversations.open response missing channel.id")
	}
	return channelID, nil
}

func slackChatPostEphemeralResponseError(statusCode int, header http.Header, raw []byte) error {
	return slackWebAPIResponseError("chat.postEphemeral", nil, statusCode, header, raw)
}

// slackReactionAddBenign / slackReactionRemoveBenign are the idempotent no-op ok:false
// codes the best-effort working-on-it ack treats as success: the reaction already
// exists (add) or is already absent (remove) — the desired end state already holds.
var (
	slackReactionAddBenign    = map[string]struct{}{"already_reacted": {}}
	slackReactionRemoveBenign = map[string]struct{}{"no_reaction": {}}
)

// slackReactionPort implements [internal.ReactionPort] over reactions.add /
// reactions.remove. Each verb has its own shared-transport poster (token lookup + Grid
// fallback + benign-tolerant parse), and the two share one http.Client (which is safe
// for the concurrent turns the async pool may run). This is the conversation-mode ack
// seam.
type slackReactionPort struct {
	add    *slackWebAPIPoster
	remove *slackWebAPIPoster
}

func newSlackReactionPortWithTokenLookup(lookup slackBotTokenLookup, userAgent, addURL, removeURL string, httpClient *http.Client) internal.ReactionPort {
	if httpClient == nil {
		// Reuse the chat.postMessage transport posture: 4s timeout + redirect-as-response.
		httpClient = defaultSlackPostMessageClient()
	}
	add := newSlackWebAPIPoster(lookup, userAgent, addURL, "reactions.add",
		func(s int, h http.Header, raw []byte) error {
			return slackWebAPIResponseError("reactions.add", slackReactionAddBenign, s, h, raw)
		}, httpClient)
	remove := newSlackWebAPIPoster(lookup, userAgent, removeURL, "reactions.remove",
		func(s int, h http.Header, raw []byte) error {
			return slackWebAPIResponseError("reactions.remove", slackReactionRemoveBenign, s, h, raw)
		}, httpClient)
	return &slackReactionPort{add: add, remove: remove}
}

func (p *slackReactionPort) Add(ctx context.Context, teamID, enterpriseID, channelID, timestamp, name string) error {
	return p.react(ctx, p.add, teamID, enterpriseID, channelID, timestamp, name)
}

func (p *slackReactionPort) Remove(ctx context.Context, teamID, enterpriseID, channelID, timestamp, name string) error {
	return p.react(ctx, p.remove, teamID, enterpriseID, channelID, timestamp, name)
}

// react marshals the (identical add/remove) reactions body once and posts it via the
// given verb's poster — the shared body of Add and Remove.
func (p *slackReactionPort) react(ctx context.Context, poster *slackWebAPIPoster, teamID, enterpriseID, channelID, timestamp, name string) error {
	body, err := marshalReactionBody(channelID, timestamp, name)
	if err != nil {
		return err
	}
	return poster.post(ctx, teamID, enterpriseID, body)
}

// marshalReactionBody builds the reactions.add/remove payload (identical for both).
func marshalReactionBody(channelID, timestamp, name string) ([]byte, error) {
	body, err := json.Marshal(struct {
		Channel   string `json:"channel"`
		Timestamp string `json:"timestamp"`
		Name      string `json:"name"`
	}{Channel: channelID, Timestamp: timestamp, Name: name})
	if err != nil {
		return nil, fmt.Errorf("reactions request marshal: %w", err)
	}
	return body, nil
}
