package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/layervai/qurl-integrations/shared/client"
)

// revokeUsageMessage is the arg hint for `/qurl-admin revoke`. A static prod
// literal like [getUsageMessage] — these inline usage hints don't run the
// non-prod command-name rewrite that the help messages do.
const revokeUsageMessage = "Usage: `/qurl-admin revoke $<id>` — revoke a protected resource (and all its qURLs) by the id shown in `/qurl list`."

// commonRevokeFailedMessage is the catch-all when a non-userError leaks from
// the resolve step. Defensive — [Handler.resolveTokenForGet] always returns a
// *userError — but a future refactor mustn't leak an internal error to Slack.
const commonRevokeFailedMessage = "Failed to revoke the resource. Please try again."

// handleRevoke implements `/qurl-admin revoke $<id|alias>`. Rather than
// revoking immediately, it resolves + channel-authorizes the token and posts
// the SAME red Revoke button the `/qurl list` row carries (with its native
// confirm dialog); the delete happens only when the admin clicks + confirms
// (handleListRevokeClick). So the typed and button surfaces share one
// confirmation and one delete path — a destructive verb shouldn't fire on a
// bare keystroke. Resolving is multi-hop (channel alias → slug fallback), so it
// acks via [Handler.runAsync] and posts the prompt to response_url, like
// `/qurl get`.
//
// The admin gate runs SYNC before the ack so a non-admin gets an immediate
// "admin-only" reply (and never sees a Revoke button) rather than "Working on
// it…" followed by a denial. requireAdminSync writes the denial and returns
// false; on success it writes nothing, leaving runAsync to own the ack — so
// `w` is written exactly once on either path.
func (h *Handler) handleRevoke(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	cmd, err := Parse(text)
	if err != nil {
		// Bare `revoke` (ErrEmptyResource) or a sigil-less token
		// (ErrMissingSigil) → the usage hint so the user learns the grammar;
		// other shape errors (invalid alias, surplus arg) surface verbatim.
		// Mirrors handleGet's ErrEmptyResource handling.
		if errors.Is(err, ErrEmptyResource) || errors.Is(err, ErrMissingSigil) {
			respondSlack(w, ":warning: "+revokeUsageMessage)
			return
		}
		respondSlack(w, ":warning: "+err.Error())
		return
	}
	if cmd.Subcommand != SubcmdRevoke || cmd.Alias == "" {
		// Defensive: the dispatcher routed `revoke` here but the parser
		// disagreed (or a future parser change left Alias empty). Surface the
		// usage hint rather than dispatching an unresolvable revoke.
		respondSlack(w, ":warning: "+revokeUsageMessage)
		return
	}
	teamID := strings.TrimSpace(values.Get(fieldTeamID))
	userID := strings.TrimSpace(values.Get(fieldUserID))
	// Gate AdminStore-nil FIRST, matching handleAdmin. requireAdminSync
	// dereferences AdminStore (CheckAdmin) with no nil check, and on a
	// no-DDB deploy (buildAdminStore returns nil when the QURL_*_TABLE env
	// vars are unset) that's a nil-deref — and it fires SYNC, before the
	// runAsync hop, so startAsyncWorker's recover doesn't cover it. Routing
	// `revoke` straight here (rather than through handleAdmin) bypassed the
	// store guard the membership verbs get, so re-add it: a no-DDB deploy
	// gets the same friendly "not configured" reply on revoke as on
	// add/remove/admins instead of a panic + broken response.
	if !h.requireAdminStoreSync(w) {
		return
	}
	if !h.requireAdminSync(w, teamID, userID, AdminActionRevoke) {
		return
	}
	h.runAsync(w, "revoke", values, func(ctx context.Context, log *slog.Logger) {
		h.processRevoke(ctx, log, values, cmd)
	})
}

