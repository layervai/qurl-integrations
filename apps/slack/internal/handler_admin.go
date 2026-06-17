package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
)

// handleAdmin parses a flat bot-admin membership/ownership verb (`add`,
// `remove`, `admins`, `transfer-ownership`) via the shared parser and
// dispatches to the action-specific handler. Each is sync — one DDB
// GetItem/UpdateItem — so the full gate + body + reply chain fits inside
// Slack's 3s slash-command ack window without the async runAsync hop.
// (Resource `revoke` is multi-hop and lives in its own async handleRevoke; it
// does not route here.)
//
// Parse runs first, then requireAdminStoreSync gates the rest. So on a
// sandbox deploy without the QURL_*_TABLE env vars: malformed verb text
// surfaces as a parser error (`:warning: missing @user mention`, etc.);
// parser-valid verbs reply with "Admin features are not configured". The
// distinction is intentional — parser errors are useful feedback regardless
// of DDB wiring, and reordering the checks would mask shape errors behind the
// not-configured surface.
func (h *Handler) handleAdmin(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	cmd, err := Parse(text)
	if err != nil {
		// Surface the terse parser sentinel verbatim — bare `add`/`remove`
		// yield "missing @user mention", a malformed mention yields the
		// invalid-mention hint. This is ungated (discovery, not execution —
		// the admin gate is on the verb body), matching set-alias /
		// set-display-name.
		respondSlack(w, ":warning: "+err.Error())
		return
	}
	// No `cmd.Subcommand != SubcmdAdmin` guard: this entry point is only
	// reached from the add/remove/admins/transfer-ownership dispatch arm in
	// dispatchAdminCommand, and Parse maps those to SubcmdAdmin + an
	// AdminAction. If the parser ever drifts and emits a different subcommand
	// here, cmd.AdminAction lands empty and the switch's `default:` arm renders
	// the same "unknown" copy a guard would have. TrimSpace mirrors
	// handleSlashCommand's other entry points — a whitespace-only team_id or
	// user_id otherwise sneaks past requireAdminSync's `== ""` check.
	teamID := strings.TrimSpace(values.Get(fieldTeamID))
	enterpriseID := strings.TrimSpace(values.Get(fieldEnterpriseID))
	userID := strings.TrimSpace(values.Get(fieldUserID))
	// Every recognized verb needs AdminStore. Short-circuit once here
	// instead of repeating the guard in each switch arm.
	if !h.requireAdminStoreSync(w) {
		return
	}
	switch cmd.AdminAction { //nolint:exhaustive // dispatch handles only the membership actions Parse produces here; gate-audit labels never reach here, default covers the rest
	case AdminAdd:
		h.handleAdminAdd(w, teamID, userID, cmd)
	case AdminRemove:
		h.handleAdminRemove(w, teamID, userID, cmd)
	case AdminList:
		h.handleAdminList(w, teamID, userID)
	case AdminTransferOwnership:
		h.handleAdminTransferOwnership(w, teamID, enterpriseID, userID, cmd)
	default:
		// Unreachable in practice — Parse only produces the AdminAction values
		// switched above on verbs routed here. Kept for refactor safety. The
		// reply intentionally OMITS cmd.AdminAction; a future parser drift could
		// land an arbitrary string here, and echoing it back risks confusing copy
		// on already-confused input.
		slog.Warn("admin dispatcher: unknown action reached default arm — parser drift?", "team_id", teamID, "user_id", userID, "action", string(cmd.AdminAction))
		respondSlack(w, "Unknown admin command. Try `/qurl-admin help`.")
	}
}

