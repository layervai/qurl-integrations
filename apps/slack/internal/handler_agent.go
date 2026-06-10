package internal

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// Slack Events API event types this handler reacts to.
const (
	slackEventTypeAppMention = "app_mention"
	slackEventTypeMessage    = "message"
	slackChannelTypeIM       = "im"
)

// agentProposalPreviewPrefix prefixes a proposed-mutation reply while
// conversation mode is read-only (the confirm flow lands in a follow-up). The
// agent only ever proposes; it never executes, so a preview is the honest reply.
const agentProposalPreviewPrefix = "I can set that up, but applying changes from conversation mode isn't enabled yet. Here's what I'd do once it is:\n• "

// agentErrorReply is posted when a turn fails unexpectedly. Deliberately vague —
// internals never reach the channel.
const agentErrorReply = "Something went wrong handling that. Please try again, or use a `/qurl` command."

// agentTransientReply is posted when a turn fails for a likely-transient reason —
// the turn-budget deadline elapsed, or the context was canceled — as opposed to
// agentErrorReply's generic failure. Slack's agent-design guidance is to separate
// "temporarily unavailable, worth retrying" from a capability limit so the user
// knows a retry is worthwhile; a done turn ctx is our reliable signal for the
// former. Still leaks no internals.
const agentTransientReply = "That took longer than I could handle just now — please try again, or use a `/qurl` command."

// agentRateLimitedReply is posted when a turn is dropped for hitting the per-user or
// per-team turn-rate cap. Deliberately uniform across both limits (don't leak which
// cap, or its value) and points at the always-available slash commands.
const agentRateLimitedReply = "You've reached the conversation-mode limit for now — give it a few minutes, or use a `/qurl` command in the meantime."

// agentTurnRateWindow is the fixed window for the per-user / per-team turn counters.
// The env limits are expressed per hour, so the window is one hour.
const agentTurnRateWindow = time.Hour

// slackEventEnvelope is the Events API outer payload. Only the fields the agent
// surface needs are modeled.
type slackEventEnvelope struct {
	Type         string          `json:"type"`
	Challenge    string          `json:"challenge"`
	TeamID       string          `json:"team_id"`
	EnterpriseID string          `json:"enterprise_id"`
	APIAppID     string          `json:"api_app_id"`
	EventID      string          `json:"event_id"`
	Event        slackInnerEvent `json:"event"`
}

// slackInnerEvent is the inner `event` object for app_mention / message events.
type slackInnerEvent struct {
	Type        string `json:"type"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Subtype     string `json:"subtype"`
	Text        string `json:"text"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
}

// agentEnabled reports whether conversation mode is fully wired and not killed.
func (h *Handler) agentEnabled() bool {
	return !h.cfg.AgentDisabled &&
		h.cfg.AgentLLM != nil &&
		h.cfg.AgentStore != nil &&
		h.cfg.PostMessage != nil
}

// workspaceAgentEnabled resolves the per-workspace conversation-mode toggle, on top
// of the org-level agentEnabled gate: the stored agent_enabled flag if the workspace
// set it (AgentEnabledFor), else Config.AgentDefaultEnabled. It FAILS CLOSED on a
// read error — don't run the agent if we can't confirm the workspace opted in, and
// never override an explicit opt-out on a transient blip. With no AdminStore wired
// there's no per-workspace store to read, so the org default governs. The read is a
// single workspace_mappings GetItem, off the ack path (callers are already async).
func (h *Handler) workspaceAgentEnabled(ctx context.Context, log *slog.Logger, teamID string) bool {
	if h.cfg.AdminStore == nil {
		return h.cfg.AgentDefaultEnabled
	}
	enabled, set, err := h.cfg.AdminStore.AgentEnabledFor(ctx, teamID)
	if err != nil {
		log.Warn("agent: per-workspace toggle read failed; treating as disabled", "team_id", teamID, "error", err)
		return false
	}
	if set {
		return enabled
	}
	return h.cfg.AgentDefaultEnabled
}

