package internal

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

// Block Kit wiring for the `/qurl uninstall` confirmation.
//
// The teardown clears this workspace's qURL configuration — the admin list, the
// per-channel resource access set, and channel aliases — and those rows live
// only in this app's own tables, keyed by Slack team id. Nothing upstream can
// rebuild them. So the destructive step sits behind Slack's native confirm
// dialog rather than firing on the bare verb, matching the posture `/qurl-admin
// revoke` already uses for a destructive action.
const (
	uninstallConfirmActionID = "uninstall_confirm"
	// uninstallConfirmFallbackText is the notification/fallback string for the
	// blocks message; Slack requires non-empty text alongside blocks.
	uninstallConfirmFallbackText = "Confirm disconnecting qURL from this workspace"
	// uninstallConfirmButtonLabel is deliberately the verb, not "Yes": the
	// dialog's own copy carries the consequence.
	uninstallConfirmButtonLabel = "Disconnect qURL"
	// uninstallPurgeIDSeparator joins the resolved purge partitions inside the
	// button value. Slack team/enterprise ids never contain it.
	uninstallPurgeIDSeparator = ","
)

// uninstallConfirmBlocks renders the confirmation card. The copy states what is
// cleared and what is not: "uninstall" reads like it deletes the customer's
// protected resources, and it does not — they live on the qURL account, not in
// Slack. It also names `--rotate`, because refreshing a key or picking up new
// scopes is the far more common intent and does not touch this configuration.
func uninstallConfirmBlocks(command string, purgeWorkspaceIDs []string) []any {
	adminCommand := command + adminCommandSuffix
	return []any{
		sectionBlock("*Disconnect qURL from this workspace?*\n\n" +
			"This clears this workspace's qURL configuration:\n" +
			"• Admin list — everyone added with `" + adminCommand + " add`\n" +
			"• Channel access — which resources are available in which channel\n" +
			"• Channel aliases — the `$name` shortcuts bound in each channel"),
		sectionBlock("Your protected resources and qURL Connectors are *not* affected — they stay on your qURL account. This workspace regains access to them after `" + command + " setup <email>`."),
		contextBlock("To refresh this workspace's qURL key or pick up new scopes, use `" + command + " setup <email> --rotate` instead. That keeps this configuration."),
		actionsBlock(withConfirmDialog(
			dangerButtonElement(uninstallConfirmButtonLabel, uninstallConfirmActionID, strings.Join(purgeWorkspaceIDs, uninstallPurgeIDSeparator)),
			"Disconnect qURL?",
			"This clears this workspace's admin list, channel access, and channel aliases. Your protected resources are not affected.",
			"Disconnect",
		)),
	}
}

// uninstallPurgeIDsForClick re-validates the partitions the confirmation card
// carried. The value round-trips through Slack (which signs the interaction), so
// it cannot be forged, but it is still echoed input: constrain it to the ids
// this very interaction is authenticated for, so a replayed card can never point
// the purge at a partition outside the clicking workspace. Falls back to the
// payload's own team id when nothing survives validation.
func uninstallPurgeIDsForClick(value, teamID, enterpriseID string) []string {
	allowed := map[string]struct{}{}
	for _, id := range []string{teamID, enterpriseID} {
		if id = strings.TrimSpace(id); id != "" {
			allowed[id] = struct{}{}
		}
	}
	var ids orderedIDSet
	for _, raw := range strings.Split(value, uninstallPurgeIDSeparator) {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, ok := allowed[id]; !ok {
			continue
		}
		ids.add(id)
	}
	if ids.empty() {
		ids.add(teamID)
	}
	return ids.ids
}

// handleUninstallConfirmClick performs the teardown after the admin confirms.
// Ack fast, re-gate on the async worker, deliver via response_url — the same
// shape as the `/qurl list` Revoke button. The authorization re-check is the
// real mutation boundary: the card is an ephemeral only its requester can see,
// but a click must never be trusted on the strength of having rendered a button.
func (h *Handler) handleUninstallConfirmClick(w http.ResponseWriter, payload *interactionPayload, action interactionAction) {
	log := slog.With(
		"command", "uninstall_confirm",
		"team_id", payload.Team.ID,
		"enterprise_id", payload.Enterprise.ID,
		"user_id", payload.User.ID,
	)
	responseURL := payload.ResponseURL
	teamID, userID := payload.Team.ID, payload.User.ID
	purgeIDs := uninstallPurgeIDsForClick(action.Value, teamID, payload.Enterprise.ID)

	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		if !h.requireUninstallAdminOrOwnerForClick(ctx, log, responseURL, teamID, userID) {
			return
		}
		_ = h.postResponse(log, responseURL, h.uninstallWorkspaceReply(ctx, teamID, userID, purgeIDs))
	}) {
		log.Warn("async pool saturated — dropping uninstall confirmation click")
		h.Go(func() { _ = h.postResponse(log, responseURL, ackBusy) })
	}
	respondJSON(w, http.StatusOK, map[string]any{})
}

// requireUninstallAdminOrOwnerForClick is requireUninstallAdminOrOwner's
// click-surface twin: same owner-or-admin rule, but it reports refusals over
// response_url instead of the slash writer, and fails closed on a store error.
func (h *Handler) requireUninstallAdminOrOwnerForClick(ctx context.Context, log *slog.Logger, responseURL, teamID, userID string) bool {
	if userID == "" {
		_ = h.postResponse(log, responseURL, ":warning: missing user_id in the Slack interaction payload")
		return false
	}
	if h.cfg.AdminStore == nil {
		log.Error("uninstall confirm: owner gate unavailable")
		_ = h.postResponse(log, responseURL, ":warning: qURL uninstall is not available on this Secure Access Agent deployment.")
		return false
	}
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(ctx, teamID, userID)
	if err != nil {
		log.Error("uninstall confirm: owner check failed", "error", err)
		_ = h.postResponse(log, responseURL, ":warning: failed to verify who connected qURL to this workspace (upstream error; see logs). Try again in a moment.")
		return false
	}
	if !isAdmin {
		log.Warn("uninstall confirm: non-admin denied")
		_ = h.postResponse(log, responseURL, ":warning: only the person who connected qURL to this workspace, or a qURL admin, can disconnect it.")
		return false
	}
	return true
}
