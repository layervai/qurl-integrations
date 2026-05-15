package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// callbackIDAdminClaim is the modal callback ID for the bootstrap-
// code claim flow. Distinct from any future modal callback so a
// single submission router can dispatch by ID. Mirrors the constant
// in views.go (Go-package-internal — duplicated here so the
// dispatcher can match without a views import).

// interactionAsyncBudget bounds the view_submission handler. Slack
// gives view_submission a 3s ack budget (same as slash commands).
// 2s leaves headroom for the JSON encode + write.
const interactionAsyncBudget = 2 * time.Second

// handleInteraction routes Slack interaction POSTs (button clicks,
// modal submissions) to the right inner handler. Today only the
// `view_submission` type carrying [callbackIDAdminClaim] is wired;
// every other interaction acks 200 with an empty body (Slack
// requires a 200 even when we ignore the event).
//
// The payload arrives form-URL-encoded with a single `payload` field
// carrying the JSON; we decode that nested shape into
// [interactionPayload].
func (h *Handler) handleInteraction(w http.ResponseWriter, body []byte) {
	payload, err := parseInteractionPayload(string(body))
	if err != nil {
		slog.Warn("interaction payload parse failed", "error", err, "body_length", len(body))
		// Slack expects 200 to dismiss the modal even when we can't
		// process it; signaling 4xx here would leave the modal stuck.
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}
	// LogValue redacts secret-bearing blocks (e.g. blockIDClaimCode)
	// before serialization — this slog line is safe to keep even
	// against future log-injection regressions.
	slog.Info("interaction received", "payload", payload)

	if payload.Type != "view_submission" {
		// Buttons + select menus + shortcut entries land here. Ack
		// 200 with an empty body and ignore until a feature wires
		// the dispatch.
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}
	switch payload.View.CallbackID {
	case callbackIDAdminClaim:
		ctx, cancel := context.WithTimeout(h.baseCtx, interactionAsyncBudget)
		defer cancel()
		h.handleAdminClaimSubmit(ctx, w, payload)
	default:
		// Unknown callback_id — ack 200 (Slack hangs the modal
		// otherwise) and log so a future view drift is visible.
		slog.Info("unknown view_submission callback_id", "callback_id", payload.View.CallbackID)
		respondJSON(w, http.StatusOK, map[string]any{})
	}
}

// interactionPayload is the subset of Slack's view_submission
// payload we read. Fields we don't touch are intentionally elided so
// the JSON unmarshal is forgiving to upstream additions.
//
// SECURITY: this struct can carry the bootstrap code in
// `View.State.Values[blockIDClaimCode][actionIDClaimCode].Value`
// (Blocker #3). [interactionPayload.LogValue] redacts every block
// whose ID is in [redactedSubmissionBlockIDs] before slog serializes
// the payload — log sites that pass `payload` to slog go through
// LogValue automatically. To extend redaction to a new
// secret-bearing block, add the block_id to redactedSubmissionBlockIDs
// in views.go rather than hand-rolling masking at each log site.
type interactionPayload struct {
	Type string `json:"type"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	View struct {
		ID              string `json:"id"`
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
		State           struct {
			Values map[string]map[string]struct {
				Value string `json:"value"`
			} `json:"values"`
		} `json:"state"`
	} `json:"view"`
	TriggerID string `json:"trigger_id"`
}

// LogValue implements [slog.LogValuer] so a `slog` call that takes
// the payload as a value (`slog.Info("interaction", "payload", p)`)
// emits a redacted form. Block IDs named in
// [redactedSubmissionBlockIDs] have their value replaced with the
// literal "<redacted>" before reaching the log writer.
//
// Defense-in-depth: a future log line added during incident response
// doesn't have to know about the redaction obligation — the slog
// path consults LogValue automatically.
func (p *interactionPayload) LogValue() slog.Value {
	if p == nil {
		return slog.AnyValue(nil)
	}
	const redactedSentinel = "<redacted>"
	redactedValues := make(map[string]map[string]string, len(p.View.State.Values))
	for blockID, actions := range p.View.State.Values {
		inner := make(map[string]string, len(actions))
		for actionID, v := range actions {
			if IsRedactedSubmissionBlock(blockID) {
				inner[actionID] = redactedSentinel
			} else {
				inner[actionID] = v.Value
			}
		}
		redactedValues[blockID] = inner
	}
	return slog.GroupValue(
		slog.String("type", p.Type),
		slog.String("team_id", p.Team.ID),
		slog.String("user_id", p.User.ID),
		slog.String("trigger_id", p.TriggerID),
		slog.String("view_id", p.View.ID),
		slog.String("callback_id", p.View.CallbackID),
		slog.Any("state_values", redactedValues),
	)
}

// submissionValue extracts state.values[blockID][actionID].value
// from a view-submission payload. Returns "" if missing — the caller
// is expected to surface a friendly "field empty" error.
func (p *interactionPayload) submissionValue(blockID, actionID string) string {
	if p == nil {
		return ""
	}
	block, ok := p.View.State.Values[blockID]
	if !ok {
		return ""
	}
	action, ok := block[actionID]
	if !ok {
		return ""
	}
	return action.Value
}

// parseInteractionPayload decodes the `payload=` form field Slack
// sends on every interaction POST. Returns nil + error if the
// payload field is missing or malformed.
func parseInteractionPayload(formBody string) (*interactionPayload, error) {
	v, err := url.ParseQuery(formBody)
	if err != nil {
		return nil, fmt.Errorf("parse form: %w", err)
	}
	raw := v.Get("payload")
	if raw == "" {
		return nil, errors.New("missing payload field")
	}
	var p interactionPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}
	return &p, nil
}
