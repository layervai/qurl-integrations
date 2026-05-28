package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// handleInteraction routes Slack interaction POSTs (button clicks,
// modal submissions) to the right inner handler. Unknown interactions
// still ack 200 with an empty body because Slack requires a prompt 200
// even when the bot ignores the event.
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
	slog.Info("interaction received",
		"type", payload.Type,
		"callback_id", payload.View.CallbackID,
		"team_id", payload.Team.ID,
		"user_id", payload.User.ID,
		"view_id", payload.View.ID,
	)

	if payload.Type != "view_submission" {
		// Buttons + select menus + shortcut entries land here. Ack
		// 200 with an empty body and ignore until a feature wires
		// the dispatch.
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}
	switch payload.View.CallbackID {
	case callbackIDTunnelInstall:
		h.handleTunnelInstallSubmission(w, payload)
	default:
		// Unknown callback_id — ack 200 (Slack hangs the modal
		// otherwise) and log so a future view drift is visible.
		slog.Info("unknown view_submission callback_id", "callback_id", payload.View.CallbackID)
		respondJSON(w, http.StatusOK, map[string]any{})
	}
}

func (h *Handler) handleTunnelInstallSubmission(w http.ResponseWriter, payload *interactionPayload) {
	args, fieldErrors := parseTunnelInstallModalArgs(payload.View.State.Values)
	if len(fieldErrors) > 0 {
		respondViewErrors(w, fieldErrors)
		return
	}

	var meta TunnelInstallModalMetadata
	if err := json.Unmarshal([]byte(payload.View.PrivateMetadata), &meta); err != nil {
		slog.Warn("tunnel install modal metadata parse failed", "error", err, "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		respondTunnelInstallModalError(w, "Could not verify this modal. Run /qurl tunnel install again.")
		return
	}
	if meta.TeamID == "" || meta.ChannelID == "" || meta.UserID == "" || meta.ResponseURL == "" {
		slog.Warn("tunnel install modal metadata incomplete", "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		respondTunnelInstallModalError(w, "Could not verify this modal. Run /qurl tunnel install again.")
		return
	}
	// The timestamp is minted and checked by Slack app pods. Platform clock
	// sync should keep drift tiny; stale modals and far-future timestamps both
	// fail closed instead of minting a fresh bootstrap key from stale state.
	modalAge := tunnelBootstrapNow().Sub(time.Unix(meta.CreatedAtUnix, 0))
	if meta.CreatedAtUnix <= 0 || modalAge > tunnelInstallModalTTL || modalAge < -tunnelBootstrapSkew {
		slog.Warn("tunnel install modal expired", "team_id", meta.TeamID, "user_id", meta.UserID, "view_id", payload.View.ID, "created_at_unix", meta.CreatedAtUnix)
		respondTunnelInstallModalError(w, "This modal expired. Run /qurl tunnel install again.")
		return
	}
	// Slack signs the request envelope, not our private_metadata value by
	// itself. These request-field cross-checks prevent replaying modal state
	// across workspaces or users.
	if payload.Team.ID == "" || payload.Team.ID != meta.TeamID {
		slog.Warn("tunnel install modal team mismatch", "payload_team_id", payload.Team.ID, "metadata_team_id", meta.TeamID, "view_id", payload.View.ID)
		respondTunnelInstallModalError(w, "This modal was opened for a different workspace. Run /qurl tunnel install again.")
		return
	}
	if payload.User.ID == "" || payload.User.ID != meta.UserID {
		slog.Warn("tunnel install modal user mismatch", "payload_user_id", payload.User.ID, "metadata_user_id", meta.UserID, "view_id", payload.View.ID)
		respondTunnelInstallModalError(w, "Only the admin who opened this modal can submit it. Run /qurl tunnel install again to start a new setup.")
		return
	}
	if h.cfg.AdminStore == nil {
		respondTunnelInstallModalError(w, "Admin features are not configured on this Slack bot deployment.")
		return
	}
	if h.aliasStore == nil {
		respondTunnelInstallModalError(w, "Channel shortcut storage is not configured on this Slack bot deployment.")
		return
	}

	// Slack expects modal submissions to be acknowledged quickly; keep this
	// synchronous admin re-check bounded so a slow store fails closed. Use the
	// handler base context, matching slash-command admin gates, so a client
	// abort does not cancel the deliberate fail-closed authorization check.
	adminCtx, cancel := context.WithTimeout(h.baseCtx, adminGateBudget)
	defer cancel()
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(adminCtx, meta.TeamID, meta.UserID)
	if err != nil {
		slog.Error("tunnel install modal admin check failed", "error", err, "team_id", meta.TeamID, "user_id", meta.UserID, "view_id", payload.View.ID)
		respondTunnelInstallModalError(w, "Could not verify admin status. Retry in a moment.")
		return
	}
	if !isAdmin {
		slog.Warn("tunnel install modal denied: non-admin", "team_id", meta.TeamID, "user_id", meta.UserID, "view_id", payload.View.ID)
		respondTunnelInstallModalError(w, "This command is admin-only.")
		return
	}

	log := slog.With(
		"command", "tunnel_install_modal",
		"team_id", meta.TeamID,
		"channel_id", meta.ChannelID,
		"user_id", meta.UserID,
		"view_id", payload.View.ID,
	)
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		h.processTunnelInstall(ctx, log, meta.TeamID, meta.ChannelID, meta.UserID, meta.ResponseURL, args)
	}) {
		respondTunnelInstallModalError(w, "Slack bot is busy. Retry in a moment.")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{})
}