// agentTurnLimited enforces the per-user and per-team turn-rate caps for one turn,
// returning the reply to post when the turn must be dropped. It is a COST BACKSTOP,
// not a security gate, so it FAILS OPEN: a transient counter error logs and allows
// the turn rather than dropping a legitimate member — the opposite of the
// fail-closed workspace/dedupe gates. The per-user counter is bumped FIRST so one
// member spamming can't inflate the shared per-team counter for everyone else.
//
// One asymmetry, by design: a turn the per-user cap denies never reaches the
// per-team counter, but a turn the per-team cap denies has ALREADY incremented the
// per-user counter (it was a real attempt). Both only inflate within the window and
// reset when it rolls, so it's a non-issue for a backstop.
func (h *Handler) agentTurnLimited(ctx context.Context, log *slog.Logger, env *slackEventEnvelope) (reply string, limited bool) {
	// A non-positive limit disables that scope (unlimited), so each guard also
	// short-circuits the counter bump when its cap is off — both off ⇒ no DDB calls.
	// env.Event.User is non-empty here: shouldDispatchAgentEvent (the only gate before
	// processAgentEvent) rejects e.User == "", so the per-user scope can't collapse
	// into one shared "user#" bucket.
	if l := h.cfg.AgentMaxTurnsPerUserPerHour; l > 0 && h.overTurnLimit(ctx, log, env.TeamID, "user#"+env.Event.User, l) {
		return agentRateLimitedReply, true
	}
	if l := h.cfg.AgentMaxTurnsPerTeamPerHour; l > 0 && h.overTurnLimit(ctx, log, env.TeamID, "team", l) {
		return agentRateLimitedReply, true
	}
	return "", false
}

// overTurnLimit bumps the named fixed-window counter and reports whether this turn
// crossed the limit. Fails OPEN (returns false) on a counter error — a conscious
// tradeoff: this leaves the cap weakest exactly under DDB stress (throttling is also
// when a busy workspace racks up cost), but dropping a legitimate member's turn on a
// transient blip is worse for a backstop than briefly running uncapped.
func (h *Handler) overTurnLimit(ctx context.Context, log *slog.Logger, teamID, scope string, limit int) bool {
	count, err := h.cfg.AgentStore.BumpTurnCount(ctx, teamID, scope, agentTurnRateWindow)
	if err != nil {
		log.Warn("agent: turn-rate counter failed; allowing turn (fail-open)", "scope", scope, "team_id", teamID, "error", err)
		return false
	}
	if count > int64(limit) {
		log.Info("agent: turn rate limit reached", "scope", scope, "team_id", teamID, "count", count, "limit", limit)
		return true
	}
	return false
}

// handleAgentEvent decides whether an event_callback should drive a
// conversation turn and, if so, dispatches it to the async pool. The caller
// (handleEvent) always acks 200 regardless — Slack must not retry — so this only
// schedules work; it never writes the response.
func (h *Handler) handleAgentEvent(env *slackEventEnvelope) {
	if !h.agentEnabled() || !shouldDispatchAgentEvent(env) {
		return
	}
	log := slog.With(
		"surface", "agent",
		"team_id", env.TeamID,
		"enterprise_id", env.EnterpriseID,
		"channel_id", env.Event.Channel,
		"event_id", env.EventID,
	)
	envCopy := *env
	if !h.startAsyncWorkerWithTimeout(log, agentTurnTimeout, func(ctx context.Context, log *slog.Logger) {
		h.processAgentEvent(ctx, log, &envCopy)
	}) {
		log.Warn("agent: async pool saturated — dropping event")
	}
}

// agentTurnTimeout bounds one conversation turn. A turn makes up to
// defaultMaxIterations Anthropic round-trips plus channel-scoped reads, so it
// needs more than the 25s slash-command budget — 25s could cancel a legitimate
// multi-tool-call turn mid-flight and surface a spurious error to the user. The
// iteration cap and (later) per-user rate limiting bound how long a slot is held.
const agentTurnTimeout = 90 * time.Second

