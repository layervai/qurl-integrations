package internal

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/layervai/qurl-integrations/shared/client"
)

// Block Kit JSON templates for the Slack bot's view-side surfaces.
//
// PR-3c.1 keeps these as pure JSON-producing helpers — the handler
// in PR-3c.3+ will pass them to `views.open` / `response_url` /
// `chat.postMessage` as appropriate. We don't depend on the
// slack-go/slack library here to avoid pulling a heavyweight
// transitive into the binary just for a fixed set of view payloads. If the
// bot grows a broader Slack Web API surface, revisit that decision instead
// of expanding hand-rolled clients indefinitely.

// Modal callback IDs. The view-submission handler matches on these to
// dispatch the submitted Block Kit state into the right command path.
const (
	callbackIDSetAliasRebind = "setalias_rebind_confirm"
	callbackIDTunnelInstall  = "tunnel_install"
	callbackIDTunnelEdit     = "tunnel_edit"
	callbackIDFeedback       = "feedback"
)

// listCreateQurlActionID is the action_id on the "Create qURL" button
// rendered next to each `/qurl list` row. The block_actions handler
// matches on it to mint a one-time qURL for that row's tunnel — the same
// resolve→authorize→mint work as `/qurl get $<slug>`. The button's value
// carries the row's `$<slug>`/`$<alias>` token with the `$` sigil
// stripped. Reused as the action_id on every row (Slack only requires
// action_id uniqueness within a block, not across the message), so the
// clicked row is identified by the button's value, not its action_id.
const listCreateQurlActionID = "list_create_qurl"

// listEditTunnelActionID is the action_id on the admin-only "Edit" button
// rendered alongside "Create qURL" on each `/qurl list` row when the caller is
// a qURL admin (and the modal/alias/admin wiring is present). The
// block_actions handler matches on it to open the [TunnelEditModal]
// pre-filled from the button's value snapshot. Like listCreateQurlActionID it
// is reused across rows — the clicked tunnel is identified by the button's
// value (a [tunnelEditButtonValue] JSON snapshot), not the action_id.
const listEditTunnelActionID = "list_edit_tunnel"

// listRevokeTunnelActionID is the action_id on the admin-only red "Revoke"
// button rendered beside "Edit" on each `/qurl list` row. The button carries a
// Slack confirm dialog, so the block_actions handler only sees the click after
// the admin confirms; it then revokes the row's resource (and all its qURLs).
// The clicked tunnel is identified by the button's value (a
// [tunnelRevokeButtonValue] JSON snapshot), not the action_id.
const listRevokeTunnelActionID = "list_revoke_tunnel"

const (
	blockKitFieldActionID        = "action_id"
	blockKitFieldValue           = "value"
	blockKitTypeActions          = "actions"
	blockKitFieldBlocks          = "blocks"
	blockKitFieldCallbackID      = "callback_id"
	blockKitFieldClose           = "close"
	blockKitFieldElements        = "elements"
	blockKitFieldPrivateMetadata = "private_metadata"
	blockKitFieldSubmit          = "submit"
	blockKitFieldTitle           = "title"
	blockKitFieldType            = "type"
	blockKitTypeModal            = "modal"
	blockKitTypeMultiConvSelect  = "multi_conversations_select"
	// Button styles: Slack renders `primary` filled-green and `danger` red.
	blockKitStylePrimary = "primary"
	blockKitStyleDanger  = "danger"
	// Slack caps private_metadata at 3000 bytes. Today's tunnel metadata is
	// small; this guard is mainly defense against future field additions or a
	// pathological response_url making modal submission fail only after open.
	slackPrivateMetadataMaxBytes = 3000
)

const slackChannelFallbackText = "the channel where setup started"

