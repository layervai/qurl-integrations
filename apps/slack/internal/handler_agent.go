package internal

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
)

// Slack Events API event types this handler reacts to.
const (
	slackEventTypeAppMention                    = "app_mention"
	slackEventTypeMessage                       = "message"
	slackEventTypeAssistantThreadStarted        = "assistant_thread_started"
	slackEventTypeAssistantThreadContextChanged = "assistant_thread_context_changed"
	slackEventTypeAppHomeOpened                 = "app_home_opened"
	slackChannelTypeChannel                     = "channel"
	slackChannelTypeGroup                       = "group"
	slackChannelTypeIM                          = "im"
	slackChannelTypeMPIM                        = "mpim"
	slackMessageSubtypeFileShare                = "file_share"
	slackMessageSubtypeThreadBroadcast          = "thread_broadcast"
)

// agentProposalPreviewPrefix prefixes a proposed-mutation reply while
// conversation mode is read-only (the confirm flow lands in a follow-up). The
// agent only ever proposes; it never executes, so a preview is the honest reply.
const agentProposalPreviewPrefix = "I can set that up, but applying changes from conversation mode isn't enabled yet. Here's what I'd do once it is:\n• "

// agentErrorReply is posted when a turn fails unexpectedly. Deliberately vague —
// internals never reach the channel.
const agentErrorReply = "Something went wrong handling that. Please try again, or use a `/qurl` command."

// agentHelpReply is the deterministic usage response for a literal `help` turn.
// Keep it independent of the LLM so Slack reviewers always get instructions,
// even when the model or its downstream tools are unavailable.
const agentHelpReply = "I can help with qURL operations in this Slack context:\n" +
	"• List accessible resources and aliases\n" +
	"• Check qURL usage or resolve a qURL token\n" +
	"• Propose access, protection, alias, and revoke changes for human approval\n\n" +
	"Try \"What can I access here?\" or \"What's our qURL usage?\""

// agentUnsupportedMediaReply makes the text-only boundary explicit instead of
// silently ignoring file-only messages or sending attachment captions to the LLM
// without the attachment. Files include Slack-hosted images and canvases.
//
// The claim is scoped to what agentEventHasUpload actually detects: things
// ATTACHED to the message. It leads with the rule that produces that behavior —
// this surface reads a message's text and nothing else — instead of naming
// canvases as a standalone capability gap. A canvas pasted as a LINK is ordinary
// message text and is not detected (see agentEventHasUpload), so copy that read
// as "canvases are refused" would describe a boundary this surface does not
// enforce. "I can only read a message's text" stays true in both shapes, and
// correctly predicts that a linked document's contents don't reach the agent
// either.
//
// It names the snippet case because Slack converts a long paste into an attached
// snippet, so a purely textual request lands here too. The paste's text is in the
// file, not in the event, so this surface still cannot read it — but the earlier
// "start a new text-only message" advice reproduced the snippet on the retry,
// leaving a paste-shaped request with no route at all. Presence detection cannot
// tell a snippet from a PDF (see agentEventHasUpload), so one string covers both
// and points at `/qurl`, which does not go through this surface.
// TODO(upstream-contract): asserts that Slack clients turn a long paste into a
// snippet rather than a plain message.
const agentUnsupportedMediaReply = "I can only read a message's text, so an attached file, image, or canvas doesn't reach me — and Slack turns a long paste into an attached snippet, so a big block of text lands here too.\nSend a shorter message (mentioning qURL again if you're in a channel), or use a `/qurl` command — run `/qurl help` for the list."

// agentAIPrivacyURL is the privacy notice for the Secure Access Agent's AI
// features. Surfaced in every AI-disclosure string below so users always have a
// route to how their messages are processed.
const agentAIPrivacyURL = "https://layerv.ai/privacy/"

// agentAIDisclosure is the Slack-Marketplace-required AI disclosure for the
// agent surface: it names the AI provider (Anthropic Claude), warns that AI can
// be wrong (review before approving), notes the paid-plan requirement for Slack
// AI apps, and links the privacy notice. Used as the pane's first-run intro and
// kept as one const so the App Home / pane copy can't drift on the load-bearing
// points (AI used + can be wrong + privacy link).
const agentAIDisclosure = "I use AI (Anthropic Claude) to interpret requests and can make mistakes — review any proposed action before approving. AI features require a paid Slack plan. Privacy: " + agentAIPrivacyURL

// agentAIDisclosureShort is the App Home context-block variant of
// agentAIDisclosure — the same load-bearing points (AI used + can be wrong +
// privacy link) in the tighter form a context block wants.
const agentAIDisclosureShort = "🤖 Uses AI (Anthropic Claude) and can make mistakes — review actions before approving. Privacy: " + agentAIPrivacyURL

// agentConfirmAIDisclosure is the small AI-provenance line on the proposed-action
// confirm card, reminding the approver the proposal came from the AI agent before
// they approve it.
const agentConfirmAIDisclosure = "🤖 Proposed by the AI agent — review before approving."

// agentLLMReplyDisclaimer is appended to ordinary free-text model replies and
// LLM-distilled proposal previews. Fixed errors and deterministic help must not be
// mislabeled; confirmation cards have their own provenance line. The privacy link
// stays in first-run/App Home disclosure instead of repeating on every reply. Keep
// this string invariant under the one-shot and stream-reconcile Markdown hardeners.
// Both delivery paths append the trusted constant only after hardening the reply,
// so malformed reply Markdown cannot absorb the footer into its pending state.
const agentLLMReplyDisclaimer = "\n\n_Generated by AI (Anthropic Claude). It may contain mistakes; review before acting._"

func agentLLMReplyWithDisclaimer(markdown string) string {
	return markdown + agentLLMReplyDisclaimer
}

func agentProposalPreview(summary string) string {
	return agentProposalPreviewPrefix + escapeMrkdwnText(summary) + agentLLMReplyDisclaimer
}

// agentTransientReply is posted when a turn fails for a likely-transient reason —
// the turn-budget deadline elapsed, or the context was canceled — as opposed to
// agentErrorReply's generic failure. Slack's agent-design guidance is to separate
// "temporarily unavailable, worth retrying" from a capability limit so the user
// knows a retry is worthwhile; a done turn ctx is our reliable signal for the
// former. Still leaks no internals.
const agentTransientReply = "That took longer than I could handle just now — please try again, or use a `/qurl` command."

// agentRateLimitedReply is posted when a turn is dropped for hitting the per-user or
// per-team turn-rate cap. Deliberately uniform across both limits (don't leak which
// cap, or its value) and points at the always-available slash commands. Phrased
// neutrally ("conversation mode is at its limit", not "you've reached…") so it
// doesn't wrongly blame an innocent member when it's the per-workspace cap that hit.
const agentRateLimitedReply = "Conversation mode is at its limit for now — give it a few minutes, or use a `/qurl` command in the meantime."

// agentInvalidProtectURLReply rejects explicit non-HTTPS protection requests
// before they reach the model. Keep the copy generic so an attacker-controlled
// target is never reflected into the channel.
const agentInvalidProtectURLReply = "I can only protect HTTPS URLs. Use a URL that starts with `https://`."

// agentInvalidAliasReply rejects invalid aliases without reflecting the
// attacker-controlled token into the channel.
const agentInvalidAliasReply = "That alias isn't valid. Use lowercase letters, numbers, and dashes only."

// agentTurnRateWindow is the fixed window for the per-user / per-team turn counters.
// The env limits are expressed per hour, so the window is one hour.
const agentTurnRateWindow = time.Hour

// agentTurnRateCounterFailOpenMsg is an infra-observed contract: the CloudWatch
// metric filter added in qurl-integrations-infra#1065 keys on this exact slog
// msg value for the fail-open path introduced by qurl-integrations-infra#1055.
// TODO(upstream-contract): keep this value in lockstep with that infra filter.
const agentTurnRateCounterFailOpenMsg = "agent: turn-rate counter failed; allowing turn (fail-open)"

