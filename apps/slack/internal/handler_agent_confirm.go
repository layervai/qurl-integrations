package internal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/shared/auth"
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
	agentConfirmGetNotAskerReply    = "Only the person who requested this access link can approve it. Ask me for your own link."
	agentConfirmScopeMismatchReply  = "That request belongs to a different channel."
	agentConfirmCanceledReply       = "Canceled — nothing was changed."
	agentConfirmUnsupportedReply    = "I can't apply that kind of change yet."
	agentConfirmGetDeliveredReply   = "Handled — the access link was sent privately to the approver."
	agentConfirmFailedReply         = "Something went wrong applying that. Please try again, or use a `/qurl` command."
	// agentAttributionAgentName names the actor in an executed action's attribution
	// footer — the product's user-facing agent name, never "bot".
	agentAttributionAgentName = "qURL Secure Access Agent"
	// agentConfirmInvalidAliasReply is generic ON PURPOSE: the confirm card is
	// public, so an invalid (LLM-distilled, possibly injected) alias/target must NOT
	// be echoed back into it — unlike the slash path, whose validation reply is
	// ephemeral and can echo the bad token.
	agentConfirmInvalidAliasReply = "I couldn't apply that — the alias or target isn't valid (lowercase letters, numbers, and dashes only). Try rephrasing your request."
	// agentConfirmInvalidProtectURLReply is the protect-url sibling: generic for the
	// same public-card reason — the LLM-distilled URL/alias must not be echoed back.
	agentConfirmInvalidProtectURLReply = "I couldn't protect that — the URL or channel alias isn't valid. Use an absolute http(s) URL and an alias of lowercase letters, numbers, and dashes. Try rephrasing your request."
	// protect-connector terminal card copy. The action is already claimed by the
	// time these post, so an expired/failed open means "ask the agent again" (a new
	// proposal mints a fresh trigger) — there's no retry on this consumed card.
	agentConfirmConnectorOpenedReply        = "Approved — opening guided qURL Connector setup. Complete the form in the dialog to finish."
	agentConfirmConnectorWindowExpiredReply = "Approved, but Slack's setup window closed before the form could open. Ask me to protect that connector again."
	agentConfirmConnectorUnavailableReply   = "I couldn't open guided qURL Connector setup on this workspace. Ask an admin to run `/qurl-admin protect-connector` instead."
	// agentConfirmConnectorRateLimitedReply is distinct from the window-expired copy:
	// re-asking immediately won't help (Slack is throttling views.open), so tell the
	// admin to wait — mirrors the slash wizard distinguishing the rate-limit cause.
	agentConfirmConnectorRateLimitedReply = "Slack is rate-limiting setup right now. Wait a moment, then ask me to protect that connector again."
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
	Token     string           `json:"token,omitempty"`  // slug/alias (sigil stripped) for get/revoke
	Reason    string           `json:"reason,omitempty"` // audit reason, forwarded to the mint on get
	Alias     string           `json:"alias,omitempty"`  // alias name for set/unset-alias; channel alias for protect-url
	Target    string           `json:"target,omitempty"` // target slug for set-alias
	URL       string           `json:"url,omitempty"`    // target URL for protect-url
	Asker     string           `json:"asker,omitempty"`  // Slack user who requested the turn; get is asker-only (see processAgentConfirm)
	ChannelID string           `json:"channel_id"`
}

// confirmValidAliasBind validates a bare (no leading "$") alias + target — as the
// confirm path stores them — against the slash set-alias grammar (charset, length,
// no backticks/non-printables, tunnel-only target) and returns the parser's
// CANONICAL forms. It reuses parseAliasArgs as the single validation source, so the
// confirm and slash paths can't drift. Returning (and executing with) the canonical
// values — not the raw inputs — closes the "validate a reconstruction, execute the
// original" seam: e.g. a trailing tab validates clean (Fields trims it) but must not
// be echoed verbatim onto the public card.
func confirmValidAliasBind(alias, target string) (canonAlias, canonTarget string, ok bool) {
	parsed, msg := parseAliasArgs("$"+alias+" $"+target, true)
	if msg != "" {
		return "", "", false
	}
	return parsed.Alias, strings.TrimPrefix(parsed.Target, "$"), true
}

