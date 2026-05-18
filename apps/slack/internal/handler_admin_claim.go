package internal

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// errCodeBootstrapInvalid is the slackdata error code returned when
// the bootstrap_code is wrong, expired, or already redeemed
// (single-use). Mapped to a friendly user-facing message instead of
// surfacing the raw service title.
const errCodeBootstrapInvalid = "bootstrap_code_invalid"

// adminClaimSuccessMessage is the post-redeem confirmation surfaced
// via DM when PostDM is wired. The slash-command HTTP body has
// already been written by the time the redeem completes (view_submission
// has no response_url), so DM is the only post-success surface.
const adminClaimSuccessMessage = ":white_check_mark: You're now an admin for this workspace."

// modalOpenFailureMessage is the user-facing copy when views.open
// fails. Mirrors the surfaceClaimError pattern: log the raw error,
// show the user a generic line. Slack's views.open returns codes
// like `trigger_id_expired` / `not_authed` / `account_inactive`
// which are useless or scary to end users.
const modalOpenFailureMessage = "Could not open the modal. Please retry the command."

// handleAdminClaim opens the bootstrap-code modal. The code is NEVER
// accepted as a slash-command argument — the parser's parseAdmin
// rejects `admin claim <args>` (Blocker #3). The modal collects the
// code via a plain_text_input whose block_id is in
// [redactedSubmissionBlockIDs], so the bot's logging boundary
// redacts the code on submission.
//
// views.open must hit Slack within the 3s slash-command budget —
// async-defer would fire after the trigger_id has expired (Slack
// rotates trigger IDs after one use). Sync dispatch is the only
// correct shape.
func (h *Handler) handleAdminClaim(w http.ResponseWriter, values url.Values) {
	if h.cfg.OpenView == nil {
		// Bot token isn't wired — surface a friendly message rather
		// than nil-deref. Production wires OpenView via cmd/main.go
		// (Fargate runtime) using the workspace bot token.
		respondSlack(w, ":warning: Modal cannot be opened: Slack web API is not configured on this deployment.")
		return
	}
	triggerID := strings.TrimSpace(values.Get(fieldTriggerID))
	if triggerID == "" {
		respondSlack(w, ":warning: Slack did not provide a trigger_id. Please retry the command.")
		return
	}
	view, err := AdminClaimModal()
	if err != nil {
		slog.Error("admin claim modal render failed", "error", err)
		respondSlack(w, ":warning: "+modalOpenFailureMessage)
		return
	}
	// Bound views.open with a tight budget — the slash-command ack
	// must land inside Slack's 3s window. h.baseCtx already carries
	// SIGTERM propagation; layering a per-request deadline keeps a
	// hung Slack edge from holding the request goroutine.
	ctx, cancel := context.WithTimeout(h.baseCtx, adminClaimViewOpenBudget)
	defer cancel()
	if err := h.cfg.OpenView(ctx, triggerID, view); err != nil {
		// Generic surface to the user; raw error in slog. A
		// `trigger_id_expired` reply leaking through would tell the
		// user something they can't act on usefully.
		slog.Warn("views.open failed for admin claim", "error", err, "trigger_id_set", triggerID != "")
		respondSlack(w, ":warning: "+modalOpenFailureMessage)
		return
	}
	// Empty 200 body = ack the slash command with no spinner. The
	// modal itself is the user-visible response.
	respondJSON(w, http.StatusOK, map[string]any{})
}

// handleAdminClaimSubmit is invoked by [Handler.handleInteraction]
// when a view_submission carries [callbackIDAdminClaim]. The flow:
//  1. Extract bootstrap_code from the redacted block (the value
//     never reaches slog because [interactionPayload.LogValue]
//     consults [IsRedactedSubmissionBlock] before serializing).
//  2. Call slackdata.Store.RedeemBootstrap (atomic single-use flip
//     against the bootstrap_codes table).
//  3. Surface the result via DM if PostDM is wired, else via the
//     modal close (view_submission has no response_url).
func (h *Handler) handleAdminClaimSubmit(ctx context.Context, w http.ResponseWriter, payload *interactionPayload) {
	if h.cfg.AdminStore == nil {
		// No DDB → no redeem path. Surface a field-level error so
		// the user knows to ping support.
		respondModalError(w, blockIDClaimCode, "Admin features are not configured for this deployment.")
		return
	}
	code := strings.TrimSpace(payload.submissionValue(blockIDClaimCode, actionIDClaimCode))
	if code == "" {
		// Slack would normally reject this client-side via the
		// input block's required-by-default behavior, but a hand-
		// crafted POST could bypass it. Surface a view_submission
		// errors envelope so the modal stays open with the field
		// highlighted.
		respondModalError(w, blockIDClaimCode, "Bootstrap code is required.")
		return
	}

	teamID := payload.Team.ID
	userID := payload.User.ID
	if teamID == "" || userID == "" {
		respondModalError(w, blockIDClaimCode, "Slack did not include workspace context. Please retry the command.")
		return
	}

	mapping, err := h.cfg.AdminStore.RedeemBootstrap(ctx, code, teamID, userID)
	if err != nil {
		h.surfaceClaimError(ctx, w, userID, err)
		return
	}
	// Persist the workspace mapping so CheckAdmin returns true on
	// subsequent /qurl admin commands. RedeemBootstrap atomically
	// burns the one-time code but leaves the workspace_mappings row
	// to the caller — without this Put the user is told they're an
	// admin but every follow-up admin verb returns "you are not an
	// admin." Bind seeds admin_slack_user_ids with the redeemer's
	// userID.
	if bindErr := h.cfg.AdminStore.BindWorkspace(ctx, mapping, userID); bindErr != nil {
		// The bootstrap code is already burned. Surface the failure
		// instead of lying about the outcome — surfaceClaimError maps
		// 409 (workspace already bound to a different owner) to the
		// right copy and falls everything else through the generic
		// path so operators see the failure in slog.
		h.surfaceClaimError(ctx, w, userID, bindErr)
		return
	}
	slog.Info("admin claim redeemed", "team_id", teamID, "user_id", userID)

	// DM the success message if wired; otherwise the modal close
	// is the only user-visible signal.
	if h.cfg.PostDM != nil {
		if dmErr := h.cfg.PostDM(ctx, userID, adminClaimSuccessMessage); dmErr != nil {
			slog.Warn("admin claim success DM failed", "error", dmErr, "user_id", userID)
		}
	}
	// Empty-body 200 closes the modal.
	respondJSON(w, http.StatusOK, map[string]any{})
}