// agentAckReaction is the glanceable "working on it" emoji the agent adds to the
// triggering message while a turn runs (reactions.add), then removes when it ends.
const agentAckReaction = "eyes"

// defaultAgentAckTimeout bounds each cosmetic working-on-it round-trip. Channel
// turns use async reactions.add plus deferred reactions.remove; exclusive pane
// mode uses assistant-pane setStatus; default pre-pane mode still attempts both
// the reaction fallback and setStatus for im turns. It's deliberately tight and
// decoupled from the 4s chat.postMessage budget: a "working on it" ack that
// hasn't landed in ~2s is already too late to feel responsive, so giving up (no
// 👀 — the deferred remove then hits no_reaction → benign) beats delaying the
// turn it's meant to make feel responsive.
// On a very fast turn with a still-running add, deferred clear can spend one budget
// joining the add before spending a fresh budget on remove; both happen after reply
// delivery, so the user-visible turn stays unblocked, but the agent worker slot stays
// occupied until the deferred clear returns. In that slow-reaction/fast-reply case,
// the reaction may briefly appear after the reply before clear removes it; preserving
// add-before-remove ordering is the no-stale-reaction priority.
const defaultAgentAckTimeout = 2 * time.Second

// agentThinkingStatus is the native assistant-pane status text shown while a DM (pane)
// turn runs (assistant.threads.setStatus); Slack renders it as "<app> is thinking…".
// See setAgentThinkingStatus.
const agentThinkingStatus = "is thinking…"

// slackEventEnvelope is the Events API outer payload. Only the fields the agent
// surface needs are modeled.
type slackEventEnvelope struct {
	Type           string                    `json:"type"`
	Challenge      string                    `json:"challenge"`
	TeamID         string                    `json:"team_id"`
	EnterpriseID   string                    `json:"enterprise_id"`
	APIAppID       string                    `json:"api_app_id"`
	EventID        string                    `json:"event_id"`
	EventTime      int64                     `json:"event_time,omitempty"`
	Authorizations []slackEventAuthorization `json:"authorizations,omitempty"`
	Event          slackInnerEvent           `json:"event"`
}

type slackEventAuthorization struct {
	EnterpriseID        string `json:"enterprise_id,omitempty"`
	TeamID              string `json:"team_id,omitempty"`
	UserID              string `json:"user_id,omitempty"`
	IsEnterpriseInstall bool   `json:"is_enterprise_install,omitempty"`
}

// slackInnerEvent is the inner `event` object for app_mention / message events,
// plus the assistant_thread object on the container events (assistant_thread_started
// and assistant_thread_context_changed).
// slackEventFiles models an event's files array for PRESENCE ONLY: qURL never
// fetches a file or reads inside one while conversation mode is text-only, so
// only "did this carry an attachment" and "how many" survive the decode.
//
// It decodes tolerantly on purpose, at two levels, because handleEvent treats ANY
// envelope decode error as "log at Debug, ack 200, dispatch nothing" — so a single
// shape surprise anywhere in `files` would silently drop the whole event, taking
// ordinary text turns and lifecycle/uninstall routing with it. That is the exact
// silent disappearance agentUnsupportedMediaReply exists to prevent.
//
//   - ELEMENT shape: a bare []struct{} fails on a non-object element, so entries
//     are decoded as json.RawMessage, which accepts any JSON value. (This is the
//     standing answer to "why not []struct{}" — it is not about the bytes.)
//   - FIELD shape: even []json.RawMessage returns an UnmarshalTypeError if `files`
//     itself is not an array, which is why this type parses by shape rather than
//     letting the decoder decide.
//
// An unrecognized shape therefore degrades to "an attachment we cannot count"
// rather than taking the message down with it.
type slackEventFiles struct {
	// count is how many entries Slack sent, or 0 when files arrived in a shape this
	// app does not recognize. Never an inventory — the entries themselves are dropped.
	count int
	// present is whether the event carries an attachment at all. True for a non-empty
	// array AND for any unrecognized non-null shape, so detection fails toward
	// refusing rather than toward answering past an attachment.
	present bool
}

// MarshalJSON always fails, making this type decode-only by construction.
//
// The entries are discarded at decode time, so there is nothing faithful left to
// emit — and because count/present are unexported, the DEFAULT marshaling would
// emit `{}`, which this type's own UnmarshalJSON reads back as an uncountable
// attachment. A round-tripped envelope would therefore refuse EVERY turn,
// including purely textual ones, from a value that never carried a file. That is
// silent and would be brutal to diagnose, so it is an error at the point of the
// mistake instead. No marshal site exists today (verified); this keeps it that way.
func (slackEventFiles) MarshalJSON() ([]byte, error) {
	return nil, errors.New("slackEventFiles is decode-only: re-marshaling an event would round-trip files into an attachment that was never there")
}

// UnmarshalJSON implements json.Unmarshaler. It classifies by SHAPE first so that
// an unexpected files value is a recognized outcome rather than a decode failure —
// see the type doc for why failing here would be so costly.
//
// encoding/json calls this only when the key is present, hands over a complete and
// syntactically valid JSON value, and delivers an explicit null rather than
// skipping it. The array decode below is therefore reached only for a value that
// already begins with '[' — a valid array, whose elements always decode into
// json.RawMessage — so its error return is unreachable in practice and kept only
// because silently discarding an error would be worse than a branch never taken.
//
// The value also arrives unpadded — encoding/json strips the whitespace around it
// before calling here — so the "null" comparison below can be byte-for-byte. That
// guarantee is load-bearing (a padded "null " would classify as an attachment and
// refuse a clean text turn) and is pinned by TestSlackEventFilesNestedDecodeIsUnpadded
// rather than assumed. The length check is panic insurance, not whitespace handling.
func (f *slackEventFiles) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || b[0] != '[' {
		// null means no attachment. Any other non-array shape is an attachment we
		// cannot count: presence stays true so the turn is refused rather than answered
		// past a file, and count stays 0.
		f.present = string(b) != "null"
		return nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(b, &entries); err != nil {
		return err
	}
	f.count = len(entries)
	f.present = len(entries) > 0
	return nil
}

