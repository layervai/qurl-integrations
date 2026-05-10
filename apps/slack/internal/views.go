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
// the value out by a known key.
const blockIDClaimCode = "claim_code_block"

// actionIDClaimCode is the action ID for the bootstrap-code input
// element. Slack's view_submission payload nests the value as
// `state.values[block_id][action_id].value`, so both IDs need to be
// stable.
const actionIDClaimCode = "claim_code_input"

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

// SetAliasRebindModal renders the confirmation modal shown when a user
// runs `setalias $alias <new-target>` and the alias already points
// at a different resource. The user has to explicitly confirm the
// rebind — silently overwriting an alias is the kind of action that
// causes incident-response chaos in a multi-admin workspace.
//
// `aliasName` is the alias being rebound (no `$` sigil); `oldTarget`
// and `newTarget` are the human-readable strings to show side-by-side.
// `triggerID` is the Slack-supplied trigger to pass to `views.open`
// (the caller wraps this payload accordingly).
func SetAliasRebindModal(aliasName, oldTarget, newTarget string) ([]byte, error) {
	payload := map[string]any{
		"type":             "modal",
		"callback_id":      callbackIDSetAliasRebind,
		"title":            plainTextObj("Confirm alias rebind"),
		"submit":           plainTextObj("Rebind"),
		"close":            plainTextObj("Cancel"),
		"private_metadata": "alias=" + aliasName,
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
// `/qurl admin claim`. The bootstrap code is a `private_value` field
// so it is NOT echoed back into the `view_submission` payload as
// plaintext (Slack treats `private_value` inputs differently from
// regular text inputs — they're masked in client UI and redacted in
// audit-logging surfaces). This satisfies Blocker #3 (no plaintext
// codes via the slash-command grammar) and adds an additional layer
// of defense for the post-submission hop.
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
					"type":      "plain_text_input",
					"action_id": actionIDClaimCode,
					// `private_value: true` masks the value in the
					// Slack client UI and elides it from
					// view_submission audit logs. Belt-and-braces
					// against Blocker #3.
					"private_value": true,
					"placeholder":   plainTextObj("e.g. boot_xxxx-xxxx-xxxx"),
				},
			},
			contextBlock(":lock: Codes are never echoed in slash-command text or audit logs."),
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
		"response_type":    "ephemeral",
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
