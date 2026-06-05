package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

// handleExpose renders the `/qurl-admin expose` chooser: an ephemeral message
// with two buttons — "Expose qURL Connector" and "Expose URL" — each of which
// opens the matching guided modal. It's the front door that replaces having to
// remember the typed `connector install … env: port:` / `resource expose
// $alias as:` grammar; the buttons route to the same flows.
//
// Admin-gated in code (Slack does not gate slash-command invocation on
// workspace-admin role — the "admins only" picker hint is cosmetic). The gate
// here means "don't even show the picker to a non-admin"; the real mutation
// boundary is each modal's submit handler, which re-checks CheckAdmin. OpenView
// must be wired because the buttons open modals — without it the picker would
// be dead, so it's checked up front (and adminHelpMessage only advertises
// `expose` when OpenView is configured).
func (h *Handler) handleExpose(w http.ResponseWriter, values url.Values) {
	teamID, channelID, ok := h.aliasValidate(w, values, "expose")
	if !ok {
		return
	}
	if h.cfg.OpenView == nil {
		respondSlack(w, "Guided setup is not configured on this Slack bot deployment. Use `/qurl-admin connector install <id>` or `/qurl-admin resource expose $<alias>` instead.")
		return
	}
	if !h.requireAliasAdminGate(w, teamID, values, AdminActionExpose) {
		return
	}
	respondSlackBlocks(w, "What do you want to expose in this channel?", exposeChooserBlocks(channelID))
}

// handleExposeConnectorClick opens the existing guided connector installer in
// response to the "Expose qURL Connector" button. It reuses TunnelInstallModal
// and its existing submission handler wholesale — the button is just a second
// entry point to the wizard the bare `/qurl-admin connector install` opens.
// Mirrors handleListEditClick: ack fast, render+open on the async goroutine
// within Slack's trigger window, fail open via the interaction's response_url.
// Not admin-re-gated at open (the picker only renders for admins and the modal
// discloses nothing new); handleTunnelInstallSubmission is the mutation gate.
func (h *Handler) handleExposeConnectorClick(w http.ResponseWriter, payload *interactionPayload) {
	log := slog.With(
		"command", "expose_connector_click",
		"team_id", payload.Team.ID,
		"channel_id", payload.Channel.ID,
		"user_id", payload.User.ID,
	)
	responseURL := payload.ResponseURL
	if h.cfg.OpenView == nil {
		// The button shouldn't render without OpenView wired; fail safe.
		log.Warn("expose connector: OpenView not configured")
		h.Go(func() { _ = h.postResponse(log, responseURL, ":warning: "+exposeOpenFailedMessage) })
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}
	meta := TunnelInstallModalMetadata{
		TeamID:        payload.Team.ID,
		ChannelID:     payload.Channel.ID,
		UserID:        payload.User.ID,
		ResponseURL:   responseURL,
		CreatedAtUnix: h.now().Unix(),
	}
	teamID, triggerID := payload.Team.ID, payload.TriggerID
	h.Go(func() {
		view, err := TunnelInstallModal(meta)
		if err != nil {
			log.Error("expose connector: modal render failed", "error", err)
			_ = h.postResponse(log, responseURL, ":warning: "+exposeOpenFailedMessage)
			return
		}
		openCtx, openCancel := context.WithTimeout(h.baseCtx, slackTriggerOpenViewBudget)
		defer openCancel()
		if err := h.cfg.OpenView(openCtx, teamID, triggerID, view); err != nil {
			log.Warn("expose connector: views.open failed", "error", err)
			_ = h.postResponse(log, responseURL, ":warning: "+exposeOpenFailedMessage)
		}
	})
	respondJSON(w, http.StatusOK, map[string]any{})
}

