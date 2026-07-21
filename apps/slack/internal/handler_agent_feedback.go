package internal

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

const (
	agentFeedbackActionID      = "agent_response_feedback"
	agentFeedbackPositiveValue = "helpful"
	agentFeedbackNegativeValue = "not_helpful"
)

// agentFeedbackBlock is the native Slack feedback affordance appended to each
// generated answer or proposal. The action value is a fixed rating, never model
// output or user content.
func agentFeedbackBlock() map[string]any {
	return map[string]any{
		blockKitFieldType: "context_actions",
		blockKitFieldElements: []any{
			map[string]any{
				blockKitFieldType:     "feedback_buttons",
				blockKitFieldActionID: agentFeedbackActionID,
				"positive_button": map[string]any{
					"text":                plainTextObj("Helpful"),
					"accessibility_label": "Mark this qURL response as helpful",
					blockKitFieldValue:    agentFeedbackPositiveValue,
				},
				"negative_button": map[string]any{
					"text":                plainTextObj("Not helpful"),
					"accessibility_label": "Mark this qURL response as not helpful",
					blockKitFieldValue:    agentFeedbackNegativeValue,
				},
			},
		},
	}
}

func agentGeneratedReplyBlocks(markdown string) []any {
	return []any{
		map[string]any{
			blockKitFieldType: "markdown",
			"text":            hardenAgentMarkdown(markdown),
		},
		agentFeedbackBlock(),
	}
}

func (h *Handler) agentFeedbackEnabled() bool {
	return h.cfg.PostFeedback != nil && h.cfg.PostMessageBlocks != nil
}

// postAgentGeneratedReply preserves the existing text/Markdown delivery seam
// when feedback is not wired. When it is wired, it posts the answer and native
// feedback buttons atomically so no generated response can lose its affordance.
func (h *Handler) postAgentGeneratedReply(log *slog.Logger, env *slackEventEnvelope, threadTS, text string, fallback PostMessageFunc) {
	if !h.agentFeedbackEnabled() {
		h.deliverAgentText(log, env, threadTS, text, fallback)
		return
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, agentDeliveryBudget)
	defer cancel()
	if err := h.cfg.PostMessageBlocks(ctx, env.TeamID, env.EnterpriseID, env.Event.Channel, threadTS, agentGeneratedReplyBlocks(text), strings.TrimSpace(text)); err != nil {
		log.Warn("agent: post generated reply with feedback failed; falling back to text", "error", err)
		h.deliverAgentText(log, env, threadTS, text, fallback)
	}
}

func agentFeedbackSummary(value string) (string, bool) {
	switch strings.TrimSpace(value) {
	case agentFeedbackPositiveValue:
		return "Helpful Secure Access Agent response", true
	case agentFeedbackNegativeValue:
		return "Secure Access Agent response needs improvement", true
	default:
		return "", false
	}
}

// handleAgentFeedbackClick promptly acknowledges a signed Slack block action,
// then reuses the existing operator feedback webhook asynchronously. The
// submission contains only the fixed rating plus Slack-issued message metadata.
func (h *Handler) handleAgentFeedbackClick(w http.ResponseWriter, payload *interactionPayload, action interactionAction) {
	summary, ok := agentFeedbackSummary(action.Value)
	if !ok {
		slog.Warn("agent feedback: unknown rating", "team_id", payload.Team.ID)
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}
	if h.cfg.PostFeedback == nil {
		h.Go(func() {
			_ = h.postResponse(slog.Default(), payload.ResponseURL, ":warning: Feedback isn't enabled on this qURL Slack deployment yet.")
		})
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}

	meta := FeedbackModalMetadata{
		TeamID:       payload.Team.ID,
		UserID:       payload.User.ID,
		ChannelID:    payload.Channel.ID,
		EnterpriseID: payload.Enterprise.ID,
		ResponseURL:  payload.ResponseURL,
	}
	details := fmt.Sprintf("Agent message `%s` in channel `%s`.", escapeMrkdwnCode(payload.Container.MessageTS), escapeMrkdwnCode(payload.Channel.ID))
	log := slog.With("surface", "agent_feedback", "team_id", meta.TeamID, "user_id", meta.UserID, "rating", action.Value)
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		body, err := FeedbackMessage(&meta, feedbackTypeOther, summary, details)
		if err != nil {
			log.Error("agent feedback message render failed", "error", err)
			_ = h.postResponse(log, meta.ResponseURL, ":warning: Couldn't send your feedback. Try `/qurl feedback` instead.")
			return
		}
		if err := h.cfg.PostFeedback(ctx, body); err != nil {
			log.Error("agent feedback webhook post failed", "error", err)
			_ = h.postResponse(log, meta.ResponseURL, ":warning: Couldn't send your feedback right now. Try `/qurl feedback` in a moment.")
			return
		}
		_ = h.postResponse(log, meta.ResponseURL, ":white_check_mark: Thanks — your feedback helps improve the Secure Access Agent.")
	}) {
		h.Go(func() { _ = h.postResponse(log, payload.ResponseURL, modalBusyMsg) })
	}
	respondJSON(w, http.StatusOK, map[string]any{})
}
