package internal

import (
	"context"
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

// agentGeneratedReplyBlocks wraps renderer-ready Markdown with the feedback
// affordance. Callers must harden model output before appending trusted copy;
// hardening again here would corrupt escaped links and re-process the trusted
// LLM disclaimer.
func agentGeneratedReplyBlocks(markdown string) []any {
	return []any{
		map[string]any{
			blockKitFieldType: "markdown",
			"text":            markdown,
		},
		agentFeedbackBlock(),
	}
}

func (h *Handler) agentFeedbackEnabled() bool {
	return h.cfg.OpenView != nil && h.cfg.PostFeedback != nil
}

// postAgentGeneratedReply preserves the existing text/Markdown delivery seam
// when feedback is not wired. When it is wired, it posts the answer and native
// feedback buttons atomically so no generated response can lose its affordance.
func (h *Handler) postAgentGeneratedReply(log *slog.Logger, env *slackEventEnvelope, threadTS, text string, fallback PostMessageFunc) {
	if !h.agentFeedbackEnabled() || h.cfg.PostMessageBlocks == nil {
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

// handleAgentFeedbackClick keeps positive feedback lightweight and makes
// negative feedback actionable. Helpful clicks receive only an ephemeral
// acknowledgement; not-helpful clicks reuse the existing `/qurl feedback`
// modal, whose eventual submission follows the existing webhook path.
func (h *Handler) handleAgentFeedbackClick(w http.ResponseWriter, payload *interactionPayload, action interactionAction) {
	log := slog.With("surface", "agent_feedback")
	switch strings.TrimSpace(action.Value) {
	case agentFeedbackPositiveValue:
		log.Info("agent feedback received", "rating", agentFeedbackPositiveValue)
		respondJSON(w, http.StatusOK, map[string]any{})
		h.Go(func() {
			_ = h.postResponse(log, payload.ResponseURL, ":white_check_mark: Thanks — your feedback helps improve the Secure Access Agent.")
		})
		return
	case agentFeedbackNegativeValue:
		log.Info("agent feedback received", "rating", agentFeedbackNegativeValue)
		// Continue below and open the reusable feedback modal.
	default:
		log.Warn("agent feedback: unknown rating")
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}

	if !h.agentFeedbackEnabled() {
		respondJSON(w, http.StatusOK, map[string]any{})
		h.Go(func() {
			_ = h.postResponse(log, payload.ResponseURL, ":warning: Feedback isn't enabled on this qURL Slack deployment yet.")
		})
		return
	}
	triggerID := strings.TrimSpace(payload.TriggerID)
	if triggerID == "" {
		respondJSON(w, http.StatusOK, map[string]any{})
		h.Go(func() {
			_ = h.postResponse(log, payload.ResponseURL, ":warning: Slack couldn't open the feedback form. Try `/qurl feedback` instead.")
		})
		return
	}
	meta := FeedbackModalMetadata{
		TeamID:       payload.Team.ID,
		UserID:       payload.User.ID,
		ChannelID:    payload.Channel.ID,
		EnterpriseID: payload.Enterprise.ID,
		ResponseURL:  payload.ResponseURL,
	}
	view, err := FeedbackModal(&meta)
	if err != nil {
		log.Error("agent feedback modal render failed", "error", err)
		respondJSON(w, http.StatusOK, map[string]any{})
		h.Go(func() {
			_ = h.postResponse(log, payload.ResponseURL, ":warning: Couldn't open the feedback form. Try `/qurl feedback` instead.")
		})
		return
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, slackTriggerOpenViewBudget)
	defer cancel()
	err = h.openViewWithGridFallback(ctx, log, meta.TeamID, meta.EnterpriseID, triggerID, view)
	respondJSON(w, http.StatusOK, map[string]any{})
	if err != nil {
		log.Error("agent feedback views.open failed", "error", err)
		h.Go(func() {
			_ = h.postResponse(log, payload.ResponseURL, ":warning: Couldn't open the feedback form. Try `/qurl feedback` instead.")
		})
	}
}
