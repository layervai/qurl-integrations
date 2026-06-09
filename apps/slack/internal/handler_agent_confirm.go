package internal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
)

// Confirm-card button action_ids. Distinct from every slash-command button so a
// click routes unambiguously through findActionByID.
const (
	agentConfirmApproveActionID = "agent_confirm_approve"
	agentConfirmRejectActionID  = "agent_confirm_reject"
)

// User-facing confirm-flow copy. Deliberately free of internals (same posture as
// agentErrorReply); the warning prefix is added at the post site where present.
const (
	agentConfirmExpiredReply        = "This request expired — ask me again."
	agentConfirmAlreadyHandledReply = "That request was already handled."
	agentConfirmAdminOnlyReply      = "That action is admin-only — ask a workspace admin to approve it."
	agentConfirmScopeMismatchReply  = "That request belongs to a different channel."
	agentConfirmCanceledReply       = "Canceled — nothing was changed."
	agentConfirmUnsupportedReply    = "I can't apply that kind of change yet."
	agentConfirmGetDeliveredReply   = "Handled — the access link was sent privately to the approver."
	agentConfirmFailedReply         = "Something went wrong applying that. Please try again, or use a `/qurl` command."
)

// pendingAction is the ephemeral snapshot persisted between proposing a mutation
// and the Approve/Reject click — only what the click needs to re-authorize and
// execute. There is deliberately NO admin-gated field: the gate is derived at
// click time from the stored Action via adminGatedFor, never read off the wire,
// so tampering with the button can't bypass the admin re-check. ChannelID is the
// proposing channel (a cheap click-channel guard); the load-bearing channel scope
// is the token re-resolution at execute.
type pendingAction struct {
	Action    agent.ActionKind `json:"action"`
	Token     string           `json:"token,omitempty"`  // $slug/$alias for get/revoke
	Reason    string           `json:"reason,omitempty"` // audit reason, forwarded to the mint on get
	Alias     string           `json:"alias,omitempty"`  // alias name for set/unset-alias
	Target    string           `json:"target,omitempty"` // target slug for set-alias
	ChannelID string           `json:"channel_id"`
}

// confirmExecutable reports whether the confirm flow can actually EXECUTE this
// action kind in this build. Deferred kinds (protect → PR4c) must fall back to the
// text preview rather than render a live Approve button that can only reply "can't
// apply that yet" — keep in lockstep with executeAgentAction's switch. PR4c lands
// by extending this set.
func confirmExecutable(kind agent.ActionKind) bool {
	return kind == agent.ActionGet || kind == agent.ActionRevoke ||
		kind == agent.ActionSetAlias || kind == agent.ActionUnsetAlias
}

// deliverAgentResult posts a completed turn's result: an interactive confirm card
// only when the confirm flow is enabled AND the proposed action is actually
// executable here; otherwise the text reply/preview (the merged-#650 behavior).
// Gating the card on confirmExecutable keeps the dark-launch promise that only
// fully-wired actions get an Approve button — a deferred-kind proposal stays an
// honest "…isn't enabled yet" preview instead of a button that can't act.
func (h *Handler) deliverAgentResult(log *slog.Logger, env *slackEventEnvelope, threadTS string, result *agent.Result) {
	if result.Proposal != nil && h.agentConfirmEnabled() && confirmExecutable(result.Proposal.Action) {
		h.postAgentConfirm(log, env, threadTS, result.Proposal)
		return
	}
	h.postAgentReply(log, env, threadTS, agentReplyText(result))
}

// adminGatedFor is the SINGLE source of truth for whether an action needs an
// admin re-check at confirm time, used both when snapshotting a proposal and at
// click time. An unrecognized kind fails closed (gated).
func adminGatedFor(kind agent.ActionKind) bool {
	return kind != agent.ActionGet
}

// newPendingActionID returns an unguessable id — 16 crypto/rand bytes hex-encoded,
// the repo's nonce scheme (oauth/state.go). Only this id rides in the button
// value; the action kind, token, channel, and admin-gate stay server-side.
func newPendingActionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// agentConfirmEnabled reports whether the propose→confirm→execute flow is live:
// the read-only agent must be enabled, the gate flag set, and the blocks seam
// wired. While false the agent keeps #650's text-preview behavior.
func (h *Handler) agentConfirmEnabled() bool {
	return h.agentEnabled() && h.cfg.AgentConfirmEnabled && h.cfg.PostMessageBlocks != nil
}

