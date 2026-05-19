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
	"github.com/layervai/qurl-integrations/shared/client"
)

// handleAdmin parses the `admin <verb> ...` form via the shared parser
// and dispatches to the action-specific handler. Every recognized verb
// is sync — one DDB GetItem/UpdateItem or one qURL API DELETE — so the
// full gate + body + reply chain fits inside Slack's 3s slash-command
// ack window without the async runAsync hop. Async verbs were retired
// with the v1 admin-surface scope cut.
//
// Parse runs first, then `admin claim` short-circuits, then
// requireAdminStoreSync gates the rest. So on a sandbox deploy without
// the three QURL_*_TABLE env vars: malformed/unknown admin text
// surfaces as a parser error (`:warning: unknown admin action`,
// `:warning: missing @user mention`, etc.); parser-valid verbs other
// than `claim` reply with "Admin features are not configured". The
// distinction is intentional — parser errors are useful feedback
// regardless of DDB wiring, and reordering the checks would mask
// shape errors behind the not-configured surface.
func (h *Handler) handleAdmin(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	cmd, err := Parse(text)
	if err != nil {
		respondSlack(w, ":warning: "+err.Error())
		return
	}
	// No `cmd.Subcommand != SubcmdAdmin` guard: this entry point is
	// only reached from the `text == "admin"` / `HasPrefix("admin ")`
	// branches in handleSlashCommand, and the parser's parseAdmin
	// dispatch produces SubcmdAdmin for that input class. If the
	// parser ever drifts and emits a different subcommand here,
	// cmd.AdminAction will land empty and the switch's `default:`
	// arm renders the same "unknown" copy a guard would have.
	teamID := values.Get(fieldTeamID)
	userID := values.Get(fieldUserID)
	// `admin claim` is routed to [Handler.handleAdminClaim] directly
	// by handleSlashCommand BEFORE this dispatcher (see handler.go).
	// It never reaches handleAdmin in normal Slack traffic. The
	// short-circuit here catches a defensive misroute (e.g. a
	// synthetic test that posts `text=admin claim` through this entry
	// point) AND bypasses requireAdminStoreSync — the whole point of
	// the claim flow is to create the first admin from a bootstrap
	// code on a workspace where CheckAdmin returns (false, "") and
	// AdminStore presence is irrelevant to the modal-open call.
	if cmd.AdminAction == AdminClaim {
		h.handleAdminClaim(w, values)
		return
	}
	// Every other verb needs AdminStore. Short-circuit once here
	// instead of repeating the guard in each switch arm.
	if !h.requireAdminStoreSync(w) {
		return
	}
	// AdminClaim is short-circuited above so its case here is
	// unreachable in practice — but the `exhaustive` linter requires
	// it to be listed explicitly (a `default:` arm doesn't satisfy
	// the check when an enumerated value is omitted).
	switch cmd.AdminAction {
	case AdminClaim: // unreachable in practice — short-circuited above
		// Warn (not Error) so a synthetic test or a misroute is
		// visible in CloudWatch without paging on-call. Operators
		// want to see this; they don't need to be woken up for it.
		slog.Warn("admin claim reached dispatcher switch — defensive misroute (short-circuit above should have caught it)", "team_id", teamID, "user_id", userID)
		h.handleAdminClaim(w, values)
	case AdminRevoke:
		h.handleAdminRevoke(w, teamID, userID, cmd)
	case AdminAdd:
		h.handleAdminAdd(w, teamID, userID, cmd)
	case AdminRemove:
		h.handleAdminRemove(w, teamID, userID, cmd)
	case AdminList:
		h.handleAdminList(w, teamID, userID)
	default:
		respondSlack(w, fmt.Sprintf("Unknown admin action: `%s`. Try `/qurl help`.", cmd.AdminAction))
	}
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
const workspaceUnboundReply = "Workspace isn't bound — run `/qurl setup` first."

// adminGateBudget bounds the sync admin-gate CheckAdmin call so a
// hung DDB can't out-block Slack's 3s slash-command ack window. The
// gate is the FIRST upstream call on every sync admin verb; using
// `context.Background()` here would let a misbehaving upstream
// silently consume the entire user-visible budget. 1s leaves
// adminSyncVerbBudget (1.5s) for the verb body and ~500ms for the
// JSON-encode of the reply.
const adminGateBudget = 1 * time.Second

// adminSyncVerbBudget bounds the verb-body work for sync admin
// verbs (revoke / add / remove / list) so the full gate + body +
// encode chain fits inside Slack's 3s slash-command ack window.
// Without this, asyncWorkTimeout (25s) would silently let the verb
// body wedge past 3s and the user would see no reply at all (Slack
// drops slash-command responses that miss the ack).
//
// 1.5s leaves ~1s of the 3s window for the gate (adminGateBudget=1s)
// and ~500ms for response_encode + write.
const adminSyncVerbBudget = 1500 * time.Millisecond

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

// handleAdminRevoke deletes a single qURL by its `qurl_id`. Reuses the
// customer-facing DELETE so quota/audit logs reflect the action. 404
// surfaces as a friendly "already revoked or typo'd?" message; other
// failures surface a generic upstream-error.
func (h *Handler) handleAdminRevoke(w http.ResponseWriter, teamID, userID string, cmd *Command) {
	if !h.requireAdminSync(w, teamID, userID, AdminRevoke) {
		return
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, adminSyncVerbBudget)
	defer cancel()
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		slog.Error("failed to get API key", "error", err, "team_id", teamID, "user_id", userID)
		respondSlack(w, authErrorMessage(err))
		return
	}
	if err := c.Delete(ctx, cmd.Target); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case http.StatusNotFound:
				slog.Info("admin revoke: qURL not found (already revoked or typo'd)", "team_id", teamID, "user_id", userID, "qurl_id", cmd.Target)
				respondSlack(w, fmt.Sprintf("`%s` not found — already revoked, or check the qurl_id.", cmd.Target))
				return
			case http.StatusUnauthorized, http.StatusForbidden:
				// API key rotated or invalidated — generic upstream-error
				// would leave the admin guessing. Point at /qurl setup so
				// they have a concrete next step.
				slog.Warn("admin revoke: upstream auth rejected (API key rotated?)", "status", apiErr.StatusCode, "team_id", teamID, "user_id", userID, "qurl_id", cmd.Target)
				respondSlack(w, "This workspace's API key was rejected by qurl-service — re-run `/qurl setup` to rotate.")
				return
			}
		}
		slog.Error("revoke qURL failed", "error", err, "team_id", teamID, "user_id", userID, "qurl_id", cmd.Target)
		respondSlack(w, fmt.Sprintf(":warning: failed to revoke `%s` (upstream error; see logs).", cmd.Target))
		return
	}
	slog.Info("admin revoke succeeded", "team_id", teamID, "user_id", userID, "qurl_id", cmd.Target)
	respondSlack(w, fmt.Sprintf("Revoked `%s`.", cmd.Target))
}

