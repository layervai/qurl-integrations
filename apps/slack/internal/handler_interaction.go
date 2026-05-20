package internal

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
)

// handleInteraction routes Slack interaction POSTs (button clicks,
// modal submissions) to the right inner handler. With the
// admin-claim bootstrap-code modal gone, no view_submission
// callbacks are routed today — every interaction acks 200 with an
// empty body (Slack requires a 200 even when we ignore the event).
//
// The payload arrives form-URL-encoded with a single `payload` field
// carrying the JSON; we decode that nested shape into
// [interactionPayload] so future routes (e.g. the setalias rebind
// confirm modal) can pull state.values out by a known key.
func (h *Handler) handleInteraction(w http.ResponseWriter, body []byte) {
	payload, err := parseInteractionPayload(string(body))
	if err != nil {
		slog.Warn("interaction payload parse failed", "error", err, "body_length", len(body))
		// Slack expects 200 to dismiss the modal even when we can't
		// process it; signaling 4xx here would leave the modal stuck.
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}
	slog.Info("interaction received", "payload", payload)

	if payload.Type != "view_submission" {
		// Buttons + select menus + shortcut entries land here. Ack
		// 200 with an empty body and ignore until a feature wires
		// the dispatch.
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}
	// Unknown callback_id — ack 200 (Slack hangs the modal
	// otherwise) and log so a future view drift is visible.
	slog.Info("unknown view_submission callback_id", "callback_id", payload.View.CallbackID)
	respondJSON(w, http.StatusOK, map[string]any{})
}

// interactionPayload is the subset of Slack's view_submission
// payload we read. Fields we don't touch are intentionally elided so
// the JSON unmarshal is forgiving to upstream additions.
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
// emits a stable group shape. Today no fields need redacting (the
// bootstrap-code modal that did is gone); a future secret-bearing
// block should add a block-id allowlist consulted here before
// emitting state.values.
func (p *interactionPayload) LogValue() slog.Value {
	if p == nil {
		return slog.AnyValue(nil)
	}
	values := make(map[string]map[string]string, len(p.View.State.Values))
	for blockID, actions := range p.View.State.Values {
		inner := make(map[string]string, len(actions))
		for actionID, v := range actions {
			inner[actionID] = v.Value
		}
		values[blockID] = inner
	}
	return slog.GroupValue(
		slog.String("type", p.Type),
		slog.String("team_id", p.Team.ID),
		slog.String("user_id", p.User.ID),
		slog.String("trigger_id", p.TriggerID),
		slog.String("view_id", p.View.ID),
		slog.String("callback_id", p.View.CallbackID),
		slog.Any("state_values", values),
	)
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