// handleExposeURLClick opens the URL-expose modal in response to the "Expose
// URL" button. Unlike the connector installer (a static form), this modal lists
// the workspace's existing URL resources in a dropdown fetched at open time, so
// the admin picks one rather than typing an alias. With no URL resources to
// expose it posts a short ephemeral via response_url instead of opening an empty
// picker. Same open posture as handleExposeConnectorClick (ack fast, open on the
// async goroutine inside the trigger window, fail open via response_url).
func (h *Handler) handleExposeURLClick(w http.ResponseWriter, payload *interactionPayload) {
	log := slog.With(
		"command", "expose_url_click",
		"team_id", payload.Team.ID,
		"channel_id", payload.Channel.ID,
		"user_id", payload.User.ID,
	)
	responseURL := payload.ResponseURL
	if h.cfg.OpenView == nil {
		log.Warn("expose url: OpenView not configured")
		h.Go(func() { _ = h.postResponse(log, responseURL, ":warning: "+exposeOpenFailedMessage) })
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}
	meta := ExposeURLModalMetadata{
		TeamID:      payload.Team.ID,
		ChannelID:   payload.Channel.ID,
		UserID:      payload.User.ID,
		ResponseURL: responseURL,
	}
	teamID, triggerID := payload.Team.ID, payload.TriggerID
	h.Go(func() {
		// Bound the resource fetch on its own short budget so a slow upstream
		// can't eat into the views.open trigger window; the open then gets the
		// full slackTriggerOpenViewBudget. Both derive from h.baseCtx so a
		// process shutdown cancels them coherently. Mirrors handleListEditClick's
		// enumeration/open split.
		fetchCtx, fetchCancel := context.WithTimeout(h.baseCtx, adminGateBudget)
		options, userMsg := h.urlResourceSelectOptions(fetchCtx, log, teamID)
		fetchCancel()
		if userMsg != "" {
			_ = h.postResponse(log, responseURL, ":warning: "+userMsg)
			return
		}
		if len(options) == 0 {
			_ = h.postResponse(log, responseURL, "No URL resources found to expose. Create one in the qURL dashboard, then run `/qurl-admin expose` again.")
			return
		}
		view, err := ExposeURLModal(meta, options)
		if err != nil {
			log.Error("expose url: modal render failed", "error", err)
			_ = h.postResponse(log, responseURL, ":warning: "+exposeOpenFailedMessage)
			return
		}
		openCtx, openCancel := context.WithTimeout(h.baseCtx, slackTriggerOpenViewBudget)
		defer openCancel()
		if err := h.cfg.OpenView(openCtx, teamID, triggerID, view); err != nil {
			log.Warn("expose url: views.open failed", "error", err)
			_ = h.postResponse(log, responseURL, ":warning: "+exposeOpenFailedMessage)
		}
	})
	respondJSON(w, http.StatusOK, map[string]any{})
}

// urlResourceSelectOptions fetches the workspace's URL resources (the same
// first-page scan as /qurl list/get) and returns them as static_select option
// objects for the URL-expose modal: each option's text is a human label (the
// resource's alias, display name, or target URL) and its value is the
// resource_id the submission binds the channel alias to. Revoked resources and
// tunnel resources are skipped. Returns (nil, userMsg) on an upstream failure
// (userMsg is sanitized for display); (empty, "") when the workspace has no URL
// resources to expose.
func (h *Handler) urlResourceSelectOptions(ctx context.Context, log *slog.Logger, teamID string) (options []map[string]any, userMsg string) {
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("expose url: API key lookup failed", "error", err, "team_id", teamID)
		return nil, "Failed to look up URL resources. Please try again."
	}
	page, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesScanLimit})
	if err != nil {
		log.Warn("expose url: resource lookup failed", "error", err, "team_id", teamID)
		return nil, sanitizeAPIError(err, "Failed to look up URL resources")
	}
	options = make([]map[string]any, 0, len(page.Resources))
	for i := range page.Resources {
		r := page.Resources[i]
		if r.Status == client.StatusRevoked || !isURLResource(&r) {
			continue
		}
		options = append(options, optionObj(exposeURLOptionLabel(&r), r.ResourceID))
		if len(options) >= exposeURLMaxOptions {
			break
		}
	}
	if page.HasMore && len(options) < exposeURLMaxOptions {
		log.Debug("expose url: scanned first resource page only", "scan_limit", listResourcesScanLimit, "team_id", teamID)
	}
	return options, ""
}

// exposeURLOptionLabel renders a URL resource as a dropdown option label,
// preferring its `$alias`, then its Display Name, then its target URL, and
// finally the resource_id so the label is never empty (Slack rejects an
// empty-text option). Truncated to Slack's per-option text cap.
func exposeURLOptionLabel(r *client.Resource) string {
	switch {
	case r.Alias != "":
		return truncateRunes("$"+r.Alias, slackOptionTextMaxRunes)
	case r.Description != "":
		return truncateRunes(r.Description, slackOptionTextMaxRunes)
	case r.TargetURL != "":
		return truncateRunes(r.TargetURL, slackOptionTextMaxRunes)
	default:
		return truncateRunes(r.ResourceID, slackOptionTextMaxRunes)
	}
}

