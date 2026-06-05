package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// handleFeedback opens the `/qurl feedback` modal. Unlike the guided tunnel
// installer, feedback has no admin gate, so the modal opens synchronously
// inside the slash-command response: a single views.open RPC bounded by
// slackTriggerOpenViewBudget stays comfortably inside Slack's ~3s trigger
// window, and opening inline lets an open failure surface as an immediate
// ephemeral instead of a response_url follow-up.
func (h *Handler) handleFeedback(w http.ResponseWriter, values url.Values) {
	// Both seams must be wired for feedback to work end-to-end: OpenView to
	// show the modal, PostFeedback to deliver the submission. Gate on both so
	// a user never fills in a form that has nowhere to go.
	if h.cfg.OpenView == nil || h.cfg.PostFeedback == nil {
		respondSlack(w, "Feedback isn't enabled on this qURL Slack deployment yet.")
		return
	}
	triggerID := strings.TrimSpace(values.Get(fieldTriggerID))
	if triggerID == "" {
		respondSlack(w, "Slack didn't include a trigger_id, so the feedback form couldn't open. Try `/qurl feedback` again.")
		return
	}
	teamID := strings.TrimSpace(values.Get(fieldTeamID))
	enterpriseID := strings.TrimSpace(values.Get(fieldEnterpriseID))
	meta := FeedbackModalMetadata{
		TeamID:       teamID,
		TeamDomain:   strings.TrimSpace(values.Get(fieldTeamDomain)),
		UserID:       strings.TrimSpace(values.Get(fieldUserID)),
		UserName:     strings.TrimSpace(values.Get(fieldUserName)),
		ChannelID:    strings.TrimSpace(values.Get(fieldChannelID)),
		ChannelName:  strings.TrimSpace(values.Get(fieldChannelName)),
		EnterpriseID: enterpriseID,
		ResponseURL:  strings.TrimSpace(values.Get(fieldResponseURL)),
	}
	view, err := FeedbackModal(&meta)
	if err != nil {
		slog.Error("feedback modal render failed", "error", err, "team_id", teamID)
		respondSlack(w, "Couldn't open the feedback form. Please try again in a moment.")
		return
	}
	log := slog.With("command", "feedback", "team_id", teamID, "user_id", meta.UserID)
	ctx, cancel := context.WithTimeout(h.baseCtx, slackTriggerOpenViewBudget)
	defer cancel()
	// openViewWithGridFallback retries with the Enterprise Grid org token when
	// the workspace itself has none — the same token-owner fallback as the
	// guided tunnel installer.
	if err := h.openViewWithGridFallback(ctx, log, teamID, enterpriseID, triggerID, view); err != nil {
		log.Error("feedback views.open failed", "error", err)
		respondSlack(w, "Couldn't open the feedback form. Please try again in a moment.")
		return
	}
	// Modal is open; the slash command needs only a prompt 200 with no body so
	// Slack doesn't post a redundant ephemeral on top of the modal.
	respondJSON(w, http.StatusOK, map[string]any{})
}

type feedbackArgs struct {
	Type    string
	Summary string
	Details string
}

func parseFeedbackModalArgs(values map[string]map[string]interactionStateValue) (args *feedbackArgs, fieldErrors map[string]string) {
	fieldErrors = map[string]string{}

	typeValue := strings.TrimSpace(interactionStateText(values, feedbackBlockType, feedbackActionType))
	if _, _, ok := feedbackTypeDisplay(typeValue); !ok {
		fieldErrors[feedbackBlockType] = "Choose a feedback type."
	}

	summary := strings.TrimSpace(interactionStateText(values, feedbackBlockSummary, feedbackActionSummary))
	switch {
	case summary == "":
		fieldErrors[feedbackBlockSummary] = "Add a short summary."
	case utf8.RuneCountInString(summary) > feedbackSummaryMaxLen:
		fieldErrors[feedbackBlockSummary] = fmt.Sprintf("Keep the summary under %d characters.", feedbackSummaryMaxLen)
	}

	details := strings.TrimSpace(interactionStateText(values, feedbackBlockDetails, feedbackActionDetails))
	if utf8.RuneCountInString(details) > feedbackDetailsMaxLen {
		fieldErrors[feedbackBlockDetails] = fmt.Sprintf("Keep the details under %d characters.", feedbackDetailsMaxLen)
	}

	if len(fieldErrors) > 0 {
		return nil, fieldErrors
	}
	return &feedbackArgs{Type: typeValue, Summary: summary, Details: details}, nil
}