func parseTunnelInstallModalArgs(values map[string]map[string]interactionStateValue) (args *tunnelInstallArgs, fieldErrors map[string]string) {
	fieldErrors = map[string]string{}

	slug := strings.TrimPrefix(strings.TrimSpace(interactionStateText(values, tunnelInstallBlockSlug, tunnelInstallActionSlug)), "$")
	if !tunnelSlugPattern.MatchString(slug) {
		fieldErrors[tunnelInstallBlockSlug] = "Use 3-64 lowercase letters, numbers, and hyphens. Start with a letter and end with a letter or number."
	}

	shortcutRaw := strings.TrimSpace(interactionStateText(values, tunnelInstallBlockShortcut, tunnelInstallActionShortcut))
	alias := slug
	if shortcutRaw != "" && !strings.HasPrefix(shortcutRaw, "$") {
		shortcutRaw = "$" + shortcutRaw
	}
	if shortcutRaw != "" {
		var aliasReason string
		alias, aliasReason = validateChannelShortcutToken(shortcutRaw)
		if aliasReason != "" {
			fieldErrors[tunnelInstallBlockShortcut] = aliasReason
		}
	}

	portText, portFound := interactionStateTextOK(values, tunnelInstallBlockLocalPort, tunnelInstallActionLocalPort)
	portRaw := strings.TrimSpace(portText)
	port := defaultTunnelLocalPort
	if !portFound {
		fieldErrors[tunnelInstallBlockLocalPort] = "Use a TCP port from 1 to 65535."
	} else if portRaw != "" {
		var err error
		port, err = strconv.Atoi(portRaw)
		if err != nil || port < 1 || port > 65535 {
			fieldErrors[tunnelInstallBlockLocalPort] = "Use a TCP port from 1 to 65535."
		}
	}

	envRaw := strings.TrimSpace(interactionStateText(values, tunnelInstallBlockEnvironment, tunnelInstallActionEnvironment))
	env, envMsg := parseTunnelEnvironment(envRaw)
	if envRaw == "" || envMsg != "" {
		fieldErrors[tunnelInstallBlockEnvironment] = "Choose one of the listed target environments."
	}

	webRef := strings.TrimSpace(interactionStateText(values, tunnelInstallBlockWebRef, tunnelInstallActionWebRef))
	if envRaw != "" && envMsg == "" {
		if msg := tunnelWebRefValidationMessage(env, webRef); msg != "" {
			fieldErrors[tunnelInstallBlockWebRef] = msg
		}
	}

	if len(fieldErrors) > 0 {
		return nil, fieldErrors
	}
	args = &tunnelInstallArgs{
		Slug:        slug,
		Alias:       alias,
		LocalPort:   port,
		Environment: env,
		WebRef:      webRef,
	}
	return args, nil
}

func interactionStateText(values map[string]map[string]interactionStateValue, blockID, actionID string) string {
	text, _ := interactionStateTextOK(values, blockID, actionID)
	return text
}

func interactionStateTextOK(values map[string]map[string]interactionStateValue, blockID, actionID string) (string, bool) {
	block, ok := values[blockID]
	if !ok {
		return "", false
	}
	value, ok := block[actionID]
	if !ok {
		return "", false
	}
	return value.text(), true
}

func respondViewErrors(w http.ResponseWriter, fieldErrors map[string]string) {
	respondJSON(w, http.StatusOK, map[string]any{
		"response_action": "errors",
		"errors":          fieldErrors,
	})
}

func respondTunnelInstallModalError(w http.ResponseWriter, message string) {
	view, err := TunnelInstallErrorModal(message)
	if err != nil {
		respondViewErrors(w, map[string]string{tunnelInstallBlockSlug: "Tunnel setup failed. Contact support."})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"response_action": "update",
		"view":            json.RawMessage(view),
	})
}

func tunnelWebRefValidationMessage(env tunnelInstallEnvironment, value string) string {
	if value == "" {
		return ""
	}
	if env == tunnelEnvCompose {
		if dockerComposeServicePattern.MatchString(value) {
			return ""
		}
		return "Use a Docker Compose service name with letters, numbers, underscores, or hyphens. Dots are not allowed."
	}
	if dockerContainerRefPattern.MatchString(value) {
		return ""
	}
	return "Use a Docker container name or ID with letters, numbers, dots, underscores, or hyphens."
}

func tunnelWebRefKindValidationMessage(env tunnelInstallEnvironment, kind tunnelInstallWebRefKind) string {
	if kind == tunnelWebRefKindNone {
		return ""
	}
	if env == tunnelEnvCompose {
		if kind == tunnelWebRefKindService {
			return ""
		}
		return "Use `service:<name>` with Docker Compose installs."
	}
	if kind == tunnelWebRefKindService {
		return "Use `service:<name>` only with `env:docker-compose`; use `container:<name>` or `web_container:<name>` for Docker container installs."
	}
	return ""
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
			Values map[string]map[string]interactionStateValue `json:"values"`
		} `json:"state"`
	} `json:"view"`
	TriggerID string `json:"trigger_id"`
}

type interactionStateValue struct {
	Value          string                     `json:"value"`
	SelectedOption *interactionSelectedOption `json:"selected_option"`
}

func (v interactionStateValue) text() string {
	if v.Value != "" {
		return v.Value
	}
	if v.SelectedOption != nil {
		return v.SelectedOption.Value
	}
	return ""
}

type interactionSelectedOption struct {
	Value string `json:"value"`
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
			inner[actionID] = v.text()
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
