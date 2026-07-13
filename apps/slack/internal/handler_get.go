package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	slackoauth "github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackaudit"
	"github.com/layervai/qurl-integrations/shared/client"
)

// commonGetMintFailedMessage is the generic catch-all error shown to
// the user when the multi-hop `/qurl get` work fails on a branch we
// don't have a more specific message for. Lifted to a constant
// because three different mapMintError branches need it.
const commonGetMintFailedMessage = "Failed to create qURL. Please try again."

func getMintLimitMessage(apiErr *client.APIError) string {
	requestID := ""
	if apiErr != nil {
		requestID = apiErr.RequestID
	}
	return appendSlackReference("Cannot create another qURL right now", requestID) + ". Try again later or ask your Slack admin."
}

// getUsageMessage is the arg hint shown when `/qurl get` is invoked
// with no token. Bare `get` parses to [ErrEmptyResource]; the
// defensive empty-Alias guard below reuses the same copy so the user
// learns the `$<id>|$<alias>` grammar rather than seeing a terse
// sentinel.
const getUsageMessage = "Usage: `/qurl get <$id|$alias>` to create a qURL for a resource. Run `/qurl list` to see what's available."

// Access limits applied to every `/qurl get` mint. Raw URLs are unsupported
// (see urlNotSupportedGetMessage), but existing URL resources can be minted by
// their listed `$alias`. These limits bound a shared Slack-created link to a
// single short-lived viewer:
//   - resourceLinkExpiry: the link only admits a NEW visitor session for this
//     long after minting (qurl-service `expires_in`).
//   - resourceSessionDuration: how long an admitted visitor session lasts
//     (qurl-service `session_duration`).
//   - resourceMaxSessions: max concurrent visitor sessions (qurl-service
//     `max_sessions`).
//
// How the four limits stack (the OneTimeUse=true mint plus these three):
// OneTimeUse burns the link on its first redemption, so in steady state only
// one session is ever established and resourceMaxSessions=1 is belt-and-
// suspenders — it makes the "one viewer" intent explicit on the wire and
// stays correct if the one-time-use default is ever relaxed. resourceLinkExpiry
// bounds the window to redeem; resourceSessionDuration bounds how long that one
// session then lives.
//
// Enforcement is entirely server-side (qurl-service + qurl-router); this just
// sets the policy at mint time and requires the qurl-service resource-link
// session-limit support to be deployed (otherwise create returns 400 —
// hence the gating in the PR). Per-visitor identity is IP-based for now;
// layervai/qurl-service#777 tracks the cookie follow-up.
//
// As of qurl-service#778 these values are ALSO the server-side resource-link
// defaults (session_duration→1h, and one_time_use→single-visitor when
// max_sessions<=1), so setting them explicitly here is belt-and-suspenders:
// it pins the bot's intent on the wire and decouples it from any future
// change to the server defaults, rather than being load-bearing.
const (
	resourceLinkExpiry      = "1m"
	resourceSessionDuration = "1h"
	resourceMaxSessions     = 1
	// resourceLinkExpiryHuman is the user-facing rendering of resourceLinkExpiry
	// for the Slack reply — "1 minute" reads clearer to a recipient than the
	// terse "1m" duration syntax. Keep in sync with resourceLinkExpiry above.
	resourceLinkExpiryHuman = "1 minute"
)

// urlNotSupportedGetMessage is the user-facing copy for a raw-URL `/qurl get`.
// The parser flags the case with the terse [ErrURLNotSupportedGet] sentinel;
// this is the rich reply the handler renders so multi-sentence prose stays out
// of the parser's error values (repo convention). Existing URL resources are
// still mintable by their listed `$alias`; only ad hoc raw URL input is refused.
const urlNotSupportedGetMessage = "`/qurl get` works with a listed `$id` or `$alias` — raw URLs aren't supported. Run `/qurl list` and copy the URL resource's alias."

// resourceIDNotSupportedGetMessage is the user-facing copy for a `$r_<id>`
// `/qurl get` (the resource-id form is gone). Same terse-sentinel
// ([ErrResourceIDNotSupportedGet]) → rich-handler-copy split as the URL case.
const resourceIDNotSupportedGetMessage = "`/qurl get` takes a `$id` or a channel `$alias`, not an internal `r_...` identifier. Run `/qurl list` and copy the `$id` instead."

// unexpectedGetShapeMessage is the reply for getWork's defensive
// empty-alias arm; it routes the user to their operator rather than
// looping them on a retry. See that arm for why it's distinct from
// [commonGetMintFailedMessage].
const unexpectedGetShapeMessage = "Couldn't process that command. Please contact your Slack admin for assistance."

func ambiguousResourceAliasMessage(alias string) string {
	return fmt.Sprintf("`$%s` matches multiple resources in this channel. Ask your Slack admin to set a channel-specific alias for the one you need.", alias)
}

// errCodeConnectorDisabled is the qurl-service error-envelope `code` returned
// when qURL Connector resource minting is disabled for the workspace.
// TODO(upstream-contract): keep in lockstep with qurl-service's public
// connector-disabled error contract.
const errCodeConnectorDisabled = "connector_disabled"

// connectorDisabledMessage is shown when qurl-service returns
// [errCodeConnectorDisabled].
const connectorDisabledMessage = "Protected resources are not yet enabled for this workspace. Ask LayerV support."

// serviceUnreachableMessage is the "honest retry-friendly" copy
// surfaced for transport-class failures (5xx, dial errors, network
// errors). Distinct from the generic "Failed to create qURL" copy so
// the user knows a retry is the right next move.
const serviceUnreachableMessage = "Could not reach qURL. Please try again."

// serviceUnreachableMessageWith builds the retry-friendly message plus
// the opaque RequestID support handle. Upstream Title and Detail are
// intentionally suppressed because either can carry operator-grade text
// under service regressions. Falls back to [serviceUnreachableMessage]
// when no RequestID is present.
func serviceUnreachableMessageWith(apiErr *client.APIError) string {
	if apiErr == nil || apiErr.RequestID == "" {
		return serviceUnreachableMessage
	}
	return appendSlackReference("Could not reach qURL", apiErr.RequestID) + ". Please try again."
}