// agentDeliveryBudget bounds each post-turn delivery step — the transcript save
// and the reply post each derive their own context with this budget off
// h.baseCtx, never the turn ctx. By delivery time the turn ctx may be spent or
// canceled (the turn hit agentTurnTimeout), and a SaveConversation / PostMessage
// on a dead ctx fails instantly — yet the dedupe write is already committed and
// Slack won't retry, so the user would get silence. Deriving off baseCtx (like
// callerIsAdmin) lets delivery outlive the turn deadline; bounding it (not
// baseCtx directly) keeps a wedged Slack/DDB call from pinning an async-pool slot
// and lets SIGTERM still drain in-flight delivery.
const agentDeliveryBudget = 15 * time.Second

// shouldDispatchAgentEvent filters out everything that isn't a human asking the
// agent something: non-mention/DM events, bot and system/edited messages (the
// self-loop guard), authorless events, channel messages that aren't @-mentions,
// and empty text.
func shouldDispatchAgentEvent(env *slackEventEnvelope) bool {
	e := &env.Event
	if e.BotID != "" || e.Subtype != "" || e.User == "" {
		return false
	}
	switch e.Type {
	case slackEventTypeAppMention:
		// Channel @-mention — always a deliberate address.
	case slackEventTypeMessage:
		// Only DMs; we don't subscribe to the channel-message firehose.
		if e.ChannelType != slackChannelTypeIM {
			return false
		}
	default:
		return false
	}
	return strings.TrimSpace(stripBotMention(e.Text)) != ""
}

// botMentionPattern matches a leading Slack user mention, e.g. "<@U123>" or
// "<@U123|name>", so an @-mention's text can be reduced to the actual request.
// The [UW][A-Z0-9]{8,63} id body matches the established mention-id grammar in
// parser.go's userMentionPattern (rejects toy ids; caps pathological pastes) —
// this one strips a leading mention rather than validating a whole token, so the
// anchoring differs, but the id charset is kept in sync.
var botMentionPattern = regexp.MustCompile(`^\s*<@[UW][A-Z0-9]{8,63}(?:\|[^>]*)?>\s*`)

// stripBotMention removes a leading bot mention from app_mention text.
func stripBotMention(text string) string {
	return strings.TrimSpace(botMentionPattern.ReplaceAllString(text, ""))
}

// agentEventPartition is the conversation-state partition key: the Enterprise
// Grid org id when present (stable across the org's workspaces), else the team.
func agentEventPartition(env *slackEventEnvelope) string {
	if env.EnterpriseID != "" {
		return env.EnterpriseID
	}
	return env.TeamID
}

// agentEventRootTS is the thread root a turn belongs to: the parent thread_ts
// when the message is already in a thread, else the message's own ts (which the
// reply threads under).
func agentEventRootTS(e *slackInnerEvent) string {
	if e.ThreadTS != "" {
		return e.ThreadTS
	}
	return e.TS
}

// agentEventThreadKey identifies one conversation: channel + thread root.
func agentEventThreadKey(env *slackEventEnvelope) string {
	return env.Event.Channel + ":" + agentEventRootTS(&env.Event)
}

// agentEventDedupeKey identifies the inbound MESSAGE — channel + the message's
// OWN ts — so every delivery of one message (a retry, or overlapping app_mention
// + message.im events with distinct event_ids) shares it and dedupes to one turn.
// It is deliberately env.Event.TS, NOT agentEventRootTS: a follow-up in a thread
// shares the thread root but has its own ts, so keying on the root would make the
// dedupe drop every threaded follow-up. Distinct from agentEventThreadKey for
// exactly that reason.
func agentEventDedupeKey(env *slackEventEnvelope) string {
	return env.Event.Channel + ":" + env.Event.TS
}

