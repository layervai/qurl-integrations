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

	switch payload.Type {
	case "block_actions":
		// Button clicks in messages — currently the per-row "Create qURL"
		// button on `/qurl list`. handleBlockActions acks and dispatches.
		h.handleBlockActions(w, payload)
	case "view_submission":
		switch payload.View.CallbackID {
		case callbackIDTunnelInstall:
			h.handleTunnelInstallSubmission(w, payload)
		case callbackIDTunnelEdit:
			h.handleTunnelEditSubmission(w, payload)
		case callbackIDExposeURL:
			h.handleExposeURLSubmission(w, payload)
		case callbackIDFeedback:
			h.handleFeedbackSubmission(w, payload)
		default:
			// Unknown callback_id — ack 200 (Slack hangs the modal
			// otherwise) and log so a future view drift is visible.
			slog.Info("unknown view_submission callback_id", "callback_id", payload.View.CallbackID)
			respondJSON(w, http.StatusOK, map[string]any{})
		}
	default:
		// Select menus, shortcuts, and any other interaction type we
		// don't wire yet. Ack 200 with an empty body and ignore.
		respondJSON(w, http.StatusOK, map[string]any{})
	}
}

// handleBlockActions routes Slack block_actions interactions (button clicks in
// posted messages) from `/qurl list` rows: "Create qURL" (mint a one-time qURL,
// same resolve→authorize→mint work as `/qurl get $<slug>`) and the admin-only
// "Edit" (open the tunnel edit modal).
//
// Slack requires a fast 200 ack on the interaction, so the mint runs on the
// bounded async pool and the link is delivered out-of-band via the
// interaction's response_url as a NEW ephemeral message (replace_original
// defaults to false), leaving the list message itself intact. The Edit button
// opens a modal (views.open) inside Slack's trigger window — see
// handleListEditClick.
func (h *Handler) handleBlockActions(w http.ResponseWriter, payload *interactionPayload) {
	// `/qurl-admin expose` chooser buttons open a guided modal; checked first
	// (distinct action_ids) so a click routes to the opener, not a list mint.
	if _, ok := findActionByID(payload.Actions, exposeConnectorActionID); ok {
		h.handleExposeConnectorClick(w, payload)
		return
	}
	if _, ok := findActionByID(payload.Actions, exposeURLActionID); ok {
		h.handleExposeURLClick(w, payload)
		return
	}
	// Revoke and Edit are checked before Create: a row carries all three
	// buttons, but a single click yields exactly one matching action_id, so
	// matching on action_id routes each button to its own handler.
	if revokeAction, ok := findActionByID(payload.Actions, listRevokeTunnelActionID); ok {
		h.handleListRevokeClick(w, payload, revokeAction)
		return
	}
	// Edit is checked before Create so a row carrying both routes to the modal
	// opener rather than the mint when Edit is the clicked element.
	if editAction, ok := findActionByID(payload.Actions, listEditTunnelActionID); ok {
		h.handleListEditClick(w, payload, editAction)
		return
	}
	action, ok := findActionByID(payload.Actions, listCreateQurlActionID)
	if !ok {
		// A button we don't handle (or an empty actions array). Ack and
		// ignore so Slack doesn't surface an error to the clicking user.
		// Log the action_ids present so a future button that's rendered
		// into a message but never wired here surfaces as a breadcrumb
		// rather than a silent no-op.
		slog.Info("block_actions: no recognized action", "team_id", payload.Team.ID, "action_ids", blockActionIDs(payload.Actions))
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}

	responseURL := payload.ResponseURL
	log := slog.With(
		"command", "list_create_qurl",
		"team_id", payload.Team.ID,
		"channel_id", payload.Channel.ID,
		"user_id", payload.User.ID,
	)

	// The button value is the `$<slug>`/`$<alias>` token (sigil stripped)
	// written when the list rendered. Re-validate it through the SAME
	// grammar `/qurl get` uses so a malformed value fails the same way a
	// typed token would, instead of reaching the resolve/mint path or
	// burning an upstream slug lookup. Our own buttons always carry a
	// valid token, so this is defense-in-depth; the notice is posted
	// out-of-band (h.Go, not the async pool) to keep the ack prompt.
	token, tokErr := parseAliasToken("$" + strings.TrimSpace(action.Value))
	if tokErr != nil {
		log.Warn("list create-qurl: button carried an unparseable token", "error", tokErr)
		h.Go(func() { _ = h.postResponse(log, responseURL, ":warning: "+unexpectedGetShapeMessage) })
		respondJSON(w, http.StatusOK, map[string]any{})
		return
	}

	cmd := &Command{
		Subcommand: SubcmdGet,
		Alias:      token,
		Flags:      map[string]string{},
		Raw:        "get $" + token,
	}
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		h.processButtonGet(ctx, log, responseURL, payload.Team.ID, payload.Channel.ID, payload.User.ID, payload.TriggerID, cmd)
	}) {
		// Pool saturated — don't let the click be a silent no-op. h.Go is
		// wg-tracked but does NOT consume an async slot, so reporting the
		// saturation can't itself deepen it.
		log.Warn("async pool saturated — dropping list Create qURL click")
		h.Go(func() { _ = h.postResponse(log, responseURL, ackBusy) })
	}
	respondJSON(w, http.StatusOK, map[string]any{})
}