type slackInnerEvent struct {
	Type        string `json:"type"`
	User        string `json:"user"`
	UserTeam    string `json:"user_team,omitempty"`
	SourceTeam  string `json:"source_team,omitempty"`
	BotID       string `json:"bot_id"`
	Subtype     string `json:"subtype"`
	Text        string `json:"text"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	// Files is decoded but never read into — only its presence and count are
	// consulted — while conversation mode remains text-only. See slackEventFiles for
	// why it decodes tolerantly instead of strictly. No omitempty, unlike the
	// pointer and string fields around it: encoding/json never treats a non-pointer
	// struct as empty, so the tag would claim an omission that cannot happen.
	Files slackEventFiles `json:"files"`
	// Tab is the App Home tab a user opened ("home" / "messages") on an
	// app_home_opened event; empty on every other event type.
	Tab string `json:"tab,omitempty"`
	// AssistantThread is set on the container events (assistant_thread_started and
	// assistant_thread_context_changed), which carry a nested object, not the flat fields.
	AssistantThread *assistantThread    `json:"assistant_thread,omitempty"`
	Tokens          *slackRevokedTokens `json:"tokens,omitempty"`
}

type slackRevokedTokens struct {
	Bot   []string `json:"bot,omitempty"`
	OAuth []string `json:"oauth,omitempty"`
}

// assistantThread is the assistant_thread object on a container event: the assistant DM
// channel + thread the user opened, plus the context (the channel they were viewing).
// Context.ChannelID is persisted by the container handlers for a later turn to scope its
// reads to.
type assistantThread struct {
	UserID    string                 `json:"user_id"`
	ChannelID string                 `json:"channel_id"`
	ThreadTS  string                 `json:"thread_ts"`
	Context   assistantThreadContext `json:"context"`
}

type assistantThreadContext struct {
	ChannelID    string `json:"channel_id"`
	TeamID       string `json:"team_id"`
	EnterpriseID string `json:"enterprise_id"`
}

// agentEnabled reports whether conversation mode is fully wired and not killed.
func (h *Handler) agentEnabled() bool {
	return !h.cfg.AgentDisabled &&
		h.cfg.AgentLLM != nil &&
		h.cfg.AgentStore != nil &&
		h.cfg.PostMessage != nil
}

// agentChannelFollowupsEnabled reports whether the agent answers non-@mention thread
// replies in channel threads it already joined (see shouldDispatchAgentEvent). Gated
// on top of agentEnabled and dark by default: turning it on means subscribing to
// message.channels/groups (channels:history/groups:history), so the bot then RECEIVES
// every message in channels it's a member of — a data-handling expansion that must be
// reviewed and re-OAuth'd before enabling. Until then channels stay @mention-per-turn.
func (h *Handler) agentChannelFollowupsEnabled() bool {
	return h.agentEnabled() && h.cfg.AgentChannelFollowups
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
		log.Warn(agentTurnRateCounterFailOpenMsg, "scope", scope, "team_id", teamID, "error", err)
		return false
	}
	if count > int64(limit) {
		log.Info("agent: turn rate limit reached", "scope", scope, "team_id", teamID, "count", count, "limit", limit)
		return true
	}
	return false
}

func (h *Handler) effectiveAgentAckTimeout() time.Duration {
	if h.agentAckTimeout > 0 {
		return h.agentAckTimeout
	}
	// NewHandler normalizes this for production. This fallback, like the nil-base
	// fallback in agentAckContext, only protects package-local bare Handler literals in
	// focused unit tests that call ack helpers directly.
	return defaultAgentAckTimeout
}

func (h *Handler) agentAckContext() (context.Context, context.CancelFunc) {
	baseCtx := h.baseCtx
	if baseCtx == nil {
		// NewHandler normalizes this for production; this is the matching bare-test-literal
		// fallback for direct ack-helper tests.
		baseCtx = context.Background()
	}
	return context.WithTimeout(baseCtx, h.effectiveAgentAckTimeout())
}

type agentAckAdd struct {
	done   <-chan struct{}
	cancel context.CancelFunc
}

// addAgentAck reacts agentAckReaction (👀) on the triggering message to acknowledge
// the turn is being worked. Best-effort: a failed ack never fails the turn, and a nil
// Reactions seam is a no-op (the ack is simply absent). The add runs asynchronously so
// Slack reaction latency stays off the LLM turn's critical path. The returned handle
// is load-bearing: clearAgentAck waits for it before removing so reactions.remove can
// never race ahead of an in-flight reactions.add and strand the 👀.
func (h *Handler) addAgentAck(log *slog.Logger, env *slackEventEnvelope) agentAckAdd {
	if h.cfg.Reactions == nil {
		return agentAckAdd{}
	}
	done := make(chan struct{})
	ctx, cancel := h.agentAckContext()
	// Nested Add is safe because the caller is processAgentEvent, already running
	// inside runOnPool's wg slot; the counter cannot hit zero between this Add and
	// the goroutine start.
	// The goroutine is wg-tracked, so shutdown drain relies on ReactionPort.Add
	// honoring ctx just like the other Slack seams wired through Handler.
	h.asyncStart()
	go func() {
		defer h.asyncDone()
		defer close(done)
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic in agent ack add", "recover", rec, "stack", string(debug.Stack()))
			}
		}()

		defer cancel()
		if err := h.cfg.Reactions.Add(ctx, env.TeamID, env.EnterpriseID, env.Event.Channel, env.Event.TS, agentAckReaction); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				log.Debug("agent: ack reaction add canceled or timed out (best-effort)", "error", err)
				return
			}
			log.Warn("agent: ack reaction add failed (best-effort)", "error", err)
		}
	}()
	return agentAckAdd{done: done, cancel: cancel}
}

// clearAgentAck removes the working-on-it reaction when the turn ends — deferred so it
// runs on EVERY exit (reply posted, error, panic). Best-effort and nil-safe like
// addAgentAck. It waits on the add handle before removing so even a very fast turn cannot
// remove before the async add lands. The wait is bounded too: if a future seam ignores
// its add context, or the add goroutine starts late enough that the join budget wins,
// clear abandons the best-effort remove rather than recreating the remove-before-add
// race. On abandon, clear also cancels the in-flight add context: if the ReactionPort
// honors ctx, no late 👀 lands; if it ignores ctx, the operator-facing Warn still says
// the ack may remain. The remove uses a FRESH ctx off baseCtx: by defer time the turn
// ctx is spent (agentTurnTimeout elapsed, or shutdown), so removing on it would fail
// instantly and strand the 👀 — the same spent-ctx lesson as postAgentReply. It gets
// a full fresh ack budget rather than the leftover join budget so a slow add that
// finally settled does not starve the cleanup call.
func (h *Handler) clearAgentAck(log *slog.Logger, env *slackEventEnvelope, add agentAckAdd) {
	if h.cfg.Reactions == nil || add.done == nil {
		return
	}
	// Non-blocking peek first: on a turn where the async add already landed, skip the
	// joinCtx allocation entirely (and avoid the deterministic-shutdown nuance below
	// where add.done and joinCtx.Done() can be ready simultaneously after a successful
	// add -- Go's select would pick uniformly at random and spuriously log).
	select {
	case <-add.done:
	default:
		joinCtx, joinCancel := h.agentAckContext()
		defer joinCancel()
		select {
		case <-add.done:
		case <-joinCtx.Done():
			if add.cancel != nil {
				add.cancel()
			}
			if h.baseCtx != nil && h.baseCtx.Err() != nil {
				log.Debug("agent: ack reaction add unfinished during shutdown; skipping remove")
			} else {
				log.Warn("agent: ack reaction add still in flight; skipping remove to avoid racing add; ack may remain")
			}
			return
		}
	}

	ctx, cancel := h.agentAckContext()
	defer cancel()
	if err := h.cfg.Reactions.Remove(ctx, env.TeamID, env.EnterpriseID, env.Event.Channel, env.Event.TS, agentAckReaction); err != nil {
		if h.baseCtx != nil && h.baseCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			log.Debug("agent: ack reaction remove canceled during shutdown; ack may remain", "error", err)
			return
		}
		log.Warn("agent: ack reaction remove failed (best-effort)", "error", err)
	}
}

// setAgentThinkingStatus shows the native assistant "thinking…" status in the pane for a
// DM (message.im) turn — the pane-native counterpart to addAgentAck's 👀 reaction.
// Best-effort behind the AssistantThreads seam (nil = no-op) and ONLY for im turns:
// app_mention is a channel, not an assistant thread, so setStatus has nothing to scope
// to. Set SYNCHRONOUSLY on the live turn ctx before the LLM call so it's visible while
// the turn runs, under its own agentAckTimeout cap. The old #693 additive pre-LLM
// concern no longer stacks with reaction add (now async); setStatus remains the one
// synchronous working-on-it seam here. Slack normally auto-clears the status when the
// agent posts its reply, but native streamed replies can leave it behind, so every
// successful set registers an explicit deferred clear. Both calls MUST use the reply
// thread — all three derive from agentEventRootTS(&env.Event); keep them coupled.
//
// Post-enablement exclusive mode treats a pane setStatus failure as evidence the
// native status path is broken (scope, rate limit, malformed thread, etc.), so it logs
// at Warn while keeping the turn best-effort. Pre-enable additive mode still logs at
// Debug because setStatus may fail on every ordinary DM until the pane is live and the
// reaction remains the working cue.
func (h *Handler) setAgentThinkingStatus(ctx context.Context, log *slog.Logger, env *slackEventEnvelope) bool {
	if h.cfg.AssistantThreads == nil || env.Event.ChannelType != slackChannelTypeIM {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, h.effectiveAgentAckTimeout())
	defer cancel()
	if err := h.cfg.AssistantThreads.SetStatus(ctx, env.TeamID, env.EnterpriseID, env.Event.Channel, agentEventRootTS(&env.Event), agentThinkingStatus); err != nil {
		if !h.cfg.AgentSurfaceExclusiveAcks {
			log.Debug("agent: set assistant pane status failed (best-effort)", "error", err)
			return false
		}
		log.Warn("agent: set assistant pane status failed in exclusive mode", "error", err)
		return false
	}
	return true
}

// clearAgentThinkingStatus explicitly clears a pane status on every turn exit. The
// cleanup is best-effort and uses a fresh bounded context because the turn context may
// already be spent by the time this deferred call runs.
func (h *Handler) clearAgentThinkingStatus(log *slog.Logger, env *slackEventEnvelope) {
	ctx, cancel := h.agentAckContext()
	defer cancel()
	if err := h.cfg.AssistantThreads.SetStatus(ctx, env.TeamID, env.EnterpriseID, env.Event.Channel, agentEventRootTS(&env.Event), ""); err != nil {
		if h.baseCtx != nil && h.baseCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			log.Debug("agent: assistant pane status clear canceled during shutdown", "error", err)
			return
		}
		log.Warn("agent: clear assistant pane status failed (best-effort)", "error", err)
	}
}

// startAgentReactionAck marks an admitted turn with the reaction indicator when
// this surface still uses one. The add is best-effort; cleanup is also
// nil-/no-reaction-safe.
func (h *Handler) startAgentReactionAck(log *slog.Logger, env *slackEventEnvelope) agentAckAdd {
	if env.Event.ChannelType == slackChannelTypeIM && h.cfg.AgentSurfaceExclusiveAcks {
		return agentAckAdd{}
	}
	return h.addAgentAck(log, env)
}

// agentOperatingChannel is the channel the agent OPERATES on for this turn (scopes
// every channel-scoped read via TurnContext.ChannelID). For an @mention it's the
// channel the mention came from; for an assistant-pane (DM) turn it's the channel the
// user opened the pane FROM — but only when they're a confirmed member of it
// (paneContextChannel), else the bare DM. The reply still posts to env.Event.Channel;
// only the operating channel changes. The mutation path is unaffected — it anchors to
// env.Event.Channel / the click's channel, never this — so the override widens reads
// only, never actions.
func (h *Handler) agentOperatingChannel(ctx context.Context, log *slog.Logger, env *slackEventEnvelope) string {
	if env.Event.ChannelType == slackChannelTypeIM {
		if c := h.paneContextChannel(ctx, log, env); c != "" {
			return c
		}
	}
	return env.Event.Channel
}

// handleAgentEvent decides whether an event_callback should drive a
// conversation turn and, if so, dispatches it to the async pool. The caller
// (handleEvent) always acks 200 regardless — Slack must not retry — so this only
// schedules work; it never writes the response.
func (h *Handler) handleAgentEvent(env *slackEventEnvelope) {
	if !h.agentEnabled() {
		return
	}
	// Assistants-container events are additive (no conversation turn) — handle them
	// before the turn-dispatch filter, which only admits app_mention/message. Both carry
	// the pane's context.channel_id (the channel the user opened it from); the started
	// event also sets the first-run title + prompts.
	switch env.Event.Type {
	case slackEventTypeAssistantThreadStarted:
		h.handleAssistantThreadStarted(env)
		return
	case slackEventTypeAssistantThreadContextChanged:
		h.handleAssistantThreadContextChanged(env)
		return
	case slackEventTypeAppHomeOpened:
		// App Home tab opened — publish the viewer's own agent-action review surface.
		// Additive (no conversation turn), like the container events above.
		h.handleAppHomeOpened(env)
		return
	}
	if !shouldDispatchAgentEvent(env, h.agentChannelFollowupsEnabled()) {
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
	turn := func(ctx context.Context, log *slog.Logger) {
		h.processAgentEvent(ctx, log, &envCopy)
	}
	// Channel thread follow-ups first pass through a short SEPARATE gate pool before
	// admitted turns move to h.followupSem. That keeps a message.channels firehose from
	// spending long-running turn slots on "is this our thread?" DDB reads, while both
	// stages stay isolated from the main pool that @mention/DM/slash/interaction work
	// shares (#712/#719).
	if isAgentChannelFollowup(&envCopy.Event) {
		if !h.runAgentFollowupPipeline(log, &envCopy) {
			log.Warn("agent: follow-up gate pool saturated — dropping channel reply")
		}
		return
	}
	// Main pool, via runOnPool directly (not startAsyncWorkerWithTimeout) so this path
	// logs its drop exactly once — symmetric with the follow-up branch above. (The
	// startAsyncWorkerWithTimeout wrapper keeps its own saturation log for runAsync/slash
	// callers, which have no caller-side log.)
	if !h.runOnPool(h.sem, log, agentTurnTimeout, turn) {
		log.Warn("agent: async pool saturated — dropping event")
	}
}

// runAgentFollowupPipeline runs the channel-follow-up admission gate and, only when the
// thread is one the agent already joined, the full follow-up turn. It is intentionally a
// single wg-tracked goroutine: wg.Add happens on the request goroutine before spawn, while
// the short gate semaphore is released before the long follow-up turn starts.
func (h *Handler) runAgentFollowupPipeline(log *slog.Logger, env *slackEventEnvelope) bool {
	select {
	case h.followupGateSem <- struct{}{}:
	default:
		return false
	}

	h.asyncStart()
	go func() {
		gateHeld := true
		defer h.asyncDone()
		defer func() {
			if gateHeld {
				<-h.followupGateSem
			}
		}()
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic in agent follow-up pipeline", "recover", rec, "stack", string(debug.Stack()))
			}
		}()

		partition := agentEventPartition(env)
		admitted, pre := func() (bool, *loadedHistory) {
			gateCtx, cancelGate := context.WithTimeout(h.baseCtx, agentFollowupGateTimeout)
			defer cancelGate()
			return h.admitAgentChannelFollowup(gateCtx, log, env, partition)
		}()

		<-h.followupGateSem
		gateHeld = false
		if !admitted {
			return
		}

		select {
		case h.followupSem <- struct{}{}:
		default:
			log.Warn("agent: follow-up turn pool saturated — dropping admitted channel reply")
			return
		}
		defer func() { <-h.followupSem }()

		turnCtx, cancelTurn := context.WithTimeout(h.baseCtx, agentTurnTimeout)
		defer cancelTurn()
		h.processAdmittedAgentEvent(turnCtx, log, env, partition, pre)
	}()
	return true
}

// agentTurnTimeout bounds one conversation turn. A turn makes up to
// defaultMaxIterations Anthropic round-trips plus channel-scoped reads, so it
// needs more than the 25s slash-command budget — 25s could cancel a legitimate
// multi-tool-call turn mid-flight and surface a spurious error to the user. The
// iteration cap and per-user rate limiting bound how long a slot is held.
//
// It was 90s while overrunning the budget meant losing the turn: the only way to
// protect a slow-but-legitimate turn was to wait longer. The agent package now
// RATIONS this deadline (see agent.finalAnswerReserve) and finalizes into a real
// answer instead of failing, which inverts the tradeoff — a longer budget no
// longer buys safety, it only buys a longer wait before the same graceful answer.
//
// So size it for the user instead. At the reserve's 15s tail, a turn finalizes by
// 60s and delivers a few seconds after, leaving ample margin inside the 90s window
// the misuse suite scores a reply against (and well inside what a member in a
// Slack thread will tolerate). Gathering still gets 45s — six round-trips at
// typical latency — so the set of turns that converge on their own is unchanged.
const agentTurnTimeout = 60 * time.Second

// agentFollowupGateTimeout bounds the pre-turn Slack read for a channel follow-up
// admission decision. A slow gate fails closed silently because the message may be
// unrelated channel chatter; admitted turns get the larger agentTurnTimeout budget.
const agentFollowupGateTimeout = 5 * time.Second

// agentDeliveryBudget bounds each post-turn delivery step. The reply post derives
// its context with this budget off h.baseCtx, never the turn ctx. By delivery
// time the turn ctx may be spent or canceled (the turn hit agentTurnTimeout), and
// a PostMessage on a dead ctx fails instantly — yet the dedupe write is already
// committed and Slack won't retry, so the user would get silence. Deriving off baseCtx (like
// callerIsAdmin) lets delivery outlive the turn deadline; bounding it (not
// baseCtx directly) keeps a wedged Slack/DDB call from pinning an async-pool slot
// and lets SIGTERM still drain in-flight delivery.
const agentDeliveryBudget = 15 * time.Second

// agentEventHasUpload reports whether this event carries an attachment. The
// file_share subtype is evidence on its own, so an upload cannot fall through to
// silence when the files array is absent — and the text-only limitation stays
// correct when it does.
//
// Detection is deliberately presence-only AND deliberately attachment-only. A
// Slack canvas or file pasted as a LINK arrives as ordinary message text with an
// unfurl: no files entry, no file_share subtype. It is not detected, and
// agentUnsupportedMediaReply is worded so it does not claim otherwise. Matching
// Slack file permalinks in the text was considered and rejected: this branch wins
// the turn at its call site and returns before the text is classified at all, so
// any message merely CONTAINING such a URL would stop being answered — including
// "protect https://…/docs/… as $handbook", a legitimate propose_protect_url
// request against a raw https:// endpoint (a capability prompt_test.go pins).
// Losing that is the worse failure, and the model is separately told never to
// describe a page it has not fetched through the confirmed inspect path.
// slackInnerEvent likewise does not decode `attachments` (the unfurl block), for
// the same reason: an unfurl is evidence about a link, not about an attachment.
// TODO(upstream-contract): the two signals back each other up, so this relies on
// Slack sending AT LEAST ONE of them per upload — not on file_share being
// universal, and not on the files array always arriving.
func agentEventHasUpload(e *slackInnerEvent) bool {
	return e.Files.present || e.Subtype == slackMessageSubtypeFileShare
}

// agentAdmitsSubtype reports whether this surface answers a message carrying this
// subtype. It is a POLICY whitelist, not a taxonomy: thread_broadcast and
// me_message are perfectly deliberate human messages and still return false here —
// thread_broadcast because it is a channel-only exception handled at its call site,
// me_message because nothing has asked for it. Only file_share joins the empty
// subtype, because an upload is a turn this surface answers (with the text-only
// limitation) rather than ignores. Everything else — edits, joins, bot posts — is
// system noise from here.
func agentAdmitsSubtype(subtype string) bool {
	return subtype == "" || subtype == slackMessageSubtypeFileShare
}

// shouldDispatchAgentEvent filters out everything that isn't a human asking the
// agent something: non-mention/DM events, bot and system/edited messages (the
// self-loop guard), authorless events, top-level channel messages, and events
// with neither text nor an attached file. File-only deliberate messages are
// admitted so processAgentEventWithAdmission can explain the text-only boundary.
//
// When channelFollowupsEnabled is true, a channel message that is a thread REPLY is
// also admitted — so a follow-up in a thread the agent is already in continues the
// conversation without a re-@mention. Slack's thread_broadcast subtype follows that
// same path when a user also sends the thread reply to the channel.
// runAgentFollowupPipeline then confirms it's the agent's OWN thread (it has saved
// history) before answering; a top-level channel message is never admitted, so we never
// respond to un-addressed channel chatter.
func shouldDispatchAgentEvent(env *slackEventEnvelope, channelFollowupsEnabled bool) bool {
	e := &env.Event
	// Drop bot posts and the agent's own messages before considering event shape.
	if e.BotID != "" || e.User == "" {
		return false
	}
	switch e.Type {
	case slackEventTypeAppMention:
		// Channel @-mention — always a deliberate address.
		// TODO(upstream-contract): app_mention is known to carry a subtype in the wild,
		// so a stamped mention-with-upload must not fall back into silence here.
		if !agentAdmitsSubtype(e.Subtype) {
			return false
		}
	case slackEventTypeMessage:
		if e.ChannelType == slackChannelTypeIM {
			if !agentAdmitsSubtype(e.Subtype) {
				return false
			}
		} else {
			if !agentAdmitsSubtype(e.Subtype) && e.Subtype != slackMessageSubtypeThreadBroadcast {
				return false
			}
			// A channel message reaches the follow-up pipeline only when channel
			// follow-ups are enabled AND it's a thread reply. The pipeline then checks
			// whether this is already an agent thread, using store access.
			//
			// An upload is deliberately not special-cased here. With follow-ups off it
			// is dropped like any other follow-up, which reads oddly against "never
			// disappear silently" — but the limitation reply answers turns that ADDRESS
			// the agent, and a file dropped into a channel mid-conversation is not one.
			// Replying to it would make the bot interject on people talking to each
			// other, which is the louder failure.
			//
			// Two costs come with admitting it rather than dropping it at this filter,
			// both inert while AgentChannelFollowups is off:
			//   - a thread upload now pays a followupGateSem slot and a
			//     conversations.replies read before dedupe, even for threads the agent
			//     never joined — and that pool's saturation path drops legitimate text
			//     follow-ups. An upload can only ever produce the fixed limitation, so
			//     it never needs history; short-circuiting it before the gate read is
			//     the fix if that flag ships.
			//   - answering an upload marks the thread as one the agent joined (see
			//     loadAgentThreadHistory), so later replies there reach the model. That
			//     mechanism predates uploads — help and the invalid-alias replies do it
			//     too — but an upload is a new way to trigger it.
			if !channelFollowupsEnabled || e.ThreadTS == "" {
				return false
			}
		}
	default:
		return false
	}
	return agentEventHasUpload(e) || strings.TrimSpace(stripBotMention(e.Text)) != ""
}

// isAgentChannelFollowup reports whether this event is a non-@mention reply in a
// channel thread (vs an app_mention or a DM). When shouldDispatchAgentEvent admits a
// channel message it is, by construction, a thread reply — so the follow-up pipeline
// uses this to apply the extra "must be the agent's own thread" gate that DMs and
// @mentions skip.
func isAgentChannelFollowup(e *slackInnerEvent) bool {
	return e.Type == slackEventTypeMessage && e.ChannelType != slackChannelTypeIM && e.ThreadTS != ""
}

// loadedHistory carries a thread's zero-copy Slack transcript from the channel-
// follow-up gate to the turn, so the accepted path calls conversations.replies once.
// A nil value means the direct @mention/DM path has not loaded Slack history yet.
type loadedHistory struct {
	history []agent.Message
}

// agentHistoryWindow preserves the previous 30-minute conversation-continuity
// window without persisting Slack content. Each turn pulls that window directly
// from Slack and keeps only the most recent completed exchanges in memory.
const agentHistoryWindow = 30 * time.Minute

// maxAgentHistoryMessages bounds model context reconstructed from Slack. At two
// visible messages per ordinary exchange, 40 keeps roughly the last 20 exchanges.
const maxAgentHistoryMessages = 40

// admitAgentChannelFollowup performs the short pre-turn checks for a channel follow-up:
// workspace toggle plus "is this already an agent thread?" transcript lookup. Accepted
// replies carry the loaded transcript forward so the turn does not repeat the read.
func (h *Handler) admitAgentChannelFollowup(ctx context.Context, log *slog.Logger, env *slackEventEnvelope, partition string) (admitted bool, pre *loadedHistory) {
	// Per-workspace toggle stays before the transcript gate so a disabled workspace
	// consumes no dedupe marker and performs no agent turn. This gate runs off the
	// request path, so the extra read does not threaten Slack's ack deadline.
	if !h.workspaceAgentEnabled(ctx, log, env.TeamID) {
		log.Info("agent: conversation mode disabled for this workspace; ignoring channel follow-up")
		return false, nil
	}
	dropped, pre := h.agentChannelFollowupDropped(ctx, log, env, partition)
	return !dropped, pre
}

// agentChannelFollowupDropped reports whether this event is a channel-thread reply
// the agent should not answer. It pulls the live Slack thread and admits the reply
// only when that thread already contains this app's own response. Called before
// dedupe/ack so unrelated channel chatter stays silent and consumes no marker.
func (h *Handler) agentChannelFollowupDropped(ctx context.Context, log *slog.Logger, env *slackEventEnvelope, _ string) (dropped bool, pre *loadedHistory) {
	if !isAgentChannelFollowup(&env.Event) {
		return false, nil
	}
	history, joined, err := h.loadAgentThreadHistory(ctx, env)
	if err != nil {
		log.Error("agent: thread-continuity lookup failed; dropping channel reply", "error", err)
		return true, nil
	}
	if !joined {
		log.Debug("agent: channel reply outside an agent thread; ignoring")
		return true, nil
	}
	return false, &loadedHistory{history: history}
}

// resolveTurnHistory returns live Slack context for this turn. A direct turn with
// no configured history seam safely starts single-turn; production always wires
// the seam. On a Slack read error, the already-deduped deliberate request gets a
// generic reply rather than silence.
func (h *Handler) resolveTurnHistory(ctx context.Context, log *slog.Logger, env *slackEventEnvelope, pre *loadedHistory) (history []agent.Message, ok bool) {
	if pre != nil {
		return pre.history, true
	}
	if h.cfg.AgentThreadHistory == nil {
		return nil, true
	}
	history, _, err := h.loadAgentThreadHistory(ctx, env)
	if err != nil {
		log.Error("agent: live thread history lookup failed", "error", err)
		h.postAgentReply(log, env, agentEventRootTS(&env.Event), agentErrorReply)
		return nil, false
	}
	return history, true
}

// loadAgentThreadHistory reconstructs completed user/agent exchanges directly
// from Slack. It never writes message content to LayerV storage. Messages from
// other apps are excluded, the current inbound turn is excluded (Agent.Run adds
// it), and any incomplete tail after the last qURL response is dropped so the
// model always receives completed prior exchanges.
func (h *Handler) loadAgentThreadHistory(ctx context.Context, env *slackEventEnvelope) (history []agent.Message, joined bool, err error) {
	if h.cfg.AgentThreadHistory == nil {
		return nil, false, nil
	}
	raw, err := h.cfg.AgentThreadHistory(
		ctx,
		env.TeamID,
		env.EnterpriseID,
		env.Event.Channel,
		agentEventRootTS(&env.Event),
		agentHistoryOldestTS(env.Event.TS),
	)
	if err != nil {
		return nil, false, err
	}

	botUsers := make(map[string]struct{}, len(env.Authorizations))
	for _, authz := range env.Authorizations {
		if authz.UserID != "" {
			botUsers[authz.UserID] = struct{}{}
		}
	}

	visible := make([]agent.Message, 0, len(raw))
	lastAssistant := -1
	for _, msg := range raw {
		if msg.TS == env.Event.TS {
			continue
		}
		_, ownUser := botUsers[msg.UserID]
		ownReply := (msg.AppID != "" && msg.AppID == env.APIAppID) ||
			(msg.BotID != "" && ownUser)
		if ownReply {
			// A block-only qURL response still proves this is an agent thread
			// even when Slack supplies no top-level text to rebuild as context.
			joined = true
		}
		text := strings.TrimSpace(msg.Text)
		if text == "" {
			continue
		}
		switch {
		case ownReply:
			visible = appendVisibleAgentMessage(visible, "assistant", text)
			lastAssistant = len(visible) - 1
		case msg.BotID == "" && msg.UserID != "":
			text = stripBotMention(text)
			if text != "" {
				visible = appendVisibleAgentMessage(visible, "user", text)
			}
		}
	}
	if lastAssistant < 0 {
		return nil, joined, nil
	}
	visible = visible[:lastAssistant+1]
	if len(visible) > maxAgentHistoryMessages {
		visible = visible[len(visible)-maxAgentHistoryMessages:]
	}
	for len(visible) > 0 && visible[0].Role != "user" {
		visible = visible[1:]
	}
	return visible, joined, nil
}

func appendVisibleAgentMessage(history []agent.Message, role, text string) []agent.Message {
	if n := len(history); n > 0 && history[n-1].Role == role {
		// Slack threads can contain adjacent messages from different people.
		// Agent context is intentionally role-based and does not retain user
		// attribution, so adjacent human messages become one user turn.
		history[n-1].Text += "\n" + text
		return history
	}
	return append(history, agent.Message{Role: role, Text: text})
}

func agentHistoryOldestTS(currentTS string) string {
	seconds, _, ok := strings.Cut(currentTS, ".")
	if !ok {
		seconds = currentTS
	}
	unixSeconds, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		// Signed Slack events carry valid timestamps. Preserve the time-bound
		// invariant if a malformed value still reaches this seam.
		unixSeconds = time.Now().Unix()
	}
	oldest := unixSeconds - int64(agentHistoryWindow/time.Second)
	if oldest < 0 {
		oldest = 0
	}
	return strconv.FormatInt(oldest, 10) + ".000000"
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

// agentHasExplicitNonHTTPSProtectURL recognizes the direct conversation form
// "Protect <target> ..." when the target declares a non-HTTPS URI scheme. It is
// intentionally narrow: aliases, scheme-less targets, and explanatory prose
// still go through the agent, while values such as javascript: and http: are
// rejected deterministically before any LLM call.
func agentHasExplicitNonHTTPSProtectURL(message string) bool {
	fields := strings.Fields(message)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "protect") {
		return false
	}
	targetText := unwrapSlackURLArg(fields[1])
	// url.Parse treats a scheme-less host:port as an opaque URI scheme. Leave a
	// numeric port target to the normal agent path instead of misclassifying it.
	hostPort := targetText
	if !strings.Contains(hostPort, "://") {
		hostPort, _, _ = strings.Cut(hostPort, "/")
	}
	if _, port, err := net.SplitHostPort(hostPort); err == nil {
		if _, err := strconv.ParseUint(port, 10, 16); err == nil {
			return false
		}
	}
	target, err := url.Parse(targetText)
	return err == nil && target.Scheme != "" && !strings.EqualFold(target.Scheme, resourceExposeSchemeHTTPS)
}

// agentHasExplicitInvalidSetAlias recognizes the direct conversation form
// "Set alias <alias> ..." and applies the existing alias grammar before any LLM
// call. It is intentionally narrow so questions about alias syntax and other
// explanatory prose still follow the normal agent path.
func agentHasExplicitInvalidSetAlias(message string) bool {
	fields := strings.Fields(message)
	if len(fields) < 3 ||
		!strings.EqualFold(fields[0], "set") ||
		!strings.EqualFold(fields[1], "alias") ||
		!strings.HasPrefix(fields[2], "$") {
		return false
	}
	_, err := parseAliasToken(fields[2])
	return err != nil
}

// agentEventPartition is the conversation/dedupe partition key. It deliberately
// uses the same resolver as lifecycleWorkspaceIDs: org-level installs write those
// rows under enterprise_id, workspace-level installs write under team_id, and the
// lifecycle purge also sweeps any team-keyed agent-state partitions Slack
// includes on org callbacks for pending actions, audit, pane context, and rate
// counters.
func agentEventPartition(env *slackEventEnvelope) string {
	return resolveSlackEventPartitions(env).agentWrite
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

// agentThreadKey identifies one thread — channel + thread root — and is the single
// format for both the conversation-history key and (for an assistant pane) the stored
// context key. The container events persist context under agentThreadKey(at.ChannelID,
// at.ThreadTS); a pane turn reads it under agentEventThreadKey(env), which delegates
// here — so the two can't drift out of alignment.
func agentThreadKey(channelID, threadRootTS string) string {
	return channelID + ":" + threadRootTS
}

// agentEventThreadKey identifies one conversation: channel + thread root.
func agentEventThreadKey(env *slackEventEnvelope) string {
	return agentThreadKey(env.Event.Channel, agentEventRootTS(&env.Event))
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

// processAgentEvent runs one deliberate @mention/DM conversation turn on the async
// pool: workspace gate, dedupe, reconstruct live Slack history, run the agent,
// and post the reply.
func (h *Handler) processAgentEvent(ctx context.Context, log *slog.Logger, env *slackEventEnvelope) {
	h.processAgentEventWithAdmission(ctx, log, env, "", nil, false)
}

// processAdmittedAgentEvent runs a channel follow-up after runAgentFollowupPipeline has
// already checked the workspace toggle and loaded the thread transcript under the short
// admission gate.
func (h *Handler) processAdmittedAgentEvent(ctx context.Context, log *slog.Logger, env *slackEventEnvelope, partition string, pre *loadedHistory) {
	h.processAgentEventWithAdmission(ctx, log, env, partition, pre, true)
}

// agentDeterministicReply returns the fixed reply for a turn whose TEXT is
// answered without the LLM, and whether one applies. message is the caller's
// already-stripped text so it isn't recomputed here. The upload case is not here:
// it is a property of the event envelope, not of the text, and it is decided by
// the caller before this runs (see processAgentEventWithAdmission).
//
// Callers run this after dedupe and before rate limiting, the model, and — on the
// direct @mention/DM path — the thread-history read. A channel follow-up has
// already paid its history read in the admission gate. Every reply here is free of
// MODEL cost — so none consumes a limiter slot and none is written to any store —
// but each still costs one dedupe write and one chat.postMessage.
//
// "Written to no store" is not "the model never sees it": thread history is
// rebuilt live from the Slack transcript, so a deterministic reply still re-enters
// the model's context on the NEXT turn in that thread, like any other bot message.
func agentDeterministicReply(message string) (reply string, ok bool) {
	switch {
	// Keep help literal-only: punctuation or extra words stay on the normal agent path.
	case strings.EqualFold(message, "help"):
		return agentHelpReply, true
	case agentHasExplicitNonHTTPSProtectURL(message):
		return agentInvalidProtectURLReply, true
	case agentHasExplicitInvalidSetAlias(message):
		return agentInvalidAliasReply, true
	}
	return "", false
}

func (h *Handler) processAgentEventWithAdmission(ctx context.Context, log *slog.Logger, env *slackEventEnvelope, admittedPartition string, pre *loadedHistory, preadmitted bool) {
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

	partition, pre, ok := h.prepareAgentEventAdmission(ctx, log, env, admittedPartition, pre, preadmitted)
	if !ok {
		return
	}

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

	// The upload check comes first and wins outright: an upload carrying a complete,
	// answerable request still gets the limitation rather than an answer, and so does
	// one whose caption reads as a deterministic text keyword. qURL conversation mode
	// is text-only, so an upload must never draw a reply that silently ignores it —
	// and answering the text while saying nothing about the file is exactly that. The
	// cost is real: a valid question with an incidental screenshot has to be re-sent.
	// That is the deliberate trade — failing the whole turn is honest, half-answering
	// it is not. It is a branch here rather than a case inside agentDeterministicReply
	// so the log below keys off the CAUSE, not off the identity of the reply string.
	// Keying on the reply was the earlier shape, defended on the grounds that a
	// re-derivation could fall out of step with a reordered switch. Hoisting the
	// branch removes the second derivation instead of guarding it: there is no switch
	// case left to reorder, and the log cannot fire for the wrong turn or go quiet if
	// the reply text is ever decorated.
	//
	// Metering these through agentTurnLimited would not help either. That limiter caps
	// MODEL spend, so routing uploads into it would still post one reply per upload,
	// just with the rate-limit wording. Capping outbound volume is a separate control —
	// a short-lived per-thread notice marker — not a limiter change; see #1045, which
	// implements it.
	if agentEventHasUpload(&env.Event) {
		// This is the only deterministic reply that logs. Not because the others are
		// visible — none of them reach "agent: turn complete" either — but because this
		// one is the demand signal for building real file support, and nothing else
		// counts it. Every field is a count, a bool, or an opaque Slack ID: names, ids
		// and mimetypes are user content and stay out of the log.
		//
		// files_visible is 0 in two operationally OPPOSITE cases, which is why the two
		// bools are here to separate them:
		//   - files_field_present=false, file_share_subtype=true — Slack described the
		//     upload by subtype alone. Normal, high volume, and the refusal is correct.
		//   - files_field_present=true with files_visible=0 — the files value arrived in
		//     a shape the decoder could not count, i.e. Slack changed the wire format.
		//     This is the ONLY path where the refusal may be wrong: a text-only turn
		//     that merely carried an unrecognized files value gets refused with no
		//     attachment involved. Alert on that pair; it is the "the agent refused my
		//     message" report.
		log.Info("agent: unsupported media; replied with the text-only limitation",
			"files_visible", env.Event.Files.count,
			"files_field_present", env.Event.Files.present,
			"file_share_subtype", env.Event.Subtype == slackMessageSubtypeFileShare,
			"user_id", env.Event.User)
		h.postAgentReply(log, env, agentEventRootTS(&env.Event), agentUnsupportedMediaReply)
		return
	}

	message := stripBotMention(env.Event.Text)
	if reply, deterministic := agentDeterministicReply(message); deterministic {
		h.postAgentReply(log, env, agentEventRootTS(&env.Event), reply)
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

	// Working-on-it ack: before the pane rollout flag flips, pane turns keep the
	// reaction fallback plus best-effort native status; after it flips, pane turns use
	// only Slack's native assistant status. Channel turns always use the eyes reaction.
	// Register reaction cleanup before attempting native status so a status-path panic
	// cannot strand the fallback reaction. The add runs async, but clear joins its
	// completion handle before removing so the remove can't race ahead.
	add := h.startAgentReactionAck(log, env)
	defer h.clearAgentAck(log, env, add)
	if h.setAgentThinkingStatus(ctx, log, env) {
		defer h.clearAgentThinkingStatus(log, env)
	}

	history, ok := h.resolveTurnHistory(ctx, log, env, pre)
	if !ok {
		return
	}

	operatingChannel := h.agentOperatingChannel(ctx, log, env)

	// Resolve the operating channel's name for a friendlier system prompt ("#general (C123)"
	// vs the bare id). A 1:1 DM with no pane context has no usable name → skipped; a scoped
	// pane operates on a real channel, so it resolves like an @mention. (An app_mention in a
	// group DM still resolves but yields no name → negative-cached, harmless.) Cached per
	// channel (channelNameTTL), best-effort; describeChannel falls back to the id when empty.
	channelName := ""
	if operatingChannel != env.Event.Channel || env.Event.ChannelType != slackChannelTypeIM {
		channelName = h.resolveChannelName(ctx, log, env.TeamID, env.EnterpriseID, operatingChannel)
	}
	tc := agent.TurnContext{
		TeamID:        env.TeamID,
		EnterpriseID:  env.EnterpriseID,
		ChannelID:     operatingChannel,
		ChannelName:   channelName,
		UserID:        env.Event.User,
		CallerIsAdmin: h.callerIsAdmin(log, env.TeamID, env.Event.User),
	}

	replyTS := agentEventRootTS(&env.Event)
	// Native reply streaming: when the AgentStream seam is wired and the event is a
	// pane DM or channel @mention, the reply renders token-by-token instead of as one
	// posted message. nil otherwise — the agent keeps the normal post path. Per-turn
	// (a fresh streamer/Run).
	streamer := h.newAgentReplyStreamer(ctx, log, env, replyTS)
	var streamOpts []agent.Option
	if streamer != nil {
		streamOpts = append(streamOpts, agent.WithStreamSink(streamer.onDelta))
	}
	// Keep the backend reference: its per-turn scan memo carries whether the
	// workspace scan completed, which the turn-complete log reports below.
	backend := h.newAgentBackend(log)
	a := agent.New(h.cfg.AgentLLM, backend, streamOpts...)
	result, _, err := a.Run(ctx, &tc, history, message)

	if err != nil {
		log.Error("agent: turn failed", "error", err)
		// A HEALTHY live stream owns the (partial) outcome — deltas already delivered aren't
		// rolled back — so finalize the partial rather than double-posting an error over it.
		// finalizeError returns false (→ post the error below) when no stream opened, or when the
		// stream BROKE mid-flight and left a truncated partial the user shouldn't read as final.
		if streamer == nil || !streamer.finalizeError() {
			reply := agentErrorReply
			if ctx.Err() != nil {
				// The turn ctx is done (agentTurnTimeout elapsed, or baseCtx canceled on
				// shutdown): a transient timeout, not a capability limit — invite a retry.
				reply = agentTransientReply
			}
			h.postAgentReply(log, env, replyTS, reply)
		}
		return
	}

	// Token usage per turn (summed across the agent's round-trips). The cache
	// counters are the operator hook for confirming whether prompt caching is
	// paying off once conversation mode is live (see the agent package).
	// cutoff is empty on a turn that converged on its own, and names the ration that
	// ran out otherwise ("budget" / "iterations"). It is the operator signal for
	// agent latency regressions: a rising cutoff rate means turns are being answered
	// from a partial picture, which no other field here would reveal.
	//
	// resources_partial is the same signal one layer down, and needs its own field
	// because a partial scan does NOT raise cutoff — the turn converges normally,
	// just over an incomplete resource list. The per-page and per-read budgets are
	// far tighter than the qURL client's own 30s timeout, so a slow-but-working
	// backend now yields partial answers where it used to yield complete ones. That
	// is the intended trade, but without this field it would degrade answer quality
	// silently, which is the blind spot cutoff exists to close.
	log.Info("agent: turn complete",
		"proposed", result.Proposal != nil,
		"cutoff", string(result.Cutoff),
		"resources_partial", backend.resourceScanPartial(),
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"cache_read_tokens", result.Usage.CacheReadInputTokens,
		"cache_creation_tokens", result.Usage.CacheCreationInputTokens,
	)

	// A live stream delivers the reply itself (finalizeReply flushes + stops it), so the
	// caller skips the post — the no-double-post invariant. It returns false when no stream
	// opened (no deltas), when the stream broke mid-flight (the caller posts the full reply
	// over the truncated partial), or for a proposal, whose streamed text was only the agent's
	// narration; the confirm card is still delivered below as a separate message.
	if streamer != nil && streamer.finalizeReply(&result) {
		return
	}
	// Deliver: an interactive confirm card for an executable proposal once the
	// confirm flow is enabled, else the text reply/preview (merged #650 behavior).
	h.deliverAgentResultScoped(log, env, replyTS, operatingChannel, &result)
}

func (h *Handler) prepareAgentEventAdmission(ctx context.Context, log *slog.Logger, env *slackEventEnvelope, partition string, pre *loadedHistory, preadmitted bool) (string, *loadedHistory, bool) {
	if partition == "" {
		partition = agentEventPartition(env)
	}
	if preadmitted {
		return partition, pre, true
	}

	// Per-workspace toggle, BEFORE the dedupe marker so a disabled workspace consumes
	// nothing. A workspace that hasn't opted in (or opted out) gets no reply — the
	// same silent behavior as the org-level dark surface; members use slash commands.
	if !h.workspaceAgentEnabled(ctx, log, env.TeamID) {
		log.Info("agent: conversation mode disabled for this workspace; ignoring @mention/DM")
		return partition, nil, false
	}

	// Channel thread-reply continuity gate — BEFORE dedupe/ack so a reply that isn't
	// ours consumes nothing and is never acked (see agentChannelFollowupDropped). On an
	// admitted follow-up it returns the loaded transcript so the turn below reuses it.
	dropped, loaded := h.agentChannelFollowupDropped(ctx, log, env, partition)
	if dropped {
		return partition, nil, false
	}
	return partition, loaded, true
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

// agentReplyText renders the mrkdwn text-seam reply for a turn: the escaped
// proposal preview while conversation mode is read-only, else the generic error
// reply. The agent's own free-text answer does NOT come through here — it posts as
// standard Markdown (see deliverAgentResult), which intercepts a non-blank answer
// before the fallback reaches this function. So a non-proposal result arriving here
// is the blank-answer case, and renders the error reply.
func agentReplyText(result *agent.Result) string {
	if result.Proposal == nil {
		return agentErrorReply
	}
	// A blank summary would render as a dangling "• " bullet; fall back to the error
	// reply rather than post an empty preview.
	if strings.TrimSpace(result.Proposal.Summary) == "" {
		return agentErrorReply
	}
	// The preview posts as mrkdwn, and the summary is LLM-distilled — escape it
	// (consistent with the confirm card's fallback) so a prompt-injected masked link
	// can't surface.
	return agentProposalPreview(result.Proposal.Summary)
}

// postAgentReply delivers a mrkdwn reply in-thread — the escaped proposal preview
// or a fixed error string. (The agent's free-text answer goes via
// postAgentMarkdownReply instead; see deliverAgentResult.)
func (h *Handler) postAgentReply(log *slog.Logger, env *slackEventEnvelope, threadTS, text string) {
	h.deliverAgentText(log, env, threadTS, text, h.cfg.PostMessage)
}

// postAgentMarkdownReply delivers the agent's free-text answer as standard
// Markdown rendered by Slack, with masked links already neutralized by the caller.
// When the markdown seam is unwired (PostMarkdownMessage nil) it falls back to the
// mrkdwn PostMessage seam: degraded rendering, and not a full defense for Slack's
// own <url|label> mrkdwn masking syntax, but still a delivered pre-enablement
// answer rather than a dropped one. The production seam wires PostMarkdownMessage.
func (h *Handler) postAgentMarkdownReply(log *slog.Logger, env *slackEventEnvelope, threadTS, markdown string) {
	post := h.cfg.PostMarkdownMessage
	if post == nil {
		post = h.cfg.PostMessage
	}
	h.postAgentGeneratedReply(log, env, threadTS, markdown, post)
}

// deliverAgentText posts text to the thread via the given seam. It derives its own
// context off h.baseCtx (see agentDeliveryBudget) rather than the turn ctx, so a
// turn that spent its deadline still delivers its reply. Failures are logged, not
// surfaced.
func (h *Handler) deliverAgentText(log *slog.Logger, env *slackEventEnvelope, threadTS, text string, post PostMessageFunc) {
	ctx, cancel := context.WithTimeout(h.baseCtx, agentDeliveryBudget)
	defer cancel()
	if err := post(ctx, env.TeamID, env.EnterpriseID, env.Event.Channel, threadTS, text); err != nil {
		log.Error("agent: post reply failed", "error", err)
	}
}
