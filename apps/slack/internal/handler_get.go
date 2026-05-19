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

// errAdminStoreNotConfigured is returned by getWork's policy/
// rate-limit gates when the handler's AdminStore is nil (sandbox /
// no-DDB deployment). Surfaces as a friendly user-facing message
// rather than a stack-trace.
var errAdminStoreNotConfigured = &userError{msg: "Admin features are not configured for this deployment."}

// handleGet implements `/qurl get $<alias>`:
//  1. Parse the slash-command text → [Command].
//  2. Ack within 3s via [runAsync] (200 + ackWorkingOnIt).
//  3. Async goroutine resolves alias → resource_id → policy check →
//     rate-limit → mint, and POSTs the result to response_url.
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
	if cmd.Alias == "" {
		respondSlack(w, ":warning: missing $alias argument. Usage: `/qurl get $alias`.")
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
		h.postResponse(log, responseURL, ":warning: This command must be invoked from a channel.")
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
			h.postResponse(log, responseURL, ":warning: "+ue.msg)
			return
		}
		log.Error("get: unexpected non-userError leaked through getWork", "error", err)
		h.postResponse(log, responseURL, ":warning: "+commonGetMintFailedMessage)
		return
	}
	h.postResponse(log, responseURL, text)
}

// getWorkArgs bundles the closure inputs for [Handler.getWork].
type getWorkArgs struct {
	cmd       *Command
	teamID    string
	channelID string
	userID    string
	triggerID string
}

// getWork runs the inner alias→policy→rate-limit→mint pipeline.
// Returns the rendered reply text (without leading `:warning:`) on
// success, or a [*userError] whose msg routes to the user.
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

	// 1. Refuse early on a no-DDB sandbox deploy. AdminStore-nil
	//    means there's no policy gate to enforce, so the result is
	//    the same regardless of alias resolution — but checking now
	//    avoids burning a customer-API GetResourceByAlias round-trip
	//    on a workspace that can't gate it (and lets the user probe
	//    alias existence ahead of the not-configured signal).
	if h.cfg.AdminStore == nil {
		log.Warn("get: AdminStore is nil; refusing to mint without policy gate", "team_id", args.teamID)
		return "", errAdminStoreNotConfigured
	}

	// 2. Resolve alias → resource via the customer API (uses the
	//    workspace's API key).
	c, err := h.authenticatedClient(ctx, args.teamID)
	if err != nil {
		log.Error("get: API key lookup failed", "error", err)
		return "", &userError{msg: authErrorMessage(err)}
	}
	resource, err := c.GetResourceByAlias(ctx, alias)
	if err != nil {
		return "", mapAliasResolutionError(log, alias, err)
	}

	// 3. Policy + rate-limit checks via the DDB-direct AdminStore.
	allowed, err := h.cfg.AdminStore.ResolvePolicy(ctx, args.teamID, args.channelID, resource.ResourceID)
	if err != nil {
		log.Warn("get: policy check failed", "error", err, "team_id", args.teamID, "channel_id", args.channelID, "resource_id", resource.ResourceID)
		return "", &userError{msg: commonGetMintFailedMessage}
	}
	if !allowed {
		return "", userErrorf("Alias `$%s` is not allowed in this channel. Ask an admin to run `/qurl admin allow #channel $%s`.", alias, alias)
	}

	ok, retry, err := h.cfg.AdminStore.CheckRateLimit(ctx, args.userID, args.teamID)
	if err != nil {
		log.Warn("get: rate-limit check failed", "error", err, "team_id", args.teamID, "user_id", args.userID)
		return "", &userError{msg: commonGetMintFailedMessage}
	}
	if !ok {
		return "", userErrorf("Rate limit hit. Try again in %s.", humanizeRetry(retry))
	}

	// 4. Mint. Idempotency key derived from (team, channel, user,
	//    trigger_id) so a Slack-side retry on the 3s ack budget
	//    dedupes to the same qURL.
	idemKey := IdempotencyKey(args.teamID, args.channelID, args.userID, args.triggerID)
	out, err := c.Create(ctx, client.CreateInput{
		ResourceID:     resource.ResourceID,
		Reason:         args.cmd.Reason(),
		IdempotencyKey: idemKey,
	})
	if err != nil {
		return "", mapMintError(log, err)
	}
	// Defensive: a 200 with an empty qurl_link would render as
	// `:link: *qURL ready:* \n_alias_: …` — useless. Log loud and
	// surface the generic message so the user retries.
	if out.QURLLink == "" {
		log.Error("get: mint returned empty qurl_link — server contract surprise", "resource_id", resource.ResourceID)
		return "", &userError{msg: commonGetMintFailedMessage}
	}

	// 5. Surface. dm:true routes through PostDM; default is the
	//    channel ephemeral (response_url).
	message := fmt.Sprintf(":link: *qURL ready:* %s\n_alias_: `$%s`", out.QURLLink, alias)
	if args.cmd.DM() {
		return h.deliverGetDM(ctx, log, args.userID, message), nil
	}
	return message, nil
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

// mapAliasResolutionError converts an [*client.APIError] from the
// alias-resolution call into a friendly user-facing message. Known
// codes get specific text; everything else falls through to
// [serviceUnreachableMessage]. Uses the caller-supplied request-
// scoped logger so the failure log carries the same team/channel
// attribution as the mint path (mapMintError threads `log` too).
func mapAliasResolutionError(log *slog.Logger, alias string, err error) error {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusNotFound:
			return userErrorf("No resource has alias `$%s`.", alias)
		case apiErr.StatusCode == http.StatusForbidden && apiErr.Code == "tunnel_disabled":
			return &userError{msg: tunnelDisabledMessage}
		case apiErr.StatusCode == http.StatusTooManyRequests:
			// Mirror mapMintError's 429 handling — the customer API
			// can rate-limit the resolution call independently of
			// the mint call.
			retry := time.Duration(apiErr.RetryAfter) * time.Second
			return userErrorf("Rate limit hit. Try again in %s.", humanizeRetry(retry))
		}
	}
	log.Warn("get: alias resolution failed", "error", err, "alias", alias)
	return &userError{msg: serviceUnreachableMessage}
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
			log.Warn("get: mint failed with transport-class error", "status", apiErr.StatusCode, "code", apiErr.Code)
			return &userError{msg: serviceUnreachableMessage}
		default:
			// Unmapped 5xx (e.g. 500, 599) is server-side trouble —
			// same retry-friendly disposition as 502/503/504 above.
			// Falling through to commonGetMintFailedMessage on a 500
			// would tell the user "permanent failure, do not retry"
			// when the upstream is actually transient.
			if apiErr.StatusCode >= 500 && apiErr.StatusCode < 600 {
				log.Warn("get: mint failed with unmapped 5xx", "status", apiErr.StatusCode, "code", apiErr.Code)
				return &userError{msg: serviceUnreachableMessage}
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