// channelRequiredMessage is the user-facing copy surfaced when a
// slash command that requires channel context (`/qurl get`,
// `/qurl aliases`) is invoked from a payload without a channel_id.
// Slack always sends channel_id on real slash commands; an empty
// value is a synthetic payload (test harness or future channel-less
// surface), so fail-closed.
const channelRequiredMessage = "This command must be invoked from a channel."

// noResourceForAliasMessage formats the channel-scoped "not visible here"
// copy surfaced when a token has no binding in this channel or resolves to a
// resource that is not allowed here. Phrased for an end user who doesn't know
// what an "alias" is: name the literal token the user typed, say plainly what
// state it's in, and give them BOTH a self-serve action (`/qurl aliases` lists
// what is configured here, so a typo is one tab away) AND the escalation path
// (ask the admin to wire it up) since only the admin can run setalias.
//
// The `/qurl aliases` breadcrumb is channel-scoped (it shows aliases
// bound here), and so is `/qurl list` now — both surface only what
// resolves in this channel, so the breadcrumb stays accurate. This also
// closes the former list/mint asymmetry once tracked by TODO(#460): list,
// aliases, and mint share one channel-scoped set ([Handler.allowedResourceIDsForGet]),
// so a user sees here exactly what they can mint here. The remaining UX
// nicety — telling a user which OTHER channels an alias is bound in — is
// deliberately not surfaced (that cross-channel disclosure is what the
// scoping closes).
func noResourceForAliasMessage(alias string) string {
	return fmt.Sprintf("`$%s` is not configured for this channel. Run `/qurl aliases` to see what's available here, or contact your Slack admin to add it.", alias)
}

// legacyAliasBindingMessage is the copy surfaced when a channel alias resolves
// to a value that isn't a resource id — for example, a raw URL bound by the
// pre-resource `/qurl set-alias`. Existing URL resources are supported, but raw
// URL binding values are not resource rows and cannot be passed to
// `POST /v1/resources/{id}/qurls`. Name the dead shortcut plainly and route to
// the admin (only an admin can re-point it at a resource). Same posture as
// [noResourceForAliasMessage].
//
// `alias` is interpolated verbatim into the reply AND a `/qurl-admin set-alias
// $<alias>` hint, both inside Slack inline-code fences. Callers pass a
// parser-validated token today, but rather than rely on that convention the
// charset is re-asserted here: a value that isn't [aliasCharsetPattern]-clean
// (e.g. one carrying a backtick) falls back to token-free copy, so a future
// caller can't reopen the Slack-fence-escaping surface the parser guards.
func legacyAliasBindingMessage(alias string) string {
	if !aliasCharsetPattern.MatchString(alias) {
		return "That channel alias points at a target that's no longer supported. Please ask your Slack admin to re-point it at a resource with `/qurl-admin set-alias`."
	}
	return fmt.Sprintf("`$%s` points directly at a URL, which is no longer supported for channel aliases. Please ask your Slack admin to re-point it at a resource with `/qurl-admin set-alias $%s $<id>`.", alias, alias)
}

// authFailureMessageGet is the auth-failure copy shown when API-key
// lookup fails for /qurl get. Same shape as authFailureMessage but
// distinguished because the get path also needs to gracefully fall
// back when the workspace isn't configured yet — the parent
// authErrorMessage helper handles that.

// humanFallbackMoment is the placeholder we surface when a
// rate-limit retry-after window isn't resolvable to a concrete
// duration (server returned 0 / negative / sub-second).
const humanFallbackMoment = "a moment"

// userError is the typed error returned from getWork when the
// message text is intended for direct user display. Wrapping the
// message in a typed error documents that it routes to the user via
// the `:warning:` ephemeral (not into a Go log/error chain) and
// dodges revive ST1005's sentence-case warning on user-visible text.
type userError struct{ msg string }

// Error returns the user-facing message verbatim.
func (e *userError) Error() string { return e.msg }

// errAdminStoreNotConfigured is returned by getWork when the handler's
// AdminStore is nil (sandbox / no-DDB deployment). Surfaces as a user-
// facing message that doesn't expose the "AdminStore" implementation
// term — it points the user at the workspace admin who would have
// completed the install.
var errAdminStoreNotConfigured = &userError{msg: "qURL admin features are not yet configured for this workspace. Ask the workspace owner who connected qURL, or contact qURL support at " + qurlContactURL + "."}

