package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// handleAgentToggle serves `/qurl-admin agent [on|off]`: a workspace admin opts the
// workspace in or out of conversation mode (the per-workspace toggle in
// workspace_mappings). Bare `agent` reports the current per-workspace setting. The
// toggle gates the agent on TOP of the org-level surface — while the deployment's
// conversation mode is dark, setting it just pre-stages the opt-in.
func (h *Handler) handleAgentToggle(w http.ResponseWriter, values url.Values) {
	// Admin-gate BEFORE parsing the arg, so every unauthorized caller takes the same
	// audited admin-only path: a non-admin probing `agent <anything>` must not get a
	// different (and unaudited) reply than `agent on`.
	teamID := strings.TrimSpace(values.Get(fieldTeamID))
	if teamID == "" {
		respondSlack(w, ":warning: missing team_id in slash command payload")
		return
	}
	userID := strings.TrimSpace(values.Get(fieldUserID))
	if !h.requireAdminStoreSync(w) {
		return
	}
	if !h.requireAdminSync(w, teamID, userID, AdminActionAgentToggle) {
		return
	}

	_, rest := slashVerb(strings.TrimSpace(values.Get(fieldText)), adminVerbAgent)
	arg := strings.ToLower(strings.TrimSpace(rest))

	var enable, show bool
	switch arg {
	case "", "status", "show":
		show = true
	case "on", "enable", "enabled", "true":
		enable = true
	case "off", "disable", "disabled", "false":
		enable = false
	default:
		respondSlack(w, "Usage: `agent on` or `agent off` (or bare `agent` to show the current state).")
		return
	}

	// Single-DDB sync admin verb (one UpdateItem to set, or one GetItem to show) —
	// same shape and budget as add/remove/admins, not the multi-hop alias verbs.
	ctx, cancel := context.WithTimeout(h.baseCtx, adminSyncVerbBudget)
	defer cancel()

	if show {
		respondSlack(w, h.agentToggleStatus(ctx, teamID))
		return
	}
	if err := h.cfg.AdminStore.SetAgentEnabled(ctx, teamID, enable); err != nil {
		respondSlack(w, agentToggleSetError(err))
		return
	}
	// Audit the config change: a successful toggle is a security-relevant mutation, so
	// it must reach on-call like the other admin writes — the AdminAction gate label
	// otherwise only surfaces in CloudWatch on the denial path.
	slog.Info("agent toggle succeeded", "team_id", teamID, "user_id", userID, "enabled", enable)
	if enable {
		respondSlack(w, "Conversation mode is now *on* for this workspace — members can @mention or DM the qURL Secure Access Agent."+h.agentToggleOrgDarkSuffix())
		return
	}
	respondSlack(w, "Conversation mode is now *off* for this workspace — the qURL Secure Access Agent won't respond to @mentions or DMs here.")
}

// agentToggleStatus renders the bare `agent` reply: the per-workspace setting,
// distinguishing an explicit on/off from "using the default".
func (h *Handler) agentToggleStatus(ctx context.Context, teamID string) string {
	enabled, set, err := h.cfg.AdminStore.AgentEnabledFor(ctx, teamID)
	if err != nil {
		slog.Warn("agent toggle: status read failed", "team_id", teamID, "error", err)
		return "Couldn't read the conversation-mode setting right now. Please try again."
	}
	if !set {
		return fmt.Sprintf("Conversation mode is *%s* for this workspace (the default — not explicitly set). Use `agent on` or `agent off` to set it.", onOffLabel(h.cfg.AgentDefaultEnabled)) + h.agentToggleOrgDarkSuffix()
	}
	return fmt.Sprintf("Conversation mode is explicitly *%s* for this workspace.", onOffLabel(enabled)) + h.agentToggleOrgDarkSuffix()
}

// onOffLabel renders a conversation-mode toggle bool as its user-facing word.
func onOffLabel(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// agentToggleOrgDarkSuffix appends a heads-up when the org-level surface isn't wired
// yet, so an admin who enables the toggle isn't surprised by silence.
func (h *Handler) agentToggleOrgDarkSuffix() string {
	if h.agentEnabled() {
		return ""
	}
	return " (Conversation mode isn't enabled for this deployment yet, so this takes effect once the operator turns it on.)"
}

// agentToggleSetError maps a SetAgentEnabled failure to user-facing copy without
// leaking internals — the only expected error is the unbound-workspace 404.
func agentToggleSetError(err error) string {
	var se *slackdata.Error
	if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
		return "This workspace isn't set up yet — run `/qurl setup` first, then try again."
	}
	return "Couldn't update the conversation-mode setting. Please try again."
}