// processAgentEvent runs one conversation turn on the async pool: dedupe, load
// history, run the agent, persist, and post the reply.
func (h *Handler) processAgentEvent(ctx context.Context, log *slog.Logger, env *slackEventEnvelope) {
	// Panic safety-net: we've already acked 200 and may have committed the dedupe
	// marker, so Slack won't retry. If the turn panics, startAsyncWorker's recover
	// would log+swallow but post nothing, leaving the @-mention silently
	// unanswered. Absorb the panic here instead — log the stack (the worker recover
	// won't see it) and post the generic reply on a fresh ctx (postAgentReply
	// self-derives one) so the user always hears something went wrong.
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("agent: panic during turn", "recover", rec, "stack", string(debug.Stack()))
			h.postAgentReply(log, env, agentEventRootTS(&env.Event), agentErrorReply)
		}
	}()

	// Per-workspace toggle, BEFORE the dedupe marker so a disabled workspace consumes
	// nothing. A workspace that hasn't opted in (or opted out) gets no reply — the
	// same silent behavior as the org-level dark surface; members use slash commands.
	if !h.workspaceAgentEnabled(ctx, log, env.TeamID) {
		log.Info("agent: conversation mode disabled for this workspace; ignoring @mention/DM")
		return
	}

	partition := agentEventPartition(env)

	// Dedupe on message identity (see agentEventDedupeKey), not the per-delivery
	// event_id: two events for one message would otherwise both win and double-reply.
	first, err := h.cfg.AgentStore.MarkEventSeen(ctx, partition, agentEventDedupeKey(env))
	if err != nil {
		// Fail closed: dropping a turn on a transient error beats a double reply.
		log.Error("agent: dedupe check failed; dropping event", "error", err)
		return
	}
	if !first {
		log.Info("agent: duplicate event ignored")
		return
	}

	// Rate-limit AFTER dedupe (count unique messages, not redeliveries) and BEFORE
	// the turn runs (the LLM is the cost we're capping). Confirm-clicks
	// (processAgentConfirm) are deliberately NOT limited: they're consume-once and
	// admin-gated and carry no LLM cost. A limited turn still gets a reply — silence
	// would read as the agent ignoring the member.
	//
	// The count is of turn ATTEMPTS, not answered turns — a turn bumped here that then
	// fails transiently (agentTransientReply) still counts, so the cap is "N
	// attempts/hour". That's the right unit for a COST backstop: the LLM round-trip is
	// the spend whether or not it produced a usable answer.
	if reply, limited := h.agentTurnLimited(ctx, log, env); limited {
		h.postAgentReply(log, env, agentEventRootTS(&env.Event), reply)
		return
	}

	threadKey := agentEventThreadKey(env)
	history, version, err := h.loadAgentHistory(ctx, log, partition, threadKey)
	if err != nil {
		// Dedupe already committed, so Slack won't retry and we own the reply:
		// tell the user something went wrong rather than leaving their @-mention
		// silently unanswered (already logged in loadAgentHistory).
		h.postAgentReply(log, env, agentEventRootTS(&env.Event), agentErrorReply)
		return
	}

	// ChannelName is intentionally left unset: the Events payload carries only the
	// channel id (describeChannel falls back to it), and resolving the name would
	// cost a conversations.info call per turn. Tracked in #659.
	tc := agent.TurnContext{
		TeamID:        env.TeamID,
		EnterpriseID:  env.EnterpriseID,
		ChannelID:     env.Event.Channel,
		UserID:        env.Event.User,
		CallerIsAdmin: h.callerIsAdmin(log, env.TeamID, env.Event.User),
	}

	a := agent.New(h.cfg.AgentLLM, h.newAgentBackend(log))
	result, newHistory, err := a.Run(ctx, &tc, history, stripBotMention(env.Event.Text))

	replyTS := agentEventRootTS(&env.Event)
	if err != nil {
		log.Error("agent: turn failed", "error", err)
		reply := agentErrorReply
		if ctx.Err() != nil {
			// The turn ctx is done (agentTurnTimeout elapsed, or baseCtx canceled on
			// shutdown): a transient timeout, not a capability limit — invite a retry.
			reply = agentTransientReply
		}
		h.postAgentReply(log, env, replyTS, reply)
		return
	}

	// Token usage per turn (summed across the agent's round-trips). The cache
	// counters are the operator hook for confirming whether prompt caching is
	// paying off once conversation mode is live (see the agent package).
	log.Info("agent: turn complete",
		"proposed", result.Proposal != nil,
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"cache_read_tokens", result.Usage.CacheReadInputTokens,
		"cache_creation_tokens", result.Usage.CacheCreationInputTokens,
	)

	// Save before posting: the transcript must be durably consistent before the
	// user can fire a follow-up turn against it. The post is the slower,
	// user-visible step, so this trades a little reply latency for that ordering.
	h.saveAgentHistory(log, partition, threadKey, newHistory, version)

	// Deliver: an interactive confirm card for an executable proposal once the
	// confirm flow is enabled, else the text reply/preview (merged #650 behavior).
	h.deliverAgentResult(log, env, replyTS, &result)
}

