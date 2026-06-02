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

// listEditMaxChannels caps how many channels the Edit modal pre-fills (and thus
// how many the reconcile baseline carries). A tunnel realistically reaches a
// handful of channels; the cap keeps the modal's private_metadata under Slack's
// 3000-byte limit and the multi-select pre-fill bounded. If a tunnel is exposed
// to more than this, the pre-fill truncates — which is SAFE: the reconcile only
// acts on channels in the (truncated) baseline, so an un-shown channel is never
// revoked, it just keeps its access.
const listEditMaxChannels = 30

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
// tapping the per-row Edit button on `/qurl list`. Display Name and channel
// aliases are pre-filled from the button's value snapshot; the channels
// multi-select is pre-filled from a single [exposedChannelsForEdit] read. To
// keep the ack prompt and views.open inside Slack's ~3s trigger window, the
// enumeration + render + open all run on the async goroutine — the enumeration
// on its own short budget so a slow Query can't starve the open — and the ack
// returns immediately.
//
// Opening the modal is intentionally NOT admin-re-gated: the Edit button only
// renders for admins, and the data the modal shows (Display Name + channel
// aliases + the channels the tunnel is exposed to) is already visible to every
// member via `/qurl list` and `/qurl aliases`, so opening it discloses nothing
// new. The MUTATION is gated at submission time (handleTunnelEditSubmission
// re-checks CheckAdmin).
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
		Aliases:     snapshot.Aliases,
	}
	teamID, triggerID := payload.Team.ID, payload.TriggerID
	h.Go(func() {
		// Bound the channel enumeration on its own short budget so a slow Query
		// can't eat into the views.open trigger window; the open then gets the
		// full slackTriggerOpenViewBudget. Both derive from h.baseCtx so a
		// process shutdown cancels them coherently.
		enumCtx, enumCancel := context.WithTimeout(h.baseCtx, adminGateBudget)
		meta.ExposedChannels = h.exposedChannelsForEdit(enumCtx, log, &meta)
		enumCancel()

		view, err := TunnelEditModal(&meta, snapshot.DisplayName, snapshot.Aliases)
		if err != nil {
			log.Error("list edit: modal render failed", "error", err)
			_ = h.postResponse(log, responseURL, ":warning: "+listEditOpenFailedMessage)
			return
		}
		openCtx, openCancel := context.WithTimeout(h.baseCtx, slackTriggerOpenViewBudget)
		defer openCancel()
		if err := h.cfg.OpenView(openCtx, teamID, triggerID, view); err != nil {
			log.Warn("list edit: views.open failed", "error", err)
			_ = h.postResponse(log, responseURL, ":warning: "+listEditOpenFailedMessage)
		}
	})
	respondJSON(w, http.StatusOK, map[string]any{})
}

