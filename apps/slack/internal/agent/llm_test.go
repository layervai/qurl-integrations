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

// TestToSDKMessages_KeepsConsecutiveUserTurnsAfterProposal pins the no-coalescing
// invariant documented on toSDKMessages: a proposal/iteration-cap turn ends in a
// user{tool_results} message and the next turn prepends a fresh user{Text}, so the
// translation layer must emit that consecutive-user pair as two distinct messages
// (the Messages API merges same-role turns server-side — see toSDKMessages). If a
// future edit "fixes" the pair by coalescing it here, this fails.
func TestToSDKMessages_KeepsConsecutiveUserTurnsAfterProposal(t *testing.T) {
	const proposeID = "tu_propose"
	// Full cross-turn shape: turn 1 (user ask → assistant propose tool_use →
	// persisted propose ack tool_result) followed by turn 2's prepended user text.
	history := []Message{
		{Role: roleUser, Text: "protect the staging connector"},
		{Role: roleAssistant, ToolCalls: []ToolCall{
			{ID: proposeID, Name: toolProposeProtectConnector, Input: json.RawMessage(`{}`)},
		}},
		{Role: roleUser, ToolResults: []ToolResult{
			{ToolUseID: proposeID, Content: proposalAckResult},
		}},
		{Role: roleUser, Text: "actually, revoke it instead"},
	}

	params := toSDKMessages(history)

	// No coalescing of the consecutive user turns at positions 2 and 3: this fixture
	// has no empty (droppable) turns, so each domain Message maps to exactly one param
	// — merging the user pair would drop the count below len(history).
	if len(params) != len(history) {
		t.Fatalf("expected %d SDK messages (one per domain message, no coalescing), got %d", len(history), len(params))
	}
	// The trailing pair are both user-role: the persisted propose ack and the next
	// turn's prepended user text. These are what the API merges into one turn.
	if params[2].Role != anthropic.MessageParamRoleUser || params[3].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("trailing messages must both be user-role, got [2]=%q [3]=%q", params[2].Role, params[3].Role)
	}

	// The merged turn stays well-formed — assert the decoded blocks, not the marshaled
	// bytes, so the pairing is genuinely pinned (a substring match would pass even if
	// one half were dropped). Each asserted turn carries exactly one content block, so
	// the Content[0] indexing below is unambiguous and fails loudly if a fixture change
	// adds a block (e.g. giving the assistant turn Text would push tool_use off index 0).
	if len(params[1].Content) != 1 || len(params[2].Content) != 1 || len(params[3].Content) != 1 {
		t.Fatalf("each asserted turn should carry exactly one content block, got %d/%d/%d",
			len(params[1].Content), len(params[2].Content), len(params[3].Content))
	}
	// The assistant tool_use and the user tool_result carry the SAME id, the result
	// carries the propose ack, and the follow-up user text rides along as the second
	// user turn.
	toolUse := params[1].Content[0].OfToolUse
	toolResult := params[2].Content[0].OfToolResult
	followUp := params[3].Content[0].OfText
	if toolUse == nil || toolResult == nil || followUp == nil {
		t.Fatalf("unexpected block shapes: tool_use=%v tool_result=%v text=%v", toolUse, toolResult, followUp)
	}
	if toolUse.ID != proposeID || toolResult.ToolUseID != proposeID {
		t.Fatalf("tool_use/tool_result pairing broken: tool_use.ID=%q tool_result.ToolUseID=%q, want both %q", toolUse.ID, toolResult.ToolUseID, proposeID)
	}
	if ack := toolResult.Content[0].OfText; ack == nil || ack.Text != proposalAckResult {
		t.Fatalf("tool_result must carry the propose ack %q, got %+v", proposalAckResult, toolResult.Content)
	}
	if followUp.Text != "actually, revoke it instead" {
		t.Fatalf("follow-up user text = %q, want %q", followUp.Text, "actually, revoke it instead")
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

// TestStreamTextDelta_ExtractsTextDeltasOnly pins the streaming hot-path tap against
// real stream-event wire shapes. The fake-LLM loop tests (stream_test.go) drive the
// sink directly and never exercise anthropicLLM's SDK decode, so the extraction that
// turns raw events into live tokens is verified here by unmarshaling the SDK union —
// the same wire-shape approach TestToSDKMessages uses for the request seam. The
// load-bearing case is input_json_delta: it IS a content_block_delta, so the type
// guard passes, but it carries tool-call JSON (not assistant text) and must yield "".
func TestStreamTextDelta_ExtractsTextDeltasOnly(t *testing.T) {
	cases := []struct {
		name string
		wire string
		want string
	}{
		{
			name: "text delta is forwarded",
			wire: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`,
			want: "Hello ",
		},
		{
			name: "tool-args (input_json) delta is not assistant text",
			wire: `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"token\""}}`,
			want: "",
		},
		{
			name: "message_delta carries a stop reason, not text",
			wire: `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
			want: "",
		},
		{
			name: "content_block_start is not a delta",
			wire: `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var event anthropic.MessageStreamEventUnion
			if err := json.Unmarshal([]byte(tc.wire), &event); err != nil {
				t.Fatalf("unmarshal stream event: %v", err)
			}
			if got := streamTextDelta(&event); got != tc.want {
				t.Fatalf("streamTextDelta = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStreamAccumulate_MergesUsageAndContent verifies the production claim that the
// streaming path yields the same Response a non-streaming Complete would: that rests
// on the SDK's Accumulate merging input/cache tokens (from message_start) with the
// final output tokens (from message_delta) and reassembling the text — exactly what
// StreamComplete relies on. The fake-LLM loop tests (stream_test.go) bypass Accumulate,
// so it's driven here directly with a real message_start → delta → message_delta event
// sequence (closing the coverage gap the byte-identical-usage guarantee otherwise has).
func TestStreamAccumulate_MergesUsageAndContent(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":42,"output_tokens":1,"cache_creation_input_tokens":7,"cache_read_input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"there"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":13}}`,
		`{"type":"message_stop"}`,
	}
	var msg anthropic.Message
	for _, w := range events {
		var event anthropic.MessageStreamEventUnion
		if err := json.Unmarshal([]byte(w), &event); err != nil {
			t.Fatalf("unmarshal %q: %v", w, err)
		}
		if err := msg.Accumulate(event); err != nil {
			t.Fatalf("accumulate %q: %v", w, err)
		}
	}
	resp := fromSDKMessage(&msg)
	// Input + cache counters come from message_start; output is the final message_delta
	// value (not message_start's placeholder 1).
	want := Usage{InputTokens: 42, OutputTokens: 13, CacheCreationInputTokens: 7, CacheReadInputTokens: 5}
	if resp.Usage != want {
		t.Fatalf("accumulated usage = %+v, want %+v", resp.Usage, want)
	}
	if resp.Text != "hi there" {
		t.Fatalf("accumulated text = %q, want %q", resp.Text, "hi there")
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("accumulated stop reason = %q, want %q", resp.StopReason, "end_turn")
	}
}