// requireTransferOwnerSync is a preflight for precise Slack replies; TransferOwnership's
// conditional UpdateItem remains the authoritative race guard.
func (h *Handler) requireTransferOwnerSync(ctx context.Context, w http.ResponseWriter, teamID, userID string) bool {
	if teamID == "" || userID == "" {
		respondSlack(w, ":warning: missing team_id or user_id in slash command payload")
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, adminGateBudget)
	defer cancel()
	_, ownerID, err := h.cfg.AdminStore.CheckAdmin(ctx, teamID, userID)
	if err != nil {
		slog.Error("owner check failed", "error", err, "team_id", teamID, "user_id", userID, "action", string(AdminTransferOwnership))
		respondSlack(w, ":warning: failed to verify owner status (upstream error; see logs).")
		return false
	}
	if ownerID == "" {
		respondSlack(w, workspaceUnboundReply)
		return false
	}
	if !slackdata.LooksLikeSlackUserID(ownerID) {
		slog.Warn("owner command denied: legacy shape-bad owner_id requires setup refresh", "team_id", teamID, "user_id", userID, "action", string(AdminTransferOwnership), "legacy_owner_prefix", slackdata.LegacyOwnerPrefix(ownerID), "owner_id_len", len(ownerID))
		respondSlack(w, ":warning: qURL is connected with a legacy workspace ownership record. Run `/qurl setup <email>` to refresh it, then retry `/qurl-admin transfer-ownership @user`.")
		return false
	}
	if ownerID != userID {
		slog.Warn("owner command denied", "team_id", teamID, "user_id", userID, "action", string(AdminTransferOwnership))
		respondSlack(w, ":warning: this command is owner-only")
		return false
	}
	return true
}

// requireAdminStoreSync renders the "admin features not configured"
// reply when AdminStore is nil (sandbox / no-DDB deployments).
// Returns true when the caller may proceed; false when a reply has
// already been written.
func (h *Handler) requireAdminStoreSync(w http.ResponseWriter) bool {
	if h.cfg.AdminStore == nil {
		respondSlack(w, "Admin features are not configured for this deployment.")
		return false
	}
	return true
}

// workspaceUnboundReply is the user-visible reply for the
// defensive 404-workspace-not-bound branches in handleAdmin{Add,
// Remove,List}. Lifted to a const so the three copies can't drift
// if the gate posture ever changes (today requireAdminSync short-
// circuits before any of those branches fires).
const workspaceUnboundReply = "Workspace isn't bound — run `/qurl setup <email>` first."

// adminGateBudget bounds the sync admin-gate CheckAdmin call so a
// hung DDB can't out-block Slack's 3s slash-command ack window. The
// gate is the FIRST upstream call on every sync admin verb (and is
// reused by the owner gate in handleSetup, which makes the same
// CheckAdmin call); using `context.Background()` here would let a
// misbehaving upstream silently consume the entire user-visible
// budget.
//
// 800ms covers a healthy DDB GetItem with ample tail-latency margin
// (warm-path p99 is well under 100ms; 800ms is ~10x that). The
// remainder of the 3s Slack window is split between the verb body
// (adminSyncVerbBudget=1.2s) and ~1s of network+encode headroom so
// a transient DDB spike or AWS-network blip can absorb without
// missing the ack.
const adminGateBudget = 800 * time.Millisecond

// adminSyncVerbBudget bounds the verb-body work for the sync admin
// membership/ownership verbs (add / remove / admins / transfer-ownership) so
// the full gate + body + encode chain fits inside Slack's 3s slash-command ack
// window.
// Without this, asyncWorkTimeout (25s) would silently let the verb
// body wedge past 3s and the user would see no reply at all (Slack
// drops slash-command responses that miss the ack). (Resource `revoke`
// is multi-hop and runs async via runAsync, so it isn't bounded here.)
//
// 1.2s + adminGateBudget=800ms = 2s of upstream work — leaves ~1s of
// the 3s window for response_encode + write + the Slack-side network
// hop. Generous compared to typical timings (each verb is one DDB
// UpdateItem, well under 100ms warm) but the headroom is the point:
// missing Slack's ack costs the user any visible reply at all.
const adminSyncVerbBudget = 1200 * time.Millisecond

// adminTargetLookupBudget bounds transfer-ownership's Slack users.info phase.
const adminTargetLookupBudget = 600 * time.Millisecond

// adminTransferSyncBudget is the whole sync budget for transfer-ownership's
// owner gate + Slack target lookup + DDB mutation. transfer-ownership is the
// only sync admin verb with all three hops before the slash-command reply. Keep
// this budget narrow so the phase ceilings stay below Slack's 3s ack window:
// each phase still has its own narrower timeout, but the shared parent stops
// their ceilings from adding up past the reply budget. If the parent deadline
// fires after DynamoDB applies but before the SDK sees the response, a retry can
// correctly fail the owner_id condition with owner-only copy; if that appears in
// telemetry, move this verb to an async response_url flow instead of widening
// the sync path.
const adminTransferSyncBudget = 2400 * time.Millisecond

