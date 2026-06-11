package internal

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

const (
	// homeTabName is the App Home tab whose open we react to; a "messages" tab open
	// carries no review surface.
	homeTabName = "home"
	// agentHomeMaxEntries bounds how many recent actions the Home view lists (well
	// under Slack's 100-block view limit, with header/intro/divider on top).
	agentHomeMaxEntries = 20

	agentHomeTitle = "qURL Secure Access Agent"
	agentHomeIntro = "Your recent actions through the Secure Access Agent."
	agentHomeEmpty = "No actions yet. Ask the Secure Access Agent in a channel or DM to get access, protect a resource, or manage aliases — anything you confirm shows up here."
)

// handleAppHomeOpened publishes the viewer's own agent-action review surface when they
// open the App Home tab. Additive (no conversation turn) — it runs on the async pool
// off h.baseCtx behind the same per-workspace opt-in gate the turn path uses, so a
// workspace still defaulted-off during the staged rollout gets nothing. Only the "home"
// tab is handled; an empty user (malformed event) is ignored. The 200 ack is already
// sent by handleEvent, so this only schedules.
func (h *Handler) handleAppHomeOpened(env *slackEventEnvelope) {
	if env.Event.Tab != homeTabName || env.Event.User == "" {
		return
	}
	teamID, enterpriseID, userID := env.TeamID, env.EnterpriseID, env.Event.User
	log := slog.With(
		"surface", "agent",
		"event", slackEventTypeAppHomeOpened,
		"team_id", teamID,
		"enterprise_id", enterpriseID,
		"user_id", userID,
	)
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		if !h.workspaceAgentEnabled(ctx, log, teamID) {
			return
		}
		h.publishAgentHome(ctx, log, teamID, enterpriseID, userID)
	}) {
		log.Warn("agent: async pool saturated — dropping " + slackEventTypeAppHomeOpened)
	}
}

// publishAgentHome loads the viewer's recent confirmed actions and publishes the Home
// view. Best-effort: a nil publish seam (pre-enablement) is a no-op; a store read error
// still publishes the empty-state view rather than leaving a stale tab. The list is
// per-user (ListAuditEntries scopes to userID), so the surface never aggregates actions
// across channels the viewer can't see.
func (h *Handler) publishAgentHome(ctx context.Context, log *slog.Logger, teamID, enterpriseID, userID string) {
	if h.cfg.AppHomePublish == nil {
		return
	}
	var entries []slackdata.AuditEntry
	if h.cfg.AgentStore != nil {
		var err error
		if entries, err = h.cfg.AgentStore.ListAuditEntries(ctx, teamID, userID, agentHomeMaxEntries); err != nil {
			log.Warn("agent: list audit entries for App Home failed", "error", err)
		}
	}
	if err := h.cfg.AppHomePublish(ctx, teamID, enterpriseID, userID, buildAgentHomeView(entries)); err != nil {
		log.Warn("agent: publish App Home failed", "error", err)
	}
}

// buildAgentHomeView renders the Home tab blocks: a title, a one-line intro, then one
// section per recent action (newest-first, as ListAuditEntries returns them), or an
// empty-state line when there are none.
func buildAgentHomeView(entries []slackdata.AuditEntry) []any {
	blocks := make([]any, 0, 4+len(entries)) // header + intro + divider + entries (or the empty-state line)
	blocks = append(blocks,
		headerBlock(agentHomeTitle),
		sectionBlock(agentHomeIntro),
		map[string]any{"type": "divider"},
	)
	if len(entries) == 0 {
		return append(blocks, sectionBlock(agentHomeEmpty))
	}
	for i := range entries {
		blocks = append(blocks, sectionBlock(agentHomeEntryText(&entries[i])))
	}
	return blocks
}

// agentHomeEntryText renders one audit entry as mrkdwn. This is a PUBLIC-echo surface:
// Target and Reason are partly LLM-distilled, so they're escaped with the same
// primitives the confirm card uses (escapeMrkdwnCode for the backticked target,
// escapeMrkdwnText for the free-text reason) — a stored alias named "*ignore*" or one
// carrying bidi/zero-width characters renders inert. The label is a NEUTRAL description
// of the action attempted (not a success claim), so a failed action doesn't read as a
// success. The stored Outcome is deliberately NOT echoed here: it's the formatted public
// card text, and escaping its intentional backticks for safety would render it degraded
// (and it largely repeats the target shown cleanly above) — a clean per-result line is a
// follow-up (#704) that needs the action core to hand back a structured result.
func agentHomeEntryText(e *slackdata.AuditEntry) string {
	var b strings.Builder
	b.WriteString("*")
	b.WriteString(escapeMrkdwnText(agentHomeActionLabel(e.Action)))
	b.WriteString("*")
	if e.Target != "" {
		b.WriteString(" `")
		b.WriteString(escapeMrkdwnCode(e.Target))
		b.WriteString("`")
	}
	// Guard the channel id like the escaped fields around it: render the mention only for
	// a well-formed id (an id is normally a signature-verified payload.Channel.ID, but a
	// stray ">" would close the mention early), so this surface is uniformly defended.
	if slackChannelIDPattern.MatchString(e.Channel) {
		b.WriteString(" in <#")
		b.WriteString(e.Channel)
		b.WriteString(">")
	}
	b.WriteString("\n_")
	b.WriteString(time.Unix(e.UnixSec, 0).UTC().Format("2006-01-02 15:04 MST"))
	b.WriteString("_")
	if e.Reason != "" {
		b.WriteString(" · ")
		b.WriteString(escapeMrkdwnText(e.Reason))
	}
	return b.String()
}

// agentHomeActionLabel maps a stored action kind to a NEUTRAL display label for the
// action attempted (not a success claim — per-result success/failure is #704), falling
// back to the raw kind (which the caller still escapes) for an unrecognized value.
func agentHomeActionLabel(action string) string {
	switch action {
	case string(agent.ActionGet):
		return "Get access"
	case string(agent.ActionRevoke):
		return "Revoke"
	case string(agent.ActionSetAlias):
		return "Set alias"
	case string(agent.ActionUnsetAlias):
		return "Clear alias"
	case string(agent.ActionProtectURL):
		return "Protect URL"
	case string(agent.ActionProtectConnector):
		return "Protect connector"
	default:
		return action
	}
}
