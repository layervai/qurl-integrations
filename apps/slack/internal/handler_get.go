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

	"github.com/layervai/qurl-integrations/shared/client"
)

// commonGetMintFailedMessage is the generic catch-all error shown to
// the user when the multi-hop `/qurl get` work fails on a branch we
// don't have a more specific message for. Lifted to a constant
// because three different mapMintError branches need it.
const commonGetMintFailedMessage = "Failed to mint qURL. Please try again."

// urlNotSupportedGetMessage is the user-facing copy for a raw-URL
// `/qurl get`. The parser flags the case with the terse
// [ErrURLNotSupportedGet] sentinel; this is the rich reply the handler
// renders so multi-sentence prose stays out of the parser's error
// values (repo convention).
// The breadcrumb points only at `/qurl list` (not `/qurl aliases`): list is
// ungated and now renders each tunnel's channel `$alias` shortcuts inline, so
// it's the single surface that always works and shows both slugs and aliases.
const urlNotSupportedGetMessage = "`/qurl get` only works with a `$name` or `$alias` now — raw URLs aren't supported. Run `/qurl list` to see your tunnels and their channel aliases."

// resourceIDNotSupportedGetMessage is the user-facing copy for a `$r_<id>`
// `/qurl get` (the resource-id form is gone). Same terse-sentinel
// ([ErrResourceIDNotSupportedGet]) → rich-handler-copy split as the URL case.
const resourceIDNotSupportedGetMessage = "`/qurl get` takes a tunnel `$name` or a channel `$alias`, not a resource ID. Run `/qurl list` and copy the `$name` instead."

// unexpectedGetShapeMessage is the reply for getWork's defensive
// empty-alias arm; it routes the user to their operator rather than
// looping them on a retry. See that arm for why it's distinct from
// [commonGetMintFailedMessage].
const unexpectedGetShapeMessage = "Couldn't process that command. Please contact your Slack admin for assistance."

// tunnelDisabledMessage is shown when the qURL service returns the
// `tunnel_disabled` error code (the workspace doesn't have
// tunnel-resource minting enabled yet). Lifted because both alias
// resolution and mint can surface this.
const tunnelDisabledMessage = "Tunnel resources are not yet enabled for this workspace. Ask LayerV support."

// serviceUnreachableMessage is the "honest retry-friendly" copy
// surfaced for transport-class failures (5xx, dial errors, network
// errors). Distinct from the generic "Failed to mint qURL" copy so
// the user knows a retry is the right next move.
const serviceUnreachableMessage = "Could not reach qURL. Please try again."

// serviceUnreachableMessageWith builds the retry-friendly message
// augmented with the upstream's bounded Title and the opaque
// RequestID for support correlation. Detail is suppressed — it can
// carry internal hostnames / DB error strings. Mirrors the
// pre-consolidation /qurl create behavior (sanitizeAPIError) so
// users keep the reference handle they previously had for support
// tickets; on-call can paste the RequestID directly into
// qurl-service CloudWatch to find the failed request server-side.
// Falls back to [serviceUnreachableMessage] when neither field is
// present.
func serviceUnreachableMessageWith(apiErr *client.APIError) string {
	if apiErr == nil || (apiErr.Title == "" && apiErr.RequestID == "") {
		return serviceUnreachableMessage
	}
	msg := "Could not reach qURL"
	if apiErr.Title != "" {
		msg += ": " + strings.TrimRight(apiErr.Title, ".")
	}
	if apiErr.RequestID != "" {
		msg += fmt.Sprintf(" (Reference: `%s`)", apiErr.RequestID)
	}
	return msg + ". Please try again."
}

// channelRequiredMessage is the user-facing copy surfaced when a
// slash command that requires channel context (`/qurl get`,
// `/qurl aliases`) is invoked from a payload without a channel_id.
// Slack always sends channel_id on real slash commands; an empty
// value is a synthetic payload (test harness or future channel-less
// surface), so fail-closed.
const channelRequiredMessage = "This command must be invoked from a channel."

