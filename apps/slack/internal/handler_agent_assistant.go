package internal

import (
	"context"
	"log/slog"
)

// assistantThreadTitle is the title set on a freshly-opened Assistants-container
// thread (assistant.threads.setTitle) — the product's user-facing agent name.
const assistantThreadTitle = "qURL Secure Access Agent"

// assistantStarterPrompts are the DM-SAFE fallback first-run prompts: capability/how-to
// starters the agent answers without a channel read. They're used when the pane has no
// context channel (opened from a DM or the App Home) or its name can't be resolved (no
// channels:read scope). When the context channel does resolve, channelAwareStarterPrompts
// names it instead (see assistantStarterPromptsFor).
var assistantStarterPrompts = []SuggestedPrompt{
	{Title: "What can you do?", Message: "What can you help me with?"},
	{Title: "How do I get access?", Message: "How do I request access to a connector?"},
	{Title: "How do I protect a connector?", Message: "How do I protect a connector?"},
}

// channelAwareStarterPrompts names the channel the user opened the pane from, so the
// starters leverage the pane's channel scope ("What can I reach in #general?"). Leak-free:
// the prompt only NAMES a channel the user is already viewing (channelName came from the
// context.channel_id they opened the pane from); the scoped ANSWER stays membership-gated at
// turn time (paneContextChannel). Titles stay short + channel-neutral (Slack truncates the
// label); the message carries the channel name. Interpolating channelName is safe: Slack
// normalizes channel names to a constrained charset (lowercase/digits/hyphens/underscores)
// before conversations.info returns it, and it renders here as plain Slack text (not
// interpreted) — the same trust the turn-path system prompt already places in the name. The
// generic "What can you do?" capability starter is kept (last) so a brand-new user opening
// the pane from a channel isn't worse off than from a DM.
func channelAwareStarterPrompts(channelName string) []SuggestedPrompt {
	ch := "#" + channelName
	return []SuggestedPrompt{
		{Title: "What can I reach?", Message: "What can I reach in " + ch + "?"},
		{Title: "How do I get access?", Message: "How do I request access to a connector in " + ch + "?"},
		{Title: "How do I protect a connector?", Message: "How do I protect a connector?"},
		{Title: "What can you do?", Message: "What can you help me with?"},
	}
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
	prompts := h.assistantStarterPromptsFor(ctx, log, teamID, enterpriseID, at)
	if err := port.SetSuggestedPrompts(ctx, teamID, enterpriseID, at.ChannelID, at.ThreadTS, prompts); err != nil {
		log.Warn("agent: set assistant suggested prompts failed", "error", err)
	}
	if err := port.SetTitle(ctx, teamID, enterpriseID, at.ChannelID, at.ThreadTS, assistantThreadTitle); err != nil {
		log.Warn("agent: set assistant title failed", "error", err)
	}
}

// assistantStarterPromptsFor picks the first-run prompts for a freshly-opened pane: the
// channel-aware set naming the channel the user opened it from when that channel's name
// resolves, else the DM-safe generic set. The resolve is the same best-effort, cached
// conversations.info lookup the turn path uses — and it returns "" (without hitting Slack)
// for an empty context channel (a DM / App-Home open), so this one branch covers both "no
// context" and "name didn't resolve". At most one cached lookup per pane open; never blocks.
func (h *Handler) assistantStarterPromptsFor(ctx context.Context, log *slog.Logger, teamID, enterpriseID string, at *assistantThread) []SuggestedPrompt {
	if name := h.resolveChannelName(ctx, log, teamID, enterpriseID, at.Context.ChannelID); name != "" {
		return channelAwareStarterPrompts(name)
	}
	return assistantStarterPrompts
}