func (h *Handler) handleFeedbackSubmission(w http.ResponseWriter, payload *interactionPayload) {
	args, fieldErrors := parseFeedbackModalArgs(payload.View.State.Values)
	if len(fieldErrors) > 0 {
		respondViewErrors(w, fieldErrors)
		return
	}

	var meta FeedbackModalMetadata
	if err := json.Unmarshal([]byte(payload.View.PrivateMetadata), &meta); err != nil {
		slog.Warn("feedback modal metadata parse failed", "error", err, "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		respondFeedbackModalError(w, "Couldn't read this form. Run `/qurl feedback` again.")
		return
	}
	// Slack signs the request envelope (including private_metadata), so these
	// cross-checks against the payload's own team/user are defense-in-depth:
	// they stop a captured modal from being replayed to post feedback
	// attributed to a different workspace or user.
	if payload.Team.ID == "" || payload.Team.ID != meta.TeamID {
		slog.Warn("feedback modal team mismatch", "payload_team_id", payload.Team.ID, "metadata_team_id", meta.TeamID, "view_id", payload.View.ID)
		respondFeedbackModalError(w, "This form was opened for a different workspace. Run `/qurl feedback` again.")
		return
	}
	if payload.User.ID == "" || payload.User.ID != meta.UserID {
		slog.Warn("feedback modal user mismatch", "payload_user_id", payload.User.ID, "metadata_user_id", meta.UserID, "view_id", payload.View.ID)
		respondFeedbackModalError(w, "Only the person who opened this form can submit it. Run `/qurl feedback` again.")
		return
	}
	if h.cfg.PostFeedback == nil {
		respondFeedbackModalError(w, "Feedback isn't enabled on this qURL Slack deployment yet.")
		return
	}
	if meta.ResponseURL == "" {
		// Without the slash command's response_url the async worker can't
		// confirm receipt, so a successful post would look silent to the user.
		// Treat it as a structural error so they retry rather than wonder.
		slog.Warn("feedback modal missing response_url", "team_id", meta.TeamID, "user_id", meta.UserID, "view_id", payload.View.ID)
		respondFeedbackModalError(w, "Couldn't read this form. Run `/qurl feedback` again.")
		return
	}

	log := slog.With("command", "feedback_submit", "team_id", meta.TeamID, "user_id", meta.UserID, "view_id", payload.View.ID)
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		h.processFeedback(ctx, log, &meta, args)
	}) {
		respondFeedbackModalError(w, "qURL bot is busy. Try again in a moment.")
		return
	}
	// Empty 200 closes the modal; the async worker confirms receipt (or reports
	// a delivery failure) back through the slash command's response_url.
	respondJSON(w, http.StatusOK, map[string]any{})
}

// processFeedback renders the feedback message and delivers it to the internal
// channel webhook, then confirms receipt to the submitter via the response_url.
// A render or delivery failure posts a retry-shaped ephemeral instead — the
// feedback is never silently dropped.
func (h *Handler) processFeedback(ctx context.Context, log *slog.Logger, meta *FeedbackModalMetadata, args *feedbackArgs) {
	body, err := FeedbackMessage(meta, args.Type, args.Summary, args.Details)
	if err != nil {
		log.Error("feedback message render failed", "error", err, "feedback_type", args.Type)
		_ = h.postErrorResponse(log, meta.ResponseURL, "Couldn't send your feedback. Please try `/qurl feedback` again.", false)
		return
	}
	if err := h.cfg.PostFeedback(ctx, body); err != nil {
		log.Error("feedback webhook post failed", "error", err, "feedback_type", args.Type)
		_ = h.postErrorResponse(log, meta.ResponseURL, "Couldn't send your feedback right now. Please try `/qurl feedback` again in a moment.", false)
		return
	}
	log.Info("feedback submitted", "feedback_type", args.Type)
	_ = h.postResponse(log, meta.ResponseURL, ":white_check_mark: Thanks for the feedback — the qURL team has it.")
}

// respondFeedbackModalError replaces the submitted feedback modal with a
// form-level error view. Mirrors respondTunnelInstallModalError: on a render
// failure it falls back to a field-level error so the user still sees a
// failure path.
func respondFeedbackModalError(w http.ResponseWriter, message string) {
	view, err := FeedbackErrorModal(message)
	if err != nil {
		slog.Error("feedback modal error render failed", "error", err)
		respondViewErrors(w, map[string]string{feedbackBlockSummary: "Couldn't send your feedback. Please try again."})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		respFieldResponseAction: respActionUpdate,
		respFieldView:           json.RawMessage(view),
	})
}