// loadAgentHistory reads and decodes a thread's transcript. A decode error is
// treated as an empty thread (start fresh) rather than a hard failure; the
// loaded version is preserved either way so the next SaveConversation still
// passes the optimistic-concurrency check (and a corrupt blob gets overwritten).
func (h *Handler) loadAgentHistory(ctx context.Context, log *slog.Logger, partition, threadKey string) ([]agent.Message, int64, error) {
	blob, version, err := h.cfg.AgentStore.LoadConversation(ctx, partition, threadKey)
	if err != nil {
		log.Error("agent: load conversation failed", "error", err)
		return nil, 0, err
	}
	if len(blob) == 0 {
		return nil, version, nil
	}
	var history []agent.Message
	if err := json.Unmarshal(blob, &history); err != nil {
		log.Warn("agent: corrupt conversation history; starting fresh", "error", err)
		return nil, version, nil
	}
	return history, version, nil
}

// maxPersistedMessages bounds the transcript persisted per thread so a long
// thread can't grow the DynamoDB item toward the 400KB limit (at which point the
// save fails and the thread loses continuity). At ~2 messages per plain Q&A turn
// and ~4 per tool-using turn, 40 messages is roughly the last 10–20 turns —
// ample given the per-turn work cap; older turns are trimmed.
const maxPersistedMessages = 40

// maxPersistedBytes caps the serialized transcript well under DynamoDB's 400KB
// item limit. The message-count cap alone doesn't bound bytes — a single large
// tool_result could still bloat the item — so we also drop oldest turns until
// the blob fits. (Read-only tool output is compact today; this matters more once
// mutation tool_results land.)
const maxPersistedBytes = 350 * 1024