// Keep this intentionally wider than today's C*/D*/G* Slack channel prefixes:
// it is only used for benign fallback display text, and Slack has changed ID
// shapes before. The channel ID is not an auth signal here; signature
// verification on the Slack request envelope is the trust boundary.
var slackChannelIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{1,127}$`)

const (
	tunnelInstallBlockSlug         = "tunnel_slug"
	tunnelInstallActionSlug        = "slug_input"
	tunnelInstallBlockShortcut     = "channel_shortcut"
	tunnelInstallActionShortcut    = "channel_shortcut_input"
	tunnelInstallBlockEnvironment  = "target_environment"
	tunnelInstallActionEnvironment = "target_environment_select"
	tunnelInstallBlockLocalPort    = "local_port"
	tunnelInstallActionLocalPort   = "local_port_input"
	tunnelInstallBlockWebRef       = "web_container"
	tunnelInstallActionWebRef      = "web_container_input"
)

const (
	tunnelEditBlockDisplayName  = "edit_display_name"
	tunnelEditActionDisplayName = "edit_display_name_input"
	tunnelEditBlockAliases      = "edit_aliases"
	tunnelEditActionAliases     = "edit_aliases_input"
	tunnelEditBlockChannels     = "edit_channels"
	tunnelEditActionChannels    = "edit_channels_select"
)

// SetAliasRebindMetadata is the typed shape the rebind modal stores
// in `private_metadata`. JSON-encoded so the view-submission handler
// (PR-3c.3+) can `json.Unmarshal` into a known struct rather than
// parsing an ad-hoc query-string. Slack caps `private_metadata` at
// 3000 chars; a JSON object with an alias name and a target URL
// (typically <100 chars combined) fits comfortably.
//
// NewTarget rides along so the submission handler can apply the
// rebind without re-reading from the submission state — `state`
// belongs to whichever input blocks the modal renders, and the
// rebind modal has none (it's a confirm-only flow). Putting the
// target on `private_metadata` keeps the contract stable from day
// one rather than requiring a Slack `views.update` roundtrip.
type SetAliasRebindMetadata struct {
	Alias     string `json:"alias"`
	NewTarget string `json:"new_target"`
}

// SetAliasRebindModal renders the confirmation modal shown when a user
// runs `setalias $alias <new-target>` and the alias already points
// at a different resource. The user has to explicitly confirm the
// rebind — silently overwriting an alias is the kind of action that
// causes incident-response chaos in a multi-admin workspace.
//
// `aliasName` is the alias being rebound (no `$` sigil); `oldTarget`
// and `newTarget` are the human-readable strings to show side-by-side.
// The caller is expected to pass this payload to `views.open` along
// with the Slack-supplied trigger ID.
//
// oldTarget/newTarget are interpolated into mrkdwn code spans
// (a backtick-wrapped %s). URLs and r_... resource IDs realistically
// never contain backticks, but the rebind modal is shown to one admin
// after another admin set the target, so a malicious admin could
// otherwise break out of the code span and inject mrkdwn
// (e.g. <!channel>, <@U…>) into the confirming admin's view.
// [escapeMrkdwnCode] neutralizes the only character that matters
// for the code-span surface.
func SetAliasRebindModal(aliasName, oldTarget, newTarget string) ([]byte, error) {
	meta, err := json.Marshal(SetAliasRebindMetadata{Alias: aliasName, NewTarget: newTarget})
	if err != nil {
		return nil, fmt.Errorf("marshal private_metadata: %w", err)
	}
	payload := map[string]any{
		blockKitFieldType:            blockKitTypeModal,
		blockKitFieldCallbackID:      callbackIDSetAliasRebind,
		blockKitFieldTitle:           plainTextObj("Confirm alias rebind"),
		blockKitFieldSubmit:          plainTextObj("Rebind"),
		blockKitFieldClose:           plainTextObj("Cancel"),
		blockKitFieldPrivateMetadata: string(meta),
		blockKitFieldBlocks: []any{
			sectionBlock(fmt.Sprintf("Alias `$%s` is already bound.", escapeMrkdwnCode(aliasName))),
			sectionBlock(fmt.Sprintf("*Current target:* `%s`", escapeMrkdwnCode(oldTarget))),
			sectionBlock(fmt.Sprintf("*New target:* `%s`", escapeMrkdwnCode(newTarget))),
			contextBlock("This action overwrites the existing binding for everyone in the workspace."),
		},
	}
	return json.Marshal(payload)
}

// TunnelInstallModalMetadata is carried through Slack private_metadata from
// the slash-command request that opened the modal to the later
// view_submission. The response_url lets the async installer post the same
// ephemeral follow-up shape as the direct `/qurl-admin protect-connector <slug>` path.
// CreatedAtUnix lets the submit handler reject stale modals before creating a
// resource or minting a bootstrap key; Slack response URLs are time-limited.
type TunnelInstallModalMetadata struct {
	TeamID        string `json:"team_id"`
	ChannelID     string `json:"channel_id"`
	UserID        string `json:"user_id"`
	ResponseURL   string `json:"response_url"`
	CreatedAtUnix int64  `json:"created_at_unix,omitempty"`
}

// TunnelInstallModal renders the guided tunnel installer. The modal collects
// only customer-facing choices: the stable slug, optional channel shortcut,
// local service port, target environment, and an optional Docker/Compose target
// name used only by the Docker and Docker Compose renderers.
func TunnelInstallModal(meta TunnelInstallModalMetadata) ([]byte, error) {
	privateMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal private_metadata: %w", err)
	}
	if len(privateMeta) > slackPrivateMetadataMaxBytes {
		return nil, fmt.Errorf("private_metadata exceeds Slack limit: %d bytes", len(privateMeta))
	}
	defaultPort := strconv.Itoa(defaultTunnelLocalPort)
	payload := map[string]any{
		blockKitFieldType:            blockKitTypeModal,
		blockKitFieldCallbackID:      callbackIDTunnelInstall,
		blockKitFieldTitle:           plainTextObj("Install qURL Connector"),
		blockKitFieldSubmit:          plainTextObj("Generate"),
		blockKitFieldClose:           plainTextObj("Cancel"),
		blockKitFieldPrivateMetadata: string(privateMeta),
		blockKitFieldBlocks: []any{
			contextBlock("Target channel: " + slackChannelMention(meta.ChannelID)),
			inputBlock(tunnelInstallBlockSlug, "qURL Connector ID", "3-64 lowercase letters, numbers, and hyphens. Start with a letter, end with a letter or number.", false,
				plainTextInput(tunnelInstallActionSlug, "prod-dashboard", "")),
			inputBlock(tunnelInstallBlockShortcut, "Channel alias", "Optional. Leave blank to use the qURL Connector ID.", true,
				plainTextInput(tunnelInstallActionShortcut, "prod", "")),
			inputBlock(tunnelInstallBlockEnvironment, "Target environment", "Choose the runtime shape so Slack can tailor the install output. Docker snippets assume a Linux host.", false,
				staticSelect(tunnelInstallActionEnvironment, []map[string]any{
					optionObj("Docker sidecar", string(tunnelEnvDocker)),
					optionObj("Docker Compose", string(tunnelEnvCompose)),
					optionObj("AWS ECS/Fargate task", string(tunnelEnvECSFargate)),
					optionObj("Kubernetes pod", string(tunnelEnvKubernetes)),
				}, optionObj("Docker sidecar", string(tunnelEnvDocker)))),
			inputBlock(tunnelInstallBlockLocalPort, "Local HTTP port", "The port the local service listens on inside the shared network namespace.", false,
				plainTextInput(tunnelInstallActionLocalPort, defaultPort, defaultPort)),
			inputBlock(tunnelInstallBlockWebRef, "Docker service/container", "Optional for Linux Docker and Docker Compose only. Leave blank for ECS/Fargate or Kubernetes.", true,
				plainTextInput(tunnelInstallActionWebRef, "web", "")),
		},
	}
	return json.Marshal(payload)
}

// TunnelInstallErrorModal replaces a submitted tunnel-install modal with a
// form-level error. Slack's `response_action: errors` can only attach copy to
// input fields, which makes auth/config failures look like bad user input.
func TunnelInstallErrorModal(message string) ([]byte, error) {
	payload := map[string]any{
		blockKitFieldType:  blockKitTypeModal,
		blockKitFieldTitle: plainTextObj("qURL Connector setup"),
		blockKitFieldClose: plainTextObj("Close"),
		blockKitFieldBlocks: []any{
			sectionBlock(":warning: " + message),
		},
	}
	return json.Marshal(payload)
}

// TunnelEditErrorModal replaces a submitted edit modal with a form-level
// error, for the rare structural failures (stale/forged metadata, admin
// re-check denial, missing wiring) that aren't tied to a specific input field.
// Per-field validation problems use response_action:errors instead.
func TunnelEditErrorModal(message string) ([]byte, error) {
	payload := map[string]any{
		blockKitFieldType:  blockKitTypeModal,
		blockKitFieldTitle: plainTextObj("Edit resource"),
		blockKitFieldClose: plainTextObj("Close"),
		blockKitFieldBlocks: []any{
			sectionBlock(":warning: " + message),
		},
	}
	return json.Marshal(payload)
}

// TunnelEditModalMetadata is carried through Slack private_metadata from the
// `/qurl list` Edit button click (block_actions) to the later view_submission.
// ResourceID is the PATCH/alias-bind target; Token is the displayed `$<token>`
// (slug or promoted alias) shown in the modal context and excluded from the
// editable alias set so the tunnel's own name is never unbound by the modal.
// ResponseURL is the list message's, where the async edit result is posted.
type TunnelEditModalMetadata struct {
	TeamID      string `json:"team_id"`
	ChannelID   string `json:"channel_id"`
	UserID      string `json:"user_id"`
	ResponseURL string `json:"response_url"`
	ResourceID  string `json:"resource_id"`
	Token       string `json:"token"`
	// DisplayName is the tunnel's Display Name at the moment the modal opened.
	// The submit handler diffs the submitted name against it to skip a no-op
	// PATCH (so an alias-only edit can't clobber a concurrent display-name
	// change). No freshness/TTL field: the edit mints no secret and is
	// effectively idempotent, so a late submission just re-applies the admin's
	// intent.
	DisplayName string `json:"display_name,omitempty"`
	// Aliases is the extra-alias set (sigil-free, token excluded) the modal was
	// pre-filled with. The submit handler caps only NEWLY-added aliases against
	// it, so a tunnel that already carries more than listEditMaxAliases aliases
	// stays editable for a name-only or removal-only change.
	Aliases []string `json:"aliases,omitempty"`
	// ExposedChannels is the set of channel IDs the tunnel was already exposed
	// to when the modal opened (its ChannelsForResource result), used both to
	// pre-fill the channels multi-select and as the reconcile baseline on
	// submit: only a channel the admin SAW here and de-selected is revoked, so
	// a partial enumeration (e.g. the Query grant is missing) can never cause
	// the submit to drop a channel the admin never saw. Always includes the
	// current channel (ChannelID), which the reconcile never revokes.
	ExposedChannels []string `json:"exposed_channels,omitempty"`
	// ResourceType is carried for copy only. Empty is treated as a qURL
	// Connector for backwards compatibility with already-rendered connector
	// buttons that predate this field.
	ResourceType string `json:"resource_type,omitempty"`
}

// TunnelEditModal renders the admin Edit modal opened from a `/qurl list` row.
// It pre-fills the tunnel's current Display Name, its additional channel
// aliases (the bound aliases other than the row's primary `$<token>`), and the
// channels the tunnel is currently protected in (meta.ExposedChannels), so the
// admin edits an authoritative snapshot rather than re-typing from scratch.
// `aliases` is the extra-alias set (sigil-free); they render one per line with
// a leading `$` to match how the admin types them. The channels field is a
// multi_conversations_select pre-selected with meta.ExposedChannels — adding a
// channel exposes the tunnel there (it shows in that channel's `/qurl list`
// and is mintable via `/qurl get`); removing one revokes it (except the
// channel being edited from, which the submit handler always keeps).
func TunnelEditModal(meta *TunnelEditModalMetadata, displayName string, aliases []string) ([]byte, error) {
	privateMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal private_metadata: %w", err)
	}
	if len(privateMeta) > slackPrivateMetadataMaxBytes {
		return nil, fmt.Errorf("private_metadata exceeds Slack limit: %d bytes", len(privateMeta))
	}
	aliasInitial := ""
	if len(aliases) > 0 {
		aliasInitial = "$" + strings.Join(aliases, "\n$")
	}
	label := editResourceLabel(meta.ResourceType)
	payload := map[string]any{
		blockKitFieldType:            blockKitTypeModal,
		blockKitFieldCallbackID:      callbackIDTunnelEdit,
		blockKitFieldTitle:           plainTextObj("Edit " + label),
		blockKitFieldSubmit:          plainTextObj("Save"),
		blockKitFieldClose:           plainTextObj("Cancel"),
		blockKitFieldPrivateMetadata: string(privateMeta),
		blockKitFieldBlocks: []any{
			contextBlock("Editing " + label + " " + editResourceTokenLabel(meta.Token, meta.ResourceType)),
			// Optional: a Display Name is not mandatory (a tunnel can have none,
			// and `/qurl-admin unset-display-name` clears it), so a required field
			// would block an alias-only edit on an unnamed tunnel — the empty input
			// pre-fills empty and Slack would refuse submission. With the
			// changed-only diff, an empty submission on an empty-named tunnel is a
			// no-op (normalizes to "" == "" → nameChanged=false → PATCH skipped).
			inputBlock(tunnelEditBlockDisplayName, "Display name", "Optional. Shown next to this resource in /qurl list.", true,
				plainTextInput(tunnelEditActionDisplayName, "Prod dashboard", displayName)),
			inputBlock(tunnelEditBlockAliases, "Channel aliases", "Optional. One alias per line (e.g. $staging). These are extra names that resolve to this resource in this channel. Clear a line to remove that alias.", true,
				multilinePlainTextInput(tunnelEditActionAliases, "$staging\n$db", aliasInitial)),
			inputBlock(tunnelEditBlockChannels, "Channels", "Channels where this resource shows in /qurl list and can be minted with /qurl get. The channel you're editing from always keeps access. Add channels to protect it there; remove one to revoke it.", true,
				multiConversationsSelect(tunnelEditActionChannels, meta.ExposedChannels)),
		},
	}
	return json.Marshal(payload)
}

func editResourceLabel(resourceType string) string {
	if resourceType == client.ResourceTypeURL {
		return "URL resource"
	}
	return "qURL Connector"
}

// editResourceTokenLabel renders the edited resource's `$<token>` for the modal
// context line. The token is a charset-validated slug/alias, but it is escaped
// for the mrkdwn code span as defense-in-depth (same posture as the rebind
// modal's target labels).
func editResourceTokenLabel(token, resourceType string) string {
	if token == "" {
		return "this " + editResourceLabel(resourceType)
	}
	return "`$" + escapeMrkdwnCode(token) + "`"
}

func slackChannelMention(channelID string) string {
	channelID = strings.TrimSpace(channelID)
	if !slackChannelIDPattern.MatchString(channelID) {
		slog.Warn("using fallback Slack channel label for malformed channel id", "channel_id_present", channelID != "")
		return slackChannelFallbackText
	}
	return "<#" + channelID + ">"
}

// escapeMrkdwnCode neutralizes the characters that can break out of
// a mrkdwn code span: backtick (closes the span) and any line break
// — \n, \r, or the \r\n pair (Slack's renderer ends the span at a
// hard newline, and on CRLF input the bare \r reaches the renderer
// first). Without escaping, a value containing any of these would
// let the remainder render as mrkdwn — opening an injection vector
// for user-supplied targets (admin-set DDB rows). Backtick is
// replaced with U+02CA (MODIFIER LETTER ACUTE ACCENT) which keeps
// a close visual approximation; line breaks become a single space
// so the rebind modal stays one-line per target.
//
// **Scope is view rendering only.** This is NOT a general-purpose
// sanitizer — the original target bytes still live in DDB intact,
// and any code path that reads them for non-display purposes
// (URL fetching, audit logging, comparison) sees the raw value.
// The asymmetry is correct (raw bytes in storage, escaped bytes
// in view); the rule for callers is "only run this on the way to
// a mrkdwn code span in Block Kit JSON."
//
// Note on `aliasName`: the parser's `aliasCharsetPattern` rejects
// backticks and line breaks at the slash-command grammar layer, so
// running this on an alias name from `Command.Alias` is pure
// defense-in-depth (the escape is a no-op for any input that
// reaches the modal via the supported `setalias` path). Kept
// applied to all three string args so a future code path that
// fabricates a Command (tests, admin overrides) can't bypass the
// code-span fence by handing a hand-built alias through.
//
// `strings.NewReplacer` builds the substitution table once at call
// time and runs all four replacements in a single pass — strictly
// fewer allocations than chained `strings.ReplaceAll` and the
// substitution table reads as a single block at the security-fence
// boundary, which is the right shape for a mitigation primitive.
func escapeMrkdwnCode(s string) string {
	return mrkdwnCodeEscaper.Replace(s)
}

// escapeMrkdwnText escapes user-controlled text that is interpolated directly
// into Slack mrkdwn (not inside an inline code span). Slack requires escaping
// &, <, and >; backticks, hard line breaks, and cosmetic formatting markers are
// neutralized too so a displayed value cannot open a code span, split a row, or
// apply bold/italic/strike formatting to the surrounding message.
func escapeMrkdwnText(s string) string {
	return mrkdwnTextEscaper.Replace(s)
}

// mrkdwnCodeEscaper is the single-pass substitution table used by
// `escapeMrkdwnCode`. Defined at package scope so the replacer is
// constructed once at init rather than per-call. Order matters in
// `strings.NewReplacer` only for prefix overlap; `\r\n` is listed
// before `\r` so the CRLF pair collapses to one space rather than
// two. (`\n` and `\r` standalone are handled by their own entries.)
var mrkdwnCodeEscaper = strings.NewReplacer(
	"`", "ˊ",
	"\r\n", " ",
	"\n", " ",
	"\r", " ",
)

var mrkdwnTextEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	"`", "ˊ",
	"*", "∗",
	"_", "＿",
	"~", "～",
	"\r\n", " ",
	"\n", " ",
	"\r", " ",
)

// ErrorResponse renders an ephemeral-channel error message via the
// `response_url` shape. Used by every parser/handler error path so
// the user sees a friendly `:warning: <message>` instead of an
// unhandled-error stack trace.
//
// `replaceOriginal` is true when the message replaces an in-flight
// "Working on it..." spinner (the async-defer pattern from the
// plan); false for direct slash-command response bodies.
func ErrorResponse(message string, replaceOriginal bool) ([]byte, error) {
	payload := map[string]any{
		respFieldResponseType: respTypeEphemeral,
		"replace_original":    replaceOriginal,
		respFieldText:         ":warning: " + message,
	}
	return json.Marshal(payload)
}

// --- Block Kit primitives ---

// sectionBlock returns a `section` block with mrkdwn text. Lifted to a
// helper because every view template uses it and the literal map
// shape is verbose.
func sectionBlock(text string) map[string]any {
	return map[string]any{
		"type": "section",
		"text": map[string]any{
			"type": "mrkdwn",
			"text": text,
		},
	}
}

// headerBlock returns a `header` block — Slack's large, bold title text. Its
// text object must be plain_text, so `:emoji:` shortcodes render but mrkdwn
// (e.g. `*bold*`) does not. Slack silently rejects a `header` whose text
// exceeds 150 characters, so a caller passing a dynamic string must keep it
// short; today the only caller uses the short [listHeaderBlockText] constant.
func headerBlock(text string) map[string]any {
	return map[string]any{
		"type": "header",
		"text": plainTextObj(text),
	}
}