// handleGet implements `/qurl get <$id|$alias>`:
//  1. Parse the slash-command text → [Command]. The positional arg is a
//     listed resource ID/alias token: a tunnel `$slug`, a channel-scoped
//     `$alias`, or a resource-level URL alias visible in this channel. Raw URLs
//     and `$r_<id>` resource IDs are rejected at parse time.
//  2. Ack within 3s via [runAsync] (200 + ackWorkingOnIt).
//  3. Async goroutine: rate-limit, resolve the token → resource_id (channel
//     alias binding, tunnel-slug fallback, then URL resource-alias fallback,
//     all channel-scoped), then mint. POSTs the result to response_url.
//
// Optional flags:
//   - `dm:true` → the minted link is delivered via PostDMBlocks to the
//     user's DM (an Enter Portal button) instead of the channel ephemeral.
//     Refused up front (getWork) with a "DM delivery is not configured —
//     re-run without dm:true" warning when PostDMBlocks is nil, rather than
//     falling back in-channel against the user's privacy intent.
//   - `reason:"…"` → forwarded as [client.CreateInput.Reason] so it
//     lands in the audit row.
func (h *Handler) handleGet(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	cmd, err := Parse(text)
	if err != nil {
		// ErrURLNotSupportedGet is a terse parser sentinel; the handler
		// owns the rich, fix-naming user copy (the parser keeps prose out
		// of error values). Every other parse error's terse text is fine
		// to surface verbatim.
		if errors.Is(err, ErrURLNotSupportedGet) {
			respondSlack(w, ":warning: "+urlNotSupportedGetMessage)
			return
		}
		if errors.Is(err, ErrResourceIDNotSupportedGet) {
			respondSlack(w, ":warning: "+resourceIDNotSupportedGetMessage)
			return
		}
		// Bare `get` (no token) parses to ErrEmptyResource. Surface the
		// arg hint instead of the terse sentinel text so the user learns
		// the grammar — same copy as the defensive empty-Alias guard below.
		if errors.Is(err, ErrEmptyResource) {
			respondSlack(w, ":warning: "+getUsageMessage)
			return
		}
		respondSlack(w, ":warning: "+err.Error())
		return
	}
	if cmd.Subcommand != SubcmdGet {
		// Defensive: dispatcher routed `get*` here but the parser
		// disagreed. Fall through to the unknown-subcommand reply.
		respondSlack(w, fmt.Sprintf("Unknown subcommand: `%s`. Try `/qurl help`.", text))
		return
	}
	// Defensive: a successful parseGet always sets cmd.Alias (the tunnel
	// `$slug` / channel `$alias` form) — empty args and raw URLs already
	// returned an error above. This guard only fires if a future parser
	// change adds a form that leaves Alias empty; surface the usage hint
	// rather than silently dispatching an unmintable command.
	if cmd.Alias == "" {
		respondSlack(w, ":warning: "+getUsageMessage)
		return
	}

	h.runAsync(w, "get", values, func(ctx context.Context, log *slog.Logger) {
		h.processGet(ctx, log, values, cmd)
	})
}

// processGet is the async-worker body for /qurl get. Builds the
// reply text and POSTs it via response_url. Errors from the inner
// pipeline reach the user as a friendly `:warning:` message.
func (h *Handler) processGet(ctx context.Context, log *slog.Logger, values url.Values, cmd *Command) {
	responseURL := values.Get(fieldResponseURL)
	teamID := values.Get(fieldTeamID)
	enterpriseID := strings.TrimSpace(values.Get(fieldEnterpriseID))
	channelID := values.Get(fieldChannelID)
	userID := values.Get(fieldUserID)
	triggerID := values.Get(fieldTriggerID)

	if channelID == "" {
		// Channel-scope guard; see [Handler.processAliases] for the
		// full rationale (single source of truth).
		log.Warn("get: empty channel_id; refusing channel-less invocation")
		_ = h.postResponse(log, responseURL, ":warning: "+channelRequiredMessage)
		return
	}

	res, err := h.getWork(ctx, log, &getWorkArgs{
		cmd:          cmd,
		teamID:       teamID,
		enterpriseID: enterpriseID,
		channelID:    channelID,
		userID:       userID,
		triggerID:    triggerID,
	})
	h.finishGet(log, responseURL, res, err)
}

// finishGet posts a [Handler.getWork] outcome to response_url as an
// ephemeral: the Enter Portal link render on success, or the [*userError]
// message (prefixed with `:warning:`) on failure. A non-userError leak is a
// programmer mistake — log it loud and surface the generic catch-all so
// internals never reach Slack. Shared by the `/qurl get` slash path
// ([Handler.processGet]) and the `/qurl list` "Create qURL" button
// ([Handler.processButtonGet]) so both render identical replies.
func (h *Handler) finishGet(log *slog.Logger, responseURL string, res getResult, err error) {
	if err != nil {
		_ = h.postResponse(log, responseURL, mapCoreError(log, err, commonGetMintFailedMessage))
		return
	}
	// A minted link carries blocks (the Enter Portal button); post them with the
	// text as the notification / non-block-client fallback. The dm:true path
	// returns a blocks-less confirmation ("Sent to your DM."), which posts as
	// plain text.
	if res.blocks != nil {
		_ = h.postResponseBlocks(log, responseURL, res.text, res.blocks)
		return
	}
	_ = h.postResponse(log, responseURL, res.text)
}

// mapCoreError renders a delivery-agnostic mutation core's error as a Slack-safe
// string: a [*userError]'s message (warning-prefixed), or a generic fallback for
// an unexpected non-userError leak (logged loud — internals never reach Slack).
// Shared by finishGet (the /qurl get + Create-qURL button) and the conversation-
// mode confirm flow (executeAgentAction).
func mapCoreError(log *slog.Logger, err error, generic string) string {
	var ue *userError
	if errors.As(err, &ue) {
		return ":warning: " + ue.msg
	}
	log.Error("unexpected non-userError leaked from a mutation core", "error", err, "fallback", generic)
	return ":warning: " + generic
}

// getWorkArgs bundles the closure inputs for [Handler.getWork].
type getWorkArgs struct {
	cmd          *Command
	teamID       string
	enterpriseID string
	channelID    string
	userID       string
	triggerID    string
}

// getResult is a [Handler.getWork] success outcome. Every delivery surface
// (channel ephemeral, DM, agent-confirm private) renders the minted link the
// same way: an "Enter Portal" URL button (blocks) with text as the
// notification / non-block-client fallback.
//
//   - text: ALWAYS set. For a minted link it is the fallback that accompanies
//     blocks (and still carries the raw URL so a client that can't render
//     blocks isn't dead-ended). For the `dm:true` variant it is instead the
//     standalone ":incoming_envelope: Sent to your DM." confirmation, delivered
//     as plain text with blocks nil (the link itself already went to the DM).
//   - blocks: non-nil ONLY when this result IS the link render. A caller posts
//     blocks when present and falls back to a plain-text post of `text`
//     otherwise, so error/confirmation strings stay text-only.
type getResult struct {
	text   string
	blocks []any
}