// truncateRunes caps s at maxRunes runes, appending an ellipsis when it
// truncates (so the rendered length is maxRunes). maxRunes <= 0 returns "".
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(r[:maxRunes-1]) + "…"
}

// handleExposeURLSubmission processes the URL-expose modal's view_submission. It
// re-checks the submitter is still a qURL admin (the mutation gate — the picker
// only renders for admins, but a stateful action mustn't trust the render-time
// gate), validates the chosen resource + channel alias, then binds the alias to
// the resource_id in this channel and posts the outcome to the chooser's
// response_url. Mirrors handleTunnelEditSubmission's posture: team/user
// cross-checks against the signed private_metadata, admin re-check bounded off
// h.baseCtx; no TTL because the bind mints no secret and is idempotent.
func (h *Handler) handleExposeURLSubmission(w http.ResponseWriter, payload *interactionPayload) {
	var meta ExposeURLModalMetadata
	if err := json.Unmarshal([]byte(payload.View.PrivateMetadata), &meta); err != nil {
		slog.Warn("expose url modal metadata parse failed", "error", err, "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		respondExposeURLModalError(w, "Could not verify this dialog. Run /qurl-admin expose again.")
		return
	}
	if meta.TeamID == "" || meta.ChannelID == "" || meta.UserID == "" || meta.ResponseURL == "" {
		slog.Warn("expose url modal metadata incomplete", "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		respondExposeURLModalError(w, "Could not verify this dialog. Run /qurl-admin expose again.")
		return
	}
	// Slack signs the request envelope including private_metadata, so these
	// cross-checks prevent replaying one admin's modal as another user or across
	// workspaces.
	if payload.Team.ID == "" || payload.Team.ID != meta.TeamID {
		slog.Warn("expose url modal team mismatch", "payload_team_id", payload.Team.ID, "metadata_team_id", meta.TeamID, "view_id", payload.View.ID)
		respondExposeURLModalError(w, "This dialog was opened for a different workspace. Run /qurl-admin expose again.")
		return
	}
	if payload.User.ID == "" || payload.User.ID != meta.UserID {
		slog.Warn("expose url modal user mismatch", "payload_user_id", payload.User.ID, "metadata_user_id", meta.UserID, "view_id", payload.View.ID)
		respondExposeURLModalError(w, "Only the admin who opened this dialog can submit it. Run /qurl-admin expose again.")
		return
	}
	if h.cfg.AdminStore == nil || h.aliasStore == nil {
		respondExposeURLModalError(w, "Admin features are not configured on this Slack bot deployment.")
		return
	}

	resourceID, channelAlias, fieldErrors := parseExposeURLModalArgs(payload.View.State.Values)
	if len(fieldErrors) > 0 {
		respondViewErrors(w, fieldErrors)
		return
	}

	// Mutation gate. Bounded so a slow store fails closed inside Slack's ack
	// window; off h.baseCtx (not the request ctx) so a client abort can't cancel
	// the deliberate fail-closed check — same posture as the install/edit modals.
	adminCtx, cancel := context.WithTimeout(h.baseCtx, adminGateBudget)
	defer cancel()
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(adminCtx, meta.TeamID, meta.UserID)
	if err != nil {
		slog.Error("expose url modal admin check failed", "error", err, "team_id", meta.TeamID, "user_id", meta.UserID, "view_id", payload.View.ID)
		respondExposeURLModalError(w, "Could not verify admin status. Retry in a moment.")
		return
	}
	if !isAdmin {
		slog.Warn("expose url modal denied: non-admin", "team_id", meta.TeamID, "user_id", meta.UserID, "view_id", payload.View.ID)
		respondExposeURLModalError(w, "This action is admin-only.")
		return
	}

	log := slog.With(
		"command", "expose_url_modal",
		"team_id", meta.TeamID,
		"channel_id", meta.ChannelID,
		"user_id", meta.UserID,
		"view_id", payload.View.ID,
	)
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		msg := h.bindURLResourceToChannel(ctx, log, meta.TeamID, meta.ChannelID, channelAlias, resourceID)
		_ = h.postResponse(log, meta.ResponseURL, msg)
	}) {
		respondExposeURLModalError(w, "Slack bot is busy. Retry in a moment.")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{})
}