// confirmValidAlias is the single-alias counterpart for the unset-alias confirm
// path, returning the canonical alias. The alias charset (lowercase-alnum-dash)
// rejects backticks AND every non-printable (bidi/zero-width) — which
// escapeMrkdwnCode alone passes through — keeping garbled/spoofed text off the
// public "not bound" card.
func confirmValidAlias(alias string) (canon string, ok bool) {
	a, msg := requireAlias("$" + alias)
	return a, msg == ""
}

// confirmValidProtectURL validates the LLM-distilled protect-url URL + channel
// alias through the SAME grammar as the slash verb (parseResourceExposeArgs),
// reconstructing its `url:<target> as:$<alias>` form, and returns the parser's
// CANONICAL args. As with confirmValidAliasBind, executing those canonical args —
// not pa.URL/pa.Alias raw — closes the validate-reconstruction-execute-raw seam,
// and a parse failure surfaces a generic reply rather than echoing the (possibly
// injected) value onto the PUBLIC card. An empty alias fails here, which is
// correct: exposeURLResourceInChannel must bind a channel alias.
//
// Reusing parseResourceExposeArgs also runs the URL through unwrapSlackURLArg,
// whose unconditional HTML-unescape is documented as safe only for Slack-delivered
// text. On this path the URL is LLM-distilled, so that invariant is best-effort:
// any decoding only affects the exact-target lookup (a mismatch → benign "not
// found"), and the value is never echoed. Keeping the single grammar source is
// worth more than bypassing the unwrap for this caller.
func confirmValidProtectURL(rawURL, rawAlias string) (args *resourceExposeArgs, ok bool) {
	parsed, msg := parseResourceExposeArgs("url:" + rawURL + " as:$" + rawAlias)
	if msg != "" {
		return nil, false
	}
	return parsed, true
}

// confirmExecutable reports whether the confirm flow can actually EXECUTE this
// action kind in this build — gating the live Approve card vs the text preview, so
// a not-yet-wired kind never renders a button that can only reply "can't apply that
// yet". As of PR4c every mutation kind is executable: get/revoke/alias/protect-url
// via executeAgentAction, protect-connector via openAgentConnectorModal (the modal
// path). Keep in lockstep with both of those.
func confirmExecutable(kind agent.ActionKind) bool {
	return kind == agent.ActionGet || kind == agent.ActionRevoke ||
		kind == agent.ActionSetAlias || kind == agent.ActionUnsetAlias ||
		kind == agent.ActionProtectURL || kind == agent.ActionProtectConnector
}

// confirmModalRouted reports whether a kind executes by opening a MODAL
// (OpenView/trigger_id, then a separate view_submission) rather than the
// response_url-only executeAgentAction path. processAgentConfirm routes these to
// openAgentConnectorModal after the claim; executeAgentAction's case for them is a
// defensive, unreachable fail-closed. It's a named predicate (like confirmExecutable
// / adminGatedFor) so the click router AND the lockstep test share one source of
// truth: adding a future modal kind here both routes it and excludes it from the
// executeAgentAction lockstep check, so the two can't silently drift.
func confirmModalRouted(kind agent.ActionKind) bool {
	return kind == agent.ActionProtectConnector
}

// deliverAgentResult posts a completed turn's result: an interactive confirm card
// only when the confirm flow is enabled AND the proposed action is actually
// executable here; otherwise the text reply/preview (the merged-#650 behavior).
// Gating the card on confirmExecutable keeps the dark-launch promise that only
// fully-wired actions get an Approve button — a deferred-kind proposal stays an
// honest "…isn't enabled yet" preview instead of a button that can't act.
func (h *Handler) deliverAgentResult(log *slog.Logger, env *slackEventEnvelope, threadTS string, result *agent.Result) {
	if result.Proposal != nil && h.agentConfirmEnabled() && h.confirmDeliverable(result.Proposal) {
		h.postAgentConfirm(log, env, threadTS, result.Proposal)
		return
	}
	h.postAgentReply(log, env, threadTS, agentReplyText(result))
}