// noResourceForAliasMessage formats the "no binding" copy surfaced
// when a channel's alias_bindings map has no entry for the requested
// alias. Phrased for an end user who doesn't know what an "alias" is:
// name the literal token the user typed, say plainly what state it's
// in, and give them BOTH a self-serve action (`/qurl aliases` lists
// what is configured here, so a typo is one tab away) AND the
// escalation path (ask the admin to wire it up) since only the
// admin can run setalias.
//
// The `/qurl aliases` breadcrumb is channel-scoped (it shows aliases
// bound here), so it stays accurate even though `/qurl list` is now
// workspace-wide. If `/qurl aliases` ever widens to workspace-wide too,
// this breadcrumb deserves the same treatment.
//
// TODO(#460): a user can see `$<alias>` rendered by `/qurl list`
// (workspace-wide post-revert of #234) and still hit this surface
// when minting from a channel without the binding. Followup tracks
// either an inline "alias resolves in: #channel-a, …" annotation on
// the list output or a clearer error here distinguishing
// "alias does not exist anywhere" from "alias not bound here, but
// bound in: …".
func noResourceForAliasMessage(alias string) string {
	return fmt.Sprintf("`$%s` is not configured for this channel. Run `/qurl aliases` to see what's available here, or contact your Slack admin to add it.", alias)
}

// legacyAliasBindingMessage is the copy surfaced when a channel alias
// resolves to a value that isn't a tunnel resource id — a raw URL bound
// by the pre-tunnels-only `/qurl set-alias`. Those rows still exist in
// DDB; resolving one would hand a URL to `POST /v1/resources/<url>/qurls`
// and surface as the generic retry-friendly [commonGetMintFailedMessage],
// stranding the user. Name the dead shortcut plainly and route to the
// admin (only an admin can re-point it at a tunnel). Same posture as
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
		return "That channel alias points at a target that's no longer supported. Please ask your Slack admin to re-point it at a tunnel with `/qurl-admin set-alias`."
	}
	return fmt.Sprintf("`$%s` points at a target that's no longer supported. Please ask your Slack admin to re-point it at a tunnel with `/qurl-admin set-alias $%s $<name>`.", alias, alias)
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

// userErrorf builds a [*userError] using fmt.Sprintf semantics. The
// returned error's text is the literal Slack message — no extra
// wrapping, no "context: …" prefix.
func userErrorf(format string, args ...any) error {
	return &userError{msg: fmt.Sprintf(format, args...)}
}

// errAdminStoreNotConfigured is returned by getWork when the handler's
// AdminStore is nil (sandbox / no-DDB deployment). Surfaces as a user-
// facing message that doesn't expose the "AdminStore" implementation
// term — it points the user at the workspace admin who would have
// completed the install.
var errAdminStoreNotConfigured = &userError{msg: "qURL admin features are not yet configured for this workspace. Please contact your Slack admin for assistance."}

// handleGet implements `/qurl get <$slug|$alias>`:
//  1. Parse the slash-command text → [Command]. The positional arg is a
//     tunnel `$slug` / channel-scoped `$alias` (a workspace admin
//     configures aliases). Raw URLs and `$r_<id>` resource IDs are
//     rejected at parse time — Slack mints tunnels by slug/alias only.
//  2. Ack within 3s via [runAsync] (200 + ackWorkingOnIt).
//  3. Async goroutine: resolve `$slug`/`$alias` → resource_id
//     (channel_policies.alias_bindings, then tunnel-slug fallback gated
//     against the channel allow-set) then mint. Rate-limit gates it.
//     POSTs the result to response_url.
//
// Optional flags:
//   - `dm:true` → final message via PostDM to the user's DM instead
//     of channel ephemeral. Falls back to ephemeral with a friendly
//     "DM not configured" warning when PostDM is nil.
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
		respondSlack(w, ":warning: Usage: `/qurl get <$name|$alias>` to mint a one-time qURL for a tunnel. Run `/qurl list` to see what's available.")
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

	text, err := h.getWork(ctx, log, getWorkArgs{
		cmd:       cmd,
		teamID:    teamID,
		channelID: channelID,
		userID:    userID,
		triggerID: triggerID,
	})
	h.finishGet(log, responseURL, text, err)
}

