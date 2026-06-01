package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

// listEditMaxAliases caps how many channel aliases one edit modal can bind, so
// a single submission can't fan out into an unbounded run of DDB writes. A
// channel realistically binds a handful of aliases per tunnel; 20 is generous.
const listEditMaxAliases = 20

// listEditOpenFailedMessage is the ephemeral shown (via the list message's
// response_url) when the Edit dialog can't be opened — a stale trigger, a
// rate-limited views.open, or an unparseable button value.
const listEditOpenFailedMessage = "Couldn't open the edit dialog. Run `/qurl list` and tap *Edit* again."

// parseTunnelEditButtonValue decodes the JSON snapshot carried on a `/qurl
// list` Edit button (see [tunnelEditButtonValue]).
func parseTunnelEditButtonValue(value string) (tunnelEditButtonValue, error) {
	var v tunnelEditButtonValue
	if err := json.Unmarshal([]byte(strings.TrimSpace(value)), &v); err != nil {
		return tunnelEditButtonValue{}, fmt.Errorf("unmarshal edit button value: %w", err)
	}
	if v.ResourceID == "" {
		return tunnelEditButtonValue{}, errors.New("edit button value missing resource_id")
	}
	return v, nil
}

// handleListEditClick opens the [TunnelEditModal] in response to an admin
// tapping the per-row Edit button on `/qurl list`. The modal is pre-filled
// entirely from the button's value snapshot, so no upstream read is needed and
// views.open fits inside Slack's ~3s trigger_id window.
//
// Opening the modal is intentionally NOT admin-re-gated: the Edit button only
// renders for admins, and the data the modal shows (Display Name + channel
// aliases) is already visible to every member via `/qurl list` and `/qurl
// aliases`, so opening it discloses nothing new. The MUTATION is gated at
// submission time (handleTunnelEditSubmission re-checks CheckAdmin).
func (h *Handler) handleListEditClick(w http.ResponseWriter, payload *interactionPayload, action interactionAction) {
	log := slog.With(
		"command", "list_edit_tunnel",
		"team_id", payload.Team.ID,
		"channel_id", payload.Channel.ID,
		"user_id", payload.User.ID,
	)
	responseURL := payload.ResponseURL
	failOpen := func() {
		// Surface the failure out-of-band via the list message's response_url
		// (h.Go, not the async pool, so it can't deepen pool saturation).
		h.Go(func() { _ = h.postResponse(log, responseURL, ":warning: "+listEditOpenFailedMessage) })
		respondJSON(w, http.StatusOK, map[string]any{})
	}

	if h.cfg.OpenView == nil {
		// The Edit button shouldn't render without OpenView wired; fail safe.
		log.Warn("list edit: OpenView not configured")
		failOpen()
		return
	}
	snapshot, err := parseTunnelEditButtonValue(action.Value)
	if err != nil {
		log.Warn("list edit: unparseable button value", "error", err)
		failOpen()
		return
	}
	meta := TunnelEditModalMetadata{
		TeamID:      payload.Team.ID,
		ChannelID:   payload.Channel.ID,
		UserID:      payload.User.ID,
		ResponseURL: responseURL,
		ResourceID:  snapshot.ResourceID,
		Token:       snapshot.Token,
		DisplayName: snapshot.DisplayName,
	}
	view, err := TunnelEditModal(&meta, snapshot.DisplayName, snapshot.Aliases)
	if err != nil {
		log.Error("list edit: modal render failed", "error", err)
		failOpen()
		return
	}
	teamID, triggerID := payload.Team.ID, payload.TriggerID
	h.Go(func() {
		ctx, cancel := context.WithTimeout(h.baseCtx, slackTriggerOpenViewBudget)
		defer cancel()
		if err := h.cfg.OpenView(ctx, teamID, triggerID, view); err != nil {
			log.Warn("list edit: views.open failed", "error", err)
			_ = h.postResponse(log, responseURL, ":warning: "+listEditOpenFailedMessage)
		}
	})
	respondJSON(w, http.StatusOK, map[string]any{})
}

