package internal

import (
	"encoding/json"
	"fmt"
)

// Block Kit IDs for the `/qurl-admin protect` chooser and the URL-protect modal.
const (
	// exposeConnectorActionID / exposeURLActionID are the action_ids on the two
	// buttons posted by `/qurl-admin protect`. A click opens the matching guided
	// modal: the connector installer (reusing TunnelInstallModal) or the
	// URL-resource picker. Keep action_ids stable for in-flight
	// Slack interactions; the button values use protect wording and are non-empty
	// because Slack can reject slash-command responses with empty button values.
	exposeConnectorActionID = "expose_connector"
	exposeURLActionID       = "expose_url"
	exposeConnectorValue    = "protect_connector"
	exposeURLValue          = "protect_url"

	// callbackIDExposeURL is the view callback_id for the URL-protect picker.
	// callbackIDExposeURLCreate is retained for create forms already opened by
	// older deployments; current guided entry points reject empty URL-resource
	// dropdowns before rendering a create form.
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
	// slackOptionValueMaxChars is Slack's per-option value cap. URL picker values
	// carry resource_id, so skip overlong IDs rather than rendering a modal Slack
	// will reject.
	slackOptionValueMaxChars = 75
)

// exposeOpenFailedMessage is the ephemeral shown (via the chooser's
// response_url) when a Protect button can't open its modal — a stale trigger, a
// rate-limited views.open, or a render failure. Mirrors listEditOpenFailedMessage.
const exposeOpenFailedMessage = "Couldn't open the dialog. Run `/qurl-admin protect` and tap the button again."

// exposeChooserBlocks builds the two-button picker posted by `/qurl-admin
// protect`: "Protect qURL Connector" opens the guided connector installer and
// "Protect URL" opens the URL-resource picker. The target channel is shown so
// the admin confirms where access lands (both modals act on it).
func exposeChooserBlocks(channelID string) []any {
	return []any{
		sectionBlock("*Protect something in this channel*\n*qURL Connector:* Generate install instructions and a bootstrap key for a private service.\n*URL:* Choose an existing URL resource and bind a channel alias."),
		contextBlock("Target channel: " + slackChannelMention(channelID)),
		actionsBlock(
			buttonElement("Protect qURL Connector", exposeConnectorActionID, exposeConnectorValue),
			buttonElement("Protect URL", exposeURLActionID, exposeURLValue),
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

// ExposeURLModal renders the guided URL-protect picker: a dropdown of eligible
// workspace URL resources and a channel-alias input. On submit the chosen
// resource is exposed in the channel under that alias
// (handleExposeURLSubmission). options must be non-empty; callers reject before
// rendering when there are no URL resources available.
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

// ExposeURLCreateModal renders the retained URL create form for in-flight modals
// opened by older deployments. Current guided entry points do not call it.
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
			sectionBlock("*Create a URL resource*\nEnter an HTTPS URL and a channel alias. People will use `/qurl get $alias` here."),
			inputBlock(exposeURLBlockTarget, "Target URL", "Must start with https://.", false,
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