// processRevoke is the async-worker body for `/qurl-admin revoke`: it resolves
// the `$<id|alias>` to a resource_id (channel-scoped authorization, the same
// resolver `/qurl get` uses) and then — instead of deleting — posts the SAME
// red Revoke button + confirm dialog the `/qurl list` row carries. The delete
// runs only on click + confirm (handleListRevokeClick), so both surfaces share
// one confirmation and one delete path. Resolving up front means a typo'd or
// unauthorized `$<id>` fails fast here, before any confirm prompt is shown.
func (h *Handler) processRevoke(ctx context.Context, log *slog.Logger, values url.Values, cmd *Command) {
	responseURL := values.Get(fieldResponseURL)
	teamID := values.Get(fieldTeamID)
	channelID := values.Get(fieldChannelID)
	userID := values.Get(fieldUserID)

	if channelID == "" {
		// resolveTokenForGet is channel-scoped, so a channel-less invocation
		// can't authorize. Mirrors processGet's guard.
		log.Warn("revoke: empty channel_id; refusing channel-less invocation")
		_ = h.postResponse(log, responseURL, ":warning: "+channelRequiredMessage)
		return
	}

	resourceID, err := h.resolveTokenForGet(ctx, log, teamID, channelID, userID, cmd.Alias)
	if err != nil {
		var ue *userError
		if errors.As(err, &ue) {
			_ = h.postResponse(log, responseURL, ":warning: "+ue.msg)
			return
		}
		log.Error("revoke: unexpected non-userError from resolveTokenForGet", "error", err)
		_ = h.postResponse(log, responseURL, ":warning: "+commonRevokeFailedMessage)
		return
	}

	// Post the confirm button (the same danger button + native confirm dialog
	// the /qurl list row renders) rather than deleting now. The click →
	// handleListRevokeClick re-gates admin and performs the delete, so the
	// typed and button surfaces converge on one confirmation + one delete path.
	revokeVal, ok := buildTunnelRevokeButtonValue(resourceID, cmd.Alias)
	if !ok {
		// Unreachable: the value is two short fields, well under the cap. Bail
		// loudly rather than posting a button that can't carry the resource_id.
		log.Error("revoke: revoke-button value exceeded Slack's cap", "team_id", teamID, "resource_id", resourceID)
		_ = h.postResponse(log, responseURL, ":warning: "+commonRevokeFailedMessage)
		return
	}
	blocks := []any{
		sectionBlock(fmt.Sprintf("Revoke `$%s`? This revokes the resource *and every qURL on it* — click *Revoke* to confirm.", escapeMrkdwnCode(cmd.Alias))),
		actionsBlock(withConfirmDialog(
			dangerButtonElement(listRevokeButtonLabel, listRevokeTunnelActionID, revokeVal),
			"Revoke $"+cmd.Alias+"?",
			"This revokes the resource *and every qURL on it*. It can't be undone.",
			"Revoke",
		)),
	}
	_ = h.postResponseBlocks(log, responseURL, fmt.Sprintf("Confirm revoke of `$%s`", escapeMrkdwnCode(cmd.Alias)), blocks)
}

// revokeResource calls DELETE /v1/resources/{resourceID} and returns the
// user-facing reply. displayToken is the sigil-stripped `$<slug>` the caller
// referenced (for the reply copy); resourceID is the resolved `r_…` id. Shared
// by the `/qurl-admin revoke` slash path and the `/qurl list` Revoke button so
// both render identical replies + error mapping. Does NOT gate admin — callers
// gate first (requireAdminSync on the slash path; a CheckAdmin re-check on the
// button click).
func (h *Handler) revokeResource(ctx context.Context, log *slog.Logger, teamID, userID, resourceID, displayToken string) string {
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("revoke: failed to get API key", "error", err, "team_id", teamID, "user_id", userID)
		return authErrorMessage(err)
	}
	if err := c.DeleteResource(ctx, resourceID); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case http.StatusNotFound, http.StatusGone:
				// Already revoked, or a stale/typo'd id. displayToken is
				// alias-charset (parseAliasToken / a list-rendered slug), but
				// route it through escapeMrkdwnCode anyway — cheap insurance
				// against a code-span break-out if the charset ever widens.
				log.Info("revoke: resource not found (already revoked or typo'd)", "team_id", teamID, "user_id", userID, "resource_id", resourceID)
				return fmt.Sprintf("`$%s` not found — already revoked, or check the id.", escapeMrkdwnCode(displayToken))
			case http.StatusUnauthorized, http.StatusForbidden:
				// API key rotated/invalidated — point at /qurl setup so the
				// admin has a concrete next step rather than a generic error.
				log.Warn("revoke: upstream auth rejected (API key rotated?)", "status", apiErr.StatusCode, "team_id", teamID, "user_id", userID, "resource_id", resourceID)
				return "This workspace's API key was rejected by the qURL service — re-run `/qurl setup <email>` to rotate."
			}
		}
		log.Error("revoke resource failed", "error", err, "team_id", teamID, "user_id", userID, "resource_id", resourceID)
		return ":warning: " + sanitizeAPIError(err, fmt.Sprintf("Failed to revoke `$%s`", escapeMrkdwnCode(displayToken)))
	}
	log.Info("revoke succeeded", "team_id", teamID, "user_id", userID, "resource_id", resourceID)
	return fmt.Sprintf("Revoked `$%s` and all its qURLs.", escapeMrkdwnCode(displayToken))
}