// exposedChannelsForEdit returns the channels to pre-fill the Edit modal's
// channels multi-select: every channel the tunnel is currently exposed to
// (ChannelsForResource), always including the channel the modal was opened from
// (meta.ChannelID), deduped and capped at [listEditMaxChannels]. It is also the
// reconcile baseline carried in private_metadata, so a partial result is SAFE:
// the submit reconcile only revokes channels present here, so a channel the
// admin didn't see is never dropped.
//
// Best-effort: an enumeration failure (e.g. the dynamodb:Query grant isn't
// deployed yet) degrades to the current channel only — the modal still opens,
// the admin can still add channels, and nothing is revoked. AdminStore is
// guaranteed non-nil here (the Edit button only renders when it is wired).
func (h *Handler) exposedChannelsForEdit(ctx context.Context, log *slog.Logger, meta *TunnelEditModalMetadata) []string {
	seen := make(map[string]struct{})
	channels := make([]string, 0, listEditMaxChannels)
	// addIfRoom appends c unless it's empty, already seen, or the cap is hit.
	// It returns false ONLY on a cap hit (signaling the caller to stop paging);
	// a benign empty/duplicate skip still returns true.
	addIfRoom := func(c string) bool {
		if c == "" {
			return true
		}
		if _, dup := seen[c]; dup {
			return true
		}
		if len(channels) >= listEditMaxChannels {
			return false
		}
		seen[c] = struct{}{}
		channels = append(channels, c)
		return true
	}
	// The channel being edited from is always exposed and is never revoked, so
	// it leads the pre-fill regardless of what enumeration returns.
	addIfRoom(meta.ChannelID)
	if h.cfg.AdminStore == nil {
		return channels
	}
	found, err := h.cfg.AdminStore.ChannelsForResource(ctx, meta.TeamID, meta.ResourceID)
	if err != nil {
		log.Warn("list edit: channel enumeration failed — pre-filling current channel only",
			"error", err, "team_id", meta.TeamID, "resource_id", meta.ResourceID)
		return channels
	}
	for _, c := range found {
		if !addIfRoom(c) {
			log.Warn("list edit: exposed-channel count exceeds cap; truncating modal pre-fill (un-shown channels keep access)",
				"cap", listEditMaxChannels, "resource_id", meta.ResourceID)
			break
		}
	}
	return channels
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
		respondTunnelEditModalError(w, "Could not verify this dialog. Run /qurl list and tap Edit again.")
		return
	}
	if meta.TeamID == "" || meta.ChannelID == "" || meta.UserID == "" || meta.ResponseURL == "" || meta.ResourceID == "" {
		slog.Warn("tunnel edit modal metadata incomplete", "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		respondTunnelEditModalError(w, "Could not verify this dialog. Run /qurl list and tap Edit again.")
		return
	}
	// Slack signs the request envelope including private_metadata, so these
	// cross-checks prevent replaying one admin's modal as another user or
	// across workspaces.
	if payload.Team.ID == "" || payload.Team.ID != meta.TeamID {
		slog.Warn("tunnel edit modal team mismatch", "payload_team_id", payload.Team.ID, "metadata_team_id", meta.TeamID, "view_id", payload.View.ID)
		respondTunnelEditModalError(w, "This dialog was opened for a different workspace. Run /qurl list and tap Edit again.")
		return
	}
	if payload.User.ID == "" || payload.User.ID != meta.UserID {
		slog.Warn("tunnel edit modal user mismatch", "payload_user_id", payload.User.ID, "metadata_user_id", meta.UserID, "view_id", payload.View.ID)
		respondTunnelEditModalError(w, "Only the admin who opened this dialog can submit it. Run /qurl list and tap Edit again.")
		return
	}
	if h.cfg.AdminStore == nil || h.aliasStore == nil {
		respondTunnelEditModalError(w, "Admin features are not configured on this Slack bot deployment.")
		return
	}

	displayName, nameChanged, aliases, fieldErrors := parseTunnelEditModalArgs(payload.View.State.Values, &meta)
	if len(fieldErrors) > 0 {
		respondViewErrors(w, fieldErrors)
		return
	}
	desiredChannels := parseEditChannelSelection(payload.View.State.Values, &meta)

	// Mutation gate. Bounded so a slow store fails closed inside Slack's ack
	// window; off h.baseCtx (not the request ctx) so a client abort can't
	// cancel the deliberate fail-closed check — same posture as the install
	// modal submission.
	adminCtx, cancel := context.WithTimeout(h.baseCtx, adminGateBudget)
	defer cancel()
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(adminCtx, meta.TeamID, meta.UserID)
	if err != nil {
		slog.Error("tunnel edit modal admin check failed", "error", err, "team_id", meta.TeamID, "user_id", meta.UserID, "view_id", payload.View.ID)
		respondTunnelEditModalError(w, "Could not verify admin status. Retry in a moment.")
		return
	}
	if !isAdmin {
		slog.Warn("tunnel edit modal denied: non-admin", "team_id", meta.TeamID, "user_id", meta.UserID, "view_id", payload.View.ID)
		respondTunnelEditModalError(w, "This action is admin-only.")
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
		h.processTunnelEdit(ctx, log, &meta, displayName, nameChanged, aliases, desiredChannels)
	}) {
		respondTunnelEditModalError(w, "Slack bot is busy. Retry in a moment.")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{})
}