// postAgentConfirm stores the proposed mutation and posts the interactive confirm
// card, both on a fresh delivery context (off h.baseCtx, never the possibly-spent
// turn ctx — same reasoning as postAgentReply). On any id/marshal/store/post
// failure it falls back to the text preview, so a proposal is never silently
// dropped. The pending action is keyed on env.TeamID (the click reads the same
// team id — see PutPendingAction).
func (h *Handler) postAgentConfirm(log *slog.Logger, env *slackEventEnvelope, threadTS string, prop *agent.Proposal) {
	summary := strings.TrimSpace(prop.Summary)
	if summary == "" {
		// Nothing to confirm against — mirror agentReplyText's blank-summary guard.
		h.postAgentReply(log, env, threadTS, agentErrorReply)
		return
	}
	// Escaped: the preview posts as mrkdwn on any fallback path (same reasoning as
	// the card fallback below and agentReplyText).
	preview := agentProposalPreviewPrefix + escapeMrkdwnText(summary)

	id, err := newPendingActionID()
	if err != nil {
		log.Error("agent confirm: id generation failed", "error", err)
		h.postAgentReply(log, env, threadTS, preview)
		return
	}
	blob, err := json.Marshal(pendingAction{
		Action:    prop.Action,
		Token:     prop.Token,
		Reason:    prop.Reason,
		Alias:     prop.Alias,
		Target:    prop.Target,
		ChannelID: env.Event.Channel,
	})
	if err != nil {
		log.Error("agent confirm: marshal pending action failed", "error", err)
		h.postAgentReply(log, env, threadTS, preview)
		return
	}

	ctx, cancel := context.WithTimeout(h.baseCtx, agentDeliveryBudget)
	defer cancel()
	if err := h.cfg.AgentStore.PutPendingAction(ctx, env.TeamID, id, blob); err != nil {
		log.Error("agent confirm: store pending action failed", "error", err)
		h.postAgentReply(log, env, threadTS, preview)
		return
	}
	// The card section renders the summary as plain_text (safe), but the fallback is
	// the message's top-level text — mrkdwn by default — so the same LLM-distilled
	// summary must be escaped there too, or a prompt-injected masked link would
	// surface in the notification/push preview and non-block clients.
	if err := h.cfg.PostMessageBlocks(ctx, env.TeamID, env.EnterpriseID, env.Event.Channel, threadTS, buildAgentConfirmBlocks(summary, id), escapeMrkdwnText(summary)); err != nil {
		log.Error("agent confirm: post card failed", "error", err)
		h.postAgentReply(log, env, threadTS, preview)
		return
	}
}

// buildAgentConfirmBlocks renders the confirm card: a summary section plus
// Approve (primary) and Reject (danger) buttons. Both buttons carry ONLY the
// pending-action id in their value.
//
// The summary renders as plain_text, NOT mrkdwn: it is LLM-distilled, so mrkdwn
// would let a prompt-injected summary surface a masked link (`<http://evil|click>`)
// or other markup publicly, right next to a live Approve button. plain_text shows
// it literally.
func buildAgentConfirmBlocks(summary, id string) []any {
	return []any{
		map[string]any{"type": "section", "text": plainTextObj(summary)},
		actionsBlock(
			primaryButtonElement("Approve", agentConfirmApproveActionID, id),
			dangerButtonElement("Reject", agentConfirmRejectActionID, id),
		),
	}
}

// handleAgentConfirmClick is the block_actions entrypoint for an Approve/Reject
// click. It acks 200 immediately and runs the load→scope→admin→claim→execute body
// on the async pool (off h.baseCtx), like handleListRevokeClick.
func (h *Handler) handleAgentConfirmClick(w http.ResponseWriter, payload *interactionPayload, action interactionAction, approve bool) {
	id := strings.TrimSpace(action.Value)
	responseURL := payload.ResponseURL
	log := slog.With(
		"surface", "agent_confirm",
		"team_id", payload.Team.ID,
		"channel_id", payload.Channel.ID,
		"user_id", payload.User.ID,
		"approve", approve,
	)

	if id == "" || h.cfg.AgentStore == nil {
		// Our buttons always carry an id, and the flow needs the store — defense in
		// depth. h.Go (not the async pool) keeps the ack prompt and can't deepen
		// pool saturation.
		log.Warn("agent confirm: missing id or unconfigured store")
		h.Go(func() { _ = h.postResponse(log, responseURL, ":warning: "+agentConfirmFailedReply) })
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}

	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		h.processAgentConfirm(ctx, log, payload, id, approve)
	}) {
		log.Warn("async pool saturated — dropping agent confirm click")
		h.Go(func() { _ = h.postResponse(log, responseURL, ackBusy) })
	}
	respondJSON(w, http.StatusOK, map[string]any{})
}

