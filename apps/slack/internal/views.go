package internal

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Block Kit JSON templates for the Slack bot's view-side surfaces.
//
// PR-3c.1 keeps these as pure JSON-producing helpers — the handler
// in PR-3c.3+ will pass them to `views.open` / `response_url` /
// `chat.postMessage` as appropriate. We don't depend on the
// slack-go/slack library here to avoid pulling a heavyweight
// transitive into the binary just for a fixed set of view payloads.

// responseTypeEphemeral is the Slack response_type that scopes a
// message to the requester (visible only to them, in-channel).
// Lifted to a constant because it appears in every response payload
// shape — `:warning:` errors, the help block, the spinner replacement.
const responseTypeEphemeral = "ephemeral"

// callbackIDSetAliasRebind is the modal callback used by the
// `setalias` rebind confirmation flow. The view-submission handler
// matches on this to know which path to take.
const callbackIDSetAliasRebind = "setalias_rebind_confirm"

// callbackIDAdminClaim is the modal callback used by the bootstrap
// code claim flow. Distinct from the rebind callback so a single
// submission handler can route by ID.
const callbackIDAdminClaim = "admin_claim_redeem"

// blockIDClaimCode is the block ID for the bootstrap-code field in
// the admin-claim modal. Stable so view-submission handlers can pull
// the value out by a known key — and so the bot's logging middleware
// (PR-3c.3+) can match on it for the redaction set in
// [RedactedSubmissionBlockIDs].
const blockIDClaimCode = "claim_code_block"

// actionIDClaimCode is the action ID for the bootstrap-code input
// element. Slack's view_submission payload nests the value as
// `state.values[block_id][action_id].value`, so both IDs need to be
// stable.
const actionIDClaimCode = "claim_code_input"

// RedactedSubmissionBlockIDs is the set of view-submission `block_id`
// values whose `state.values[block_id]` payload MUST NOT be logged or
// otherwise echoed by the bot. The handler's logging middleware in
// PR-3c.3+ consults this set before serializing a `view_submission`
// for diagnostics — entries here are replaced with a sentinel.
//
// Why a set rather than a Slack-level masking primitive: Slack's
// Block Kit `plain_text_input` element has no input-masking field
// (no `private_value`, no `secret`, no `masked` — verified against
// Slack's reference docs). The Slack client UI will render the
// bootstrap code in plaintext, and the `view_submission` payload
// will carry it in `state.values[claim_code_block][claim_code_input]
// .value`. The mitigation for Blocker #3 ("no plaintext bootstrap
// codes anywhere user-visible") therefore lives at the bot's
// logging boundary, not in the modal payload itself: the code
// transits the wire once (TLS to the bot, bot to qurl-service via
// [AdminClient.RedeemBootstrap]) and is never written to logs or
// telemetry. This map is the single source of truth for that
// guarantee.
var RedactedSubmissionBlockIDs = map[string]struct{}{
	blockIDClaimCode: {},
}

// HelpResponse renders the JSON for `/qurl help`. Returned as the
// slash-command HTTP response body (not a modal).
func HelpResponse() ([]byte, error) {
	payload := map[string]any{
		"response_type": responseTypeEphemeral,
		"blocks": []any{
			sectionBlock("*/qurl* — Create and manage qURLs from Slack"),
			dividerBlock(),
			sectionBlock(strings.Join([]string{
				"*Aliased commands (alias-only world)*",
				"`/qurl get $alias` — mint an access link for an alias",
				"`/qurl get $alias dm:true` — DM the link instead of channel ephemeral",
				"`/qurl setalias $alias <url-or-resource_id>` — bind an alias (admin only)",
				"`/qurl unsetalias $alias` — clear an alias (admin only)",
				"`/qurl aliases` — list channel-allowed aliases",
			}, "\n")),
			dividerBlock(),
			sectionBlock(strings.Join([]string{
				"*Admin commands*",
				"`/qurl admin claim` — open the bootstrap-code modal",
				"`/qurl admin allow #channel $alias` — allow alias in channel",
				"`/qurl admin disallow #channel $alias` — remove alias from channel",
				"`/qurl admin policies` — list channel/alias policies",
			}, "\n")),
			dividerBlock(),
			sectionBlock("`/qurl help` — show this message"),
		},
	}
	return json.Marshal(payload)
}

