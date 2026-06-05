package internal

import (
	"encoding/json"
	"fmt"
)

// Block Kit IDs for the `/qurl-admin protect` chooser and the URL-protect modal.
const (
	// exposeConnectorActionID / exposeURLActionID are the action_ids on the two
	// buttons posted by `/qurl-admin protect`. A click opens the matching guided
	// modal — the connector installer (reusing TunnelInstallModal) or the
	// URL-resource picker (ExposeURLModal).
	exposeConnectorActionID = "expose_connector"
	exposeURLActionID       = "expose_url"

	// callbackIDExposeURL is the view callback_id for the URL-protect modal's
	// view_submission (routed in handleInteraction).
	callbackIDExposeURL       = "expose_url_modal"
	callbackIDExposeURLCreate = "expose_url_create_modal"

	// URL-protect modal block/action ids.
	exposeURLBlockResource  = "expose_url_resource"
	exposeURLActionResource = "expose_url_resource_select"
	exposeURLBlockAlias     = "expose_url_alias"
	exposeURLActionAlias    = "expose_url_alias_input"
	exposeURLBlockTarget    = "expose_url_target"
	exposeURLActionTarget   = "expose_url_target_input"

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
// response_url) when a Protect button can't open its modal — a stale trigger, a
// rate-limited views.open, or a render failure. Mirrors listEditOpenFailedMessage.
const exposeOpenFailedMessage = "Couldn't open the dialog. Run `/qurl-admin protect` and tap the button again."

// exposeChooserBlocks builds the two-button picker posted by `/qurl-admin
// expose`: "Protect qURL Connector" opens the guided connector installer and
// "Protect URL" opens the URL-resource picker. The target channel is shown so
// the admin confirms where access lands (both modals act on it).
func exposeChooserBlocks(channelID string) []any {
	return []any{
		sectionBlock("*Protect something in this channel*\nPick what to protect — a short guided form opens next."),
		contextBlock("Target channel: " + slackChannelMention(channelID)),
		actionsBlock(
			primaryButtonElement("Protect qURL Connector", exposeConnectorActionID, ""),
			buttonElement("Protect URL", exposeURLActionID, ""),
		),
	}
}

// ExposeURLModalMetadata is carried through Slack private_metadata from the
// "Protect URL" button click (block_actions) to the later view_submission.
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

// ExposeURLModal renders the guided URL-protect picker: a dropdown of the
// workspace's existing URL resources (built by the caller from a fresh scan) and
// a channel-alias input. On submit the chosen resource is exposed in the channel
// under that alias (handleExposeURLSubmission). options must be non-empty — the
// caller opens ExposeURLCreateModal when the workspace has no URL resources, so
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
		blockKitFieldTitle:           plainTextObj("Protect URL"),
		blockKitFieldSubmit:          plainTextObj("Protect"),
		blockKitFieldClose:           plainTextObj("Cancel"),
		blockKitFieldPrivateMetadata: string(privateMeta),
		blockKitFieldBlocks: []any{
			contextBlock("Target channel: " + slackChannelMention(meta.ChannelID)),
			inputBlock(exposeURLBlockResource, "URL resource", "Pick a URL resource to protect in this channel.", false,
				staticSelect(exposeURLActionResource, options, nil)),
			inputBlock(exposeURLBlockAlias, "Channel alias", "The name people type after /qurl get in this channel (e.g. $docs).", false,
				plainTextInput(exposeURLActionAlias, "$docs", "")),
		},
	}
	return json.Marshal(payload)
}

// ExposeURLCreateModal renders the first-run URL flow: when there are no
// existing URL resources to pick, ask for the target URL and channel alias so
// Slack can create and protect the resource directly.
func ExposeURLCreateModal(meta ExposeURLModalMetadata) ([]byte, error) {
	privateMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal private_metadata: %w", err)
	}
	if len(privateMeta) > slackPrivateMetadataMaxBytes {
		return nil, fmt.Errorf("private_metadata exceeds Slack limit: %d bytes", len(privateMeta))
	}
	payload := map[string]any{
		blockKitFieldType:            blockKitTypeModal,
		blockKitFieldCallbackID:      callbackIDExposeURLCreate,
		blockKitFieldTitle:           plainTextObj("Protect URL"),
		blockKitFieldSubmit:          plainTextObj("Create"),
		blockKitFieldClose:           plainTextObj("Cancel"),
		blockKitFieldPrivateMetadata: string(privateMeta),
		blockKitFieldBlocks: []any{
			contextBlock("Target channel: " + slackChannelMention(meta.ChannelID)),
			sectionBlock("*Create a URL resource*\nAdd the URL you want to protect. Slack will protect it in this channel so people can run `/qurl get $alias`."),
			inputBlock(exposeURLBlockTarget, "Target URL", "Absolute http or https URL to protect.", false,
				plainTextInput(exposeURLActionTarget, "https://docs.example.com", "")),
			inputBlock(exposeURLBlockAlias, "Channel alias", "The name people type after /qurl get in this channel (e.g. $docs).", false,
				plainTextInput(exposeURLActionAlias, "$docs", "")),
		},
	}
	return json.Marshal(payload)
}

// ExposeURLErrorModal replaces a submitted URL-protect modal with a form-level
// error notice, for the structural failures (stale/forged metadata, admin
// re-check denial, missing wiring) that aren't tied to a specific input field.
// Per-field validation problems use response_action:errors instead. Mirrors
// TunnelEditErrorModal.
func ExposeURLErrorModal(message string) ([]byte, error) {
	payload := map[string]any{
		blockKitFieldType:  blockKitTypeModal,
		blockKitFieldTitle: plainTextObj("Protect URL"),
		blockKitFieldClose: plainTextObj("Close"),
		blockKitFieldBlocks: []any{
			sectionBlock(":warning: " + message),
		},
	}
	return json.Marshal(payload)
}