// processAgentConfirm is the async body of a confirm click. The card is PUBLIC
// (any channel member can click), so denials/already-handled go out as EPHEMERAL
// replies (postResponse, replace_original false) that leave the shared card
// intact; only the authorized winner replaces the card (replaceOriginalResponse,
// terminal). Both Approve and Reject are gated for an admin-gated action, so a
// non-admin can neither execute nor cancel an admin's pending action.
func (h *Handler) processAgentConfirm(ctx context.Context, log *slog.Logger, payload *interactionPayload, id string, approve bool) {
	teamID := payload.Team.ID
	responseURL := payload.ResponseURL

	// Re-check the kill switch at click time. AgentConfirmEnabled is a deploy-time
	// flag and cards live ~10 min, so a card posted just before a redeploy-to-off
	// could otherwise still execute within that window. This makes "flag off ⇒
	// nothing executes" hold unconditionally.
	if !h.agentConfirmEnabled() {
		_ = h.postResponse(log, responseURL, agentConfirmExpiredReply)
		return
	}

	blob, found, err := h.cfg.AgentStore.LoadPendingAction(ctx, teamID, id)
	if err != nil {
		log.Error("agent confirm: load pending action failed", "error", err)
		_ = h.postResponse(log, responseURL, ":warning: "+agentConfirmFailedReply)
		return
	}
	if !found {
		_ = h.postResponse(log, responseURL, agentConfirmExpiredReply)
		return
	}
	var pa pendingAction
	if err := json.Unmarshal(blob, &pa); err != nil {
		log.Error("agent confirm: corrupt pending action", "error", err)
		_ = h.postResponse(log, responseURL, ":warning: "+agentConfirmFailedReply)
		return
	}

	// Cheap defense-in-depth: a thread button can't be clicked from another
	// channel, so this is near-always true. The load-bearing channel-scope
	// enforcement is the token re-resolution against the click's channel at execute.
	if payload.Channel.ID != pa.ChannelID {
		log.Warn("agent confirm: channel mismatch", "click_channel", payload.Channel.ID, "action_channel", pa.ChannelID)
		_ = h.postResponse(log, responseURL, agentConfirmScopeMismatchReply)
		return
	}

	// Admin re-gate, derived from the STORED ActionKind (never the wire), applied
	// to BOTH Approve and Reject. A non-admin click is denied ephemerally and
	// claims NOTHING, so an admin can still act on the pending action.
	if adminGatedFor(pa.Action) {
		if !h.requireAdminForClick(log, responseURL, teamID, payload.User.ID, agentConfirmAdminOnlyReply) {
			return
		}
	}

	// CLAIM (consume-once) — the LAST gate before execute. Claim-before-execute is
	// at-most-once BY DESIGN: a transient core failure after the claim is not
	// retryable on this card (the user re-asks the agent). Do NOT move the claim
	// after execute to "fix" that — it reintroduces double-execute.
	claimed, err := h.cfg.AgentStore.ClaimPendingAction(ctx, teamID, id)
	if err != nil {
		log.Error("agent confirm: claim failed", "error", err)
		_ = h.postResponse(log, responseURL, ":warning: "+agentConfirmFailedReply)
		return
	}
	if !claimed {
		_ = h.postResponse(log, responseURL, agentConfirmAlreadyHandledReply)
		return
	}

	// Past the claim: this click is the single authorized winner — the card becomes
	// terminal regardless of the core's outcome. replaceOriginalResponse targets the
	// message the button was on; Slack honors replace_original on a public
	// chat.postMessage card (response_type is ignored for a replace), swapping the
	// card for plain terminal text (buttons gone). If a smoke test ever shows
	// otherwise, add an in_channel replace variant (see the PR plan, R2).
	if !approve {
		_ = h.replaceOriginalResponse(log, responseURL, agentConfirmCanceledReply)
		return
	}
	res := h.executeAgentAction(ctx, log, &pa, payload)
	if res.ephemeralText != "" {
		// Sensitive output (a one-time link) goes PRIVATELY to the clicker — an
		// ephemeral on the same response_url — never on the public card.
		_ = h.postResponse(log, responseURL, res.ephemeralText)
	}
	_ = h.replaceOriginalResponse(log, responseURL, res.cardText)
}

