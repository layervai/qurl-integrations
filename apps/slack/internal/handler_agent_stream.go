package internal

import (
	"context"
	"log/slog"
	"strings"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
)

// agentStreamFlushBytes coalesces text deltas: the streamer buffers until it has at least
// this many pending bytes, then flushes one chat.appendStream. This bounds the appendStream
// call rate (Slack's streaming methods are Tier-4) without losing the progressive-reveal
// feel — roughly a short phrase per flush. The buffered tail always flushes at finalize, so
// a reply shorter than the threshold still lands.
const agentStreamFlushBytes = 48

// newAgentReplyStreamer builds the per-turn streamer for a streamable agent reply, or nil
// to keep the agent on the non-streaming post path. Pane DMs stream as before. Channel
// app_mention turns also stream now: chat.startStream requires recipient_* there, and
// recipient_team_id must be the triggering user's team when Slack provides shared-channel
// team hints, falling back to the event's workspace team for the normal same-team case.
func (h *Handler) newAgentReplyStreamer(ctx context.Context, log *slog.Logger, env *slackEventEnvelope, replyTS string) *agentReplyStreamer {
	if h.cfg.AgentStream == nil {
		return nil
	}
	if env.Event.ChannelType != slackChannelTypeIM && env.Event.Type != slackEventTypeAppMention {
		return nil
	}
	recipientTeamID := agentStreamRecipientTeamID(env)
	if recipientTeamID == "" {
		return nil
	}
	return &agentReplyStreamer{
		ctx:             ctx,
		baseCtx:         h.baseCtx,
		log:             log,
		port:            h.cfg.AgentStream,
		teamID:          env.TeamID,
		enterprise:      env.EnterpriseID,
		channelID:       env.Event.Channel,
		threadTS:        replyTS,
		recipientTeamID: recipientTeamID,
		userID:          env.Event.User,
	}
}

func agentStreamRecipientTeamID(env *slackEventEnvelope) string {
	// Pane DMs keep the pre-channel-streaming routing: the recipient team is the
	// installed event team. Shared-channel team hints are only for app_mention.
	if env.Event.ChannelType == slackChannelTypeIM {
		return env.TeamID
	}
	if env.Event.UserTeam != "" {
		return env.Event.UserTeam
	}
	if env.Event.SourceTeam != "" {
		return env.Event.SourceTeam
	}
	return env.TeamID
}

// agentReplyStreamer drives one agent turn's native reply streaming: it lazily opens a Slack
// stream on the first non-empty delta, coalesces deltas to bound appendStream calls, and
// finalizes exactly once. NOT safe for concurrent use — the agent loop calls onDelta
// synchronously, in order, from one goroutine, and finalize runs after the loop returns.
//
// On pane turns, the "thinking…" status (setAgentThinkingStatus) is cleared the same way
// the non-streaming reply clears it: Slack auto-clears on a reply landing on the SAME
// thread, and the streamed message lands on replyTS (the status's thread), so no explicit
// clear is needed (consistent with the non-streaming path's deliberate reliance on auto-clear).
//
// Rendering dialect: chat.appendStream takes standard Markdown, and the non-streaming reply
// posts the agent's own answer as standard Markdown too. Both paths run through the same
// masked-link hardener before delivery, so a split stream delta can't preserve a hidden
// [label](url) destination that the channel path would have neutralized. The escaped proposal
// preview still posts as mrkdwn text, but it carries no Markdown — see deliverAgentResult.
type agentReplyStreamer struct {
	// ctx is the turn ctx, used for streaming WHILE the turn runs (onDelta). baseCtx (h.baseCtx)
	// backs deliveryCtx() for the finalize steps — see deliveryCtx for why finalize can't reuse
	// the turn ctx.
	ctx        context.Context
	baseCtx    context.Context
	log        *slog.Logger
	port       AgentStreamPort
	teamID     string
	enterprise string
	channelID  string
	threadTS   string
	// recipientTeamID is the human recipient's team, not necessarily the bot-token
	// lookup team above. They differ for Enterprise Grid/shared-channel mentions.
	recipientTeamID string
	userID          string

	pending  strings.Builder // coalescer: delta text not yet flushed
	streamed strings.Builder // everything sent to the stream, to reconcile a synthetic reply
	streamTS string          // the Slack stream handle (empty until the first delta opens one)
	markdown agentMarkdownLinkHarden
	// broken marks a start/append failure: streaming stops and finalize/the caller fall back to
	// a posted reply. Invariant: broken ⟹ pending is empty — every site that sets broken either
	// precedes the pending write (a first-delta StartStream failure) or follows pending.Reset()
	// inside flush, and onDelta returns early once broken — so flush's "broken || empty" guard
	// never silently drops buffered text. Preserve this if you add a new broken-setting site.
	broken bool
}