// handleListRevokeClick handles the admin-only red "Revoke" button on a
// `/qurl list` row. Slack's confirm dialog has already gated the click, so this
// fires only after the admin confirmed. It acks within Slack's interaction
// window and revokes on the async pool, posting the outcome to the
// interaction's response_url — the list message itself is left intact (revoked
// tunnels drop on the next `/qurl list`). The resource is identified by the
// button's value snapshot (the already-resolved resource_id + `$<token>`), so
// no slug re-resolution is needed.
//
// The MUTATION is re-gated against CheckAdmin even though the button only
// renders for admins — a destructive block_action shouldn't trust the
// render-time gate alone; mirror handleTunnelEditSubmission's submit-time
// re-check.
func (h *Handler) handleListRevokeClick(w http.ResponseWriter, payload *interactionPayload, action interactionAction) {
	log := slog.With(
		"command", "list_revoke_tunnel",
		"team_id", payload.Team.ID,
		"channel_id", payload.Channel.ID,
		"user_id", payload.User.ID,
	)
	responseURL := payload.ResponseURL

	snapshot, err := parseTunnelRevokeButtonValue(action.Value)
	if err != nil || snapshot.ResourceID == "" {
		// Our own button always carries a valid snapshot, so this is
		// defense-in-depth. h.Go (not the async pool) keeps the ack prompt and
		// can't deepen pool saturation.
		log.Warn("list revoke: unparseable button value", "error", err)
		h.Go(func() { _ = h.postResponse(log, responseURL, ":warning: "+commonRevokeFailedMessage) })
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}

	teamID, userID := payload.Team.ID, payload.User.ID
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		if h.cfg.AdminStore == nil {
			// Unreachable in practice — the Revoke button only renders when
			// listCallerCanEdit passes, which needs AdminStore — but fail safe.
			_ = h.postResponse(log, responseURL, ":warning: Admin features are not configured for this deployment.")
			return
		}
		// Mutation gate. Bounded off h.baseCtx (not the request ctx) so a
		// client abort can't cancel the deliberate fail-closed check — same
		// posture as handleTunnelEditSubmission.
		adminCtx, cancel := context.WithTimeout(h.baseCtx, adminGateBudget)
		isAdmin, _, adminErr := h.cfg.AdminStore.CheckAdmin(adminCtx, teamID, userID)
		cancel()
		if adminErr != nil {
			log.Error("list revoke: admin check failed", "error", adminErr, "team_id", teamID, "user_id", userID)
			_ = h.postResponse(log, responseURL, ":warning: failed to verify admin status (upstream error; see logs).")
			return
		}
		if !isAdmin {
			log.Warn("list revoke: non-admin click denied", "team_id", teamID, "user_id", userID)
			_ = h.postResponse(log, responseURL, ":warning: this command is admin-only")
			return
		}
		_ = h.postResponse(log, responseURL, h.revokeResource(ctx, log, teamID, userID, snapshot.ResourceID, snapshot.Token))
	}) {
		log.Warn("async pool saturated — dropping list Revoke click")
		h.Go(func() { _ = h.postResponse(log, responseURL, ackBusy) })
	}
	respondJSON(w, http.StatusOK, map[string]any{})
}