// enterPortalActionID is the action_id on the "Enter Portal" URL button carrying
// a minted qURL. The button's `url` opens the portal directly in the browser;
// Slack still POSTs a block_actions interaction on click, which handleBlockActions
// no-op-acks via its unrecognized-action `200 OK` path — we never round-trip to
// re-mint on it.
const enterPortalActionID = "qurl_enter_portal"

// enterPortalButtonLabel is the link button's label. Kept consistent with the
// qURL Go SDK (qurl-go's EnterPortal / EnterPortalWith) and the `qurl enter` CLI
// (whose success copy is "Portal entered"): reaching a qURL's target is
// "entering the portal".
const enterPortalButtonLabel = "Enter Portal"

// oneTimeUseNotice is the shared "one-time use · link expires in X" phrase used in
// BOTH the Enter Portal headline and the plain-text fallback, so the wording (and
// the admit-window value) can't drift between the two renderings.
func oneTimeUseNotice() string {
	return "one-time use · link expires in " + resourceLinkExpiryHuman
}

// renderGetSuccess builds the two renderings of a minted one-time link: the
// Block Kit blocks (headline section + primary "Enter Portal" URL button) and
// the plain-text fallback. The link rides in the button's `url` rather than as
// raw prose, so it's one tap and Slack never link-unfurls it (an unfurl fetch
// could brush a one-time link). The fallback KEEPS the raw URL so a non-block
// client still has something actionable. Both strings here are static templates —
// no user or LLM input — so the mrkdwn headline carries no injection surface.
func renderGetSuccess(link string) (fallbackText string, blocks []any) {
	button := primaryURLButtonElement(enterPortalButtonLabel, enterPortalActionID, link)
	// sectionBlock renders mrkdwn (bold + :emoji:); the text is a static template
	// with no user/LLM input, so the mrkdwn carries no injection surface.
	blocks = []any{
		sectionBlock(":link: *qURL ready* — " + oneTimeUseNotice()),
		actionsBlock(button),
	}
	return enterPortalFallbackText(link), blocks
}

// enterPortalFallbackText is the notification / non-block-client fallback for a
// minted link. It mirrors the pre-button prose (raw URL included) so a client
// that can't render the Enter Portal button still receives a usable link. NOTE:
// Slack also uses this text as the push/desktop notification preview, so the raw
// one-time URL still transits the notification channel — same exposure as the
// pre-button prose message, and out of scope for the button's in-body privacy win.
// A link-less fallback for notification-capable clients is tracked in #922.
func enterPortalFallbackText(link string) string {
	return ":link: qURL ready: " + link + " (" + oneTimeUseNotice() + ")"
}

// slackButtonURLMaxLen is Slack's hard cap on a Block Kit button `url`; a longer
// value bounces the whole message, so it must fail the guard below like any other
// url Slack would reject.
const slackButtonURLMaxLen = 3000