// processButtonGet is the async-worker body for the `/qurl list`
// "Create qURL" button. It mints a one-time qURL for the row's resource via
// the same [Handler.getWork] pipeline as `/qurl get $<token>` (delivery guard
// → rate-limit → resolve token → channel-authorize → mint) and posts the
// outcome to the interaction's response_url. The cmd carries no dm/reason
// flags — the button is the plain one-time-use mint.
func (h *Handler) processButtonGet(ctx context.Context, log *slog.Logger, responseURL, teamID, channelID, userID, triggerID string, cmd *Command) {
	if channelID == "" {
		// Channel-scope guard mirrors processGet: the resolve path is
		// channel-scoped, so a channel-less interaction can't authorize.
		log.Warn("list create-qurl: empty channel_id; refusing channel-less invocation")
		_ = h.postResponse(log, responseURL, ":warning: "+channelRequiredMessage)
		return
	}
	text, err := h.getWork(ctx, log, getWorkArgs{
		cmd:       cmd,
		teamID:    teamID,
		channelID: channelID,
		userID:    userID,
		triggerID: triggerID,
	})
	h.finishGet(log, responseURL, text, err)
}

// findActionByID returns the first action in a block_actions payload whose
// action_id matches. Slack sends one entry per clicked element, so a single
// button click yields exactly one match and taking the first is correct for
// that contract. Matching on action_id keeps an unrelated button on a future
// message from triggering the wrong handler.
func findActionByID(actions []interactionAction, actionID string) (interactionAction, bool) {
	for _, a := range actions {
		if a.ActionID == actionID {
			return a, true
		}
	}
	return interactionAction{}, false
}

