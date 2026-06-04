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

// handleRevoke implements `/qurl-admin revoke $<id|alias>`: revoke a protected
// resource AND all its qURLs (DELETE /v1/resources/{id}). Unlike the sync
// membership verbs, revoke is multi-hop — resolve the `$token` to a
// resource_id, then delete — so it acks via [Handler.runAsync] and posts the
// outcome to response_url, the same shape as `/qurl get`.
//
// The admin gate runs SYNC before the ack so a non-admin gets an immediate
// "admin-only" reply rather than "Working on it…" followed by a denial.
// requireAdminSync writes the denial and returns false; on success it writes
// nothing, leaving runAsync to own the ack — so `w` is written exactly once on
// either path.
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
	if !h.requireAdminSync(w, teamID, userID, AdminActionRevoke) {
		return
	}
	h.runAsync(w, "revoke", values, func(ctx context.Context, log *slog.Logger) {
		h.processRevoke(ctx, log, values, cmd)
	})
}

// processRevoke is the async-worker body for `/qurl-admin revoke`: resolve the
// `$<id|alias>` to a resource_id (channel-scoped authorization, the same
// resolver `/qurl get` uses) then revoke it, POSTing the outcome to
// response_url.
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

	_ = h.postResponse(log, responseURL, h.revokeResource(ctx, log, teamID, userID, resourceID, cmd.Alias))
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
