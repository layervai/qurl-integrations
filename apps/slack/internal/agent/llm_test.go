package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestFromSDKMessage_MapsUsage(t *testing.T) {
	// Distinct values per counter so a transposed field (e.g. cache-creation vs
	// cache-read) is caught.
	resp := fromSDKMessage(&anthropic.Message{
		Usage: anthropic.Usage{
			InputTokens:              11,
			OutputTokens:             22,
			CacheCreationInputTokens: 33,
			CacheReadInputTokens:     44,
		},
	})
	want := Usage{InputTokens: 11, OutputTokens: 22, CacheCreationInputTokens: 33, CacheReadInputTokens: 44}
	if resp.Usage != want {
		t.Fatalf("usage mapping = %+v, want %+v", resp.Usage, want)
	}
}

func TestSystemBlocks_CachesStablePreambleOnly(t *testing.T) {
	blocks := systemBlocks(&Request{SystemStable: "RULES PREAMBLE", SystemPerTurn: "per-turn context"})
	if len(blocks) != 2 {
		t.Fatalf("want 2 system blocks, got %d", len(blocks))
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(raw)
	// Exactly one cache breakpoint, on the stable preamble — the per-turn block
	// must stay uncached so it doesn't invalidate the cached prefix each turn.
	if n := strings.Count(wire, "cache_control"); n != 1 {
		t.Fatalf("expected exactly one cache_control (stable block), got %d: %s", n, wire)
	}
	if !strings.Contains(wire, "RULES PREAMBLE") || !strings.Contains(wire, "per-turn context") {
		t.Fatalf("both blocks must be present: %s", wire)
	}
}

func TestSystemBlocks_OmitsEmptyParts(t *testing.T) {
	// Per-turn only → one block, and it must NOT carry the breakpoint (caching
	// per-turn context would defeat the cross-turn prefix cache).
	perTurn := systemBlocks(&Request{SystemPerTurn: "only per-turn"})
	if len(perTurn) != 1 {
		t.Fatalf("empty stable should yield 1 block, got %d", len(perTurn))
	}
	if ptRaw, _ := json.Marshal(perTurn); strings.Contains(string(ptRaw), "cache_control") {
		t.Fatalf("per-turn-only block must not carry cache_control: %s", ptRaw)
	}
	// Stable only → one block, and it MUST keep the breakpoint, so an empty
	// per-turn context never silently drops caching.
	stable := systemBlocks(&Request{SystemStable: "only stable"})
	if len(stable) != 1 {
		t.Fatalf("empty per-turn should yield 1 block, got %d", len(stable))
	}
	if stRaw, _ := json.Marshal(stable); !strings.Contains(string(stRaw), "cache_control") {
		t.Fatalf("stable-only block must carry cache_control: %s", stRaw)
	}
}

func TestBuildParams_SetsBothCacheBreakpoints(t *testing.T) {
	l := &anthropicLLM{} // model/maxTokens irrelevant to the cache wiring
	params := l.buildParams(&Request{
		SystemStable:  "RULES",
		SystemPerTurn: "ctx",
		Tools:         toolSpecs(),
		Messages:      []Message{{Role: roleUser, Text: "hi"}},
	})
	// Message-level breakpoint (auto-places on the last message block).
	if cc, _ := json.Marshal(params.CacheControl); !strings.Contains(string(cc), "ephemeral") {
		t.Fatalf("message-level cache breakpoint not set: %s", cc)
	}
	// System breakpoint: exactly one, on the stable block.
	if sys, _ := json.Marshal(params.System); strings.Count(string(sys), "cache_control") != 1 {
		t.Fatalf("expected exactly one system cache breakpoint: %s", sys)
	}
}

// TestSystemBlocks_ReassembleToSystemPrompt locks the two-block split to the
// single-string systemPrompt that the prompt-invariant tests assert on, so a
// future edit can't make the cached blocks diverge from what those tests check.
func TestSystemBlocks_ReassembleToSystemPrompt(t *testing.T) {
	tc := &TurnContext{ChannelName: "oncall", ChannelID: "C1", UserID: "U1", CallerIsAdmin: true}
	var concat strings.Builder
	for _, b := range systemBlocks(&Request{SystemStable: systemPreamble, SystemPerTurn: turnContextLines(tc)}) {
		concat.WriteString(b.Text)
	}
	if concat.String() != systemPrompt(tc) {
		t.Fatalf("system blocks must reassemble to systemPrompt(tc)")
	}
}

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