// isHTTPSURL reports whether s is an absolute https URL WITH a host and within
// Slack's button-url length cap. A minted qurl_link is always a short absolute
// https qurl.link URL, so this both matches the server contract (https-only,
// mirroring the resourceExposeSchemeHTTPS checks in handler_expose.go /
// handler_agent_confirm.go) AND is a valid Slack Block Kit button `url`; a value
// failing it is a server contract surprise (see the getWork guard), not ordinary
// input. Uses url.Parse rather than a scheme prefix so a scheme-only ("https://")
// or otherwise malformed value — which Slack would also reject — is caught here.
func isHTTPSURL(s string) bool {
	if len(s) > slackButtonURLMaxLen {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == resourceExposeSchemeHTTPS && u.Host != ""
}

// getWork runs the inner resolve→rate-limit→mint pipeline for the token form
// (`/qurl get $id` or `/qurl get $alias`). Raw URLs and `$r_<id>` resource IDs
// are rejected at parse time. Returns a [getResult] (the Enter Portal link
// render, or the `dm:true` "Sent to your DM." confirmation) on success, or a
// [*userError] whose msg routes to the user.
func (h *Handler) getWork(ctx context.Context, log *slog.Logger, args *getWorkArgs) (getResult, error) {
	alias := args.cmd.Alias

	// Refuse `dm:true` early when PostDMBlocks is not wired — the user's
	// intent is "do not leak the link in channel history", and a
	// silent channel-fallback violates that intent. Fail-fast here
	// avoids burning a mint quota on a request that can't be
	// delivered the way the user asked. (deliverGetDM delivers the Enter
	// Portal render via PostDMBlocks, so that is the seam to guard on.)
	if args.cmd.DM() && h.cfg.PostDMBlocks == nil {
		return getResult{}, &userError{msg: "DM delivery is not configured for this workspace. Re-run the command without `dm:true` to receive the link in-channel."}
	}

	if alias == "" {
		// Defensive: parseGet guarantees a non-empty alias-shaped token
		// (raw URLs and `$r_<id>` are rejected at parse time). This only
		// fires if a future parser change leaves Alias empty — refuse to
		// mint rather than fall through to an unauthorized resolve.
		// Distinct internal-error copy (not the retry-friendly
		// commonGetMintFailedMessage) so a real occurrence correlates to
		// the log.Error below instead of looping on "please try again".
		log.Error("get: empty alias token reached getWork — refusing to mint", "raw", args.cmd.Raw)
		return getResult{}, &userError{msg: unexpectedGetShapeMessage}
	}

	// AdminStore is required both for token resolution (the channel-alias lookup
	// in resolveTokenForGet) and for the rate-limit gate further down.
	if h.cfg.AdminStore == nil {
		log.Warn("get: AdminStore is nil; token-form lookup unavailable", "team_id", args.teamID)
		return getResult{}, errAdminStoreNotConfigured
	}

	boundResourceID, err := h.resolveTokenForGet(ctx, log, args.teamID, args.channelID, args.userID, alias)
	if err != nil {
		// Resolution failures (typoed / unknown / not-channel-authorized aliases)
		// return BEFORE the rate-limit gate below, so a fat-fingered
		// `/qurl get $typo` never burns the user's quota.
		return getResult{}, err
	}

	// Rate-limit AFTER a successful resolution: only a request that resolved to a
	// real, channel-authorized resource — i.e. an actual mint attempt — counts
	// against the user's quota. The dm:true delivery guard above stays earliest
	// so an undeliverable privacy request consumes nothing either. (Resolution
	// work for unknown aliases is instead bounded by Slack's own per-user
	// slash-command throttle, not by spending the user's mint quota on typos.)
	ok, retry, err := h.cfg.AdminStore.CheckRateLimit(ctx, args.userID, args.teamID)
	if err != nil {
		log.Warn("get: rate-limit check failed", "error", err, "team_id", args.teamID, "user_id", args.userID)
		return getResult{}, &userError{msg: rateLimitErrorMessage(err)}
	}
	if !ok {
		return getResult{}, &userError{msg: rateLimitMessage(retry, "")}
	}

	input := client.CreateInput{
		Reason: args.cmd.Reason(),
		// One-time use is the only mode for `/qurl get` — there is no
		// `once` flag; every minted link burns on first redemption.
		OneTimeUse: true,
		// Slack-created resource access limits — see the const block above.
		ExpiresIn:       resourceLinkExpiry,
		SessionDuration: resourceSessionDuration,
		MaxSessions:     resourceMaxSessions,
		IdempotencyKey:  IdempotencyKey(args.teamID, args.channelID, args.userID, args.triggerID),
		ResourceID:      boundResourceID,
	}

	c, err := h.authenticatedClient(ctx, args.teamID)
	if err != nil {
		log.Error("get: API key lookup failed", "error", err)
		return getResult{}, &userError{msg: authErrorMessage(err)}
	}

	out, err := c.Create(ctx, input)
	if err != nil {
		return getResult{}, mapMintError(log, err)
	}
	// Defensive: an empty OR non-https qurl_link is a server contract surprise (mints
	// return absolute https qurl.link URLs). The Enter Portal render puts the link in a
	// Block Kit button `url`, and Slack rejects the WHOLE message if that url is
	// malformed — so a bad link would bounce the block post and fail delivery after the
	// mint is already burned. Reject it here with the generic retry message + a loud log
	// (same disposition as empty), rather than ship a doomed block message. Not a text
	// fallback: a non-URL link is broken, not a rendering-mode choice.
	if !isHTTPSURL(out.QURLLink) {
		log.Error("get: mint returned empty or non-https qurl_link — server contract surprise", "resource_id", input.ResourceID, "has_link", out.QURLLink != "")
		return getResult{}, &userError{msg: commonGetMintFailedMessage}
	}

	// Render the minted link as an "Enter Portal" URL button (blocks) plus a
	// plain-text fallback. The one-time-use / admit-window suffix rides in the
	// headline: that window is tight, so a recipient who taps after it lapses
	// gets a dead portal, and the copy tells them why.
	fallbackText, blocks := renderGetSuccess(out.QURLLink)
	if args.cmd.DM() {
		// deliverGetDM posts the Enter Portal blocks to the user's DM and returns
		// the plain-text ":incoming_envelope: Sent to your DM." status. The link
		// render already went to the DM, so this result carries no blocks — the
		// status confirmation is delivered as plain text in-channel.
		return getResult{text: h.deliverGetDM(ctx, log, args.teamID, args.enterpriseID, args.userID, fallbackText, blocks)}, nil
	}
	return getResult{text: fallbackText, blocks: blocks}, nil
}

// resolveTokenForGet resolves a `$<token>` (channel alias, tunnel slug, or URL
// resource alias) to a mintable resource for /qurl get, enforcing channel
// authorization. Resolution order:
//
//  1. Channel alias binding (`channel_policies.alias_bindings`). The
//     presence of the binding in THIS channel is itself the
//     authorization signal — `/qurl-admin set-alias` is the admin act
//     that authorizes a resource for use here.
//  2. Tunnel-slug fallback. When no binding matches, the token may
//     still be a tunnel slug: `/qurl list` renders `$<slug>` for tunnels
//     granted to this channel via `allowed_resource_ids` that have no
//     `alias_bindings` entry here (e.g. a tunnel protected in this channel
//     from the `/qurl list` Edit modal without an alias). Resolve the
//     slug to its resource_id and gate it through the channel allow-set
//     ([Handler.allowedResourceIDsForGet]) so the list→get round-trip the
//     list advertises stays honest — the user references the `$<slug>`,
//     never the opaque resource_id.
//  3. Listed URL resource-alias fallback. URL resources do not have tunnel slugs,
//     but `/qurl list` renders resource aliases when present. Resolve that
//     alias from the resource page and choose a channel-allowed match before
//     minting; if more than one channel-allowed resource shares that alias,
//     fail closed as ambiguous rather than minting an arbitrary row.
//
// Cost note: this runs BEFORE the per-user rate-limit gate in getWork — the gate
// only fires once a token resolves, so a typo never burns the user's quota (see
// getWork). For a CONFIGURED channel (one with a non-empty allow-set), a binding
// miss therefore incurs its upstream resource lookups (slug, then the first-page
// alias scan when needed) un-throttled by that gate, including for plain typos.
// That's the deliberate "spend a read rather than burn the user's quota on a
// fat-fingered alias" tradeoff — don't "optimize" it away by short-circuiting the
// fallbacks on a binding miss in a configured channel. Each scan is bounded by
// listResourcesScanLimit and Slack's own per-user slash-command throttle bounds
// the request rate. If the in-bot limiter ever needs to shed this resolution
// cost too, add a cheap token-shape pre-filter before the alias scan rather than
// moving the gate back ahead of resolution.
//
// One NARROW exception (closes #534): when the channel allow-set is EMPTY (a
// "cold" channel with no protected resources), BOTH fallbacks below would be
// gated out anyway — neither a tunnel slug nor a resource alias can match an
// empty set — so the upstream GET /v1/resources?slug= would be pure waste. Worse,
// it's an UNMETERED probe surface: those upstream lookups run before the per-user
// rate-limit gate, so `/qurl get $typo1`, `$typo2`, … from a cold channel fan out
// one upstream hop each against the workspace API key, throttled only by Slack's
// own slash-command limit. So the cold-channel case is short-circuited on the
// allow-set DDB read alone. This is NOT the blanket "skip the fallbacks on a
// binding miss" the tradeoff above warns against: it suppresses the hop ONLY when
// the downstream channel gate is guaranteed to reject every fallback result.
//
// Caller must have already checked AdminStore (getWork does this before
// resolving). Returns a [*userError] on lookup failure, not-a-known-token, or
// not-allowed-here.
func (h *Handler) resolveTokenForGet(ctx context.Context, log *slog.Logger, teamID, channelID, userID, token string) (string, error) {
	resourceID, found, err := h.cfg.AdminStore.LookupChannelAlias(ctx, teamID, channelID, token)
	if err != nil {
		log.Warn("get: alias lookup failed", "error", err, "team_id", teamID, "channel_id", channelID, "token", token)
		return "", &userError{msg: serviceUnreachableMessage}
	}
	if found {
		// Legacy-binding guard: the pre-resource `/qurl set-alias`
		// stored raw URLs verbatim in alias_bindings, and those rows can
		// survive. Resolving one would hand a URL to the resource-scoped mint
		// call and surface as the generic retry error, stranding the user.
		// Gate on the `r_` prefix (not
		// an exact id-shape check — a stored id is whatever qurl-service
		// issued, length not guaranteed to match the 11-char get-token
		// shape): a `r_<id>` is a real resource and still mints, only a
		// non-`r_` value (a raw URL) is refused with a re-bind hint.
		// Residual: a junk `r_<typo>` row (the old parser rejected only
		// the bare `r_` sigil) passes this prefix check and 404s at mint
		// → the generic retry copy. Accepted as rare — an admin re-bind
		// is the same fix as for a raw URL row.
		if !strings.HasPrefix(resourceID, "r_") {
			log.Warn("get: channel alias bound to a non-resource-id target — refusing to mint", "team_id", teamID, "channel_id", channelID, "token", token)
			return "", &userError{msg: legacyAliasBindingMessage(token)}
		}
		return resourceID, nil
	}

	// No binding — fetch the channel allow-set FIRST (a DDB read), BEFORE any
	// upstream lookup. An empty set means neither the tunnel-slug fallback nor
	// the resource-alias fallback could pass the channel gate below, so the
	// upstream GET /v1/resources?slug= would be pure waste — and an unmetered
	// cold-channel probe surface (#534), since these fallbacks run ahead of the
	// per-user rate-limit gate. Short-circuit on the DDB read alone. This
	// NARROWS the "spend a read rather than burn quota on a typo" tradeoff
	// documented above: it still holds for a CONFIGURED channel; only a channel
	// with no protected resources spends nothing upstream. It is NOT a blanket
	// skip of the fallbacks on a binding miss — it suppresses the hop solely for
	// the case the downstream channel gate is guaranteed to reject anyway.
	allowedSet, authErr := h.allowedResourceIDsForGet(ctx, log, teamID, channelID)
	if authErr != nil {
		return "", authErr
	}
	if len(allowedSet) == 0 {
		log.Debug("get: channel has no protected resources — skipping slug/alias fallback", "team_id", teamID, "channel_id", channelID, "token", token)
		return "", &userError{msg: noResourceForAliasMessage(token)}
	}

	// Try the token as a tunnel slug, then authorize against the set above.
	slugResourceID, slugErr := h.resolveTunnelSlugAliasTarget(ctx, teamID, token)
	if slugErr != nil {
		if !errors.Is(slugErr, errTunnelSlugNotFound) {
			log.Warn("get: tunnel-slug fallback lookup failed", "error", slugErr, "team_id", teamID, "slug", token)
			return "", &userError{msg: serviceUnreachableMessage}
		}
		aliasResourceID, aliasFound, aliasErr := h.resolveListedResourceAliasForGet(ctx, log, teamID, channelID, userID, token, allowedSet)
		if aliasErr != nil {
			return "", aliasErr
		}
		if !aliasFound {
			// Neither a channel alias, live tunnel slug, nor resource alias.
			return "", &userError{msg: noResourceForAliasMessage(token)}
		}
		return aliasResourceID, nil
	}
	if _, allowed := allowedSet[slugResourceID]; !allowed {
		if aliasResourceID, aliasFound, aliasErr := h.resolveListedResourceAliasForGet(ctx, log, teamID, channelID, userID, token, allowedSet); aliasErr != nil {
			return "", aliasErr
		} else if aliasFound {
			return aliasResourceID, nil
		}
		// Collapse to the SAME "not configured" copy as the slug-not-found
		// branch above. A non-admin must not be able to distinguish "this slug
		// exists in the workspace but isn't allowed in this channel" from "no
		// such slug" — that gap is a tunnel-slug enumeration oracle. Logs the
		// real reason for operators; the wire text stays uniform.
		log.Debug("get: tunnel slug resolved but not allowed in channel — surfacing not-configured copy", "team_id", teamID, "channel_id", channelID, "user_id", userID, "slug", token)
		return "", &userError{msg: noResourceForAliasMessage(token)}
	}
	return slugResourceID, nil
}

func (h *Handler) resolveListedResourceAliasForGet(ctx context.Context, log *slog.Logger, teamID, channelID, userID, token string, allowedSet map[string]struct{}) (resourceID string, found bool, err error) {
	// Both current callers pass a non-nil allowedSet: the cold-channel
	// short-circuit in resolveTokenForGet returns early on an empty set, so a
	// non-empty set always reaches here. This re-fetch is therefore currently
	// unreachable and is retained only as defensive depth for any future caller.
	if allowedSet == nil {
		var authErr error
		allowedSet, authErr = h.allowedResourceIDsForGet(ctx, log, teamID, channelID)
		if authErr != nil {
			return "", false, authErr
		}
	}
	resources, aliasErr := h.lookupListedResourceAliasesForGet(ctx, log, teamID, token)
	if aliasErr != nil {
		log.Warn("get: resource-alias fallback lookup failed", "error", aliasErr, "team_id", teamID, "alias", token)
		return "", false, &userError{msg: serviceUnreachableMessage}
	}
	if len(resources) == 0 {
		return "", false, nil
	}

	allowedMatches := make([]string, 0, len(resources))
	for i := range resources {
		if _, ok := allowedSet[resources[i].ResourceID]; ok {
			allowedMatches = append(allowedMatches, resources[i].ResourceID)
		}
	}
	if len(allowedMatches) == 1 {
		return allowedMatches[0], true, nil
	}
	if len(allowedMatches) > 1 {
		log.Debug("get: resource alias matched multiple resources allowed in channel — refusing ambiguous mint", "team_id", teamID, "channel_id", channelID, "user_id", userID, "alias", token, "match_count", len(allowedMatches))
		return "", false, &userError{msg: ambiguousResourceAliasMessage(token)}
	}
	if len(resources) == 1 {
		log.Debug("get: resource alias resolved but not allowed in channel — surfacing not-configured copy", "team_id", teamID, "channel_id", channelID, "user_id", userID, "alias", token)
	} else {
		log.Debug("get: resource alias matched only resources not allowed in channel — surfacing not-configured copy", "team_id", teamID, "channel_id", channelID, "user_id", userID, "alias", token, "match_count", len(resources))
	}
	return "", false, nil
}

func (h *Handler) lookupListedResourceAliasesForGet(ctx context.Context, log *slog.Logger, teamID, alias string) ([]client.Resource, error) {
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		return nil, err
	}
	page, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesScanLimit})
	if err != nil {
		return nil, err
	}
	out := make([]client.Resource, 0, 1)
	for i := range page.Resources {
		resource := page.Resources[i]
		if resource.Status == client.StatusRevoked {
			continue
		}
		if !isURLResource(&resource) {
			continue
		}
		if resource.Alias == alias {
			out = append(out, resource)
		}
	}
	if page.HasMore {
		log.Debug("get: resource lookup scanned first page only", "scan_limit", listResourcesScanLimit, "team_id", teamID)
	}
	return out, nil
}