// richTextPreformattedBlock returns a `rich_text` block wrapping a single
// `rich_text_preformatted` element — Slack's structured code block. Unlike a
// triple-backtick mrkdwn fence inside a `section`, the preformatted element
// renders with a copy affordance in the Slack client and never risks
// fence-escaping issues, so it is the right surface for the multi-line shell /
// YAML / JSON snippets the tunnel installer hands operators to paste.
//
// `code` is emitted verbatim as a single `text` element. Rich-text
// `text` elements are NOT mrkdwn-parsed, so the snippet's own backticks,
// `<…>`, and `*` render literally — exactly what a copy-paste block wants.
// Callers pass raw, already-validated snippet text.
func richTextPreformattedBlock(code string) map[string]any {
	return map[string]any{
		"type": "rich_text",
		blockKitFieldElements: []any{
			map[string]any{
				"type": "rich_text_preformatted",
				blockKitFieldElements: []any{
					map[string]any{
						"type": "text",
						"text": code,
					},
				},
			},
		},
	}
}

// sectionWithAccessory returns a `section` block with the given element as its
// accessory. Slack renders an accessory to the RIGHT of the section text —
// Block Kit has no leading/left accessory slot, so the right-aligned accessory
// is the idiomatic "one action per row" shape. For a button accessory the
// `value` rides along and is echoed back in the block_actions payload when the
// button is clicked, so the handler knows which row was tapped.
func sectionWithAccessory(text string, accessory map[string]any) map[string]any {
	return map[string]any{
		"type": "section",
		"text": map[string]any{
			"type": "mrkdwn",
			"text": text,
		},
		"accessory": accessory,
	}
}

