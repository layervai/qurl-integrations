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

// handleAssistantThreadStarted handles a freshly-opened Assistants-container thread: it
// persists the pane's context (the channel the user opened it from, for a later turn to
// scope reads to) and sets the first-run UX — a title + suggested prompts.
func (h *Handler) handleAssistantThreadStarted(env *slackEventEnvelope) {
	h.scheduleAssistantContainerEvent(env, slackEventTypeAssistantThreadStarted,
		func(ctx context.Context, log *slog.Logger, teamID, enterpriseID string, at *assistantThread) {
			h.persistAssistantContext(ctx, log, teamID, at)
			h.setAssistantFirstRun(ctx, log, teamID, enterpriseID, at)
		})
}

// handleAssistantThreadContextChanged persists the pane's updated context when the user
// switches the channel they're viewing while the pane is open. No first-run UX — only the
// context is refreshed, so a later turn scopes to the channel now in view.
func (h *Handler) handleAssistantThreadContextChanged(env *slackEventEnvelope) {
	h.scheduleAssistantContainerEvent(env, slackEventTypeAssistantThreadContextChanged,
		func(ctx context.Context, log *slog.Logger, teamID, _ string, at *assistantThread) {
			h.persistAssistantContext(ctx, log, teamID, at)
		})
}

// scheduleAssistantContainerEvent runs the shared scaffolding for an Assistants-container
// event (assistant_thread_started / _context_changed): validate the assistant_thread,
// build the log, and schedule work on the async pool off h.baseCtx behind the
// per-workspace opt-in gate. That gate is the same one the turn path uses — the "Agents &
// AI Apps" toggle is app-level, so the pane can open in a workspace still defaulted-off
// during the staged rollout, where persisting context or setting prompts is wasted (its
// turns are dropped by this same gate). The 200 ack is already sent by handleEvent, so
// this only schedules. teamID/enterpriseID and a COPY of the assistant_thread are passed
// to work — value copies, not the env pointer, so the goroutine can't observe a reused
// envelope. Additive (no LLM, no turn); dark until the manifest toggle + scope are set.
func (h *Handler) scheduleAssistantContainerEvent(env *slackEventEnvelope, eventType string, work func(ctx context.Context, log *slog.Logger, teamID, enterpriseID string, at *assistantThread)) {
	at := env.Event.AssistantThread
	if at == nil || at.ChannelID == "" || at.ThreadTS == "" {
		return
	}
	log := slog.With(
		"surface", "agent",
		"event", eventType,
		"team_id", env.TeamID,
		"enterprise_id", env.EnterpriseID,
		"channel_id", at.ChannelID,
	)
	teamID, enterpriseID := env.TeamID, env.EnterpriseID
	atCopy := *at
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		if !h.workspaceAgentEnabled(ctx, log, teamID) {
			return
		}
		work(ctx, log, teamID, enterpriseID, &atCopy)
	}) {
		log.Warn("agent: async pool saturated — dropping " + eventType)
	}
}

// persistAssistantContext stores the channel the user opened the pane FROM
// (at.Context.ChannelID), keyed by the pane thread, so a later pane turn — which carries
// no context of its own — can scope its reads to it (consumed in a follow-up slice).
// Best-effort: a store error is logged, never surfaced. Skips when there's no context
// channel (a pane opened from a DM or the App Home has none to scope to). Keyed on the
// SLACK TEAM id (see PutThreadContext).
func (h *Handler) persistAssistantContext(ctx context.Context, log *slog.Logger, teamID string, at *assistantThread) {
	if at.Context.ChannelID == "" {
		return
	}
	key := agentThreadKey(at.ChannelID, at.ThreadTS)
	if err := h.cfg.AgentStore.PutThreadContext(ctx, teamID, key, at.Context.ChannelID); err != nil {
		log.Warn("agent: persist assistant pane context failed", "error", err)
	}
}

// setAssistantFirstRun sets the freshly-opened pane's suggested prompts and title. The
// per-workspace opt-in is already checked by the caller, so this only does the UX. A nil
// AssistantThreads seam (pre-enablement, or unwired in a test) is a no-op. The two calls
// are independent best-effort: a failure is logged and the other still runs; prompts go
// first so they land even if ctx expires before the title call — the higher-value affordance.
func (h *Handler) setAssistantFirstRun(ctx context.Context, log *slog.Logger, teamID, enterpriseID string, at *assistantThread) {
	port := h.cfg.AssistantThreads
	if port == nil {
		return
	}
	if err := port.SetSuggestedPrompts(ctx, teamID, enterpriseID, at.ChannelID, at.ThreadTS, assistantStarterPrompts); err != nil {
		log.Warn("agent: set assistant suggested prompts failed", "error", err)
	}
	if err := port.SetTitle(ctx, teamID, enterpriseID, at.ChannelID, at.ThreadTS, assistantThreadTitle); err != nil {
		log.Warn("agent: set assistant title failed", "error", err)
	}
}