// handleTunnelEditSubmission processes the Edit modal's view_submission: it
// validates the submitted Display Name + alias lines, re-checks that the
// submitter is still a qURL bot admin (the real mutation gate), then applies
// the changes asynchronously and posts the result to the list message's
// response_url. Mirrors handleTunnelInstallSubmission's posture: per-field
// validation surfaces inline (response_action:errors); structural/auth
// failures replace the modal with an error notice.
func (h *Handler) handleTunnelEditSubmission(w http.ResponseWriter, payload *interactionPayload) {
	var meta TunnelEditModalMetadata
	if err := json.Unmarshal([]byte(payload.View.PrivateMetadata), &meta); err != nil {
		slog.Warn("tunnel edit modal metadata parse failed", "error", err, "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		h.respondTunnelEditModalError(w, "Could not verify this dialog. Run /qurl list and tap Edit again.")
		return
	}
	if meta.TeamID == "" || meta.ChannelID == "" || meta.UserID == "" || meta.ResponseURL == "" || meta.ResourceID == "" {
		slog.Warn("tunnel edit modal metadata incomplete", "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		h.respondTunnelEditModalError(w, "Could not verify this dialog. Run /qurl list and tap Edit again.")
		return
	}
	// Slack signs the request envelope including private_metadata, so these
	// cross-checks prevent replaying one admin's modal as another user or
	// across workspaces.
	if payload.Team.ID == "" || payload.Team.ID != meta.TeamID {
		slog.Warn("tunnel edit modal team mismatch", "payload_team_id", payload.Team.ID, "metadata_team_id", meta.TeamID, "view_id", payload.View.ID)
		h.respondTunnelEditModalError(w, "This dialog was opened for a different workspace. Run /qurl list and tap Edit again.")
		return
	}
	if payload.User.ID == "" || payload.User.ID != meta.UserID {
		slog.Warn("tunnel edit modal user mismatch", "payload_user_id", payload.User.ID, "metadata_user_id", meta.UserID, "view_id", payload.View.ID)
		h.respondTunnelEditModalError(w, "Only the admin who opened this dialog can submit it. Run /qurl list and tap Edit again.")
		return
	}
	if h.cfg.AdminStore == nil || h.aliasStore == nil {
		h.respondTunnelEditModalError(w, "Admin features are not configured on this Slack bot deployment.")
		return
	}

	displayName, aliases, fieldErrors := parseTunnelEditModalArgs(payload.View.State.Values, meta.Token)
	if len(fieldErrors) > 0 {
		respondViewErrors(w, fieldErrors)
		return
	}

	// Mutation gate. Bounded so a slow store fails closed inside Slack's ack
	// window; off h.baseCtx (not the request ctx) so a client abort can't
	// cancel the deliberate fail-closed check — same posture as the install
	// modal submission.
	adminCtx, cancel := context.WithTimeout(h.baseCtx, adminGateBudget)
	defer cancel()
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(adminCtx, meta.TeamID, meta.UserID)
	if err != nil {
		slog.Error("tunnel edit modal admin check failed", "error", err, "team_id", meta.TeamID, "user_id", meta.UserID, "view_id", payload.View.ID)
		h.respondTunnelEditModalError(w, "Could not verify admin status. Retry in a moment.")
		return
	}
	if !isAdmin {
		slog.Warn("tunnel edit modal denied: non-admin", "team_id", meta.TeamID, "user_id", meta.UserID, "view_id", payload.View.ID)
		h.respondTunnelEditModalError(w, "This action is admin-only.")
		return
	}

	log := slog.With(
		"command", "tunnel_edit_modal",
		"team_id", meta.TeamID,
		"channel_id", meta.ChannelID,
		"user_id", meta.UserID,
		"resource_id", meta.ResourceID,
		"view_id", payload.View.ID,
	)
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		h.processTunnelEdit(ctx, log, &meta, displayName, aliases)
	}) {
		h.respondTunnelEditModalError(w, "Slack bot is busy. Retry in a moment.")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{})
}