// SetAliasRebindMetadata is the typed shape the rebind modal stores
// in `private_metadata`. JSON-encoded so the view-submission handler
// (PR-3c.3+) can `json.Unmarshal` into a known struct rather than
// parsing an ad-hoc query-string. Slack caps `private_metadata` at
// 3000 chars; a JSON object with a single alias name fits comfortably.
type SetAliasRebindMetadata struct {
	Alias string `json:"alias"`
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
func SetAliasRebindModal(aliasName, oldTarget, newTarget string) ([]byte, error) {
	meta, err := json.Marshal(SetAliasRebindMetadata{Alias: aliasName})
	if err != nil {
		return nil, fmt.Errorf("marshal private_metadata: %w", err)
	}
	payload := map[string]any{
		"type":             "modal",
		"callback_id":      callbackIDSetAliasRebind,
		"title":            plainTextObj("Confirm alias rebind"),
		"submit":           plainTextObj("Rebind"),
		"close":            plainTextObj("Cancel"),
		"private_metadata": string(meta),
		"blocks": []any{
			sectionBlock(fmt.Sprintf("Alias `$%s` is already bound.", aliasName)),
			sectionBlock(fmt.Sprintf("*Current target:* `%s`", oldTarget)),
			sectionBlock(fmt.Sprintf("*New target:* `%s`", newTarget)),
			contextBlock("This action overwrites the existing binding for everyone in the workspace."),
		},
	}
	return json.Marshal(payload)
}

// AdminClaimModal renders the modal shown when a user runs
// `/qurl admin claim`. The bootstrap code is collected via a regular
// `plain_text_input` (Slack's Block Kit has no input-masking
// primitive — see [RedactedSubmissionBlockIDs] for the mitigation
// strategy). The user-visible `:lock:` context line documents the
// guarantee: the bot never logs the submitted code; the only place
// it travels is into [AdminClient.RedeemBootstrap] over TLS.
//
// This satisfies Blocker #3 (no plaintext bootstrap codes anywhere
// user-visible) at the slash-command grammar layer (the parser
// rejects any `admin claim <args>` form so the code can't be typed
// into the slash-command box) and at the bot's logging boundary
// (handler middleware redacts [blockIDClaimCode] before serializing
// any `view_submission` payload for diagnostics).
func AdminClaimModal() ([]byte, error) {
	payload := map[string]any{
		"type":        "modal",
		"callback_id": callbackIDAdminClaim,
		"title":       plainTextObj("Claim workspace"),
		"submit":      plainTextObj("Submit"),
		"close":       plainTextObj("Cancel"),
		"blocks": []any{
			sectionBlock("Paste the bootstrap code your admin received from LayerV. The code is single-use."),
			map[string]any{
				"type":     "input",
				"block_id": blockIDClaimCode,
				"label":    plainTextObj("Bootstrap code"),
				"element": map[string]any{
					"type":        "plain_text_input",
					"action_id":   actionIDClaimCode,
					"placeholder": plainTextObj("e.g. boot_xxxx-xxxx-xxxx"),
				},
			},
			contextBlock(":lock: The bot never logs this code. It is sent once to LayerV over TLS and discarded from memory after redemption."),
		},
	}
	return json.Marshal(payload)
}

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
		"response_type":    responseTypeEphemeral,
		"replace_original": replaceOriginal,
		"text":             ":warning: " + message,
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

// contextBlock returns a `context` block with a single mrkdwn element.
// Used for the "subtext" rows in modals (e.g. the `:lock:` warning).
func contextBlock(text string) map[string]any {
	return map[string]any{
		"type": "context",
		"elements": []any{
			map[string]any{
				"type": "mrkdwn",
				"text": text,
			},
		},
	}
}

// dividerBlock returns a divider. No payload — just a structural
// separator between sections.
func dividerBlock() map[string]any {
	return map[string]any{"type": "divider"}
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