// parseTunnelEditModalArgs validates the Edit modal's submitted state: the
// Display Name (char-fenced via the shared validateDisplayNameChars) and the
// multiline aliases field. token is the row's primary `$<token>`, excluded
// from the editable alias set; currentName is the pre-filled Display Name the
// modal opened with. Returns the cleaned Display Name, whether it actually
// changed, the deduped/validated extra-alias set, or a per-field error map.
//
// The name is diffed against the IDENTICALLY-normalized currentName, and is
// validated ONLY when it changed. Two reasons: (1) a legacy or API-set name
// that predates this fence (a backtick / `<…>` / control byte) must not block
// an alias-only edit the admin never touched — the modal pre-fills it, so an
// untouched bad name would otherwise be un-saveable; (2) normalizing both
// sides means surrounding whitespace/quotes on the stored value don't register
// as a change and fire a spurious PATCH on an alias-only edit.
func parseTunnelEditModalArgs(values map[string]map[string]interactionStateValue, meta *TunnelEditModalMetadata) (displayName string, nameChanged bool, aliases []string, fieldErrors map[string]string) {
	fieldErrors = map[string]string{}

	rawName := normalizeDisplayNameInput(interactionStateText(values, tunnelEditBlockDisplayName, tunnelEditActionDisplayName))
	nameChanged = rawName != normalizeDisplayNameInput(meta.DisplayName)
	if nameChanged {
		if rawName == "" {
			// Reached only when clearing a previously-set name (empty + unchanged
			// is a no-op). This modal renames; clearing entirely is a separate verb.
			fieldErrors[tunnelEditBlockDisplayName] = "Enter a display name, or run /qurl-admin unset-display-name to remove it."
		} else if msg := validateDisplayNameChars(rawName); msg != "" {
			fieldErrors[tunnelEditBlockDisplayName] = msg
		}
	}

	aliases, aliasMsg := parseEditAliasLines(interactionStateText(values, tunnelEditBlockAliases, tunnelEditActionAliases), meta.Token, meta.Aliases)
	if aliasMsg != "" {
		fieldErrors[tunnelEditBlockAliases] = aliasMsg
	}

	if len(fieldErrors) > 0 {
		return "", false, nil, fieldErrors
	}
	return rawName, nameChanged, aliases, nil
}

// parseEditAliasLines parses the modal's one-alias-per-line field into a
// validated, deduped, order-preserving set of channel aliases (sigil-free). A
// leading `$` is optional per line (matching how the field pre-fills). The
// primary token is silently dropped if typed — the tunnel's own name is
// managed automatically, not through this field. prefilled is the set the modal
// opened with; only NEWLY-added aliases count against listEditMaxAliases.
// Returns the first invalid line's reason as userMsg.
func parseEditAliasLines(raw, token string, prefilled []string) (aliases []string, userMsg string) {
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
	// Cap only NEWLY-added aliases (those not already pre-filled), so a tunnel
	// that already carries more than listEditMaxAliases stays editable for a
	// name-only or removal-only change — the admin isn't forced to delete lines
	// they never added. New binds are what this bounds; removals are unbinds of
	// the pre-filled set, itself bounded by the button-value cap that gated the
	// Edit affordance in the first place.
	was := make(map[string]struct{}, len(prefilled))
	for _, a := range prefilled {
		was[a] = struct{}{}
	}
	added := 0
	for _, a := range aliases {
		if _, ok := was[a]; !ok {
			added++
		}
	}
	if added > listEditMaxAliases {
		return nil, fmt.Sprintf("Too many new aliases (max %d). Remove some lines.", listEditMaxAliases)
	}
	return aliases, ""
}