// saveAgentHistory persists the updated transcript, trimmed to a bounded length
// and byte size. A version conflict (a concurrent turn won) is logged and
// dropped — the reply still posts. Persistence runs on its own context off
// h.baseCtx (see agentDeliveryBudget), not the possibly-spent turn ctx.
func (h *Handler) saveAgentHistory(log *slog.Logger, partition, threadKey string, history []agent.Message, version int64) {
	trimmed := trimAgentHistory(history, maxPersistedMessages)
	blob, err := json.Marshal(trimmed)
	if err != nil {
		log.Error("agent: marshal conversation failed", "error", err)
		return
	}
	// Byte guard: drop oldest turns (one per pass — trimAgentHistory cuts at a
	// turn boundary) until the blob fits or only the latest turn remains. Break
	// when a pass makes no progress: trimAgentHistory cuts only at a user-turn
	// start, so a single turn whose own tool_result blows past the cap has no
	// boundary below it and returns unchanged — without this guard the loop would
	// spin forever (a tight CPU loop no context can interrupt). In that case we
	// save oversized and let DDB reject + log rather than hang the worker.
	for len(blob) > maxPersistedBytes && len(trimmed) > 1 {
		next := trimAgentHistory(trimmed, len(trimmed)-1)
		if len(next) == len(trimmed) {
			break
		}
		trimmed = next
		if blob, err = json.Marshal(trimmed); err != nil {
			log.Error("agent: marshal conversation failed", "error", err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, agentDeliveryBudget)
	defer cancel()
	switch err := h.cfg.AgentStore.SaveConversation(ctx, partition, threadKey, blob, version); {
	case errors.Is(err, slackdata.ErrConversationConflict):
		log.Info("agent: conversation version conflict; concurrent turn won")
	case err != nil:
		log.Error("agent: save conversation failed", "error", err)
	}
}

// trimAgentHistory bounds the transcript to roughly the most recent maxMessages,
// cutting only at the start of a user turn (a user message carrying text). That
// guarantees the kept slice never begins with an orphaned tool_result or an
// assistant tool_use whose result was trimmed away — both of which the model API
// rejects. If the trim window holds no clean boundary (an unusually long single
// turn), it falls back to the last turn start anywhere so the result is still
// bounded; only a transcript with no user-text turn at all is returned as-is.
func trimAgentHistory(msgs []agent.Message, maxMessages int) []agent.Message {
	if len(msgs) <= maxMessages {
		return msgs
	}
	for i := len(msgs) - maxMessages; i < len(msgs); i++ {
		if isUserTurnStart(&msgs[i]) {
			return msgs[i:]
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if isUserTurnStart(&msgs[i]) {
			return msgs[i:]
		}
	}
	return msgs
}

// isUserTurnStart reports whether m begins a user turn — a user message with
// text, as opposed to a user message carrying tool_results. "user" is the agent
// package's wire role value.
func isUserTurnStart(m *agent.Message) bool {
	return m.Role == "user" && strings.TrimSpace(m.Text) != ""
}

// callerIsAdmin resolves the caller's admin status off the base context (a
// client abort can't cancel the fail-closed check). Missing store → not admin.
func (h *Handler) callerIsAdmin(log *slog.Logger, teamID, userID string) bool {
	if h.cfg.AdminStore == nil {
		return false
	}
	gateCtx, cancel := context.WithTimeout(h.baseCtx, adminGateBudget)
	defer cancel()
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(gateCtx, teamID, userID)
	if err != nil {
		// Fail closed, but log for parity with the other CheckAdmin call sites
		// (requireAdminSync, the owner gate) so a systematic admin-check failure
		// — DDB throttling, a perms regression — is visible on the agent path
		// rather than silently denying admin features.
		log.Error("agent: admin check failed; treating caller as non-admin", "error", err, "team_id", teamID, "user_id", userID)
		return false
	}
	return isAdmin
}

// agentReplyText renders the channel reply for a turn result. A proposal is
// surfaced as a preview while conversation mode is read-only.
func agentReplyText(result *agent.Result) string {
	if result.Proposal != nil {
		// A blank summary would render as a dangling "• " bullet; fall back like
		// the blank-Reply guard below rather than post an empty preview.
		if strings.TrimSpace(result.Proposal.Summary) == "" {
			return agentErrorReply
		}
		// The preview posts as mrkdwn, and the summary is LLM-distilled — escape it
		// (consistent with the confirm card's fallback) so a prompt-injected masked
		// link can't surface. The Reply branch below is deliberately NOT escaped: it
		// is the agent's own answer, which is allowed light Slack formatting.
		return agentProposalPreviewPrefix + escapeMrkdwnText(result.Proposal.Summary)
	}
	if strings.TrimSpace(result.Reply) == "" {
		return agentErrorReply
	}
	return result.Reply
}

// postAgentReply delivers the reply in-thread, logging (not surfacing) failures.
// It derives its own context off h.baseCtx (see agentDeliveryBudget) rather than
// the turn ctx, so a turn that spent its deadline still delivers its reply.
func (h *Handler) postAgentReply(log *slog.Logger, env *slackEventEnvelope, threadTS, text string) {
	ctx, cancel := context.WithTimeout(h.baseCtx, agentDeliveryBudget)
	defer cancel()
	if err := h.cfg.PostMessage(ctx, env.TeamID, env.EnterpriseID, env.Event.Channel, threadTS, text); err != nil {
		log.Error("agent: post reply failed", "error", err)
	}
}