// onDelta is the agent.WithStreamSink callback: each non-empty assistant text delta, in
// order. The first delta lazily opens the stream; a no-narration propose round (and a turn
// that streams nothing) therefore opens no stream at all. Runs while the turn is live, so it
// uses the turn ctx.
//
// The Slack POSTs here are synchronous on the agent's loop goroutine (WithStreamSink is an
// in-order, single-goroutine contract), so each coalesced flush serializes an appendStream RTT
// into token consumption. Deliberate for v1: the agentStreamFlushBytes coalesce bounds the call
// rate, and the backpressure only slows the reveal — the upstream LLM stream buffers, nothing is
// lost. A buffered async hand-off (decoupling token reads from Slack latency) is the optimization
// to weigh if the interleave proves to matter once enabled — measured under #708.
func (s *agentReplyStreamer) onDelta(delta string) {
	if s.broken {
		return
	}
	delta = s.markdown.write(delta)
	if delta == "" {
		return
	}
	if s.streamTS == "" {
		ts, err := s.port.StartStream(s.ctx, &AgentStreamStart{
			TeamID:          s.teamID,
			EnterpriseID:    s.enterprise,
			ChannelID:       s.channelID,
			ThreadTS:        s.threadTS,
			RecipientTeamID: s.recipientTeamID,
			RecipientUserID: s.userID,
		})
		if err != nil {
			s.log.Warn("agent: startStream failed; falling back to a posted reply", "error", err)
			s.broken = true
			return
		}
		s.streamTS = ts
	}
	s.pending.WriteString(delta)
	if s.pending.Len() >= agentStreamFlushBytes {
		s.flush(s.ctx)
	}
}

// flush drains the coalescer to the live stream and records what it SUCCESSFULLY sent (so
// finalize can tell whether a synthesized reply was already streamed; text that failed to
// append is not recorded). No-op if the stream already broke or nothing is buffered. A failure
// marks the streamer broken so it stops appending; the caller then posts the full reply
// (finalizeReply returns false) so the user still gets the complete answer. The synthesized-reply
// reconcile in finalizeReply routes through here too, by writing the reply into pending — so
// every append flows through this one path.
func (s *agentReplyStreamer) flush(ctx context.Context) {
	if s.broken || s.pending.Len() == 0 {
		return
	}
	text := s.pending.String()
	s.pending.Reset()
	if err := s.port.AppendStream(ctx, s.teamID, s.enterprise, s.channelID, s.streamTS, text); err != nil {
		s.log.Warn("agent: appendStream failed; will fall back to the posted reply", "error", err)
		s.broken = true
		return
	}
	s.streamed.WriteString(text) // record only what actually reached Slack, so streamed never lies
}

func (s *agentReplyStreamer) stop(ctx context.Context) {
	if err := s.port.StopStream(ctx, s.teamID, s.enterprise, s.channelID, s.streamTS); err != nil {
		s.log.Warn("agent: stopStream failed", "error", err)
	}
}