// handleAdminAdd promotes the target Slack user to bot admin on the
// caller's workspace. The caller must already be an admin
// (requireAdminSync). The store call is a single conditional
// UpdateItem on workspace_mappings; a CCFE folds into one of two
// user-facing surfaces via the slackdata-side disambiguation read:
//
//   - 409 admin_already_exists → idempotent "already an admin" copy
//   - 404 workspace_not_bound  → "run /qurl setup first" nudge
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
		// admin who fat-fingered `/qurl admin add @themselves` doesn't
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
	respondSlack(w, fmt.Sprintf("Added <@%s> as a bot admin.", target))
}

// handleAdminRemove demotes the target Slack user from bot admin on
// the caller's workspace. Pre-flight rejects a self-remove with a
// clear "ask another admin" copy so a fat-fingered admin doesn't
// accidentally lock themselves out (the store-side owner check
// covers the owner case; the self-remove check covers the non-owner
// case where the user IS demoting themselves).
//
//   - 400 cannot_remove_owner → "transfer via OAuth re-install" copy
//   - 404 admin_not_found     → idempotent "not an admin" copy
//   - 404 workspace_not_bound → "run /qurl setup first" nudge
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
		// an admin who fat-fingers `/qurl admin remove @themselves`
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
				respondSlack(w, fmt.Sprintf("Can't remove <@%s> — they're the workspace owner. Transfer ownership via OAuth re-install first.", target))
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
	respondSlack(w, fmt.Sprintf("Removed <@%s> from bot admins.", target))
}

// handleAdminList renders the workspace owner + the sorted admin set
// in a two-line Slack-mrkdwn reply. The owner is always listed; the
// "Admins:" line is omitted when the admin set contains only the
// owner (or is empty) — operators want a tight reply on a single-
// admin workspace, not a redundant duplicate.
//
//   - 404 workspace_not_bound → "run /qurl setup first" nudge
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
	// Defensive: BindWorkspace stamps owner_id on first claim, so an
	// empty value would only fire on storage corruption. Surface it
	// explicitly instead of rendering a malformed `<@>` mrkdwn link,
	// and log at Error so on-call sees the corruption signal directly
	// rather than reconstructing from user reports.
	ownerCopy := fmt.Sprintf("<@%s>", ownerID)
	if ownerID == "" {
		slog.Error("admin list: workspace_mappings row missing owner_id (storage corruption)", "team_id", teamID, "user_id", callerUserID)
		ownerCopy = "(unknown — workspace_mappings missing owner_id)"
	}
	// Filter the owner out of the admins line so it doesn't duplicate
	// the owner line. The owner is on the admin set by construction
	// (BindWorkspace seeds it), so a single-admin workspace would
	// otherwise render "Owner: <@X>\nAdmins: <@X>". The `ownerID !=
	// ""` guard pairs with the storage-corruption case above — when
	// ownerID is empty we don't want to silently drop legitimately-
	// empty admin entries (impossible today via readStringSet, but
	// defensive against a future contract change).
	otherAdmins := make([]string, 0, len(admins))
	for _, a := range admins {
		if ownerID != "" && a == ownerID {
			continue
		}
		otherAdmins = append(otherAdmins, fmt.Sprintf("<@%s>", a))
	}
	body := "Owner: " + ownerCopy
	if len(otherAdmins) > 0 {
		body += "\nAdmins: " + strings.Join(otherAdmins, ", ")
	}
	// Audit list reads — operators audit-via-paste and need to know
	// who pulled the admin roster when. Mirrors the success slog on
	// add/remove/revoke. admin_set_size is the total stored set
	// (owner-inclusive) so it matches `ListAdmins`'s return shape; the
	// user-visible "Admins:" line filters the owner for tidiness, so
	// the displayed count is `len(otherAdmins)`.
	slog.Info("admin list succeeded", "team_id", teamID, "user_id", callerUserID, "admin_set_size", len(admins), "displayed_admins", len(otherAdmins))
	respondSlack(w, body)
}