// parseExposeURLModalArgs validates the URL-expose modal's submitted state: the
// chosen resource_id (the selected dropdown option's value — our own option set
// only ever carries an `r_…` resource_id, so a non-`r_` value is a crafted
// submission and is rejected) and the channel alias (required, validated against
// the shared alias contract; a leading `$` is optional). Returns a per-field
// error map on any problem.
func parseExposeURLModalArgs(values map[string]map[string]interactionStateValue) (resourceID, channelAlias string, fieldErrors map[string]string) {
	fieldErrors = map[string]string{}

	resourceID = strings.TrimSpace(interactionStateText(values, exposeURLBlockResource, exposeURLActionResource))
	if resourceID == "" {
		fieldErrors[exposeURLBlockResource] = "Pick a URL resource to expose."
	} else if !strings.HasPrefix(resourceID, "r_") || len(resourceID) > 128 {
		fieldErrors[exposeURLBlockResource] = "Pick a URL resource from the list."
	}

	aliasRaw := strings.TrimSpace(interactionStateText(values, exposeURLBlockAlias, exposeURLActionAlias))
	if aliasRaw != "" && !strings.HasPrefix(aliasRaw, "$") {
		aliasRaw = "$" + aliasRaw
	}
	alias, reason := validateAliasTokenForNoun(aliasRaw, "Channel alias", "channel alias")
	if reason != "" {
		fieldErrors[exposeURLBlockAlias] = reason
	} else {
		channelAlias = alias
	}

	if len(fieldErrors) > 0 {
		return "", "", fieldErrors
	}
	return resourceID, channelAlias, nil
}

// bindURLResourceToChannel binds channelAlias → resourceID in this channel
// (making the URL resource discoverable via /qurl list and mintable via /qurl
// get $<channelAlias>) and returns the user-facing outcome. The resource_id
// comes from the modal's dropdown, itself built from a fresh scan at open — so,
// like the /qurl list Edit button which carries its resource_id in the button
// value, this trusts that snapshot rather than re-resolving. The "already
// bound" / failure copy matches the typed `resource expose` path.
func (h *Handler) bindURLResourceToChannel(ctx context.Context, log *slog.Logger, teamID, channelID, channelAlias, resourceID string) string {
	err := h.aliasStore.BindChannelAlias(ctx, teamID, channelID, channelAlias, resourceID)
	if errors.Is(err, slackdata.ErrAliasAlreadyBound) {
		return fmt.Sprintf("Alias `$%s` is already bound in this channel. Run `/qurl-admin unset-alias $%s` first, or pick a different alias.", channelAlias, channelAlias)
	}
	if err != nil {
		log.Error("expose url: alias bind failed", "error", err, "team_id", teamID, "channel_id", channelID, "alias", channelAlias, "resource_id", resourceID)
		return exposeURLResourceFailedMsg
	}
	log.Info("URL resource exposed to Slack channel via modal", "team_id", teamID, "channel_id", channelID, "channel_alias", channelAlias, "resource_id", resourceID)
	return fmt.Sprintf("URL resource is now available as `$%s` in this channel. Run `/qurl get $%s` to create a qURL.", channelAlias, channelAlias)
}

// respondSlackBlocks writes an ephemeral slash-command response carrying Block
// Kit blocks (the `/qurl-admin expose` chooser). fallbackText is the
// notification/no-blocks fallback. The text-only sibling is respondSlack.
func respondSlackBlocks(w http.ResponseWriter, fallbackText string, blocks []any) {
	respondJSON(w, http.StatusOK, map[string]any{
		respFieldResponseType: respTypeEphemeral,
		respFieldText:         fallbackText,
		blockKitFieldBlocks:   blocks,
	})
}

// respondExposeURLModalError replaces the submitted URL-expose modal with a
// form-level error notice (structural/auth failures only; per-field problems use
// respondViewErrors). Falls back to a field-level error if the view render
// fails, so the submitter always sees a failure rather than a stuck modal.
// Mirrors respondTunnelEditModalError.
func respondExposeURLModalError(w http.ResponseWriter, message string) {
	view, err := ExposeURLErrorModal(message)
	if err != nil {
		slog.Error("expose url modal error render failed", "error", err)
		respondViewErrors(w, map[string]string{exposeURLBlockAlias: "Expose failed. Run /qurl-admin expose again."})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		respFieldResponseAction: "update",
		respFieldView:           json.RawMessage(view),
	})
}
