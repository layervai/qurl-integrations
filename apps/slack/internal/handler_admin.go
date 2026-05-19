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
// Admin commands are rejected with a graceful message when
// `Config.AdminStore` is unset — production wires one in cmd/main.go;
// sandbox configs without the three QURL_*_TABLE env vars stay
// crash-free on `/qurl admin`.
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
	// The matching `case AdminClaim` arm inside the switch is dead
	// code that only exists to satisfy the `exhaustive` lint; the
	// short-circuit above is the load-bearing branch.
	if cmd.AdminAction == AdminClaim {
		h.handleAdminClaim(w, values)
		return
	}
	// Every other verb needs AdminStore. Short-circuit once here
	// instead of repeating the guard in each switch arm.
	if !h.requireAdminStoreSync(w) {
		return
	}
	switch cmd.AdminAction {
	case AdminRevoke:
		h.handleAdminRevoke(w, values, teamID, userID, cmd)
	case AdminAdd:
		h.handleAdminAdd(w, values, teamID, userID, cmd)
	case AdminRemove:
		h.handleAdminRemove(w, values, teamID, userID, cmd)
	case AdminList:
		h.handleAdminList(w, values, teamID, userID)
	case AdminClaim:
		// Dead code — short-circuited above so the AdminStore guard
		// is skipped. Present only to satisfy the `exhaustive` lint.
		h.handleAdminClaim(w, values)
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
func (h *Handler) requireAdminSync(w http.ResponseWriter, teamID, userID string) bool {
	if teamID == "" || userID == "" {
		respondSlack(w, ":warning: missing team_id or user_id in slash command payload")
		return false
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, adminGateBudget)
	defer cancel()
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(ctx, teamID, userID)
	if err != nil {
		slog.Error("admin check failed", "error", err, "team_id", teamID, "user_id", userID)
		respondSlack(w, ":warning: failed to verify admin status (upstream error; see logs).")
		return false
	}
	if !isAdmin {
		// Audit non-admin denials so brute-force / curiosity probes
		// are visible to on-call. Distinct slog.Warn level from the
		// success path's slog.Info so dashboards can filter "denied"
		// without scanning every admin command.
		slog.Warn("admin command denied: non-admin", "team_id", teamID, "user_id", userID)
		respondSlack(w, ":warning: this command is admin-only")
		return false
	}
	return true
}

// handleAdminRevoke deletes a single qURL by its `qurl_id`. Reuses the
// customer-facing DELETE so quota/audit logs reflect the action. 404
// surfaces as a friendly "already revoked or typo'd?" message; other
// failures surface a generic upstream-error.
func (h *Handler) handleAdminRevoke(w http.ResponseWriter, _ url.Values, teamID, userID string, cmd *Command) {
	if !h.requireAdminSync(w, teamID, userID) {
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
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			slog.Info("admin revoke: qURL not found (already revoked or typo'd)", "team_id", teamID, "user_id", userID, "qurl_id", cmd.Target)
			respondSlack(w, fmt.Sprintf("`%s` not found — already revoked, or check the qurl_id.", cmd.Target))
			return
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
func (h *Handler) handleAdminAdd(w http.ResponseWriter, _ url.Values, teamID, callerUserID string, cmd *Command) {
	if !h.requireAdminSync(w, teamID, callerUserID) {
		return
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, adminSyncVerbBudget)
	defer cancel()
	target := cmd.UserID
	if err := h.cfg.AdminStore.AddAdmin(ctx, teamID, target); err != nil {
		var se *slackdata.Error
		if errors.As(err, &se) {
			switch se.StatusCode {
			case http.StatusConflict:
				respondSlack(w, fmt.Sprintf("<@%s> is already an admin — nothing to do.", target))
				return
			case http.StatusNotFound:
				respondSlack(w, "Workspace isn't bound — run `/qurl setup` first.")
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
func (h *Handler) handleAdminRemove(w http.ResponseWriter, _ url.Values, teamID, callerUserID string, cmd *Command) {
	if !h.requireAdminSync(w, teamID, callerUserID) {
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
				respondSlack(w, "Workspace isn't bound — run `/qurl setup` first.")
				return
			case se.StatusCode == http.StatusNotFound:
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
func (h *Handler) handleAdminList(w http.ResponseWriter, _ url.Values, teamID, callerUserID string) {
	if !h.requireAdminSync(w, teamID, callerUserID) {
		return
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, adminSyncVerbBudget)
	defer cancel()
	ownerID, admins, err := h.cfg.AdminStore.ListAdmins(ctx, teamID)
	if err != nil {
		var se *slackdata.Error
		if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
			respondSlack(w, "Workspace isn't bound — run `/qurl setup` first.")
			return
		}
		slog.Error("list admins failed", "error", err, "team_id", teamID, "user_id", callerUserID)
		respondSlack(w, ":warning: failed to list admins (upstream error; see logs).")
		return
	}
	// Filter the owner out of the admins line so it doesn't duplicate
	// the owner line. The owner is on the admin set by construction
	// (BindWorkspace seeds it), so a single-admin workspace would
	// otherwise render "Owner: <@X>\nAdmins: <@X>".
	otherAdmins := make([]string, 0, len(admins))
	for _, a := range admins {
		if a == ownerID {
			continue
		}
		otherAdmins = append(otherAdmins, fmt.Sprintf("<@%s>", a))
	}
	body := fmt.Sprintf("Owner: <@%s>", ownerID)
	if len(otherAdmins) > 0 {
		body += "\nAdmins: " + strings.Join(otherAdmins, ", ")
	}
	respondSlack(w, body)
}