// blockActionIDs lists the action_ids present in a block_actions payload,
// for the no-recognized-action log breadcrumb.
func blockActionIDs(actions []interactionAction) []string {
	ids := make([]string, 0, len(actions))
	for _, a := range actions {
		ids = append(ids, a.ActionID)
	}
	return ids
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
		respondTunnelInstallModalError(w, "Could not verify this modal. Run /qurl-admin expose-connector again.")
		return
	}
	if meta.TeamID == "" || meta.ChannelID == "" || meta.UserID == "" || meta.ResponseURL == "" {
		slog.Warn("tunnel install modal metadata incomplete", "team_id", payload.Team.ID, "user_id", payload.User.ID, "view_id", payload.View.ID)
		respondTunnelInstallModalError(w, "Could not verify this modal. Run /qurl-admin expose-connector again.")
		return
	}
	// The Slack request signature covers the full form body, including the
	// view_submission payload and its private_metadata, so CreatedAtUnix is
	// tamper-resistant once Slack submits the modal. It is still only freshness
	// state minted by our modal JSON; the team/user cross-checks below are the
	// authorization boundary.
	// The timestamp is minted and checked by Slack app pods. Platform clock
	// sync should keep drift tiny; stale modals and far-future timestamps both
	// fail closed instead of minting a fresh bootstrap key from stale state.
	modalAge := h.now().Sub(time.Unix(meta.CreatedAtUnix, 0))
	if meta.CreatedAtUnix <= 0 || modalAge > tunnelInstallModalTTL || modalAge < -tunnelBootstrapSkew {
		slog.Warn("tunnel install modal expired", "team_id", meta.TeamID, "user_id", meta.UserID, "view_id", payload.View.ID, "created_at_unix", meta.CreatedAtUnix, "modal_age_ms", modalAge.Milliseconds())
		respondTunnelInstallModalError(w, "This modal expired. Run /qurl-admin expose-connector again.")
		return
	}
	// Slack signs the request envelope, not our private_metadata value by
	// itself. These request-field cross-checks prevent replaying modal state
	// across workspaces or users.
	if payload.Team.ID == "" || payload.Team.ID != meta.TeamID {
		slog.Warn("tunnel install modal team mismatch", "payload_team_id", payload.Team.ID, "metadata_team_id", meta.TeamID, "view_id", payload.View.ID)
		respondTunnelInstallModalError(w, "This modal was opened for a different workspace. Run /qurl-admin expose-connector again.")
		return
	}
	if payload.User.ID == "" || payload.User.ID != meta.UserID {
		slog.Warn("tunnel install modal user mismatch", "payload_user_id", payload.User.ID, "metadata_user_id", meta.UserID, "view_id", payload.View.ID)
		respondTunnelInstallModalError(w, "Only the admin who opened this modal can submit it. Run /qurl-admin expose-connector again to start a new setup.")
		return
	}
	if h.cfg.AdminStore == nil {
		respondTunnelInstallModalError(w, "Admin features are not configured on this Slack bot deployment.")
		return
	}
	if h.aliasStore == nil {
		respondTunnelInstallModalError(w, "Channel alias storage is not configured on this Slack bot deployment.")
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
	setupStartedAt := time.Unix(meta.CreatedAtUnix, 0)
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		h.processTunnelInstall(ctx, log, meta.TeamID, meta.ChannelID, meta.UserID, meta.ResponseURL, args, setupStartedAt)
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
	// Slack marks this input required, so a missing block means client drift or
	// malformed test data. Surface the same field error as an empty value; the
	// operator action is identical.
	if !portFound || portRaw == "" {
		fieldErrors[tunnelInstallBlockLocalPort] = "Use a TCP port from 1 to 65535."
	} else {
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
		// Web-ref grammar depends on the selected environment. If Slack ever
		// sends an invalid environment value, show that primary field error
		// first instead of guessing which web-ref grammar to apply.
		if msg := tunnelWebRefValidationMessage(env, webRef); msg != "" {
			fieldErrors[tunnelInstallBlockWebRef] = msg
		}
	}

	if len(fieldErrors) > 0 {
		return nil, fieldErrors
	}
	// Re-check at the construction boundary so a future edit cannot carry a
	// stale Docker/Compose web ref after relaxing the earlier field-error path.
	if msg := tunnelWebRefValidationMessage(env, webRef); msg != "" {
		fieldErrors[tunnelInstallBlockWebRef] = msg
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

// interactionStateConversations reads a multi_conversations_select's
// selected_conversations from the submitted view state. Returns nil when the
// block/action is absent — an optional channel multi-select left empty submits
// with no entry, which the Edit modal treats as "no channels selected".
func interactionStateConversations(values map[string]map[string]interactionStateValue, blockID, actionID string) []string {
	block, ok := values[blockID]
	if !ok {
		return nil
	}
	return block[actionID].SelectedConversations
}

func respondViewErrors(w http.ResponseWriter, fieldErrors map[string]string) {
	respondJSON(w, http.StatusOK, map[string]any{
		respFieldResponseAction: "errors",
		"errors":                fieldErrors,
	})
}

func respondTunnelInstallModalError(w http.ResponseWriter, message string) {
	view, err := TunnelInstallErrorModal(message)
	if err != nil {
		slog.Error("tunnel install modal error render failed", "error", err)
		// Last-ditch fallback: Slack may silently drop this field-level error if
		// the current view no longer contains the slug block, but it still gives
		// the original install modal a user-visible failure path.
		respondViewErrors(w, map[string]string{tunnelInstallBlockSlug: "qURL tunnel setup failed. Contact support."})
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		respFieldResponseAction: respActionUpdate,
		respFieldView:           json.RawMessage(view),
	})
}

func tunnelWebRefValidationMessage(env tunnelInstallEnvironment, value string) string {
	if value == "" {
		return ""
	}
	switch env {
	case tunnelEnvCompose:
		if dockerComposeServicePattern.MatchString(value) {
			return ""
		}
		return "Use a Docker Compose service name with letters, numbers, underscores, or hyphens. Dots are not allowed."
	case tunnelEnvDocker:
		if dockerContainerRefPattern.MatchString(value) {
			return ""
		}
		return "Use a Docker container name or ID with letters, numbers, dots, underscores, or hyphens."
	case tunnelEnvECSFargate, tunnelEnvKubernetes:
		return "Leave blank for ECS/Fargate and Kubernetes; those installs run the sidecar inside the same task or pod."
	default:
		return "Choose a target environment before setting a Docker service or container."
	}
}

// tunnelWebRefKindValidationMessage protects the typed-command grammar. The
// guided modal has a single optional web-ref field, so it validates via
// tunnelWebRefValidationMessage after reading the selected environment.
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
	if kind == tunnelWebRefKindContainer && env != tunnelEnvDocker {
		return "Use `container:<name>` or `web_container:<name>` only with Docker container installs."
	}
	return ""
}