// requireAdminSync centralizes the admin-only gate for sync handlers.
// Returns true when the caller may proceed; false when the request
// is denied (and a reply has already been written to `w`). The
// asymmetry between slog attrs (team_id + user_id + raw err) and the
// user-visible reply (generic upstream-error) is deliberate: detailed
// causes live in CloudWatch where on-call can read them; the wire
// surface never includes upstream message bodies that could carry
// request IDs or stack-frame fragments.
func (h *Handler) requireAdminSync(w http.ResponseWriter, teamID, userID string, action AdminAction) bool {
	if teamID == "" || userID == "" {
		respondSlack(w, ":warning: missing team_id or user_id in slash command payload")
		return false
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, adminGateBudget)
	defer cancel()
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(ctx, teamID, userID)
	if err != nil {
		slog.Error("admin check failed", "error", err, "team_id", teamID, "user_id", userID, "action", string(action))
		respondSlack(w, ":warning: failed to verify admin status (upstream error; see logs).")
		return false
	}
	if !isAdmin {
		// Audit non-admin denials so brute-force / curiosity probes
		// are visible to on-call. Distinct slog.Warn level from the
		// success path's slog.Info so dashboards can filter "denied"
		// without scanning every admin command. `action` lets the
		// filter distinguish e.g. "probed admin revoke" from
		// "probed admin list".
		slog.Warn("admin command denied: non-admin", "team_id", teamID, "user_id", userID, "action", string(action))
		respondSlack(w, ":warning: this command is admin-only")
		return false
	}
	return true
}

// requireAdminForClick is the async (button-click) sibling of requireAdminSync:
// it runs the fail-closed workspace-admin re-check for an async mutation and
// posts the appropriate ephemeral denial to responseURL, returning false on any
// non-admin / error / unconfigured outcome. The check is bounded off h.baseCtx
// (not a request ctx) so a client abort can't cancel the deliberate gate.
// adminOnlyMsg is the surface-specific denial copy (warning prefix added here).
// Shared by the /qurl list Revoke button and the conversation-mode confirm card.
func (h *Handler) requireAdminForClick(log *slog.Logger, responseURL, teamID, userID, adminOnlyMsg string) bool {
	if h.cfg.AdminStore == nil {
		_ = h.postResponse(log, responseURL, ":warning: Admin features are not configured for this deployment.")
		return false
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, adminGateBudget)
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(ctx, teamID, userID)
	cancel()
	if err != nil {
		log.Error("admin click gate: check failed", "error", err, "team_id", teamID, "user_id", userID)
		_ = h.postResponse(log, responseURL, ":warning: failed to verify admin status (upstream error; see logs).")
		return false
	}
	if !isAdmin {
		log.Warn("admin click gate: non-admin denied", "team_id", teamID, "user_id", userID)
		_ = h.postResponse(log, responseURL, ":warning: "+adminOnlyMsg)
		return false
	}
	return true
}

