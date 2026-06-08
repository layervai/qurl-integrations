package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToSDKMessages_PreservesToolUseAndResult locks down the riskiest part of
// the SDK seam: rebuilding a valid transcript (tool_use followed by
// tool_result) from persisted domain messages. The fake-LLM loop tests bypass
// this translation, so it's verified here by marshaling to the wire shape.
func TestToSDKMessages_PreservesToolUseAndResult(t *testing.T) {
	history := []Message{
		{Role: roleUser, Text: "what can I reach?"},
		{Role: roleAssistant, Text: "Let me check.", ToolCalls: []ToolCall{
			{ID: "tu_1", Name: toolListResources, Input: json.RawMessage(`{}`)},
		}},
		{Role: roleUser, ToolResults: []ToolResult{
			{ToolUseID: "tu_1", Content: "staging-dash (r_1)", IsError: false},
		}},
		{Role: roleAssistant, Text: "You can reach staging-dash."},
	}

	params := toSDKMessages(history)
	if len(params) != 4 {
		t.Fatalf("expected 4 SDK messages, got %d", len(params))
	}

	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(raw)

	for _, want := range []string{
		`"tool_use"`, `"tu_1"`, toolListResources, // assistant tool_use preserved
		`"tool_result"`, "staging-dash (r_1)", // user tool_result preserved
		`"role":"assistant"`, `"role":"user"`,
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire history missing %q\n%s", want, wire)
		}
	}
}

func TestToSDKMessages_SkipsEmptyAssistantTurn(t *testing.T) {
	// An assistant message with neither text nor tool calls produces no blocks
	// and must be skipped (the SDK rejects empty-content messages).
	params := toSDKMessages([]Message{
		{Role: roleAssistant},
		{Role: roleUser, Text: "hi"},
	})
	if len(params) != 1 {
		t.Fatalf("expected the empty assistant turn to be skipped, got %d messages", len(params))
	}
}

func TestToSDKTools_ShapeAndRequired(t *testing.T) {
	tools := toSDKTools([]ToolSpec{{
		Name:        toolResolveToken,
		Description: "resolve a token",
		Schema:      map[string]any{fieldToken: stringProp("the token")},
		Required:    []string{fieldToken},
	}})
	raw, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(raw)
	for _, want := range []string{toolResolveToken, "resolve a token", `"input_schema"`, `"required"`, fieldToken} {
		if !strings.Contains(wire, want) {
			t.Errorf("tool wire missing %q\n%s", want, wire)
		}
	}
}

func TestToSDKTools_NilSchemaBecomesEmptyObject(t *testing.T) {
	// A read tool with no parameters must still marshal a valid object schema.
	tools := toSDKTools([]ToolSpec{{Name: toolListResources, Description: "list"}})
	raw, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"object"`) {
		t.Errorf("expected an object input schema, got %s", raw)
	}
}