// parseEditChannelSelection reads the Edit modal's channels multi-select into
// the DESIRED exposed-channel set: the admin's selection, validated to Slack
// conversation-id shape and deduped, with the current channel force-included.
// The channel the modal was opened from always keeps access — the reconcile
// never revokes it — so including it here makes "kept" the default even if the
// admin somehow cleared the field. Malformed IDs (not expected from a Slack
// multi_conversations_select) are dropped rather than surfaced as a field
// error: the field isn't free-text, so a bad value is a wire anomaly, not user
// input to correct.
func parseEditChannelSelection(values map[string]map[string]interactionStateValue, meta *TunnelEditModalMetadata) []string {
	selected := interactionStateConversations(values, tunnelEditBlockChannels, tunnelEditActionChannels)
	seen := make(map[string]struct{}, len(selected)+1)
	out := make([]string, 0, len(selected)+1)
	add := func(c string) {
		if c == "" || !slackChannelIDPattern.MatchString(c) {
			return
		}
		if _, dup := seen[c]; dup {
			return
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	add(meta.ChannelID)
	for _, c := range selected {
		add(c)
	}
	return out
}

// processTunnelEdit is the async worker for an Edit modal submission. It
// PATCHes the Display Name (only when changed, so an alias-only edit can't
// clobber a concurrent display-name change), reconciles the channel aliases AND
// the channel exposure to the submitted sets, and posts an outcome summary to
// the list message's response_url.
//
// The name PATCH fails fast (returns before any reconcile) so a name failure
// doesn't half-apply alias/channel changes. The two reconciles are each
// best-effort and independent: a per-item failure is flagged in the summary
// rather than aborting the rest.
func (h *Handler) processTunnelEdit(ctx context.Context, log *slog.Logger, meta *TunnelEditModalMetadata, displayName string, nameChanged bool, desiredAliases, desiredChannels []string) {
	c, err := h.authenticatedClient(ctx, meta.TeamID)
	if err != nil {
		log.Error("tunnel edit: API key lookup failed", "error", err)
		_ = h.postResponse(log, meta.ResponseURL, ":warning: "+authErrorMessage(err))
		return
	}

	var changes []string
	// Display Name: PATCH only when it actually changed (computed in
	// parseTunnelEditModalArgs against the normalized pre-filled value), so an
	// alias-only edit doesn't overwrite a concurrent set-display-name.
	if nameChanged {
		if _, err := c.UpdateResource(ctx, meta.ResourceID, &client.UpdateResourceInput{Description: &displayName}); err != nil {
			log.Error("tunnel edit: display name update failed", "error", err, "resource_id", meta.ResourceID)
			_ = h.postResponse(log, meta.ResponseURL, sanitizeAPIError(err, "Failed to update the Display Name"))
			return
		}
		changes = append(changes, "Display Name updated")
	}

	aliasResult := h.reconcileChannelAliases(ctx, log, meta, desiredAliases)
	channelResult := h.reconcileChannelExposure(ctx, log, meta, desiredChannels)
	_ = h.postResponse(log, meta.ResponseURL, formatTunnelEditSummary(meta.Token, changes, &aliasResult, &channelResult))
}

// aliasReconcileResult buckets the outcome of reconciling a tunnel's channel
// aliases for the edit summary.
type aliasReconcileResult struct {
	added     []string
	removed   []string
	conflicts []string // alias already bound to a DIFFERENT tunnel in this channel
	hadError  bool     // a non-conflict bind/unbind or the policy read failed
}

// reconcileChannelAliases applies the admin's alias edit. Additions are desired
// aliases not already bound to this tunnel (read fresh, primary token excluded).
// Removals are scoped to the pre-filled snapshot (meta.Aliases): only an alias
// the admin SAW and deleted from the field — and that is still bound — gets
// unbound. Best-effort: a per-alias failure is logged and flagged in the result
// rather than aborting the whole reconcile.
//
// Diffing removals against the snapshot baseline rather than the full fresh set
// is deliberate. It closes two ways the "whole field is authoritative" model
// could destroy data the admin never touched: (a) an alias bound to this tunnel
// by ANOTHER admin after the modal opened isn't in the snapshot, so it's never
// unbound (broader than a concurrent rename); and (b) a stale or transiently
// EMPTY snapshot (channelAliasesByResourceID returns nil on a transient policy
// read failure) can't wipe the real bindings — a rename-only edit with an empty
// snapshot removes nothing. The remaining race (the snapshot is stale, so an
// alias the admin kept is re-bound, or one they deleted was already gone) is
// benign and matches the admin's visible intent.
func (h *Handler) reconcileChannelAliases(ctx context.Context, log *slog.Logger, meta *TunnelEditModalMetadata, desired []string) aliasReconcileResult {
	var res aliasReconcileResult

	entries, err := h.cfg.AdminStore.GetChannelPolicy(ctx, meta.TeamID, meta.ChannelID)
	if err != nil {
		// Without the current set we can't compute adds/removes safely. Report
		// the read failure rather than guessing.
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
	// Removals come from the snapshot the admin edited, not the full fresh set —
	// see the doc comment. Only unbind a pre-filled alias the admin dropped that
	// is still bound to this tunnel.
	for _, a := range meta.Aliases {
		if a == "" || a == meta.Token {
			continue
		}
		if _, keep := desiredSet[a]; keep {
			continue
		}
		if _, bound := current[a]; !bound {
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

// channelExposureResult buckets the outcome of reconciling a tunnel's channel
// exposure for the edit summary.
type channelExposureResult struct {
	exposed  []string // channels newly granted access
	revoked  []string // channels whose access was removed
	hadError bool     // an expose/revoke write failed
}

// reconcileChannelExposure applies the admin's channel-exposure edit. The
// DESIRED set is the multi-select selection (the current channel force-included
// by parseEditChannelSelection); the BASELINE is meta.ExposedChannels — the
// channels the modal actually SHOWED. Channels in desired but not baseline are
// exposed ([slackdata.Store.ExposeResourceToChannel]); channels in baseline but
// not desired are revoked ([slackdata.Store.RevokeResourceFromChannel]) — except
// the channel being edited from (meta.ChannelID), which the reconcile never
// writes or revokes: it's implicitly always available (the admin reached this
// modal from a scoped /qurl list row there), so the admin can't make the tunnel
// vanish from the list they're managing it on.
//
// Diffing removals against the SHOWN baseline (not a fresh enumeration) is the
// same data-loss guard the alias reconcile uses: if the open-time enumeration
// was partial (e.g. the dynamodb:Query grant isn't deployed, so the pre-fill
// was current-channel-only), the un-shown channels aren't in the baseline and
// so are never revoked — they keep their access. The remaining benign race (a
// channel exposed by another admin after the modal opened) likewise isn't in
// the baseline, so it survives. Best-effort per channel: a write failure is
// flagged, not fatal.
func (h *Handler) reconcileChannelExposure(ctx context.Context, log *slog.Logger, meta *TunnelEditModalMetadata, desired []string) channelExposureResult {
	var res channelExposureResult
	baseline := make(map[string]struct{}, len(meta.ExposedChannels))
	for _, c := range meta.ExposedChannels {
		baseline[c] = struct{}{}
	}
	desiredSet := make(map[string]struct{}, len(desired))
	for _, c := range desired {
		desiredSet[c] = struct{}{}
	}

	for _, c := range desired {
		if c == meta.ChannelID {
			// The channel being edited from is implicitly always available
			// (the admin reached this modal from a scoped /qurl list row there),
			// so it's never written by this reconcile — nor revoked below.
			continue
		}
		if _, already := baseline[c]; already {
			continue
		}
		if err := h.cfg.AdminStore.ExposeResourceToChannel(ctx, meta.TeamID, c, meta.ResourceID); err != nil {
			log.Error("tunnel edit: expose channel failed", "error", err, "channel_id", c, "resource_id", meta.ResourceID)
			res.hadError = true
			continue
		}
		res.exposed = append(res.exposed, c)
	}
	for _, c := range meta.ExposedChannels {
		if c == meta.ChannelID {
			// The channel being edited from always keeps access.
			continue
		}
		if _, keep := desiredSet[c]; keep {
			continue
		}
		if err := h.cfg.AdminStore.RevokeResourceFromChannel(ctx, meta.TeamID, c, meta.ResourceID); err != nil {
			log.Error("tunnel edit: revoke channel failed", "error", err, "channel_id", c, "resource_id", meta.ResourceID)
			res.hadError = true
			continue
		}
		res.revoked = append(res.revoked, c)
	}
	sort.Strings(res.exposed)
	sort.Strings(res.revoked)
	return res
}

// formatTunnelEditSummary renders the admin-facing ephemeral summarizing an
// edit. Aliases are shown as `$<alias>` code spans (charset-validated, so
// backtick-free); the token is the tunnel's id and CAN be an upstream slug that
// never passed the alias charset fence, so it's escaped for the code span as
// defense-in-depth — same posture as the modal's tunnelEditTokenLabel. Channel
// changes render as `<#C…>` mentions so the admin sees the channel names.
func formatTunnelEditSummary(token string, changes []string, aliasRes *aliasReconcileResult, chanRes *channelExposureResult) string {
	lines := []string{fmt.Sprintf("✅ Updated tunnel `$%s`.", escapeMrkdwnCode(token))}
	lines = append(lines, changes...)
	if len(aliasRes.added) > 0 {
		lines = append(lines, "Added alias(es): "+joinAliasCodes(aliasRes.added))
	}
	if len(aliasRes.removed) > 0 {
		lines = append(lines, "Removed alias(es): "+joinAliasCodes(aliasRes.removed))
	}
	if len(aliasRes.conflicts) > 0 {
		lines = append(lines, "Skipped (already used by another tunnel in this channel): "+joinAliasCodes(aliasRes.conflicts))
	}
	if len(chanRes.exposed) > 0 {
		lines = append(lines, "Exposed to: "+joinChannelMentions(chanRes.exposed))
	}
	if len(chanRes.revoked) > 0 {
		lines = append(lines, "Revoked from: "+joinChannelMentions(chanRes.revoked))
	}
	// "No changes." only when nothing happened AND nothing errored — a failed
	// write leaves all buckets empty but isn't a clean no-op, so the warning
	// below speaks for it instead.
	nothingApplied := len(changes) == 0 &&
		len(aliasRes.added) == 0 && len(aliasRes.removed) == 0 && len(aliasRes.conflicts) == 0 &&
		len(chanRes.exposed) == 0 && len(chanRes.revoked) == 0
	if !aliasRes.hadError && !chanRes.hadError && nothingApplied {
		lines = append(lines, "No changes.")
	}
	if aliasRes.hadError || chanRes.hadError {
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

// joinChannelMentions renders channel IDs as `<#C…>` Slack mentions for the
// edit summary, so the admin sees channel names rather than opaque IDs.
// slackChannelMention validates each ID's shape and falls back to neutral text
// for a malformed one (defense-in-depth; parseEditChannelSelection already
// shape-checks).
func joinChannelMentions(channels []string) string {
	mentions := make([]string, len(channels))
	for i, c := range channels {
		mentions[i] = slackChannelMention(c)
	}
	return strings.Join(mentions, ", ")
}

// respondTunnelEditModalError replaces the submitted Edit modal with a
// form-level error notice (structural/auth failures only; per-field problems
// use respondViewErrors). Falls back to a field-level error if the view render
// fails, so the submitter always sees a failure rather than a stuck modal.
func respondTunnelEditModalError(w http.ResponseWriter, message string) {
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