// buttonElement returns a `button` block element: an action_id plus an opaque
// `value` echoed back in the block_actions payload when the button is clicked.
// Used both as a section accessory (sectionWithAccessory) and inside an
// actionsBlock (the multi-button admin `/qurl list` rows).
func buttonElement(buttonText, actionID, value string) map[string]any {
	return map[string]any{
		"type":                "button",
		"text":                plainTextObj(buttonText),
		blockKitFieldActionID: actionID,
		blockKitFieldValue:    value,
	}
}

// primaryButtonElement is a [buttonElement] rendered with Slack's `primary`
// (filled) style — used for the headline "Create qURL" action so it reads
// above the secondary Edit button on admin `/qurl list` rows.
func primaryButtonElement(buttonText, actionID, value string) map[string]any {
	b := buttonElement(buttonText, actionID, value)
	b["style"] = blockKitStylePrimary
	return b
}

// dangerButtonElement is a [buttonElement] rendered with Slack's `danger`
// (red) style — used for the destructive "Revoke" action on admin `/qurl list`
// rows, against the green `primary` "Create qURL". Pair it with
// [withConfirmDialog] so the action can't fire on a stray click.
func dangerButtonElement(buttonText, actionID, value string) map[string]any {
	b := buttonElement(buttonText, actionID, value)
	b["style"] = blockKitStyleDanger
	return b
}