// finishGet posts a [Handler.getWork] outcome to response_url as an
// ephemeral: the rendered link on success, or the [*userError] message
// (prefixed with `:warning:`) on failure. A non-userError leak is a
// programmer mistake — log it loud and surface the generic catch-all so
// internals never reach Slack. Shared by the `/qurl get` slash path
// ([Handler.processGet]) and the `/qurl list` "Create qURL" button
// ([Handler.processButtonGet]) so both render identical replies.
func (h *Handler) finishGet(log *slog.Logger, responseURL, text string, err error) {
	if err != nil {
		var ue *userError
		if errors.As(err, &ue) {
			_ = h.postResponse(log, responseURL, ":warning: "+ue.msg)
			return
		}
		log.Error("get: unexpected non-userError leaked through getWork", "error", err)
		_ = h.postResponse(log, responseURL, ":warning: "+commonGetMintFailedMessage)
		return
	}
	_ = h.postResponse(log, responseURL, text)
}

// getWorkArgs bundles the closure inputs for [Handler.getWork].
type getWorkArgs struct {
	cmd       *Command
	teamID    string
	channelID string
	userID    string
	triggerID string
}

// getWork runs the inner resolve→rate-limit→mint pipeline for the token
// form (`/qurl get $slug` or `/qurl get $alias`). Raw URLs and `$r_<id>`
// resource IDs are rejected at parse time. Returns the rendered reply
// text (without leading `:warning:`) on success, or a [*userError] whose
// msg routes to the user.
func (h *Handler) getWork(ctx context.Context, log *slog.Logger, args getWorkArgs) (string, error) {
	alias := args.cmd.Alias

	// Refuse `dm:true` early when PostDM is not wired — the user's
	// intent is "do not leak the link in channel history", and a
	// silent channel-fallback violates that intent. Fail-fast here
	// avoids burning a mint quota on a request that can't be
	// delivered the way the user asked.
	if args.cmd.DM() && h.cfg.PostDM == nil {
		return "", &userError{msg: "DM delivery is not configured for this workspace. Re-run the command without `dm:true` to receive the link in-channel."}
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
		return "", &userError{msg: unexpectedGetShapeMessage}
	}

	input := client.CreateInput{
		Reason: args.cmd.Reason(),
		// One-time use is the only mode for `/qurl get` — there is no
		// `once` flag; every minted link burns on first redemption.
		OneTimeUse:     true,
		IdempotencyKey: IdempotencyKey(args.teamID, args.channelID, args.userID, args.triggerID),
	}

	boundResourceID, err := h.resolveTokenForGet(ctx, log, args.teamID, args.channelID, args.userID, alias)
	if err != nil {
		return "", err
	}
	input.ResourceID = boundResourceID

	c, err := h.authenticatedClient(ctx, args.teamID)
	if err != nil {
		log.Error("get: API key lookup failed", "error", err)
		return "", &userError{msg: authErrorMessage(err)}
	}

	// Rate-limit gate. The resolve above (resolveTokenForGet) already
	// fails closed with errAdminStoreNotConfigured when AdminStore is nil,
	// so by here AdminStore is non-nil on the minting path and the gate
	// always runs today. The nil-check is belt-and-suspenders:
	// if a future minting arm regressed its own AdminStore guard, this
	// would skip the rate-limit rather than nil-panic — qurl-service's
	// per-key quota stays the backstop. (It is NOT a forward fence that
	// refuses such an arm; that arm would need to add its own guard.)
	if h.cfg.AdminStore != nil {
		ok, retry, err := h.cfg.AdminStore.CheckRateLimit(ctx, args.userID, args.teamID)
		if err != nil {
			log.Warn("get: rate-limit check failed", "error", err, "team_id", args.teamID, "user_id", args.userID)
			return "", &userError{msg: commonGetMintFailedMessage}
		}
		if !ok {
			return "", userErrorf("Rate limit hit. Try again in %s.", humanizeRetry(retry))
		}
	}

	out, err := c.Create(ctx, input)
	if err != nil {
		return "", mapMintError(log, err)
	}
	// Defensive: a 200 with an empty qurl_link is a server contract
	// surprise — log loud and surface the generic retry message.
	if out.QURLLink == "" {
		log.Error("get: mint returned empty qurl_link — server contract surprise", "resource_id", input.ResourceID)
		return "", &userError{msg: commonGetMintFailedMessage}
	}

	// Unconditional suffix — every `/qurl get` link is one-time use
	// (see OneTimeUse above).
	message := ":link: *qURL ready:* " + out.QURLLink + " (one-time use)"
	if args.cmd.DM() {
		return h.deliverGetDM(ctx, log, args.userID, message), nil
	}
	return message, nil
}