// parseTunnelEditModalArgs validates the Edit modal's submitted state: the
// Display Name (required, char-fenced via the shared validateDisplayNameChars)
// and the multiline aliases field. token is the row's primary `$<token>`,
// excluded from the editable alias set. Returns the cleaned Display Name and
// the deduped, validated extra-alias set, or a per-field error map.
func parseTunnelEditModalArgs(values map[string]map[string]interactionStateValue, token string) (displayName string, aliases []string, fieldErrors map[string]string) {
	fieldErrors = map[string]string{}

	rawName := strings.TrimSpace(stripSurroundingQuotes(strings.TrimSpace(interactionStateText(values, tunnelEditBlockDisplayName, tunnelEditActionDisplayName))))
	if rawName == "" {
		fieldErrors[tunnelEditBlockDisplayName] = "Enter a display name."
	} else if msg := validateDisplayNameChars(rawName); msg != "" {
		fieldErrors[tunnelEditBlockDisplayName] = msg
	}

	aliases, aliasMsg := parseEditAliasLines(interactionStateText(values, tunnelEditBlockAliases, tunnelEditActionAliases), token)
	if aliasMsg != "" {
		fieldErrors[tunnelEditBlockAliases] = aliasMsg
	}

	if len(fieldErrors) > 0 {
		return "", nil, fieldErrors
	}
	return rawName, aliases, nil
}

// parseEditAliasLines parses the modal's one-alias-per-line field into a
// validated, deduped, order-preserving set of channel aliases (sigil-free). A
// leading `$` is optional per line (matching how the field pre-fills). The
// primary token is silently dropped if typed — the tunnel's own name is
// managed automatically, not through this field. Returns the first invalid
// line's reason as userMsg.
func parseEditAliasLines(raw, token string) (aliases []string, userMsg string) {
	seen := make(map[string]struct{})
	for _, line := range strings.Split(raw, "\n") {
		tok := strings.TrimSpace(line)
		if tok == "" {
			continue
		}
		if !strings.HasPrefix(tok, "$") {
			tok = "$" + tok
		}
		alias, reason := validateChannelShortcutToken(tok)
		if reason != "" {
			return nil, reason
		}
		if alias == token {
			continue
		}
		if _, dup := seen[alias]; dup {
			continue
		}
		seen[alias] = struct{}{}
		aliases = append(aliases, alias)
	}
	if len(aliases) > listEditMaxAliases {
		return nil, fmt.Sprintf("Too many aliases (max %d). Remove some lines.", listEditMaxAliases)
	}
	return aliases, ""
}

// processTunnelEdit is the async worker for an Edit modal submission. It
// PATCHes the Display Name (only when changed, so an alias-only edit can't
// clobber a concurrent display-name change), reconciles the channel aliases to
// the submitted set, and posts an outcome summary to the list message's
// response_url.
func (h *Handler) processTunnelEdit(ctx context.Context, log *slog.Logger, meta *TunnelEditModalMetadata, displayName string, desiredAliases []string) {
	c, err := h.authenticatedClient(ctx, meta.TeamID)
	if err != nil {
		log.Error("tunnel edit: API key lookup failed", "error", err)
		_ = h.postResponse(log, meta.ResponseURL, ":warning: "+authErrorMessage(err))
		return
	}

	var changes []string
	// Display Name: skip the PATCH when unchanged (the modal pre-fills the
	// current value, so an unchanged field means the admin only touched
	// aliases — don't overwrite a concurrent set-display-name).
	if displayName != meta.DisplayName {
		if _, err := c.UpdateResource(ctx, meta.ResourceID, &client.UpdateResourceInput{Description: &displayName}); err != nil {
			log.Error("tunnel edit: display name update failed", "error", err, "resource_id", meta.ResourceID)
			_ = h.postResponse(log, meta.ResponseURL, sanitizeAPIError(err, "Failed to update the Display Name"))
			return
		}
		changes = append(changes, "Display Name updated")
	}

	result := h.reconcileChannelAliases(ctx, log, meta, desiredAliases)
	_ = h.postResponse(log, meta.ResponseURL, formatTunnelEditSummary(meta.Token, changes, &result))
}

// aliasReconcileResult buckets the outcome of reconciling a tunnel's channel
// aliases for the edit summary.
type aliasReconcileResult struct {
	added     []string
	removed   []string
	conflicts []string // alias already bound to a DIFFERENT tunnel in this channel
	hadError  bool     // a non-conflict bind/unbind or the policy read failed
}