// handleAdminAdd promotes the target Slack user to admin on the
// caller's workspace. The caller must already be an admin
// (requireAdminSync). The store call is a single conditional
// UpdateItem on workspace_mappings; a CCFE folds into one of two
// user-facing surfaces via the slackdata-side disambiguation read:
//
//   - 409 admin_already_exists → idempotent "already an admin" copy
//   - 404 workspace_not_bound  → "run /qurl setup <email> first" nudge
//
// Other store errors surface as the generic upstream-error reply.
func (h *Handler) handleAdminAdd(w http.ResponseWriter, teamID, callerUserID string, cmd *Command) {
	if !h.requireAdminSync(w, teamID, callerUserID, AdminAdd) {
		return
	}
	target := cmd.UserID
	if target == callerUserID {
		// requireAdminSync already passed → the caller is an admin →
		// adding themselves is a no-op. Render an explicit copy so an
		// admin who fat-fingered `/qurl-admin admin add @themselves` doesn't
		// read the indirect "already an admin" surface as if it
		// referred to someone else.
		respondSlack(w, "You're already an admin — nothing to do.")
		return
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, adminSyncVerbBudget)
	defer cancel()
	if err := h.cfg.AdminStore.AddAdmin(ctx, teamID, target); err != nil {
		var se *slackdata.Error
		if errors.As(err, &se) {
			// Discriminate on (StatusCode, Code) — mirrors RemoveAdmin
			// so a future second 409/404 case in slackdata.AddAdmin
			// (e.g. OAuth-state conflict) routes explicitly instead of
			// silently misrouting on the existing arm.
			switch {
			case se.StatusCode == http.StatusConflict && se.Code == slackdata.ErrCodeAdminAlreadyExists:
				respondSlack(w, fmt.Sprintf("<@%s> is already an admin — nothing to do.", target))
				return
			case se.StatusCode == http.StatusConflict && se.Code == slackdata.ErrCodeAdminAddUnverified:
				// Conditional fired but the post-CCFE disambiguation
				// read can't confirm membership — usually a transient
				// DDB blip (timeout / transport) on the disambig
				// GetItem. Warn (not Error) so dashboards tied to
				// level=ERROR don't page on a user-recoverable
				// "please retry" surface.
				slog.Warn("add admin: conditional fired but disambiguation read can't confirm membership", "team_id", teamID, "user_id", callerUserID, "target_user_id", target)
				respondSlack(w, ":warning: couldn't confirm admin add — please retry. If this persists, contact your Slack admin.")
				return
			case se.StatusCode == http.StatusNotFound && se.Code == slackdata.ErrCodeWorkspaceNotBound:
				// Unreachable in practice: requireAdminSync short-
				// circuits with "admin-only" on a missing workspace
				// row. Kept for safety against gate refactors.
				respondSlack(w, workspaceUnboundReply)
				return
			}
		}
		slog.Error("add admin failed", "error", err, "team_id", teamID, "user_id", callerUserID, "target_user_id", target)
		respondSlack(w, ":warning: failed to add admin (upstream error; see logs).")
		return
	}
	slog.Info("admin add succeeded", "team_id", teamID, "user_id", callerUserID, "target_user_id", target)
	respondSlack(w, fmt.Sprintf("Added <@%s> as an admin.", target))
}

// handleAdminRemove demotes the target Slack user from admin on
// the caller's workspace. Pre-flight rejects a self-remove with a
// clear "ask another admin" copy so a fat-fingered admin doesn't
// accidentally lock themselves out (the store-side owner check
// covers the owner case; the self-remove check covers the non-owner
// case where the user IS demoting themselves).
//
//   - 400 cannot_remove_owner → "transfer via OAuth re-install" copy
//   - 404 admin_not_found     → idempotent "not an admin" copy
//   - 404 workspace_not_bound → "run /qurl setup <email> first" nudge
//
// Other store errors surface as the generic upstream-error reply.
func (h *Handler) handleAdminRemove(w http.ResponseWriter, teamID, callerUserID string, cmd *Command) {
	if !h.requireAdminSync(w, teamID, callerUserID, AdminRemove) {
		return
	}
	target := cmd.UserID
	if target == callerUserID {
		// The store-side owner-check catches owners; this catches the
		// non-owner self-remove. We refuse rather than allowing it so
		// an admin who fat-fingers `/qurl-admin admin remove @themselves`
		// doesn't lock themselves out without an explicit confirmation.
		respondSlack(w, "You can't remove yourself — ask another admin.")
		return
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, adminSyncVerbBudget)
	defer cancel()
	if err := h.cfg.AdminStore.RemoveAdmin(ctx, teamID, target); err != nil {
		var se *slackdata.Error
		if errors.As(err, &se) {
			switch {
			case se.StatusCode == http.StatusBadRequest && se.Code == slackdata.ErrCodeCannotRemoveOwner:
				respondSlack(w, fmt.Sprintf("Can't remove <@%s> — they connected qURL to this workspace, so they can't be removed as an admin.", target))
				return
			case se.StatusCode == http.StatusNotFound && se.Code == slackdata.ErrCodeWorkspaceNotBound:
				// Unreachable in practice: requireAdminSync short-
				// circuits with "admin-only" on a missing workspace
				// row (CheckAdmin returns isAdmin=false there). Kept
				// for safety against gate refactors.
				respondSlack(w, workspaceUnboundReply)
				return
			case se.StatusCode == http.StatusNotFound && se.Code == slackdata.ErrCodeAdminNotFound:
				respondSlack(w, fmt.Sprintf("<@%s> isn't an admin — nothing to do.", target))
				return
			}
		}
		slog.Error("remove admin failed", "error", err, "team_id", teamID, "user_id", callerUserID, "target_user_id", target)
		respondSlack(w, ":warning: failed to remove admin (upstream error; see logs).")
		return
	}
	slog.Info("admin remove succeeded", "team_id", teamID, "user_id", callerUserID, "target_user_id", target)
	respondSlack(w, fmt.Sprintf("Removed <@%s> from admins.", target))
}