// confirmDeliverable reports whether a confirm card should render for this proposal
// in THIS deploy. A non-deliverable proposal falls back to the honest text preview
// instead of a button that could only dead-end on Approve (consuming the claim):
//   - not executable here → preview;
//   - modal-routed kind but OpenView unwired → would Approve into "unavailable";
//   - protect-url whose URL/alias fails the SAME grammar the execute path uses
//     (confirmValidProtectURL → parseResourceExposeArgs) → would Approve into the
//     generic invalid reply. Validating here, with that one validator, closes the
//     propose→execute drift the propose layer (in a package that can't import the
//     grammar) can't fully cover — e.g. a whitespace-bearing URL or out-of-charset
//     alias that url.Parse alone would accept.
func (h *Handler) confirmDeliverable(prop *agent.Proposal) bool {
	if !confirmExecutable(prop.Action) {
		return false
	}
	if confirmModalRouted(prop.Action) && h.cfg.OpenView == nil {
		return false
	}
	if prop.Action == agent.ActionProtectURL {
		if _, ok := confirmValidProtectURL(prop.URL, prop.Alias); !ok {
			return false
		}
	}
	return true
}

// adminGatedFor is the SINGLE source of truth for whether an action needs an
// admin re-check at confirm time, used both when snapshotting a proposal and at
// click time. An unrecognized kind fails closed (gated).
func adminGatedFor(kind agent.ActionKind) bool {
	return kind != agent.ActionGet
}