// allowedResourceIDsForGet returns the resource IDs mintable from channelID:
// the `AllowedResourceIDsForChannel` union of `alias_bindings.values()` and
// `allowed_resource_ids`. The gate is purely channel-scoped — it does NOT
// depend on who the caller is.
//
// No admin bypass: a workspace admin who runs `/qurl get $<slug>` from a
// channel the tunnel isn't protected in is refused exactly like anyone else.
// (A prior version let admins mint anything because `/qurl list` was
// workspace-wide, so an admin "saw" every slug. Now that list, aliases, and
// mint all share this one channel-scoped definition, the bypass would be a
// hole: someone who learned a slug/alias/channel-id elsewhere must still not
// be able to mint a tunnel from a channel it isn't protected in.) Admins manage
// where a tunnel is exposed via `/qurl-admin protect-connector` and the
// `/qurl list` Edit modal — not by minting from arbitrary channels. Together
// with the list-side scan, this closes the former TODO(#460) list/mint
// asymmetry for visible rows: you can only see resources protected here, and
// minted tokens are rechecked against this same allow-set. URL resource-alias
// fallback and duplicate-alias ambiguity detection still depend on the first
// listResourcesScanLimit rows until #590 moves listing/resolution to an
// allow-set-driven fetch; channel aliases and tunnel slugs do not depend on
// that scan.
//
// Returns [*userError] on AdminStore-nil or allow-set fetch failure so callers
// fail closed. Used by the tunnel-slug and listed URL resource-alias fallbacks
// in resolveTokenForGet. The alias-binding path is already channel-scoped by
// the binding's presence.
func (h *Handler) allowedResourceIDsForGet(ctx context.Context, log *slog.Logger, teamID, channelID string) (map[string]struct{}, error) {
	// Needs an AdminStore for the channel allow-set. Same fail-closed posture
	// as the alias-form lookup on a no-DDB sandbox.
	if h.cfg.AdminStore == nil {
		log.Warn("get: AdminStore is nil; authorization unavailable", "team_id", teamID)
		return nil, errAdminStoreNotConfigured
	}
	allowed, err := h.cfg.AdminStore.AllowedResourceIDsForChannel(ctx, teamID, channelID)
	if err != nil {
		log.Warn("get: allowed-resource fetch failed", "error", err, "team_id", teamID, "channel_id", channelID)
		return nil, &userError{msg: serviceUnreachableMessage}
	}
	return allowed, nil
}

