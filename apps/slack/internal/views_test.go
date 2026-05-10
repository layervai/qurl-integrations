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
	if got["response_type"] != responseTypeEphemeral {
		t.Errorf("response_type = %v, want %s", got["response_type"], responseTypeEphemeral)
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

// TestHelpResponse_MentionsSubcommands fences the help text content.
// The slash-command grammar declares a fixed set of subcommands; if
// help silently drops one, users discover it the hard way. This is
// a substring check on the rendered text — not a full parse — so
// reformatting the help doesn't churn the test.
func TestHelpResponse_MentionsSubcommands(t *testing.T) {
	t.Parallel()
	raw, err := HelpResponse()
	if err != nil {
		t.Fatalf("HelpResponse: %v", err)
	}
	body := string(raw)
	for _, want := range []string{"qurl get", "qurl setalias", "qurl admin", "qurl aliases"} {
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

// TestAdminClaimModal_PrivateValueOnCode is the load-bearing
// security fence: the bootstrap-code input MUST have
// `private_value: true` so Slack masks it in the UI and elides it
// from view_submission audit logs. A regression that drops this
// flag re-opens Blocker #3.
func TestAdminClaimModal_PrivateValueOnCode(t *testing.T) {
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
	var foundPrivate bool
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
		if pv, ok := el["private_value"].(bool); !ok || !pv {
			t.Errorf("private_value = %v (type %T), want true", el["private_value"], el["private_value"])
		}
		foundPrivate = true
	}
	if !foundPrivate {
		t.Fatal("admin claim modal: input block for bootstrap code not found — Blocker #3 fence broken")
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