// actionResult is the terminal outcome of a claimed action. cardText replaces the
// PUBLIC confirm card. ephemeralText, when non-empty, is delivered PRIVATELY to the
// clicker (response_url ephemeral) — used for sensitive output that must not be
// broadcast to the channel (a get's one-time-use link). Routing is by action KIND,
// not outcome: the whole get result (link OR error) goes ephemeral, so nothing
// get-specific ever lands on the public card.
type actionResult struct {
	cardText      string
	ephemeralText string
}

// executeAgentAction runs the mapped mutation core for a claimed action and returns
// the terminal outcome (see actionResult). Every core re-resolves its token against
// the CLICK's channel, so channel scope is enforced at execute exactly as a typed
// command would be.
func (h *Handler) executeAgentAction(ctx context.Context, log *slog.Logger, pa *pendingAction, payload *interactionPayload) actionResult {
	switch pa.Action {
	case agent.ActionGet:
		flags := map[string]string{}
		if pa.Reason != "" {
			// Carry the agent's distilled intent into the mint's audit log
			// (Command.Reason → client.CreateInput.Reason), same as `/qurl get reason:…`.
			// Audit split (get is not admin-gated, so the clicker may differ from the
			// asker): the mint actor is the CLICKER (payload.User.ID) while the reason
			// is the ASKER's distilled intent — same actor/reason parity as a typed get.
			flags["reason"] = pa.Reason
		}
		cmd := &Command{Subcommand: SubcmdGet, Alias: pa.Token, Flags: flags, Raw: "get $" + pa.Token}
		text, err := h.getWork(ctx, log, getWorkArgs{
			cmd:       cmd,
			teamID:    payload.Team.ID,
			channelID: payload.Channel.ID,
			userID:    payload.User.ID,
			// triggerID seeds only getWork's idempotency key — getWork never
			// views.open's it, so the ~3s trigger-expiry doesn't apply on this
			// async path (the consume-once claim is the real double-execute guard).
			triggerID: payload.TriggerID,
		})
		result := text
		if err != nil {
			result = mapCoreError(log, err, commonGetMintFailedMessage)
		}
		// A get result is a one-time-use credential — deliver it PRIVATELY to the
		// clicker and keep the public card neutral, exactly as a typed /qurl get
		// stays ephemeral to its requester. (Whether the right person is approving —
		// asker vs any member — is the get-authorization gate on #651; ephemeral
		// delivery becomes fully correct once that enforces asker-only.)
		return actionResult{cardText: agentConfirmGetDeliveredReply, ephemeralText: result}
	case agent.ActionRevoke:
		resourceID, err := h.resolveTokenForGet(ctx, log, payload.Team.ID, payload.Channel.ID, payload.User.ID, pa.Token)
		if err != nil {
			return actionResult{cardText: mapCoreError(log, err, commonRevokeFailedMessage)}
		}
		// A revoke result ("revoked $x") is benign and useful as a public audit line.
		return actionResult{cardText: h.revokeResource(ctx, log, payload.Team.ID, payload.User.ID, resourceID, pa.Token)}
	case agent.ActionSetAlias:
		// Binds pa.Alias → pa.Target in the CLICK's channel (channel-scoped, like
		// the slash set-alias). Benign result → public card.
		return actionResult{cardText: h.resolveAndBindTunnelSlugAlias(ctx, log, payload.Team.ID, payload.Channel.ID, pa.Alias, pa.Target)}
	case agent.ActionUnsetAlias:
		return actionResult{cardText: h.unbindAliasResult(ctx, payload.Team.ID, payload.Channel.ID, pa.Alias)}
	case agent.ActionProtectConnector, agent.ActionProtectURL:
		// Deferred to PR4c (protect opens the tunnel-install modal). Admin-gated, so
		// a non-admin can't reach here; an admin gets a clean "not supported yet".
		log.Warn("agent confirm: action not executable in this build", "action", pa.Action)
		return actionResult{cardText: agentConfirmUnsupportedReply}
	default:
		log.Warn("agent confirm: unknown action kind", "action", pa.Action)
		return actionResult{cardText: agentConfirmUnsupportedReply}
	}
}
