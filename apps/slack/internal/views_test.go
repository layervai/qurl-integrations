package internal

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHelpResponse_ValidJSON fences the help payload's wire shape.
// We don't assert exact text — that churns whenever the help message
// updates — but we do require a parseable JSON object, an
// `ephemeral` response_type, and at least a few non-empty section
// blocks so an accidentally-empty payload is caught.
func TestHelpResponse_ValidJSON(t *testing.T) {
	t.Parallel()
	raw, err := HelpResponse()
	if err != nil {
		t.Fatalf("HelpResponse: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid JSON: %v\nbody: %s", err, raw)
	}
	if got["response_type"] != respTypeEphemeral {
		t.Errorf("response_type = %v, want %s", got["response_type"], respTypeEphemeral)
	}
	blocks, ok := got["blocks"].([]any)
	if !ok {
		t.Fatalf("blocks not an array: %T", got["blocks"])
	}
	if len(blocks) < 3 {
		t.Errorf("blocks length = %d, want at least 3", len(blocks))
	}
	// Spot-check at least one block is a section with text. Slack's
	// renderer rejects sections that lack a text object, so we
	// fence that early.
	foundSection := false
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && m["type"] == "section" {
			if _, hasText := m["text"]; hasText {
				foundSection = true
				break
			}
		}
	}
	if !foundSection {
		t.Error("no section block with text — Slack will reject this payload")
	}
}

// TestHelpResponse_MentionsSubcommands fences the help text content
// against the parser grammar. Every verb declared in parser.go must
// appear in the help body — a regression that drops a verb from
// help silently degrades discoverability. Substring checks (not
// full parse) so reformatting the help doesn't churn the test.
func TestHelpResponse_MentionsSubcommands(t *testing.T) {
	t.Parallel()
	raw, err := HelpResponse()
	if err != nil {
		t.Fatalf("HelpResponse: %v", err)
	}
	body := string(raw)
	// Every Subcommand + AdminAction from parser.go must surface.
	for _, want := range []string{
		"qurl get",
		"qurl setalias",
		"qurl unsetalias",
		"qurl aliases",
		"qurl admin claim",
		"qurl admin allow",
		"qurl admin disallow",
		"qurl admin policies",
		"qurl admin status",
		"qurl admin revoke",
		"qurl create",
		"qurl list",
		"qurl help",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("help body missing %q", want)
		}
	}
}

// TestSetAliasRebindModal_Shape fences the modal payload structure.
// Slack rejects any modal missing `type`, `title`, or `callback_id`,
// so each is non-optional in the schema-shape sense.
func TestSetAliasRebindModal_Shape(t *testing.T) {
	t.Parallel()
	raw, err := SetAliasRebindModal("prod-db", "https://old.example", "https://new.example")
	if err != nil {
		t.Fatalf("SetAliasRebindModal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["type"] != "modal" {
		t.Errorf("type = %v, want modal", got["type"])
	}
	if got["callback_id"] != callbackIDSetAliasRebind {
		t.Errorf("callback_id = %v, want %s", got["callback_id"], callbackIDSetAliasRebind)
	}
	for _, k := range []string{"title", "submit", "close", "blocks"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing required key %q", k)
		}
	}
	// Confirm the alias name + targets are surfaced in the body so
	// the user can review the rebind before confirming.
	body := string(raw)
	for _, want := range []string{"prod-db", "old.example", "new.example"} {
		if !strings.Contains(body, want) {
			t.Errorf("modal body missing %q", want)
		}
	}
}

// TestSetAliasRebindModal_BacktickInjectionEscaped fences the
// mrkdwn-code-span guard. A malicious admin who sets a target
// containing a backtick could otherwise break out of the
// `oldTarget` / `newTarget` code spans and inject mrkdwn (e.g.
// `<!channel>`) into another admin's rebind confirmation modal.
// escapeMrkdwnCode replaces backticks with the modifier-letter
// prime (U+02CA), so the substring "<!channel>" never appears
// outside the escaped code span.
func TestSetAliasRebindModal_BacktickInjectionEscaped(t *testing.T) {
	t.Parallel()
	raw, err := SetAliasRebindModal("prod-db", "old`<!channel>`junk", "new`<@U123>`x")
	if err != nil {
		t.Fatalf("SetAliasRebindModal: %v", err)
	}
	body := string(raw)
	// The mrkdwn payloads should not contain a literal backtick
	// from user input — the only backticks present should be the
	// ones we wrap around the spans ourselves.
	for _, leak := range []string{"<!channel>", "<@U123>"} {
		if strings.Contains(body, leak) {
			t.Errorf("backtick-injected payload %q leaked into rendered body: %s", leak, body)
		}
	}
	// The escaped substitute (U+02CA) should appear in the body
	// so the user still sees a visual approximation of the value.
	if !strings.Contains(body, "ˊ") {
		t.Error("escapeMrkdwnCode substitute (U+02CA) missing — backticks were not escaped")
	}
}