// reconcileChannelAliases brings the channel's alias bindings for this tunnel
// in line with the submitted desired set. It diffs against the CURRENT bindings
// (read fresh, excluding the primary token), binds the additions, and unbinds
// the removals. Best-effort: a per-alias failure is logged and flagged in the
// result rather than aborting the whole reconcile.
func (h *Handler) reconcileChannelAliases(ctx context.Context, log *slog.Logger, meta *TunnelEditModalMetadata, desired []string) aliasReconcileResult {
	var res aliasReconcileResult

	entries, err := h.cfg.AdminStore.GetChannelPolicy(ctx, meta.TeamID, meta.ChannelID)
	if err != nil {
		// Without the current set we can't compute removals safely. Report the
		// read failure rather than guessing.
		log.Error("tunnel edit: channel policy read failed", "error", err, "resource_id", meta.ResourceID)
		res.hadError = true
		return res
	}
	current := make(map[string]struct{})
	for i := range entries {
		if entries[i].ResourceID == meta.ResourceID && entries[i].Alias != "" && entries[i].Alias != meta.Token {
			current[entries[i].Alias] = struct{}{}
		}
	}
	desiredSet := make(map[string]struct{}, len(desired))
	for _, a := range desired {
		desiredSet[a] = struct{}{}
	}

	for _, a := range desired {
		if _, ok := current[a]; ok {
			continue
		}
		switch err := h.aliasStore.BindChannelAlias(ctx, meta.TeamID, meta.ChannelID, a, meta.ResourceID); {
		case errors.Is(err, slackdata.ErrAliasAlreadyBound):
			res.conflicts = append(res.conflicts, a)
		case err != nil:
			log.Error("tunnel edit: bind alias failed", "error", err, "alias", a, "resource_id", meta.ResourceID)
			res.hadError = true
		default:
			res.added = append(res.added, a)
		}
	}
	for a := range current {
		if _, ok := desiredSet[a]; ok {
			continue
		}
		switch err := h.aliasStore.UnbindChannelAlias(ctx, meta.TeamID, meta.ChannelID, a); {
		case err == nil, errors.Is(err, slackdata.ErrAliasNotFound):
			res.removed = append(res.removed, a)
		default:
			log.Error("tunnel edit: unbind alias failed", "error", err, "alias", a, "resource_id", meta.ResourceID)
			res.hadError = true
		}
	}
	sort.Strings(res.added)
	sort.Strings(res.removed)
	sort.Strings(res.conflicts)
	return res
}

// formatTunnelEditSummary renders the admin-facing ephemeral summarizing an
// edit. Aliases are shown as `$<alias>` code spans; the token is echoed as the
// tunnel's id. Everything interpolated here is charset-validated (token/alias)
// or char-fenced (display name handled upstream), so it's safe in mrkdwn.
func formatTunnelEditSummary(token string, changes []string, res *aliasReconcileResult) string {
	lines := []string{fmt.Sprintf("✅ Updated tunnel `$%s`.", token)}
	lines = append(lines, changes...)
	if len(res.added) > 0 {
		lines = append(lines, "Added alias(es): "+joinAliasCodes(res.added))
	}
	if len(res.removed) > 0 {
		lines = append(lines, "Removed alias(es): "+joinAliasCodes(res.removed))
	}
	if len(res.conflicts) > 0 {
		lines = append(lines, "Skipped (already used by another tunnel in this channel): "+joinAliasCodes(res.conflicts))
	}
	if len(changes) == 0 && len(res.added) == 0 && len(res.removed) == 0 && len(res.conflicts) == 0 {
		lines = append(lines, "No changes.")
	}
	if res.hadError {
		lines = append(lines, ":warning: Some changes may not have applied. Run `/qurl list` to check, and retry if needed.")
	}
	return strings.Join(lines, "\n")
}

func joinAliasCodes(aliases []string) string {
	codes := make([]string, len(aliases))
	for i, a := range aliases {
		codes[i] = "`$" + a + "`"
	}
	return strings.Join(codes, ", ")
}

// respondTunnelEditModalError replaces the submitted Edit modal with a
// form-level error notice (structural/auth failures only; per-field problems
// use respondViewErrors). Falls back to a field-level error if the view render
// fails, so the submitter always sees a failure rather than a stuck modal.
func (h *Handler) respondTunnelEditModalError(w http.ResponseWriter, message string) {
	view, err := TunnelEditErrorModal(message)
	if err != nil {
		slog.Error("tunnel edit modal error render failed", "error", err)
		respondViewErrors(w, map[string]string{tunnelEditBlockDisplayName: "Edit failed. Run /qurl list and tap Edit again."})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		respFieldResponseAction: "update",
		respFieldView:           json.RawMessage(view),
	})
}
