package internal

import (
	"encoding/json"
	"strings"
	"testing"
)

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

// TestSetAliasRebindModal_NewlineInjectionEscaped fences the second
// code-span breakout: Slack's mrkdwn renderer ends a code span at
// a hard newline. A target containing \n, \r, or the \r\n pair
// would otherwise let subsequent mrkdwn render outside the span.
// escapeMrkdwnCode substitutes a single space. Each line-break
// shape is exercised separately so a future refactor that drops
// (e.g.) the bare \r substitution can't slip past silently — Slack
// normalizes most clients to \n, but the escape boundary must be
// tight since it's a stated security mitigation.
func TestSetAliasRebindModal_NewlineInjectionEscaped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		target string
	}{
		{"LF", "old\n<!channel>\nx"},
		{"CR", "old\r<!channel>\rx"},
		{"CRLF", "old\r\n<!channel>\r\nx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := SetAliasRebindModal("prod-db", tc.target, "new")
			if err != nil {
				t.Fatalf("SetAliasRebindModal: %v", err)
			}
			body := string(raw)
			if strings.Contains(body, "<!channel>") {
				// The mention-syntax is JSON-encoded so `<` becomes
				// `<` in the marshaled body; the actual leak vector
				// is the literal mention text reaching Slack's
				// mrkdwn renderer. Either form means escape didn't
				// apply.
				t.Errorf("line-break-injected payload leaked into body: %s", body)
			}
			// None of \n, \r, or the CRLF pair should survive into
			// the marshaled JSON for the target spans (JSON encodes
			// them as `\n` / `\r` escape sequences, which is the
			// same wire shape Slack treats as a hard newline). The
			// escape replaces them with a literal space pre-marshal.
			for _, want := range []string{`\n`, `\r`} {
				if strings.Contains(body, want) {
					t.Errorf("line-break escape sequence %q survived into body: %s", want, body)
				}
			}
		})
	}
}

// TestSetAliasRebindModal_PrivateMetadataIsJSON fences the
// `private_metadata` encoding contract. The value must round-trip
// through `json.Unmarshal` into a [SetAliasRebindMetadata] — the
// view-submission handler in PR-3c.3+ depends on this shape for
// both the alias name and the new target it applies on submit. An
// alias name containing `=` (allowed by qurl-service) would have
// broken the previous `key=value` ad-hoc encoding.
func TestSetAliasRebindModal_PrivateMetadataIsJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		alias     string
		newTarget string
	}{
		{"plain alias", "prod-db", "https://internal.example.com"},
		{"alias with equals (would have broken k=v)", "key=val", "https://x.example"},
		{"alias with ampersand", "a&b", "https://x.example?q=1&r=2"},
		{"alias with quote", `q"db`, `https://example.com/path?q="hi"`},
		{"resource_id target", "prod-db", "r_abc123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := SetAliasRebindModal(tc.alias, "old", tc.newTarget)
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
			if meta.NewTarget != tc.newTarget {
				t.Errorf("new_target = %q, want %q", meta.NewTarget, tc.newTarget)
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
//     truth — queried via [IsRedactedSubmissionBlock]).
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
				t.Errorf("element carries fictional masking key %q — Slack ignores this; redaction must live at the logging boundary, see IsRedactedSubmissionBlock", fake)
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