// deliverGetDM handles the `dm:true` variant. The Enter Portal render (blocks,
// with fallbackText as the notification/non-block fallback) goes to the user's
// DM via PostDMBlocks; the response_url ephemeral confirms (without leaking the
// link in channel history).
//
// PostDMBlocks-nil is rejected earlier in getWork — the dm:true contract is
// privacy ("do not leak the link in channel history") and a silent
// channel-fallback violates that. If PostDMBlocks is wired but the call itself
// fails, we surface the failure without re-posting the link (the user can retry
// without dm:true if they want it in-channel).
func (h *Handler) deliverGetDM(ctx context.Context, log *slog.Logger, teamID, enterpriseID, userID, fallbackText string, blocks []any) string {
	if err := h.cfg.PostDMBlocks(ctx, teamID, enterpriseID, userID, blocks, fallbackText); err != nil {
		log.Warn("get: DM post failed", "error", err)
		if errors.Is(err, ErrSlackMissingScope) {
			return ":warning: Could not DM you the link. " + h.latestSlackAppInstallMessage("Private qURL DM delivery", "re-run the command")
		}
		return ":warning: Could not DM you the link. Please re-run the command without `dm:true` to receive it in-channel."
	}
	return ":incoming_envelope: Sent to your DM."
}

// mapMintError converts an [*client.APIError] from the mint into a
// friendly message. Rate-limit + Connector-disabled get specific text;
// transport-class (5xx/network) gets the retry-friendly
// [serviceUnreachableMessage]; everything else gets the generic
// [commonGetMintFailedMessage].
func mapMintError(log *slog.Logger, err error) error {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusTooManyRequests:
			retry := time.Duration(apiErr.RetryAfter) * time.Second
			return &userError{msg: rateLimitMessage(retry, apiErr.RequestID)}
		case http.StatusForbidden:
			if apiErr.Code == errCodeConnectorDisabled {
				return &userError{msg: connectorDisabledMessage}
			}
			if isExpectedGetMintForbiddenCode(apiErr.Code) {
				log.Info("get: mint rejected with expected quota-class 403", withRequestIDAttr(apiErr.RequestID, "code", apiErr.Code, "detail", apiErr.Detail)...)
				return &userError{msg: getMintLimitMessage(apiErr)}
			}
			logGetDependencyAuthFailure(log, apiErr)
			// 403 with an unrecognized code is a server-contract
			// surprise — log loud so a future rename of
			// `connector_disabled` doesn't get silently masked.
			log.Error("get: mint rejected with 403 — unmapped error code", withRequestIDAttr(apiErr.RequestID, "code", apiErr.Code, "detail", apiErr.Detail)...)
			return &userError{msg: commonGetMintFailedMessage}
		case http.StatusBadRequest:
			// `mutually_exclusive_fields` → server contract drift,
			// not a user error. Surface friendly + log loud.
			log.Error("get: mint rejected with 400 — check resource_id/target_url contract", withRequestIDAttr(apiErr.RequestID, "code", apiErr.Code, "detail", apiErr.Detail)...)
			return &userError{msg: commonGetMintFailedMessage}
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			log.Warn("get: mint failed with transport-class error", withRequestIDAttr(apiErr.RequestID, "status", apiErr.StatusCode, "code", apiErr.Code)...)
			return &userError{msg: serviceUnreachableMessageWith(apiErr)}
		default:
			// Unmapped 5xx (e.g. 500, 599) is server-side trouble —
			// same retry-friendly disposition as 502/503/504 above.
			// Falling through to commonGetMintFailedMessage on a 500
			// would tell the user "permanent failure, do not retry"
			// when the upstream is actually transient.
			if apiErr.StatusCode >= 500 && apiErr.StatusCode < 600 {
				log.Warn("get: mint failed with unmapped 5xx", withRequestIDAttr(apiErr.RequestID, "status", apiErr.StatusCode, "code", apiErr.Code)...)
				return &userError{msg: serviceUnreachableMessageWith(apiErr)}
			}
			// Other unmapped statuses (401, 404, 422, etc.) are
			// permanent-class — log loud so the operator sees the
			// contract surprise, surface the generic message so the
			// user isn't told to retry forever.
			if apiErr.StatusCode == http.StatusUnauthorized {
				logGetDependencyAuthFailure(log, apiErr)
			}
			log.Error("get: mint rejected with unmapped status", withRequestIDAttr(apiErr.RequestID, "status", apiErr.StatusCode, "code", apiErr.Code, "detail", apiErr.Detail)...)
			return &userError{msg: commonGetMintFailedMessage}
		}
	}
	// No APIError → wrapped network/dial failure. Same retry-friendly
	// disposition as 5xx above.
	log.Warn("get: mint failed", "error", err)
	return &userError{msg: serviceUnreachableMessage}
}