// deliveryCtx derives the bounded context the finalize steps run on. It hangs off baseCtx,
// NOT the turn ctx, because by finalize time the turn ctx may be spent (agentTurnTimeout
// elapsed) or canceled (SIGTERM) — a stopStream on a dead ctx fails instantly and leaves the
// stream unfinished. Mirrors saveAgentHistory / postAgentReply, which deliver off
// h.baseCtx with the same agentDeliveryBudget.
func (s *agentReplyStreamer) deliveryCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.baseCtx, agentDeliveryBudget)
}

// finalizeReply finalizes a SUCCESSFUL turn and reports whether the stream delivered the
// reply (so the caller skips the normal post — the no-double-post invariant). It returns
// false when: no stream opened (no deltas); the stream broke at any point (the partial is
// stopped and the caller posts the full reply); or the turn is a proposal (the streamed text
// was only the agent's narration, and the caller still posts the confirm card separately).
func (s *agentReplyStreamer) finalizeReply(result *agent.Result) (deliveredReply bool) {
	if s.streamTS == "" {
		return false
	}
	ctx, cancel := s.deliveryCtx()
	defer cancel()
	if tail := s.markdown.flush(); tail != "" {
		s.pending.WriteString(tail)
	}
	s.flush(ctx) // the buffered tail (a no-op if the stream already broke)
	// Reconcile a Reply the deltas never carried: Run can synthesize one (the iteration-cap /
	// empty-text fallback — see agent.WithStreamSink) that no terminal delta streamed, which
	// would otherwise leave the finalized message truncated. Append it only when the stream is
	// healthy and doesn't already carry it — the normal case (the terminal round streamed the
	// reply) short-circuits, so we never double it. The membership test is strings.Contains, not
	// HasSuffix, on purpose: result.Reply is TrimSpace'd while streamed is not, so a strict
	// suffix match would false-negative on trailing whitespace and double-post; Contains' worst
	// case is the benign reverse — skipping a reply the user already saw stream. Routed through
	// pending+flush so it shares the one append+record+break path.
	if !s.broken {
		reply := hardenAgentMarkdown(result.Reply)
		if r := strings.TrimSpace(reply); r != "" {
			streamed := s.streamed.String()
			if !strings.Contains(streamed, r) {
				// Per-delta streaming treats chunk starts as possible Markdown parse
				// boundaries. If that stricter form is already present, do not append a
				// one-shot variant that may be less escaped around reference definitions.
				streamReply := hardenAgentMarkdownForStreamReconcile(result.Reply)
				if sr := strings.TrimSpace(streamReply); sr != "" && !strings.Contains(streamed, sr) {
					s.pending.WriteString(streamReply)
					s.flush(ctx)
				}
			}
		}
	}
	s.stop(ctx)
	if s.broken {
		// A start/append failed somewhere above (including the tail or synthesized append): the
		// caller posts the full reply so the user still gets the complete answer (a stray partial
		// stream may remain). This is why deleting this check truncates the reply — keep it.
		return false
	}
	return result.Proposal == nil
}

// finalizeError finalizes a FAILED turn and reports whether it was handled (so the caller
// skips posting an error reply). A HEALTHY live stream's already-delivered partial stands —
// deltas are never rolled back (agent.WithStreamSink) — so we flush the buffered tail, stop,
// and own the outcome rather than double-posting an error over it. But a BROKEN stream left a
// truncated partial: there we return false so the caller posts the error, rather than letting
// the user read a half-message as the final answer. Symmetric with finalizeReply's broken
// fallback. With no stream open, the caller posts the error.
func (s *agentReplyStreamer) finalizeError() (handled bool) {
	if s.streamTS == "" {
		return false
	}
	ctx, cancel := s.deliveryCtx()
	defer cancel()
	if tail := s.markdown.flush(); tail != "" {
		s.pending.WriteString(tail)
	}
	s.flush(ctx) // deliver the partial tail (a no-op if the stream already broke)
	s.stop(ctx)
	return !s.broken
}
