package internal

import (
	"encoding/json"
	"fmt"
)

// Block Kit IDs for the `/qurl-admin expose` chooser and the URL-expose modal.
const (
	// exposeConnectorActionID / exposeURLActionID are the action_ids on the two
	// buttons posted by `/qurl-admin expose`. A click opens the matching guided
	// modal — the connector installer (reusing TunnelInstallModal) or the
	// URL-resource picker (ExposeURLModal).
	exposeConnectorActionID = "expose_connector"
	exposeURLActionID       = "expose_url"

	// callbackIDExposeURL is the view callback_id for the URL-expose modal's
	// view_submission (routed in handleInteraction).
	callbackIDExposeURL = "expose_url_modal"

	// URL-expose modal block/action ids.
	exposeURLBlockResource  = "expose_url_resource"
	exposeURLActionResource = "expose_url_resource_select"
	exposeURLBlockAlias     = "expose_url_alias"
	exposeURLActionAlias    = "expose_url_alias_input"

	// exposeURLMaxOptions caps the resource dropdown at Slack's static_select
	// option limit. The first-page scan (listResourcesScanLimit=100) already
	// bounds the candidate set; this is the hard ceiling so a full page can't
	// render a view Slack rejects.
	exposeURLMaxOptions = 100
	// slackOptionTextMaxRunes is Slack's per-option text cap (75 chars). Option
	// labels are truncated to it so a long target URL or display name can't 400
	// the view at views.open.
	slackOptionTextMaxRunes = 75
)

// exposeOpenFailedMessage is the ephemeral shown (via the chooser's
// response_url) when an Expose button can't open its modal — a stale trigger, a
// rate-limited views.open, or a render failure. Mirrors listEditOpenFailedMessage.
const exposeOpenFailedMessage = "Couldn't open the dialog. Run `/qurl-admin expose` and tap the button again."

// exposeChooserBlocks builds the two-button picker posted by `/qurl-admin
// expose`: "Expose qURL Connector" opens the guided connector installer and
// "Expose URL" opens the URL-resource picker. The target channel is shown so
// the admin confirms where the exposure lands (both modals act on it).
func exposeChooserBlocks(channelID string) []any {
	return []any{
		sectionBlock("*Expose something in this channel*\nPick what to expose — a short guided form opens next."),
		contextBlock("Target channel: " + slackChannelMention(channelID)),
		actionsBlock(
			primaryButtonElement("Expose qURL Connector", exposeConnectorActionID, ""),
			buttonElement("Expose URL", exposeURLActionID, ""),
		),
	}
}

// ExposeURLModalMetadata is carried through Slack private_metadata from the
// "Expose URL" button click (block_actions) to the later view_submission.
// ResponseURL is the chooser message's, where the async outcome is posted. Like
// the edit modal's metadata it carries no TTL/freshness field: the submission
// binds a channel alias (mints no secret, idempotent), so a late submission
// just re-applies the admin's intent rather than minting from stale state.
type ExposeURLModalMetadata struct {
	TeamID      string `json:"team_id"`
	ChannelID   string `json:"channel_id"`
	UserID      string `json:"user_id"`
	ResponseURL string `json:"response_url"`
}

// ExposeURLModal renders the guided URL-expose picker: a dropdown of the
// workspace's existing URL resources (built by the caller from a fresh scan) and
// a channel-alias input. On submit the chosen resource is exposed in the channel
// under that alias (handleExposeURLSubmission). options must be non-empty — the
// caller posts an ephemeral instead when the workspace has no URL resources, so
// the picker never renders empty.
func ExposeURLModal(meta ExposeURLModalMetadata, options []map[string]any) ([]byte, error) {
	privateMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal private_metadata: %w", err)
	}
	if len(privateMeta) > slackPrivateMetadataMaxBytes {
		return nil, fmt.Errorf("private_metadata exceeds Slack limit: %d bytes", len(privateMeta))
	}
	payload := map[string]any{
		blockKitFieldType:            blockKitTypeModal,
		blockKitFieldCallbackID:      callbackIDExposeURL,
		blockKitFieldTitle:           plainTextObj("Expose URL"),
		blockKitFieldSubmit:          plainTextObj("Expose"),
		blockKitFieldClose:           plainTextObj("Cancel"),
		blockKitFieldPrivateMetadata: string(privateMeta),
		blockKitFieldBlocks: []any{
			contextBlock("Target channel: " + slackChannelMention(meta.ChannelID)),
			inputBlock(exposeURLBlockResource, "URL resource", "Pick a protected URL resource to expose in this channel.", false,
				staticSelect(exposeURLActionResource, options, nil)),
			inputBlock(exposeURLBlockAlias, "Channel alias", "The name people type after /qurl get in this channel (e.g. $docs).", false,
				plainTextInput(exposeURLActionAlias, "$docs", "")),
		},
	}
	return json.Marshal(payload)
}

// ExposeURLEmptyModal renders the first-run state for the guided URL picker.
// Slack rejects a static_select with zero options, so the handler opens this
// informational modal instead of posting a terse response_url warning.
func ExposeURLEmptyModal(channelID string) ([]byte, error) {
	payload := map[string]any{
		blockKitFieldType:       blockKitTypeModal,
		blockKitFieldCallbackID: callbackIDExposeURL,
		blockKitFieldTitle:      plainTextObj("Expose URL"),
		blockKitFieldClose:      plainTextObj("Close"),
		blockKitFieldBlocks: []any{
			contextBlock("Target channel: " + slackChannelMention(channelID)),
			sectionBlock("*No URL resources yet*\nCreate a URL resource in the qURL dashboard, then run `/qurl expose` and choose *Expose URL* again."),
			contextBlock("To create a new qURL Connector instead, run `/qurl expose` and choose qURL Connector."),
		},
	}
	return json.Marshal(payload)
}

// ExposeURLErrorModal replaces a submitted URL-expose modal with a form-level
// error notice, for the structural failures (stale/forged metadata, admin
// re-check denial, missing wiring) that aren't tied to a specific input field.
// Per-field validation problems use response_action:errors instead. Mirrors
// TunnelEditErrorModal.
func ExposeURLErrorModal(message string) ([]byte, error) {
	payload := map[string]any{
		blockKitFieldType:  blockKitTypeModal,
		blockKitFieldTitle: plainTextObj("Expose URL"),
		blockKitFieldClose: plainTextObj("Close"),
		blockKitFieldBlocks: []any{
			sectionBlock(":warning: " + message),
		},
	}
	return json.Marshal(payload)
}