// handleAdminList renders the workspace owner + the sorted admin set
// in a two-line Slack-mrkdwn reply. The owner is always listed; the
// "Admins:" line is omitted when the admin set contains only the
// owner (or is empty) — operators want a tight reply on a single-
// admin workspace, not a redundant duplicate.
//
//   - 404 workspace_not_bound → "run /qurl setup <email> first" nudge
//
// Other store errors surface as the generic upstream-error reply.
func (h *Handler) handleAdminList(w http.ResponseWriter, teamID, callerUserID string) {
	if !h.requireAdminSync(w, teamID, callerUserID, AdminList) {
		return
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, adminSyncVerbBudget)
	defer cancel()
	ownerID, admins, err := h.cfg.AdminStore.ListAdmins(ctx, teamID)
	if err != nil {
		var se *slackdata.Error
		if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
			// Unreachable in practice: requireAdminSync short-
			// circuits with "admin-only" on a missing workspace row.
			// Kept for safety against gate refactors.
			respondSlack(w, workspaceUnboundReply)
			return
		}
		slog.Error("list admins failed", "error", err, "team_id", teamID, "user_id", callerUserID)
		respondSlack(w, ":warning: failed to list admins (upstream error; see logs).")
		return
	}
	// Defensive: BindWorkspace stamps owner_id on first claim, so a
	// missing or shape-bad value would only fire on storage
	// corruption. Surface it explicitly instead of interpolating
	// anything that doesn't match the Slack-ID shape into a `<@%s>`
	// mrkdwn link (where a malformed value could break out of the
	// mention surface — defense-in-depth matches the
	// escapeMrkdwnCode posture in views.go). The user-visible copy
	// omits the internal table name; the slog.Error below carries
	// it for on-call triage.
	ownerCopy := "(unknown — the qURL setup record is missing; contact support)"
	if slackdata.LooksLikeSlackUserID(ownerID) {
		ownerCopy = fmt.Sprintf("<@%s>", ownerID)
	} else {
		slog.Error("admin list: workspace_mappings row missing or shape-bad owner_id (storage corruption)", "team_id", teamID, "user_id", callerUserID, "owner_id_len", len(ownerID))
	}
	// Filter the owner out of the admins line so it doesn't duplicate
	// the owner line. The owner is on the admin set by construction
	// (BindWorkspace seeds it), so a single-admin workspace would
	// otherwise render "Owner: <@X>\nAdmins: <@X>". The shape gate
	// pairs with the storage-corruption case above — skip entries
	// that don't match the Slack-ID shape so a corrupted SS member
	// can't render `<@>` or break out of the mention surface.
	otherAdmins := make([]string, 0, len(admins))
	for _, a := range admins {
		// Order matters: shape check FIRST so a corrupted (empty or
		// mrkdwn-shaped) member is skipped before we compare it
		// against ownerID. If ownerID is also corrupted (matching
		// shape-bad) the equality comparison would still fire and
		// silently drop the entry under the wrong reason. Keep this
		// ordering load-bearing.
		if !slackdata.LooksLikeSlackUserID(a) {
			continue
		}
		if a == ownerID {
			continue
		}
		otherAdmins = append(otherAdmins, fmt.Sprintf("<@%s>", a))
	}
	body := "Owner (connected qURL): " + ownerCopy
	if len(otherAdmins) > 0 {
		body += "\nAdmins: " + strings.Join(otherAdmins, ", ")
	}
	// Audit list reads — operators audit-via-paste and need to know
	// who pulled the admin roster when. Mirrors the success slog on
	// add/remove/revoke. `admin_set_size_raw` is the unfiltered
	// `readStringSet` return (counts whatever DDB returned, including
	// empties / shape-bad members that the render loop filters
	// defensively); `non_owner_admin_count` is the count rendered on
	// the user-visible "Admins:" line (after owner + shape filters).
	// Owner is always rendered on its own line; "displayed" would be
	// misleading because both counts contribute to the final reply.
	slog.Info("admin list succeeded", "team_id", teamID, "user_id", callerUserID, "admin_set_size_raw", len(admins), "non_owner_admin_count", len(otherAdmins))
	respondSlack(w, body)
}