func isExpectedGetMintForbiddenCode(code string) bool {
	switch code {
	case slackoauth.ErrorCodeAPIKeyLimit, slackoauth.ErrorCodeQuotaExceeded:
		return true
	default:
		return false
	}
}

func logGetDependencyAuthFailure(log *slog.Logger, apiErr *client.APIError) {
	if apiErr == nil {
		return
	}
	// Emit-once invariant: the shared client retries only 429/5xx, not
	// auth-class 401/403, so this emits once per failed mint request.
	// Keep this stable WARN audit separate from the human ERROR log the caller
	// emits with contract-surprise detail; CloudWatch filters should key here.
	slackaudit.LogDependencyAuthFailure(log, slackaudit.DependencyAuthFailureAttrs(
		"qurl_get",
		http.MethodPost,
		client.CreateForResourcePathLabel,
		apiErr.StatusCode,
		apiErr.Code,
		apiErr.RequestID,
	)...)
}

// humanizeRetry formats a retry-after duration for surfacing to the
// user. Slack messages are human-readable, not machine-parseable, so
// "30s" or "2m" reads better than "30.000s" or "2m0s".
//
// Sub-second durations (positive but `< 1s`) collapse to
// [humanFallbackMoment] so a 0.4s server-side floor doesn't print as
// the misleading "0s" — half-up rounding `int(0.4+0.5)` yields zero
// and the user sees `Try again in 0s.`, which reads as a bug.
//
// `d <= 0` (e.g. from `time.Until(past)`) also takes this branch —
// don't optimize the zero case to print "0s" or "now"; both read as
// bugs to the user.
func humanizeRetry(d time.Duration) string {
	if d < time.Second {
		return humanFallbackMoment
	}
	if d < time.Minute {
		secs := int(d.Seconds() + 0.5)
		// 59.5s ≤ d < 60s rounds half-up to 60s, which reads worse
		// than the minutes-branch "1m". Roll over to the minutes
		// branch instead so the seconds field never prints ≥60.
		if secs >= 60 {
			return "1m"
		}
		return fmt.Sprintf("%ds", secs)
	}
	mins := int(d.Minutes() + 0.5)
	return fmt.Sprintf("%dm", mins)
}