// resolveTokenForGet resolves a `$<token>` (channel alias or tunnel
// slug) to a mintable resource_id for /qurl get, enforcing channel
// authorization. Resolution order:
//
//  1. Channel alias binding (`channel_policies.alias_bindings`). The
//     presence of the binding in THIS channel is itself the
//     authorization signal — `/qurl-admin set-alias` is the admin act
//     that authorizes a resource for use here.
//  2. Tunnel-slug fallback. When no binding matches, the token may
//     still be a tunnel slug: `/qurl list` renders `$<slug>` for tunnels
//     surfaced via admin-sees-all or `allowed_resource_ids` that have no
//     `alias_bindings` row in this channel (e.g. a tunnel installed in
//     another channel, or granted via cross-channel allow). Resolve the
//     slug to its resource_id and gate it through the channel allow-set
//     ([Handler.resourceAllowedForUser]) so the list→get round-trip the
//     list advertises stays honest — the user references the `$<slug>`,
//     never the opaque resource_id.
//
// Cost note: every binding MISS now incurs one extra upstream hop
// (GET /v1/resources?slug=…), including for plain typos. That's the
// deliberate price of the round-trip honesty above — don't "optimize"
// it away by short-circuiting the fallback on a binding miss.
//
// Returns a [*userError] on AdminStore-nil, lookup failure,
// not-a-known-token, or not-allowed-here.
func (h *Handler) resolveTokenForGet(ctx context.Context, log *slog.Logger, teamID, channelID, userID, token string) (string, error) {
	// Refuse early on a no-DDB sandbox deploy — token resolution
	// requires the channel-scoped binding store.
	if h.cfg.AdminStore == nil {
		log.Warn("get: AdminStore is nil; token-form lookup unavailable", "team_id", teamID)
		return "", errAdminStoreNotConfigured
	}
	resourceID, found, err := h.cfg.AdminStore.LookupChannelAlias(ctx, teamID, channelID, token)
	if err != nil {
		log.Warn("get: alias lookup failed", "error", err, "team_id", teamID, "channel_id", channelID, "token", token)
		return "", &userError{msg: serviceUnreachableMessage}
	}
	if found {
		// Legacy-binding guard: the pre-tunnels-only `/qurl set-alias`
		// stored raw URLs verbatim in alias_bindings, and those rows
		// survive this PR. Resolving one would hand a URL to the mint
		// call and surface as the generic retry error, stranding the
		// user. Gate on the `r_` prefix (not
		// an exact id-shape check — a stored id is whatever qurl-service
		// issued, length not guaranteed to match the 11-char get-token
		// shape): a legacy `r_<id>` is a real resource and still mints,
		// only a non-`r_` value (a URL) is refused with a re-bind hint.
		// Residual: a junk `r_<typo>` row (the old parser rejected only
		// the bare `r_` sigil) passes this prefix check and 404s at mint
		// → the generic retry copy. Accepted as rare — an admin re-bind
		// is the same fix as for a URL row; not worth an upstream
		// pre-resolve on every binding hit just to special-case it.
		if !strings.HasPrefix(resourceID, "r_") {
			log.Warn("get: channel alias bound to a non-resource-id (legacy URL) target — refusing to mint", "team_id", teamID, "channel_id", channelID, "token", token)
			return "", &userError{msg: legacyAliasBindingMessage(token)}
		}
		return resourceID, nil
	}

	// No binding — try the token as a tunnel slug, then authorize.
	slugResourceID, slugErr := h.resolveTunnelSlugAliasTarget(ctx, teamID, token)
	if slugErr != nil {
		if errors.Is(slugErr, errTunnelSlugNotFound) {
			// Neither a channel alias nor a live tunnel slug — genuinely
			// unknown in this channel.
			return "", &userError{msg: noResourceForAliasMessage(token)}
		}
		log.Warn("get: tunnel-slug fallback lookup failed", "error", slugErr, "team_id", teamID, "slug", token)
		return "", &userError{msg: serviceUnreachableMessage}
	}
	allowed, authErr := h.resourceAllowedForUser(ctx, log, teamID, channelID, userID, slugResourceID)
	if authErr != nil {
		return "", authErr
	}
	if !allowed {
		// Collapse to the SAME "not configured" copy as the
		// slug-not-found branch above. A non-admin must not be able to
		// distinguish "this slug exists in the workspace but isn't
		// allowed in this channel" from "no such slug" — that gap is a
		// tunnel-slug enumeration oracle. Logs the real reason for
		// operators; the wire text stays uniform.
		log.Debug("get: tunnel slug resolved but not allowed in channel — surfacing not-configured copy", "team_id", teamID, "channel_id", channelID, "user_id", userID, "slug", token)
		return "", &userError{msg: noResourceForAliasMessage(token)}
	}
	return slugResourceID, nil
}