// TestSetAliasRebindModal_PrivateMetadataIsJSON fences the
// `private_metadata` encoding contract. The value must round-trip
// through `json.Unmarshal` into a [SetAliasRebindMetadata] — the
// view-submission handler in PR-3c.3+ depends on this shape. An
// alias name containing `=` (allowed by qurl-service) would have
// broken the previous `key=value` ad-hoc encoding.
func TestSetAliasRebindModal_PrivateMetadataIsJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		alias string
	}{
		{"plain alias", "prod-db"},
		{"alias with equals (would have broken k=v)", "key=val"},
		{"alias with ampersand", "a&b"},
		{"alias with quote", `q"db`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := SetAliasRebindModal(tc.alias, "old", "new")
			if err != nil {
				t.Fatalf("SetAliasRebindModal: %v", err)
			}
			var modal map[string]any
			if err := json.Unmarshal(raw, &modal); err != nil {
				t.Fatalf("modal JSON: %v", err)
			}
			pm, ok := modal["private_metadata"].(string)
			if !ok {
				t.Fatalf("private_metadata not a string: %T", modal["private_metadata"])
			}
			var meta SetAliasRebindMetadata
			if err := json.Unmarshal([]byte(pm), &meta); err != nil {
				t.Fatalf("private_metadata is not valid JSON: %v\nraw: %s", err, pm)
			}
			if meta.Alias != tc.alias {
				t.Errorf("alias = %q, want %q", meta.Alias, tc.alias)
			}
		})
	}
}

// TestAdminClaimModal_StableIDsAndNoFakeMasking is the load-bearing
// security fence for Blocker #3 ("no plaintext bootstrap codes
// anywhere user-visible"). Because Slack's Block Kit has no
// input-masking primitive (no `private_value`, no `secret`, no
// `masked` — verified against api.slack.com/reference), the
// mitigation lives at the bot's logging boundary: the handler
// middleware redacts `state.values[blockIDClaimCode]` before any
// view_submission payload is logged. This test fences the contract
// that boundary depends on:
//
//  1. The block_id is exactly [blockIDClaimCode] (single source of
//     truth in [RedactedSubmissionBlockIDs]).
//  2. The action_id is exactly [actionIDClaimCode] (so the
//     submission handler can pull the value out by a known key).
//  3. The element does NOT carry a misleading `private_value` /
//     `masked` / `secret` flag that would create a false sense of
//     security at review time.
func TestAdminClaimModal_StableIDsAndNoFakeMasking(t *testing.T) {
	t.Parallel()
	raw, err := AdminClaimModal()
	if err != nil {
		t.Fatalf("AdminClaimModal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["type"] != "modal" {
		t.Errorf("type = %v, want modal", got["type"])
	}
	if got["callback_id"] != callbackIDAdminClaim {
		t.Errorf("callback_id = %v, want %s", got["callback_id"], callbackIDAdminClaim)
	}
	blocks, _ := got["blocks"].([]any)
	var foundClaimBlock bool
	for _, b := range blocks {
		m, ok := b.(map[string]any)
		if !ok || m["type"] != "input" {
			continue
		}
		if m["block_id"] != blockIDClaimCode {
			continue
		}
		el, ok := m["element"].(map[string]any)
		if !ok {
			t.Fatalf("claim_code_block element is not an object: %T", m["element"])
		}
		if el["action_id"] != actionIDClaimCode {
			t.Errorf("action_id = %v, want %s", el["action_id"], actionIDClaimCode)
		}
		// Slack would silently accept any of these keys and ignore
		// them — leaving the bot exposed while review thinks the
		// field is masked. Fail loudly if any of them sneak back in.
		for _, fake := range []string{"private_value", "masked", "secret", "is_password"} {
			if _, present := el[fake]; present {
				t.Errorf("element carries fictional masking key %q — Slack ignores this; redaction must live at the logging boundary, see RedactedSubmissionBlockIDs", fake)
			}
		}
		foundClaimBlock = true
	}
	if !foundClaimBlock {
		t.Fatal("admin claim modal: input block for bootstrap code not found — Blocker #3 fence broken")
	}
	// Fence the redaction registry: the claim block_id must be
	// recognized by [IsRedactedSubmissionBlock], which is what the
	// handler middleware consults before logging.
	if !IsRedactedSubmissionBlock(blockIDClaimCode) {
		t.Errorf("IsRedactedSubmissionBlock(%q) = false — logging boundary will leak the bootstrap code", blockIDClaimCode)
	}
}

// TestIsRedactedSubmissionBlock exercises the redaction-registry
// query surface used by the (separate) handler package. Asserts the
// happy and miss paths so a refactor of the underlying storage
// shape (set, sync.Map, regex) can't silently change the contract.
func TestIsRedactedSubmissionBlock(t *testing.T) {
	t.Parallel()
	if !IsRedactedSubmissionBlock(blockIDClaimCode) {
		t.Errorf("IsRedactedSubmissionBlock(%q) = false, want true", blockIDClaimCode)
	}
	if IsRedactedSubmissionBlock("some_other_block") {
		t.Errorf("IsRedactedSubmissionBlock(\"some_other_block\") = true, want false")
	}
	if IsRedactedSubmissionBlock("") {
		t.Errorf("IsRedactedSubmissionBlock(\"\") = true, want false")
	}
}

// TestErrorResponse_Shape fences the friendly-error payload. Both
// branches (replace_original true/false) emit valid JSON with the
// `:warning:` prefix users learn to recognize.
func TestErrorResponse_Shape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		replace bool
	}{
		{"replace original", true},
		{"fresh response", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := ErrorResponse("alias not found", tc.replace)
			if err != nil {
				t.Fatalf("ErrorResponse: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			if got["response_type"] != "ephemeral" {
				t.Errorf("response_type = %v, want ephemeral", got["response_type"])
			}
			if got["replace_original"] != tc.replace {
				t.Errorf("replace_original = %v, want %v", got["replace_original"], tc.replace)
			}
			text, _ := got["text"].(string)
			if !strings.HasPrefix(text, ":warning:") {
				t.Errorf("text = %q, want :warning: prefix", text)
			}
			if !strings.Contains(text, "alias not found") {
				t.Errorf("text = %q, missing message", text)
			}
		})
	}
}