// withConfirmDialog attaches a Slack confirm dialog to a button element so the
// block_action only fires after the user confirms — Slack renders it as a
// modal-style popup. The confirm button inherits the `danger` (red) style for
// destructive actions. title/confirmLabel are plain_text; text is mrkdwn.
// Mutates and returns button for call-site chaining.
func withConfirmDialog(button map[string]any, title, text, confirmLabel string) map[string]any {
	button["confirm"] = map[string]any{
		"title":   plainTextObj(title),
		"text":    map[string]any{"type": "mrkdwn", "text": text},
		"confirm": plainTextObj(confirmLabel),
		"deny":    plainTextObj("Cancel"),
		"style":   blockKitStyleDanger,
	}
	return button
}

// actionsBlock returns an `actions` block holding the given button elements as
// a row beneath a section. A `section` accessory holds only a single element,
// so a row that carries more than one action (Create qURL + the admin-only
// Edit) uses this instead.
func actionsBlock(elements ...map[string]any) map[string]any {
	els := make([]any, len(elements))
	for i := range elements {
		els[i] = elements[i]
	}
	return map[string]any{
		blockKitFieldType:     blockKitTypeActions,
		blockKitFieldElements: els,
	}
}

// contextBlock returns a `context` block with a single mrkdwn element.
// Used for the "subtext" rows in modals (e.g. the `:lock:` warning).
func contextBlock(text string) map[string]any {
	return map[string]any{
		"type": "context",
		blockKitFieldElements: []any{
			map[string]any{
				"type": "mrkdwn",
				"text": text,
			},
		},
	}
}

// plainTextObj returns a `plain_text` text object. Slack uses two
// distinct text-object shapes (`plain_text` and `mrkdwn`); the modal
// title/submit/close fields require `plain_text` specifically.
func plainTextObj(text string) map[string]any {
	return map[string]any{
		"type":  "plain_text",
		"text":  text,
		"emoji": true,
	}
}

func inputBlock(blockID, label, hint string, optional bool, element map[string]any) map[string]any {
	block := map[string]any{
		"type":     "input",
		"block_id": blockID,
		"label":    plainTextObj(label),
		"element":  element,
	}
	if hint != "" {
		block["hint"] = plainTextObj(hint)
	}
	if optional {
		block["optional"] = true
	}
	return block
}

func plainTextInput(actionID, placeholder, initialValue string) map[string]any {
	element := map[string]any{
		"type":                "plain_text_input",
		blockKitFieldActionID: actionID,
	}
	if placeholder != "" {
		element["placeholder"] = plainTextObj(placeholder)
	}
	if initialValue != "" {
		element["initial_value"] = initialValue
	}
	return element
}

// multilinePlainTextInput is plainTextInput with multiline enabled — used by
// the edit modal's one-alias-per-line aliases field.
func multilinePlainTextInput(actionID, placeholder, initialValue string) map[string]any {
	element := plainTextInput(actionID, placeholder, initialValue)
	element["multiline"] = true
	return element
}