// resourceAllowedForUser reports whether userID may mint against
// resourceID in channelID. Workspace admins may always (so the
// list-and-get round-trip works in the admin's unfiltered list view);
// non-admins only when the ID is in `AllowedResourceIDsForChannel` (the
// union of `alias_bindings.values()` and `allowed_resource_ids`).
//
// Post-revert of #234 (PR #459), `/qurl list` is workspace-wide, so a
// non-admin can see `$<slug>` tokens for tunnels they can't mint in.
// This gate keeps mintability channel-scoped despite the widened list
// visibility — the asymmetry is intentional but surfaces a UX gap
// tracked by TODO(#460).
//
// Returns (false, [*userError]) on AdminStore-nil or allow-set fetch
// failure so callers fail closed. Used by the tunnel-slug fallback in
// resolveTokenForGet to gate a slug that has no channel alias binding.
func (h *Handler) resourceAllowedForUser(ctx context.Context, log *slog.Logger, teamID, channelID, userID, resourceID string) (bool, error) {
	// Needs an AdminStore for the admin probe + the channel allow-set.
	// Same fail-closed posture as alias-form on a no-DDB sandbox.
	if h.cfg.AdminStore == nil {
		log.Warn("get: AdminStore is nil; authorization unavailable", "team_id", teamID)
		return false, errAdminStoreNotConfigured
	}
	isAdmin, _, adminErr := h.cfg.AdminStore.CheckAdmin(ctx, teamID, userID)
	if adminErr != nil {
		log.Warn("get: admin probe failed — treating as non-admin", "error", adminErr, "team_id", teamID, "user_id", userID)
		isAdmin = false
	}
	if isAdmin {
		return true, nil
	}
	allowed, err := h.cfg.AdminStore.AllowedResourceIDsForChannel(ctx, teamID, channelID)
	if err != nil {
		log.Warn("get: allowed-resource fetch failed", "error", err, "team_id", teamID, "channel_id", channelID)
		return false, &userError{msg: serviceUnreachableMessage}
	}
	_, ok := allowed[resourceID]
	return ok, nil
}