// askerOnly reports whether a kind may be approved ONLY by the member who requested
// it (the pendingAction's Asker), not just any channel member. A get mints a
// one-time access credential delivered ephemerally to the clicker, so only the asker
// may approve+receive it. Named like adminGatedFor so the confirm authorization model
// reads as one vocabulary; get is the only asker-only kind today.
func askerOnly(kind agent.ActionKind) bool {
	return kind == agent.ActionGet
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
		URL:       prop.URL,
		Asker:     env.Event.User, // the user who requested this turn — get is asker-only
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
	// Capture the trigger arrival as early as possible (here, not in the async
	// body): a protect-connector Approve opens a modal with this click's trigger_id,
	// whose ~3s window starts ticking at the click, and the ack + async-scheduling
	// gap before processAgentConfirm runs counts against it. Threaded through so the
	// connector branch can budget views.open the same way the slash wizard does.
	triggerReceivedAt := h.now()
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
		h.processAgentConfirm(ctx, log, payload, id, approve, triggerReceivedAt)
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
func (h *Handler) processAgentConfirm(ctx context.Context, log *slog.Logger, payload *interactionPayload, id string, approve bool, triggerReceivedAt time.Time) {
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

	// Per-workspace toggle re-checked at click time, like agentConfirmEnabled above:
	// a card proposed before the workspace turned conversation mode off (within the
	// ~10-min pending-action TTL) must NOT still execute on Approve — same "disabled
	// ⇒ nothing executes" standard as the org kill switch. Before the claim, so it
	// gates BOTH Approve and Reject; fails closed on a read error.
	if !h.workspaceAgentEnabled(ctx, log, teamID) {
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

	// Asker-only gate for get. A get mints a one-time access CREDENTIAL and is
	// delivered ephemerally to the clicker, so only the member who requested it may
	// approve+receive it (clicker==asker makes "deliver to the asker" correct). This
	// is BEFORE the claim and on BOTH Approve and Reject — deliberately: gating after
	// the claim would let any member consume the asker's pending get and then get
	// denied, permanently burning the request (a DoS worse than no gate); gating
	// Reject too stops a non-asker dismissing the asker's card out from under them
	// (an abandoned card is reaped by the 10-min TTL anyway). pa.Asker=="" fails
	// closed (an asker-less get can never match a real clicker).
	if askerOnly(pa.Action) && (pa.Asker == "" || payload.User.ID != pa.Asker) {
		log.Warn("agent confirm: non-asker click on a get card", "asker", pa.Asker, "clicker", payload.User.ID)
		_ = h.postResponse(log, responseURL, agentConfirmGetNotAskerReply)
		return
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
	// protect-connector doesn't fit the response_url-only actionResult model: it
	// opens the guided install modal with this click's trigger_id, and the modal
	// SUBMIT (processTunnelInstall) is the real enforcement + key delivery. Branch
	// before executeAgentAction. Claim already happened above — required, not just
	// consistent: the modal submit isn't idempotent across opens, so consume-once is
	// what stops two approvers from double-opening → double-minting a connector.
	if confirmModalRouted(pa.Action) {
		h.openAgentConnectorModal(ctx, log, payload, triggerReceivedAt)
		return
	}
	res := h.executeAgentAction(ctx, log, &pa, payload)
	if res.ephemeralText != "" {
		// Sensitive output (a one-time link) goes PRIVATELY to the clicker — an
		// ephemeral on the same response_url — never on the public card. No
		// attribution footer here: it's a self-delivered credential, and the public
		// card already carries the attribution.
		_ = h.postResponse(log, responseURL, res.ephemeralText)
	}
	// The public terminal card for an EXECUTED action carries the on-behalf
	// attribution: who requested the change, who approved it, and that the agent
	// performed it (#662). Pre-execution rejections aren't attributed (res.attributed
	// is false), so their generic copy stays byte-exact.
	card := res.cardText
	if res.attributed {
		card = agentConfirmAttributedCard(res.cardText, pa.Asker, payload.User.ID)
	}
	_ = h.replaceOriginalResponse(log, responseURL, card)
}

// agentConfirmAttributedCard appends an attribution footer to an executed
// confirm-card result, so channel members reading the (public) terminal card see
// that a human authorized the change and the qURL Secure Access Agent carried it
// out — the accountability a consequential action needs (#662). asker is the
// member who requested the action through the agent (pendingAction.Asker);
// approver is the member who clicked Approve. They coincide for a get (asker-only)
// and may differ for an admin-gated action. The wording is neutral provenance, not
// a "done" claim, so it reads correctly on both success and failure result cards.
// asker/approver are Slack-supplied user IDs (not LLM-distilled input), so the
// `<@id>` mentions need no sanitizing.
func agentConfirmAttributedCard(cardText, asker, approver string) string {
	var footer string
	switch asker {
	case "":
		// Asker is always set at propose time; this only guards a malformed pending
		// action so the agent marker is never silently dropped.
		footer = "Performed via the " + agentAttributionAgentName
	case approver:
		footer = "Requested by <@" + asker + "> via the " + agentAttributionAgentName
	default:
		footer = "Requested by <@" + asker + ">, approved by <@" + approver + ">, via the " + agentAttributionAgentName
	}
	return cardText + "\n\n_" + footer + "._"
}

// openAgentConnectorModal is the protect-connector confirm execute: open the SAME
// guided tunnel-install modal the slash wizard opens, using the Approve click's
// fresh trigger_id. It does NOT re-check admin — processAgentConfirm already did,
// before the claim — which also keeps the trigger budget for views.open.
//
// The modal SUBMIT (processTunnelInstall) is the real enforcement point: it
// re-checks admin, collects env/port, mints the bootstrap key, and delivers the
// install instructions. The proposal is sparse, so v1 opens the blank wizard and
// the admin completes it.
//
// KEY-DELIVERY PRIVACY: meta.ResponseURL is the PUBLIC card's response_url, but
// processTunnelInstall delivers the install (with the bootstrap key) as an
// EPHEMERAL message, which Slack scopes to the response_url's interacting user —
// here the Approve clicker. meta.UserID is set to that same clicker so the modal's
// same-user-submit gate forces submitter == clicker == ephemeral target; if those
// diverged the key would render to the wrong person. This is the same privacy
// class PR4a/PR4b already ship (ephemeral denials on this card). processTunnelInstall
// also revokes the key if Slack doesn't confirm delivery, so the residual is only
// "delivery succeeds but renders in-channel" — a hard pre-enablement smoke test (#651).
//
// On trigger expiry / open failure the card goes terminal with an "ask me again"
// prompt: the action is already claimed (consumed), so the user re-asks the agent
// to mint a fresh proposal + trigger — there is no retry on this card.
func (h *Handler) openAgentConnectorModal(ctx context.Context, log *slog.Logger, payload *interactionPayload, triggerReceivedAt time.Time) {
	responseURL := payload.ResponseURL
	if h.cfg.OpenView == nil {
		log.Warn("agent confirm: protect-connector approved but OpenView is not configured")
		_ = h.replaceOriginalResponse(log, responseURL, agentConfirmConnectorUnavailableReply)
		return
	}

	view, err := TunnelInstallModal(TunnelInstallModalMetadata{
		TeamID: payload.Team.ID,
		// The click's channel (== the proposal's, mismatch-guarded), matching the
		// other execute paths. The proposal's env/port/alias hints are intentionally
		// NOT threaded in v1 — the admin completes the blank wizard.
		ChannelID: payload.Channel.ID,
		// The approving admin: aligns the modal's same-user-submit gate with the
		// ephemeral key target (see the key-delivery note above).
		UserID:        payload.User.ID,
		ResponseURL:   responseURL,
		CreatedAtUnix: h.now().Unix(),
	})
	if err != nil {
		log.Error("agent confirm: protect-connector modal render failed", "error", err)
		_ = h.replaceOriginalResponse(log, responseURL, agentConfirmConnectorUnavailableReply)
		return
	}

	// A single pre-open budget check suffices: unlike the slash wizard (which fetches
	// resources between its two checks), the only work before views.open here is the
	// in-memory modal render, so one check immediately before the RPC covers it.
	openBudget := slackTriggerOpenViewBudgetRemaining(h.now().Sub(triggerReceivedAt))
	if openBudget <= 0 {
		log.Warn("agent confirm: protect-connector trigger expired before views.open")
		_ = h.replaceOriginalResponse(log, responseURL, agentConfirmConnectorWindowExpiredReply)
		return
	}
	openCtx, cancel := context.WithTimeout(ctx, openBudget)
	defer cancel()
	if err := h.openViewWithGridFallback(openCtx, log, payload.Team.ID, payload.Enterprise.ID, payload.TriggerID, view); err != nil {
		log.Error("agent confirm: protect-connector views.open failed",
			"error", err,
			"slack_trigger_expired", errors.Is(err, ErrSlackTriggerExpired),
			"slack_views_open_deadline_exceeded", errors.Is(err, context.DeadlineExceeded),
			"slack_rate_limited", errors.Is(err, ErrSlackRateLimited),
			"slack_bot_token_not_configured", errors.Is(err, auth.ErrSlackBotTokenNotConfigured),
		)
		_ = h.replaceOriginalResponse(log, responseURL, agentConfirmConnectorOpenErrorReply(err))
		return
	}
	log.Info("agent confirm: protect-connector modal opened", "channel_id", payload.Channel.ID, "user_id", payload.User.ID)
	_ = h.replaceOriginalResponse(log, responseURL, agentConfirmConnectorOpenedReply)
}

// agentConfirmConnectorOpenErrorReply maps a views.open failure to the right
// terminal card copy, so a non-expiry cause isn't mislabeled "ask me again" (which
// won't help). Mirrors the slash wizard's cause distinction; the distinct kinds are
// also logged for observability.
func agentConfirmConnectorOpenErrorReply(err error) string {
	switch {
	case errors.Is(err, auth.ErrSlackBotTokenNotConfigured):
		// The workspace has no usable bot token (and Grid fallback couldn't cover it):
		// re-asking the agent won't help — it needs the app install / an admin.
		return agentConfirmConnectorUnavailableReply
	case errors.Is(err, ErrSlackRateLimited):
		return agentConfirmConnectorRateLimitedReply
	default:
		// Trigger expired / deadline exceeded / transient: re-asking the agent mints a
		// fresh proposal + trigger, which is exactly the fix.
		return agentConfirmConnectorWindowExpiredReply
	}
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
	// attributed marks a card that reflects an ACTUALLY-EXECUTED action (a mutation
	// core was invoked), so the click path appends the on-behalf attribution footer.
	// Pre-execution rejections (invalid LLM-distilled input, unsupported kinds) leave
	// it false: nothing was performed, so there's nothing to attribute — and their
	// generic copy stays byte-exact for the no-echo security checks.
	attributed bool
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
			// The asker-only gate (processAgentConfirm) guarantees clicker==asker here,
			// so the mint actor (payload.User.ID) and the reason (the asker's distilled
			// intent) are the same person — same actor/reason parity as a typed get.
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
		// clicker and keep the public card neutral, exactly as a typed /qurl get stays
		// ephemeral to its requester. The asker-only gate (processAgentConfirm) ensures
		// the clicker IS the asker, so ephemeral-to-clicker delivers to the right person.
		return actionResult{cardText: agentConfirmGetDeliveredReply, ephemeralText: result, attributed: true}
	case agent.ActionRevoke:
		resourceID, err := h.resolveTokenForGet(ctx, log, payload.Team.ID, payload.Channel.ID, payload.User.ID, pa.Token)
		if err != nil {
			return actionResult{cardText: mapCoreError(log, err, commonRevokeFailedMessage)}
		}
		// A revoke result ("revoked $x") is benign and useful as a public audit line.
		return actionResult{cardText: h.revokeResource(ctx, log, payload.Team.ID, payload.User.ID, resourceID, pa.Token), attributed: true}
	case agent.ActionSetAlias:
		// The confirm path has no parser gate (unlike the slash verb), so validate the
		// LLM-distilled alias/target through the SAME grammar first — see
		// confirmValidAliasBind. On failure surface a generic message rather than echo
		// the (possibly injected) value onto the PUBLIC card. Bind/echo the CANONICAL
		// (validated) values it returns, not the raw inputs.
		alias, target, ok := confirmValidAliasBind(pa.Alias, pa.Target)
		if !ok {
			return actionResult{cardText: agentConfirmInvalidAliasReply}
		}
		// Binds alias → target in the CLICK's channel (channel-scoped, like the slash
		// set-alias). Inputs are validated, so the benign result is safe on the card.
		return actionResult{cardText: h.resolveAndBindTunnelSlugAlias(ctx, log, payload.Team.ID, payload.Channel.ID, alias, target), attributed: true}
	case agent.ActionUnsetAlias:
		// Validate the alias like set-alias before echoing it onto the public card:
		// unbindAliasResult escapes backticks, but non-printables (bidi/zero-width)
		// would still garble/spoof the "not bound" line — the charset gate rejects them.
		// Clear with the CANONICAL alias it returns, not the raw input.
		alias, ok := confirmValidAlias(pa.Alias)
		if !ok {
			return actionResult{cardText: agentConfirmInvalidAliasReply}
		}
		return actionResult{cardText: h.unbindAliasResult(ctx, payload.Team.ID, payload.Channel.ID, alias), attributed: true}
	case agent.ActionProtectURL:
		// Validate the LLM-distilled URL + channel alias through the slash grammar
		// (single source) and execute the CANONICAL args — see confirmValidProtectURL.
		// On failure surface a generic reply rather than echo the value onto the
		// PUBLIC card. The result echoes only the (validated) channel alias + the
		// backend resource alias, never the raw URL, so it's safe on the card.
		args, ok := confirmValidProtectURL(pa.URL, pa.Alias)
		if !ok {
			return actionResult{cardText: agentConfirmInvalidProtectURLReply}
		}
		// Binds the URL resource as $alias in the CLICK's channel (channel-scoped,
		// like the slash protect-url and the alias confirm path). Benign public result.
		return actionResult{cardText: h.exposeURLResourceInChannel(ctx, log, payload.Team.ID, payload.Channel.ID, args), attributed: true}
	case agent.ActionProtectConnector:
		// Unreachable from the confirm flow: processAgentConfirm routes
		// protect-connector to openAgentConnectorModal (the modal/OpenView path)
		// before executeAgentAction. Kept as a defensive fail-closed.
		log.Warn("agent confirm: protect-connector reached executeAgentAction (should route to the modal)", "action", pa.Action)
		return actionResult{cardText: agentConfirmUnsupportedReply}
	default:
		log.Warn("agent confirm: unknown action kind", "action", pa.Action)
		return actionResult{cardText: agentConfirmUnsupportedReply}
	}
}