func (h *Handler) handleAdminTransferOwnership(w http.ResponseWriter, teamID, enterpriseID, callerUserID string, cmd *Command) {
	transferCtx, transferCancel := context.WithTimeout(h.baseCtx, adminTransferSyncBudget)
	defer transferCancel()
	if !h.requireTransferOwnerSync(transferCtx, w, teamID, callerUserID) {
		return
	}
	target := cmd.UserID
	if target == callerUserID {
		respondSlack(w, "You're already the workspace owner — nothing to do.")
		return
	}
	// Parse normally guarantees this shape for mention arguments; keep this
	// guard as a second fence in case a future parser path sets cmd.UserID
	// directly.
	if !slackdata.LooksLikeSlackUserID(target) {
		respondSlack(w, ":warning: invalid @user mention")
		return
	}
	if h.cfg.SlackUserLookup == nil {
		slog.Error("transfer ownership: Slack user lookup is not configured", "team_id", teamID, "user_id", callerUserID, "target_user_id", target)
		respondSlack(w, ":warning: couldn't verify that target Slack user. Contact your Slack admin.")
		return
	}
	lookupCtx, lookupCancel := context.WithTimeout(transferCtx, adminTargetLookupBudget)
	targetOK, lookupErr := h.cfg.SlackUserLookup(lookupCtx, teamID, enterpriseID, target)
	lookupCancel()
	if lookupErr != nil {
		if errors.Is(lookupErr, ErrSlackMissingScope) {
			respondSlack(w, ":warning: couldn't verify that target Slack user. Reinstall the Slack app to grant `users:read` (or update legacy `SLACK_BOT_TOKEN`), then retry.")
			return
		}
		if errors.Is(lookupErr, auth.ErrSlackBotTokenNotConfigured) {
			respondSlack(w, ":warning: couldn't verify that target Slack user. Reinstall the Slack app (or configure legacy `SLACK_BOT_TOKEN`), then retry.")
			return
		}
		slog.Error("transfer ownership: target user lookup failed", "error", lookupErr, "team_id", teamID, "user_id", callerUserID, "target_user_id", target)
		respondSlack(w, ":warning: couldn't verify that target Slack user. Retry in a moment.")
		return
	}
	if !targetOK {
		slog.Warn("transfer ownership: target user lookup rejected target", "team_id", teamID, "user_id", callerUserID, "target_user_id", target)
		respondSlack(w, fmt.Sprintf(":warning: couldn't verify <@%s> as an active Slack user visible to this app.", target))
		return
	}
	ctx, cancel := context.WithTimeout(transferCtx, adminSyncVerbBudget)
	defer cancel()
	if err := h.cfg.AdminStore.TransferOwnership(ctx, teamID, callerUserID, target); err != nil {
		var se *slackdata.Error
		if errors.As(err, &se) {
			switch {
			case se.StatusCode == http.StatusConflict && se.Code == slackdata.ErrCodeOwnerTransferNotOwner:
				respondSlack(w, ":warning: this command is owner-only (run `/qurl-admin admins` to confirm the current owner before retrying).")
				return
			case se.StatusCode == http.StatusNotFound && se.Code == slackdata.ErrCodeWorkspaceNotBound:
				respondSlack(w, workspaceUnboundReply)
				return
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			slog.Error("transfer ownership deadline reached after mutation attempt", "error", err, "context_error", ctxErr, "team_id", teamID, "user_id", callerUserID, "target_user_id", target)
			respondSlack(w, ":warning: couldn't confirm whether ownership transfer completed before the deadline. Run `/qurl-admin admins` to confirm the current owner before retrying.")
			return
		}
		slog.Error("transfer ownership failed", "error", err, "team_id", teamID, "user_id", callerUserID, "target_user_id", target)
		respondSlack(w, ":warning: failed to transfer ownership (upstream error; run `/qurl-admin admins` to confirm current owner before retrying).")
		return
	}
	slog.Info("admin transfer ownership succeeded", "team_id", teamID, "user_id", callerUserID, "target_user_id", target)
	respondSlack(w, fmt.Sprintf("Transferred qURL workspace ownership to <@%s>. They can now run `/qurl setup`; the qURL account/key will not change until they do.", target))
}