// deliverGetDM handles the `dm:true` variant. The link goes to the
// user's DM via PostDM; the response_url ephemeral confirms (without
// leaking the link in channel history).
//
// PostDM-nil is rejected earlier in getWork — the dm:true contract
// is privacy ("do not leak the link in channel history") and a
// silent channel-fallback violates that. If PostDM is wired but the
// call itself fails, we surface the failure without re-posting the
// link (the user can retry without dm:true if they want it
// in-channel).
func (h *Handler) deliverGetDM(ctx context.Context, log *slog.Logger, userID, message string) string {
	if err := h.cfg.PostDM(ctx, userID, message); err != nil {
		log.Warn("get: DM post failed", "error", err)
		return ":warning: Could not DM you the link. Please re-run the command without `dm:true` to receive it in-channel."
	}
	return ":incoming_envelope: Sent to your DM."
}

// mapMintError converts an [*client.APIError] from the mint into a
// friendly message. Rate-limit + tunnel-disabled get specific text;
// transport-class (5xx/network) gets the retry-friendly
// [serviceUnreachableMessage]; everything else gets the generic
// [commonGetMintFailedMessage].
func mapMintError(log *slog.Logger, err error) error {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusTooManyRequests:
			retry := time.Duration(apiErr.RetryAfter) * time.Second
			return userErrorf("Rate limit hit. Try again in %s.", humanizeRetry(retry))
		case http.StatusForbidden:
			if apiErr.Code == "tunnel_disabled" {
				return &userError{msg: tunnelDisabledMessage}
			}
			// 403 with an unrecognized code is a server-contract
			// surprise — log loud so a future rename of
			// `tunnel_disabled` doesn't get silently masked.
			log.Error("get: mint rejected with 403 — unmapped error code", "code", apiErr.Code, "detail", apiErr.Detail)
			return &userError{msg: commonGetMintFailedMessage}
		case http.StatusBadRequest:
			// `mutually_exclusive_fields` → server contract drift,
			// not a user error. Surface friendly + log loud.
			log.Error("get: mint rejected with 400 — check resource_id/target_url contract", "code", apiErr.Code, "detail", apiErr.Detail)
			return &userError{msg: commonGetMintFailedMessage}
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			log.Warn("get: mint failed with transport-class error", "status", apiErr.StatusCode, "code", apiErr.Code, "request_id", apiErr.RequestID)
			return &userError{msg: serviceUnreachableMessageWith(apiErr)}
		default:
			// Unmapped 5xx (e.g. 500, 599) is server-side trouble —
			// same retry-friendly disposition as 502/503/504 above.
			// Falling through to commonGetMintFailedMessage on a 500
			// would tell the user "permanent failure, do not retry"
			// when the upstream is actually transient.
			if apiErr.StatusCode >= 500 && apiErr.StatusCode < 600 {
				log.Warn("get: mint failed with unmapped 5xx", "status", apiErr.StatusCode, "code", apiErr.Code, "request_id", apiErr.RequestID)
				return &userError{msg: serviceUnreachableMessageWith(apiErr)}
			}
			// Other unmapped statuses (401, 404, 422, etc.) are
			// permanent-class — log loud so the operator sees the
			// contract surprise, surface the generic message so the
			// user isn't told to retry forever.
			log.Error("get: mint rejected with unmapped status", "status", apiErr.StatusCode, "code", apiErr.Code, "detail", apiErr.Detail)
			return &userError{msg: commonGetMintFailedMessage}
		}
	}
	// No APIError → wrapped network/dial failure. Same retry-friendly
	// disposition as 5xx above.
	log.Warn("get: mint failed", "error", err)
	return &userError{msg: serviceUnreachableMessage}
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