// surfaceClaimError maps a redeem failure to a user-visible response.
//
//   - 410 (gone — code expired/already used) + code=`bootstrap_code_invalid`
//     get the user-facing "invalid or expired" copy in a field-level
//     modal error.
//   - 409 (conflict) gets the "this workspace already has an admin
//     claimed" copy in a field-level modal error.
//   - 404 (almost always a deployment misroute, NOT user input) falls
//     through to the generic + DM path so the user gets one signal
//     and the operator sees the misroute via slog.
//   - Everything else falls through to the generic path.
//
// Single-signal contract: when PostDM is wired we DM ONE :warning:
// AND close the modal (empty 200). When PostDM is not wired we keep
// the modal open with a field-level error so the user has any
// signal at all. We never both DM AND keep the modal open — that
// would surface two contradictory signals (modal still open = the
// submission was rejected; DM saying it failed = redundant).
func (h *Handler) surfaceClaimError(ctx context.Context, w http.ResponseWriter, userID string, err error) {
	var ae *slackdata.Error
	if errors.As(err, &ae) {
		switch {
		case ae.Code == errCodeBootstrapInvalid, ae.StatusCode == http.StatusGone:
			respondModalError(w, blockIDClaimCode, "Code is invalid or expired.")
			return
		case ae.StatusCode == http.StatusConflict:
			respondModalError(w, blockIDClaimCode, "This workspace already has an admin claimed.")
			return
		}
	}
	logUnmappedClaimError(err, userID)

	if h.cfg.PostDM == nil {
		// No DM surface — keep the modal open so the user has any
		// signal at all.
		respondModalError(w, blockIDClaimCode, "Could not redeem code. Please try again.")
		return
	}
	if dmErr := h.cfg.PostDM(ctx, userID, ":warning: Could not claim workspace. Please try again or contact LayerV support."); dmErr != nil {
		// DM failed — fall through to the field-level error so the
		// user has at least one signal.
		slog.Warn("admin claim error DM failed; falling back to field-level modal error", "error", dmErr, "user_id", userID) //nolint:gosec // G706: slog's JSON handler escapes control chars in attribute values; same posture as handler.go's request-path slog sites.
		respondModalError(w, blockIDClaimCode, "Could not redeem code. Please try again.")
		return
	}
	// DM landed — close the modal so the user sees ONE signal.
	respondJSON(w, http.StatusOK, map[string]any{})
}

// logUnmappedClaimError picks the slog level based on the error
// signal. A 404 from the redeem path is almost always a deployment
// misroute and should page operators (Error level); other unmapped
// failures are routine churn (Warn).
func logUnmappedClaimError(err error, userID string) {
	var ae *slackdata.Error
	if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
		slog.Error("admin claim redeem returned 404 — likely a misrouted bootstrap_codes table or DDB endpoint", "error", err, "user_id", userID) //nolint:gosec // G706: see surfaceClaimError — slog escapes tainted attribute values.
		return
	}
	slog.Warn("admin claim redeem failed", "error", err, "user_id", userID) //nolint:gosec // G706: see surfaceClaimError — slog escapes tainted attribute values.
}

// respondModalError writes a Slack-prescribed view_submission errors
// envelope:
//
//	{
//	  "response_action": "errors",
//	  "errors": { "<block_id>": "<message>" }
//	}
//
// Returning this body keeps the modal open with the named field
// highlighted by the message. Useful for "code is invalid or
// expired" so the user can correct without re-issuing the slash
// command.
func respondModalError(w http.ResponseWriter, blockID, message string) {
	respondJSON(w, http.StatusOK, map[string]any{
		modalKeyResponseAction: modalResponseActionErrors,
		modalKeyErrors:         map[string]string{blockID: message},
	})
}

// Slack view_submission errors-envelope constants. Distinguishing
// the action-value constant ("errors") from the key constants means
// a future Slack-side rename of either side doesn't silently corrupt
// the other.
const (
	modalKeyResponseAction    = "response_action"
	modalKeyErrors            = "errors"
	modalResponseActionErrors = "errors"
)

// adminClaimViewOpenBudget bounds the views.open call inside the
// slash-command ack window. Slack's web API typically responds in
// <500ms; 2s catches a slow edge without blowing through the 3s
// ack budget (leaves ~1s for HMAC verify, parse, marshal, write).
const adminClaimViewOpenBudget = 2 * time.Second