func staticSelect(actionID string, options []map[string]any, initial map[string]any) map[string]any {
	element := map[string]any{
		"type":                "static_select",
		blockKitFieldActionID: actionID,
		"options":             options,
	}
	// Omit initial_option when nil — Slack rejects `"initial_option": null`. A
	// nil initial means "no pre-selection" (the URL-protect picker forces a
	// deliberate choice); a non-nil one pre-selects (e.g. the connector
	// installer's environment default). Mirrors multiConversationsSelect's guard.
	if initial != nil {
		element["initial_option"] = initial
	}
	return element
}

// multiConversationsSelect returns a multi_conversations_select element — used
// by the edit modal's "expose to channels" field. It is filtered to public and
// private channels (DMs/group-DMs and externally-shared channels aren't
// tunnel-exposure targets) and pre-selects initialConversations. An empty
// initial set omits the initial_conversations key entirely — Slack rejects an
// empty array there.
func multiConversationsSelect(actionID string, initialConversations []string) map[string]any {
	element := map[string]any{
		"type":                blockKitTypeMultiConvSelect,
		blockKitFieldActionID: actionID,
		"placeholder":         plainTextObj("Select channels"),
		"filter": map[string]any{
			"include":                          []any{"public", "private"},
			"exclude_external_shared_channels": true,
		},
	}
	if len(initialConversations) > 0 {
		conv := make([]any, len(initialConversations))
		for i, c := range initialConversations {
			conv[i] = c
		}
		element["initial_conversations"] = conv
	}
	return element
}

func optionObj(text, value string) map[string]any {
	return map[string]any{
		"text":             plainTextObj(text),
		blockKitFieldValue: value,
	}
}

// plainTextSectionBlock returns a `section` block whose text is a plain_text
// object. Slack does NO mrkdwn parsing on plain_text, so embedded `<…>` mention
// syntax (`<!channel>`, `<@U…>`) and `*`/`_` render literally — that is the
// reason free-form feedback (summary/details) is rendered through here rather
// than sectionBlock: it neutralizes mention/link injection from user text into
// the operator-visible feedback channel.
func plainTextSectionBlock(text string) map[string]any {
	return map[string]any{
		"type": "section",
		"text": plainTextObj(text),
	}
}

// Feedback modal block/action IDs and input bounds. The bounds are enforced
// client-side via the inputs' max_length AND re-checked in
// parseFeedbackModalArgs so a crafted submission cannot exceed them.
const (
	feedbackBlockType     = "feedback_type"
	feedbackActionType    = "feedback_type_select"
	feedbackBlockSummary  = "feedback_summary"
	feedbackActionSummary = "feedback_summary_input"
	feedbackBlockDetails  = "feedback_details"
	feedbackActionDetails = "feedback_details_input"

	// Character (rune) caps, both well under Slack's 3000-char text-object
	// limit so the summary/details render in single section blocks.
	feedbackSummaryMaxLen = 150
	feedbackDetailsMaxLen = 2800
)

// Feedback type option values. Stable lowercase tokens chosen to map cleanly
// onto the GitHub labels the triage routine applies (e.g. type:bug), so the
// value travels unchanged from modal selection to issue label.
const (
	feedbackTypeBug     = "bug"
	feedbackTypeFeature = "feature"
	feedbackTypeOther   = "other"
)