// interactionPayload is the subset of Slack's view_submission and
// block_actions payloads we read. Fields we don't touch are intentionally
// elided so the JSON unmarshal is forgiving to upstream additions.
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
	// Channel, ResponseURL, and Actions are populated on block_actions
	// (button click) payloads. Channel is the conversation the button was
	// clicked in (the mint authorizes against it); ResponseURL is where
	// the minted link is delivered; Actions carries the clicked element(s).
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	ResponseURL string              `json:"response_url"`
	Actions     []interactionAction `json:"actions"`
}

// interactionAction is one entry of a block_actions payload's `actions`
// array (the elements the user interacted with). For our list button,
// Value carries the `$<slug>`/`$<alias>` token (sigil stripped).
type interactionAction struct {
	ActionID string `json:"action_id"`
	Value    string `json:"value"`
}

type interactionStateValue struct {
	Value          string                     `json:"value"`
	SelectedOption *interactionSelectedOption `json:"selected_option"`
	// SelectedConversations carries a multi_conversations_select's chosen
	// conversation IDs (the /qurl list Edit modal's "expose to channels"
	// field). Absent for plain inputs and static selects.
	SelectedConversations []string `json:"selected_conversations"`
}

func (v interactionStateValue) text() string {
	// Slack block elements populate either value (plain inputs) or
	// selected_option (static_select). If a malformed payload sends both,
	// prefer the direct value because it is the text-input contract.
	if v.Value != "" {
		return v.Value
	}
	if v.SelectedOption != nil {
		return v.SelectedOption.Value
	}
	return ""
}

var interactionStateLogAllowlist = map[string]map[string]struct{}{
	tunnelInstallBlockSlug: {
		tunnelInstallActionSlug: {},
	},
	tunnelInstallBlockShortcut: {
		tunnelInstallActionShortcut: {},
	},
	tunnelInstallBlockEnvironment: {
		tunnelInstallActionEnvironment: {},
	},
	tunnelInstallBlockLocalPort: {
		tunnelInstallActionLocalPort: {},
	},
	tunnelInstallBlockWebRef: {
		tunnelInstallActionWebRef: {},
	},
}

func interactionStateLogValues(values map[string]map[string]interactionStateValue) map[string]map[string]string {
	logValues := make(map[string]map[string]string, len(values))
	for blockID, actions := range values {
		allowedActions, ok := interactionStateLogAllowlist[blockID]
		if !ok {
			continue
		}
		inner := make(map[string]string, len(actions))
		for actionID, v := range actions {
			if _, ok := allowedActions[actionID]; !ok {
				continue
			}
			inner[actionID] = v.text()
		}
		if len(inner) > 0 {
			logValues[blockID] = inner
		}
	}
	return logValues
}

type interactionSelectedOption struct {
	Value string `json:"value"`
}

// LogValue implements [slog.LogValuer] so a `slog` call that takes
// the payload as a value (`slog.Info("interaction", "payload", p)`)
// emits a stable group shape. State values are emitted only for known
// non-secret tunnel-install blocks; future secret-bearing blocks are redacted
// by default unless explicitly added to interactionStateLogAllowlist.
func (p *interactionPayload) LogValue() slog.Value {
	if p == nil {
		return slog.AnyValue(nil)
	}
	return slog.GroupValue(
		slog.String("type", p.Type),
		slog.String("team_id", p.Team.ID),
		slog.String("user_id", p.User.ID),
		slog.String("trigger_id", p.TriggerID),
		slog.String("view_id", p.View.ID),
		slog.String("callback_id", p.View.CallbackID),
		slog.Any("state_values", interactionStateLogValues(p.View.State.Values)),
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
