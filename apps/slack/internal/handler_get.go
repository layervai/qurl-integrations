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
func noResourceForAliasMessage(alias string) string {
	return fmt.Sprintf("`$%s` is not configured for this channel. Run `/qurl aliases` to see what's available here, or contact your Slack admin to add it.", alias)
}

// notAllowedInChannelMessage is the copy surfaced when a user passes
// a `$r_<id>` resource token that is not in the channel's allowed-set
// (the union of `alias_bindings.values()` and `allowed_resource_ids`
// returned by [Store.AllowedResourceIDsForChannel]). Same posture as
// [noResourceForAliasMessage]: name the literal token the user
// typed, plain-English state, point at `/qurl list` so the user can
// see what IS available without a manual lookup, and route to the
// admin since only the admin can extend the allow-set.
//
// Mirrored copy for the two not-allowed branches so a workspace
// member probing for valid resource IDs can't distinguish "id doesn't
// exist" from "id exists but not in this channel" through the wire
// text.
func notAllowedInChannelMessage(token string) string {
	return fmt.Sprintf("`$%s` is not allowed in this channel. Run `/qurl list` to see what's available here, or contact your Slack admin for assistance.", token)
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

// handleGet implements `/qurl get <url|$alias|$r_<id>>`:
//  1. Parse the slash-command text → [Command]. The positional arg
//     is either a URL (`http://…` / `https://…`), a channel-scoped
//     `$alias` name configured by a workspace admin, or a raw
//     `$r_<id>` resource token copy-pasted from `/qurl list`.
//  2. Ack within 3s via [runAsync] (200 + ackWorkingOnIt).
//  3. Async goroutine: for URL form, mint directly; for alias form,
//     resolve alias → resource_id via channel_policies.alias_bindings
//     then mint; for resource-ID form, gate against the channel's
//     allow-set (union of alias_bindings + allowed_resource_ids)
//     then mint by that ID. Rate-limit gates all three. POSTs the
//     result to response_url.
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
		respondSlack(w, ":warning: "+err.Error())
		return
	}
	if cmd.Subcommand != SubcmdGet {
		// Defensive: dispatcher routed `get*` here but the parser
		// disagreed. Fall through to the unknown-subcommand reply.
		respondSlack(w, fmt.Sprintf("Unknown subcommand: `%s`. Try `/qurl help`.", text))
		return
	}
	if cmd.Alias == "" && cmd.Target == "" && cmd.Resource.Kind != ResourceTokenResourceID {
		respondSlack(w, ":warning: Usage: `/qurl get <url>` to mint for a URL, or `/qurl get $slug` to mint for a tunnel or shortcut your Slack admin has configured here.")
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
	if err != nil {
		// All errors from getWork are *userError today; surface the
		// message verbatim. If a non-userError ever leaks through
		// (programmer mistake), surface the generic catch-all so we
		// don't leak internals.
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

// getWork runs the inner resolve→rate-limit→mint pipeline for the
// URL form (`/qurl get <url>`), the token form (`/qurl get $alias` or
// `/qurl get $slug`), and the resource-ID form (`/qurl get $r_<id>`).
// Returns the rendered reply text (without leading `:warning:`) on
// success, or a [*userError] whose msg routes to the user.
func (h *Handler) getWork(ctx context.Context, log *slog.Logger, args getWorkArgs) (string, error) {
	alias := args.cmd.Alias
	target := args.cmd.Target
	resourceID := ""
	if args.cmd.Resource.Kind == ResourceTokenResourceID {
		resourceID = args.cmd.Resource.Value
	}
	isAliasForm := alias != ""
	isResourceIDForm := resourceID != ""

	// Refuse `dm:true` early when PostDM is not wired — the user's
	// intent is "do not leak the link in channel history", and a
	// silent channel-fallback violates that intent. Fail-fast here
	// avoids burning a mint quota on a request that can't be
	// delivered the way the user asked.
	if args.cmd.DM() && h.cfg.PostDM == nil {
		return "", &userError{msg: "DM delivery is not configured for this workspace. Re-run the command without `dm:true` to receive the link in-channel."}
	}

	input := client.CreateInput{
		Reason: args.cmd.Reason(),
		// One-time use is the only mode for `/qurl get` — there is no
		// `once` flag; every minted link burns on first redemption.
		OneTimeUse:     true,
		IdempotencyKey: IdempotencyKey(args.teamID, args.channelID, args.userID, args.triggerID),
	}

	switch {
	case isAliasForm:
		boundResourceID, err := h.resolveTokenForGet(ctx, log, args.teamID, args.channelID, args.userID, alias)
		if err != nil {
			return "", err
		}
		input.ResourceID = boundResourceID
	case isResourceIDForm:
		if err := h.authorizeResourceIDForGet(ctx, log, args.teamID, args.channelID, args.userID, resourceID); err != nil {
			return "", err
		}
		input.ResourceID = resourceID
	default:
		// URL-form has no per-channel authorization gate — anyone in the
		// workspace who can invoke `/qurl get` can mint against any URL on
		// the workspace's API key (qurl-service's per-key quota is the
		// only enforcer). Same posture the deprecated `/qurl create` had;
		// the alias path is the surface that adds binding-scoped auth.
		input.TargetURL = target
	}

	c, err := h.authenticatedClient(ctx, args.teamID)
	if err != nil {
		log.Error("get: API key lookup failed", "error", err)
		return "", &userError{msg: authErrorMessage(err)}
	}

	// Rate-limit gate only fires when an AdminStore is wired; URL-form
	// on a no-DDB sandbox is unguarded (qurl-service's per-key quota is
	// the only enforcer in that mode).
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
		log.Error("get: mint returned empty qurl_link — server contract surprise", "alias_form", isAliasForm, "resource_id_form", isResourceIDForm, "resource_id", input.ResourceID)
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
//     authorization signal — `/qurl set-alias` is the admin act that
//     authorizes a resource for use here.
//  2. Tunnel-slug fallback. When no binding matches, the token may
//     still be a tunnel slug: `/qurl list` renders `$<slug>` for tunnels
//     surfaced via admin-sees-all or `allowed_resource_ids` that have no
//     `alias_bindings` row in this channel (e.g. a tunnel installed in
//     another channel, or granted via cross-channel allow). Resolve the
//     slug to its resource_id and gate it through the SAME channel
//     allow-set as the `$r_<id>` form ([Handler.resourceAllowedForUser])
//     so the list→get round-trip the list footer advertises stays
//     honest without re-exposing the opaque r_<id> in the list.
//
// Returns a [*userError] on AdminStore-nil, lookup failure,
// not-a-known-token, or not-allowed-here.
func (h *Handler) resolveTokenForGet(ctx context.Context, log *slog.Logger, teamID, channelID, userID, token string) (string, error) {
	// Refuse early on a no-DDB sandbox deploy. Token-form requires the
	// channel-scoped binding store; URL form does not, so this gate
	// only fires here.
	if h.cfg.AdminStore == nil {
		log.Warn("get: AdminStore is nil; token-form lookup unavailable", "team_id", teamID)
		return "", errAdminStoreNotConfigured
	}
	resourceID, found, err := h.cfg.AdminStore.LookupChannelAlias(ctx, teamID, channelID, token)
	if err != nil {
		log.Warn("get: alias lookup failed", "error", err, "team_id", teamID, "channel_id", channelID, "alias", token)
		return "", &userError{msg: serviceUnreachableMessage}
	}
	if found {
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
		// Word the rejection with the slug the user typed, not the
		// resolved r_<id> (which they never saw).
		return "", &userError{msg: notAllowedInChannelMessage(token)}
	}
	return slugResourceID, nil
}

// resourceAllowedForUser reports whether userID may mint against
// resourceID in channelID. Workspace admins may always (so the
// list-and-get round-trip works in the admin's unfiltered list view);
// non-admins only when the ID is in `AllowedResourceIDsForChannel` (the
// union of `alias_bindings.values()` and `allowed_resource_ids`),
// keeping list visibility and mintability aligned for them.
//
// Returns (false, [*userError]) on AdminStore-nil or allow-set fetch
// failure so callers fail closed. The decision is split from the
// user-facing "not allowed" message so both the `$r_<id>` form and the
// tunnel-slug fallback can reuse it while wording the rejection in terms
// of the token the user actually typed.
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

// authorizeResourceIDForGet enforces the channel-scoped allow on a
// `$r_<id>` token via [Handler.resourceAllowedForUser], wording the
// rejection with the resource_id the user pasted.
//
// Returns nil on allow, [*userError] on AdminStore-nil, allow-set
// fetch failure, or membership miss.
func (h *Handler) authorizeResourceIDForGet(ctx context.Context, log *slog.Logger, teamID, channelID, userID, resourceID string) error {
	allowed, err := h.resourceAllowedForUser(ctx, log, teamID, channelID, userID, resourceID)
	if err != nil {
		return err
	}
	if !allowed {
		return &userError{msg: notAllowedInChannelMessage(resourceID)}
	}
	return nil
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