// FeedbackModalMetadata is carried through Slack private_metadata from the
// `/qurl feedback` slash command that opened the modal to the later
// view_submission. It captures the submitter attribution at open time — the
// view_submission payload only carries team/user IDs, not the human-readable
// handle/workspace — plus the slash command's response_url so the async post
// can confirm receipt (or report a delivery failure) back in the channel the
// command was run from. No freshness TTL: feedback mints no secret and is
// idempotent, so a late submission just posts the same note (the response_url
// confirmation may simply have expired, which is harmless).
type FeedbackModalMetadata struct {
	TeamID       string `json:"team_id"`
	TeamDomain   string `json:"team_domain,omitempty"`
	UserID       string `json:"user_id"`
	UserName     string `json:"user_name,omitempty"`
	ChannelID    string `json:"channel_id,omitempty"`
	ChannelName  string `json:"channel_name,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	ResponseURL  string `json:"response_url"`
}

// FeedbackModal renders the `/qurl feedback` modal: a type select, a short
// required summary, and optional free-form details. The context line warns
// against pasting secrets because submissions land in an operator-visible
// internal channel (see Handler.processFeedback).
func FeedbackModal(meta *FeedbackModalMetadata) ([]byte, error) {
	privateMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal private_metadata: %w", err)
	}
	if len(privateMeta) > slackPrivateMetadataMaxBytes {
		return nil, fmt.Errorf("private_metadata exceeds Slack limit: %d bytes", len(privateMeta))
	}
	summaryInput := plainTextInput(feedbackActionSummary, "Short summary of the bug or idea", "")
	summaryInput["max_length"] = feedbackSummaryMaxLen
	detailsInput := multilinePlainTextInput(feedbackActionDetails, "Steps to reproduce, what you expected, or extra context", "")
	detailsInput["max_length"] = feedbackDetailsMaxLen
	payload := map[string]any{
		blockKitFieldType:            blockKitTypeModal,
		blockKitFieldCallbackID:      callbackIDFeedback,
		blockKitFieldTitle:           plainTextObj("Send feedback"),
		blockKitFieldSubmit:          plainTextObj("Send"),
		blockKitFieldClose:           plainTextObj("Cancel"),
		blockKitFieldPrivateMetadata: string(privateMeta),
		blockKitFieldBlocks: []any{
			contextBlock("Goes straight to the qURL team. Please don't include qURL keys, tokens, or other secrets."),
			inputBlock(feedbackBlockType, "Type", "", false,
				staticSelect(feedbackActionType, []map[string]any{
					optionObj("Bug report", feedbackTypeBug),
					optionObj("Feature request", feedbackTypeFeature),
					optionObj("Something else", feedbackTypeOther),
				}, optionObj("Bug report", feedbackTypeBug))),
			inputBlock(feedbackBlockSummary, "Summary", "", false, summaryInput),
			inputBlock(feedbackBlockDetails, "Details", "Optional", true, detailsInput),
		},
	}
	return json.Marshal(payload)
}

// FeedbackErrorModal replaces a submitted feedback modal with a form-level
// error for the rare structural failures (forged/stale metadata, missing
// wiring) that aren't tied to a specific input field. Per-field validation
// problems use response_action:errors via respondViewErrors instead.
func FeedbackErrorModal(message string) ([]byte, error) {
	payload := map[string]any{
		blockKitFieldType:  blockKitTypeModal,
		blockKitFieldTitle: plainTextObj("Send feedback"),
		blockKitFieldClose: plainTextObj("Close"),
		blockKitFieldBlocks: []any{
			sectionBlock(":warning: " + message),
		},
	}
	return json.Marshal(payload)
}

// FeedbackMessage builds the Block Kit payload posted to the internal feedback
// channel via the Slack incoming webhook. The user-provided summary and details
// render as plain_text blocks (see plainTextSectionBlock) so Slack performs no
// mrkdwn parsing on them; the fixed labels and the attribution line render as
// mrkdwn, with the Slack-supplied attribution values escaped because user_name /
// team_domain — while Slack-issued — sit outside this code's trust boundary. The
// `text` fallback is kept free of user content so the channel's push
// notification can't be used to smuggle a mention out of feedback text.
func FeedbackMessage(meta *FeedbackModalMetadata, typeValue, summary, details string) ([]byte, error) {
	emoji, label, ok := feedbackTypeDisplay(typeValue)
	if !ok {
		return nil, fmt.Errorf("unknown feedback type %q", typeValue)
	}
	blocks := []any{
		headerBlock(emoji + " " + label),
		plainTextSectionBlock(summary),
	}
	if details != "" {
		blocks = append(blocks,
			contextBlock("*Details*"),
			plainTextSectionBlock(details),
		)
	}
	blocks = append(blocks, contextBlock(feedbackAttribution(meta)))
	payload := map[string]any{
		blockKitFieldBlocks: blocks,
		respFieldText:       "New qURL feedback — " + label,
	}
	return json.Marshal(payload)
}

// feedbackTypeDisplay maps a stored feedback type value to its header emoji and
// label. ok is false for an unrecognized value, which parseFeedbackModalArgs
// treats as a field error and FeedbackMessage as a render error.
func feedbackTypeDisplay(typeValue string) (emoji, label string, ok bool) {
	switch typeValue {
	case feedbackTypeBug:
		return ":beetle:", "Bug report", true
	case feedbackTypeFeature:
		return ":bulb:", "Feature request", true
	case feedbackTypeOther:
		return ":speech_balloon:", "Feedback", true
	default:
		return "", "", false
	}
}

// feedbackAttribution renders the submitter context line. It pairs the
// human-readable Slack handle / workspace domain (for the team triaging in
// Slack) with the stable team/user IDs (the triage routine's join keys and the
// GitHub-issue attribution). Every dynamic value is escaped for the mrkdwn code
// span it sits in.
func feedbackAttribution(meta *FeedbackModalMetadata) string {
	userLabel := strings.TrimSpace(meta.UserName)
	if userLabel == "" {
		userLabel = meta.UserID
	}
	workspaceLabel := strings.TrimSpace(meta.TeamDomain)
	if workspaceLabel == "" {
		workspaceLabel = meta.TeamID
	}
	var b strings.Builder
	b.WriteString("From `")
	b.WriteString(escapeMrkdwnCode(userLabel))
	b.WriteString("` in workspace `")
	b.WriteString(escapeMrkdwnCode(workspaceLabel))
	b.WriteString("` · user_id `")
	b.WriteString(escapeMrkdwnCode(meta.UserID))
	b.WriteString("` · team_id `")
	b.WriteString(escapeMrkdwnCode(meta.TeamID))
	b.WriteString("`")
	return b.String()
}
