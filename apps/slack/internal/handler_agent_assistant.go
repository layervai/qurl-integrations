package internal

import (
	"context"
	"log/slog"
)

// assistantThreadTitle is the title set on a freshly-opened Assistants-container
// thread (assistant.threads.setTitle) — the product's user-facing agent name.
const assistantThreadTitle = "qURL Secure Access Agent"

// assistantStarterPrompts are the first-run suggested prompts shown when a user
// opens the assistant pane (assistant.threads.setSuggestedPrompts). Static for v1
// and deliberately DM-SAFE: the pane is a 1:1 DM with the agent, which has no
// channel scope until the context-scoping slice (assistant_thread.context.channel_id),
// so a channel-read prompt ("what can I reach here?") would resolve against the empty
// DM and read as broken. v1 uses capability/how-to starters the agent answers without
// a channel read; channel-aware prompts land with the context.channel_id slice.
var assistantStarterPrompts = []SuggestedPrompt{
	{Title: "What can you do?", Message: "What can you help me with?"},
	{Title: "How do I get access?", Message: "How do I request access to a connector?"},
	{Title: "How do I protect a connector?", Message: "How do I protect a connector?"},
}

// handleAssistantThreadStarted gives a freshly-opened Assistants-container thread
// its first-run UX — a title + suggested prompts — when a user opens the agent's
// assistant pane. UX-only (no LLM, no turn) and best-effort behind the
// AssistantThreads seam (nil = no-op). Slack delivers this only once the AI-app
// manifest toggle + assistant:write scope are set, so the surface is dark until then.
// Scheduled on the async pool off h.baseCtx like the turn path; the 200 ack is
// already sent by handleEvent.
func (h *Handler) handleAssistantThreadStarted(env *slackEventEnvelope) {
	at := env.Event.AssistantThread
	if h.cfg.AssistantThreads == nil || at == nil || at.ChannelID == "" || at.ThreadTS == "" {
		return
	}
	log := slog.With(
		"surface", "agent",
		"event", slackEventTypeAssistantThreadStarted,
		"team_id", env.TeamID,
		"enterprise_id", env.EnterpriseID,
		"channel_id", at.ChannelID,
	)
	teamID, enterpriseID := env.TeamID, env.EnterpriseID
	atCopy := *at
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		h.setAssistantFirstRun(ctx, log, teamID, enterpriseID, &atCopy)
	}) {
		log.Warn("agent: async pool saturated — dropping assistant_thread_started")
	}
}

// setAssistantFirstRun sets the thread's suggested prompts and title — but only when
// the workspace has opted into conversation mode. The "Agents & AI Apps" toggle is
// app-level, so the pane can appear in workspaces still defaulted-off during the
// staged rollout (workspaceAgentEnabled); setting live-looking prompts there is worse
// than none, because a clicked prompt's turn is dropped by that same gate. This is
// the cheap per-workspace read the turn path already does, on the right (baseCtx) ctx.
//
// The two calls are independent best-effort: a failure is logged and the other still
// runs. Prompts go first so they land even if ctx expires before the title call —
// they're the higher-value affordance.
func (h *Handler) setAssistantFirstRun(ctx context.Context, log *slog.Logger, teamID, enterpriseID string, at *assistantThread) {
	if !h.workspaceAgentEnabled(ctx, log, teamID) {
		return
	}
	port := h.cfg.AssistantThreads
	if err := port.SetSuggestedPrompts(ctx, teamID, enterpriseID, at.ChannelID, at.ThreadTS, assistantStarterPrompts); err != nil {
		log.Warn("agent: set assistant suggested prompts failed", "error", err)
	}
	if err := port.SetTitle(ctx, teamID, enterpriseID, at.ChannelID, at.ThreadTS, assistantThreadTitle); err != nil {
		log.Warn("agent: set assistant title failed", "error", err)
	}
}
