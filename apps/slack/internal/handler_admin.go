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

// adminUsageMessage is the arg hint shown when `/qurl-admin admin` is
// invoked with no action (bare `admin`, which parses to
// [ErrMissingAdminAction]). Lists the membership + revoke actions so the
// user learns the grammar rather than seeing the terse sentinel. "qURL bot
// admin" (not "workspace admin") is deliberate: membership is the bot's own
// admin set, not Slack's workspace-admin role.
//
// Keep this action roster in sync with the `/qurl-admin help` listing in
// handler.go — the two are maintained independently and would otherwise drift
// when an admin action is added or renamed.
const adminUsageMessage = "Usage:\n• `/qurl-admin admin add @user` — promote a Slack user to qURL bot admin\n• `/qurl-admin admin remove @user` — demote a qURL bot admin\n• `/qurl-admin admin list` — show who connected qURL (the owner) and current bot admins\n• `/qurl-admin admin revoke <qurl_id>` — revoke a single qURL"

// handleAdmin parses the `admin <verb> ...` form via the shared parser
// and dispatches to the action-specific handler. Every recognized verb
// is sync — one DDB GetItem/UpdateItem or one qURL API DELETE — so the
// full gate + body + reply chain fits inside Slack's 3s slash-command
// ack window without the async runAsync hop. Async verbs were retired
// with the v1 admin-surface scope cut.
//
// Parse runs first, then requireAdminStoreSync gates the rest. So on
// a sandbox deploy without the QURL_*_TABLE env vars:
// malformed/unknown admin text surfaces as a parser error
// (`:warning: unknown admin action`, `:warning: missing @user
// mention`, etc.); parser-valid verbs reply with "Admin features are
// not configured". The distinction is intentional — parser errors
// are useful feedback regardless of DDB wiring, and reordering the
// checks would mask shape errors behind the not-configured surface.
//
// The retired `admin claim` verb is rejected at the parser layer
// (ErrUnknownAdminAction) since /qurl setup now seeds the workspace
// admin in the OAuth callback. There is no in-bot path that hands a
// bootstrap code to the user anymore.
func (h *Handler) handleAdmin(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	cmd, err := Parse(text)
	if err != nil {
		// Bare `admin` (no action) parses to ErrMissingAdminAction. List the
		// available actions instead of the terse sentinel so the user learns
		// the grammar. The action roster is public (it's in `/qurl-admin help`),
		// so this hint is ungated — the admin gate is on execution, not
		// discovery, matching the set-alias / set-display-name usage paths.
		if errors.Is(err, ErrMissingAdminAction) {
			respondSlack(w, ":warning: "+adminUsageMessage)
			return
		}
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
	// TrimSpace mirrors handleSlashCommand's other entry points — a
	// whitespace-only team_id or user_id otherwise sneaks past
	// requireAdminSync's `== ""` check.
	teamID := strings.TrimSpace(values.Get(fieldTeamID))
	userID := strings.TrimSpace(values.Get(fieldUserID))
	// Every recognized verb needs AdminStore. Short-circuit once here
	// instead of repeating the guard in each switch arm.
	if !h.requireAdminStoreSync(w) {
		return
	}
	switch cmd.AdminAction { //nolint:exhaustive // dispatch handles only parser-producible actions; gate-audit labels never reach here, default covers the rest
	case AdminRevoke:
		h.handleAdminRevoke(w, teamID, userID, cmd)
	case AdminAdd:
		h.handleAdminAdd(w, teamID, userID, cmd)
	case AdminRemove:
		h.handleAdminRemove(w, teamID, userID, cmd)
	case AdminList:
		h.handleAdminList(w, teamID, userID)
	default:
		// Unreachable in practice — the parser returns
		// ErrUnknownAdminAction before reaching this dispatcher. Kept
		// for refactor safety. The reply intentionally OMITS
		// cmd.AdminAction; even though it's parser-enumerated today,
		// a future parser drift could land an arbitrary string here,
		// and echoing it back risks confusing copy on already-confused
		// input. The user already knows what they typed.
		slog.Warn("admin dispatcher: unknown action reached default arm — parser drift?", "team_id", teamID, "user_id", userID, "action", string(cmd.AdminAction))
		respondSlack(w, "Unknown admin action. Try `/qurl help`.")
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

// adminSyncVerbBudget bounds the verb-body work for sync admin
// verbs (revoke / add / remove / list) so the full gate + body +
// encode chain fits inside Slack's 3s slash-command ack window.
// Without this, asyncWorkTimeout (25s) would silently let the verb
// body wedge past 3s and the user would see no reply at all (Slack
// drops slash-command responses that miss the ack).
//
// 1.2s + adminGateBudget=800ms = 2s of upstream work — leaves ~1s of
// the 3s window for response_encode + write + the Slack-side network
// hop. Generous compared to typical timings (verb is one DDB
// UpdateItem or one qurl-service DELETE, both well under 100ms warm)
// but the headroom is the point: missing Slack's ack costs the user
// any visible reply at all.
const adminSyncVerbBudget = 1200 * time.Millisecond

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
				// cmd.Target echoes are SAFE inside backtick code
				// spans because qurlIDPattern (`^q_[A-Z0-9]{16,64}$`)
				// restricts the charset to ASCII-uppercase + digits
				// — no backtick break-out, no mrkdwn token. If
				// TODO(upstream-rebrand) ever widens the charset,
				// route the echo through truncateForError-equivalent
				// neutralization before interpolating.
				slog.Info("admin revoke: qURL not found (already revoked or typo'd)", "team_id", teamID, "user_id", userID, "qurl_id", cmd.Target)
				respondSlack(w, fmt.Sprintf("`%s` not found — already revoked, or check the qurl_id.", cmd.Target))
				return
			case http.StatusUnauthorized, http.StatusForbidden:
				// API key rotated or invalidated — generic upstream-error
				// would leave the admin guessing. Point at /qurl setup so
				// they have a concrete next step.
				slog.Warn("admin revoke: upstream auth rejected (API key rotated?)", "status", apiErr.StatusCode, "team_id", teamID, "user_id", userID, "qurl_id", cmd.Target)
				respondSlack(w, "This workspace's API key was rejected by the qURL service — re-run `/qurl setup` to rotate.")
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
	if looksLikeSlackUserID(ownerID) {
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
		if !looksLikeSlackUserID(a) {
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

// looksLikeSlackUserID reports whether s matches Slack's documented
// user-ID grammar — `U…` (workspace) or `W…` (Enterprise Grid)
// prefix followed by 8-63 uppercase-alphanumeric chars (9-64 chars
// total). The bounds match the parser-side userMentionPattern
// (`[UW][A-Z0-9]{8,63}`) so a value rejected at parse time is also
// rejected here. Used as a defensive guard on values read from DDB
// before interpolating into Slack mrkdwn `<@%s>` mentions: the
// parser already constrains values written through admin add/remove,
// but owner_id is written by BindWorkspace from the OAuth callback
// (a different code path), and admin_slack_user_ids could in
// principle be hand-mutated. Cheap insurance against a malformed
// value breaking out of the mention surface.
//
// Thin wrapper over slackdata.LooksLikeSlackUserID — the store layer
// owns the shape check because BindWorkspace also depends on it (to
// detect a legacy Auth0-sub owner_id for self-heal). Keeping one
// implementation stops the two from drifting.
func looksLikeSlackUserID(s string) bool {
	return slackdata.LooksLikeSlackUserID(s)
}
