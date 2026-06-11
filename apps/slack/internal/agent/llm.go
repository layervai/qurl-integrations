package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// defaultAgentMaxTokens caps the model's output. The agent emits either a short
// reply/question or a single tool call, so a small ceiling is plenty and keeps
// the chat surface responsive.
const defaultAgentMaxTokens = 2048

// anthropicLLM is the production [LLM], backed by the Anthropic Go SDK and
// Claude Sonnet 4.6.
type anthropicLLM struct {
	client    anthropic.Client
	model     anthropic.Model
	maxTokens int64
}

// NewAnthropicLLM constructs an [LLM] backed by Claude Sonnet 4.6 using the
// given API key.
func NewAnthropicLLM(apiKey string) LLM {
	return &anthropicLLM{
		client:    anthropic.NewClient(option.WithAPIKey(apiKey)),
		model:     anthropic.ModelClaudeSonnet4_6,
		maxTokens: defaultAgentMaxTokens,
	}
}

// Complete implements [LLM]. Parallel tool use is disabled so each turn yields at
// most one tool call, and thinking is disabled: this is a low-latency
// natural-language→tool-call translation layer, not a reasoning-heavy task.
func (l *anthropicLLM) Complete(ctx context.Context, req *Request) (Response, error) {
	msg, err := l.client.Messages.New(ctx, l.buildParams(req))
	if err != nil {
		return Response{}, fmt.Errorf("anthropic messages.new: %w", err)
	}
	return fromSDKMessage(msg), nil
}

// StreamComplete implements [streamingLLM]: it issues the same request as
// [anthropicLLM.Complete] but over the streaming endpoint, forwarding each text
// delta to onText as the model emits it, and returns the fully-assembled
// [Response] once the stream ends. The request params ([buildParams]) and the
// final flattening ([fromSDKMessage]) are shared with Complete, so a streamed turn
// and a non-streamed turn produce identical text, tool calls, and usage — only the
// delivery timing differs. onText may be nil (then this is just a streaming Complete).
func (l *anthropicLLM) StreamComplete(ctx context.Context, req *Request, onText func(delta string)) (Response, error) {
	stream := l.client.Messages.NewStreaming(ctx, l.buildParams(req))
	defer func() { _ = stream.Close() }() // best-effort cleanup; the read error is reported via stream.Err below

	// Accumulate rebuilds the full Message from the SSE events; we additionally tap
	// the text deltas as they pass so the caller can render them live.
	var msg anthropic.Message
	for stream.Next() {
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			return Response{}, fmt.Errorf("anthropic stream accumulate: %w", err)
		}
		if onText != nil {
			if delta := streamTextDelta(&event); delta != "" {
				onText(delta)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return Response{}, fmt.Errorf("anthropic messages stream: %w", err)
	}
	return fromSDKMessage(&msg), nil
}

// streamTextDelta returns the assistant text a streaming event carries, or "" for
// any non-text event. It reads the flattened [anthropic.MessageStreamEventUnion]
// fields (Type + Delta.Text) the SDK already populates at decode time, rather than
// the typed .AsAny() accessors, which re-parse the event JSON on every call — this
// runs once per token on the streaming hot path. Delta.Text is non-empty only for a
// content_block_delta's text delta (an input_json / stop / thinking delta fills a
// different field), and the explicit type guard keeps that intent legible. Taken by
// pointer: the event union is a large struct, so per-token copies would add up.
func streamTextDelta(event *anthropic.MessageStreamEventUnion) string {
	if event.Type == "content_block_delta" {
		return event.Delta.Text
	}
	return ""
}

// buildParams assembles the Messages API request. Pure (no network) so the
// cache-breakpoint placement is unit-testable.
func (l *anthropicLLM) buildParams(req *Request) anthropic.MessageNewParams {
	return anthropic.MessageNewParams{
		Model:     l.model,
		MaxTokens: l.maxTokens,
		System:    systemBlocks(req),
		Messages:  toSDKMessages(req.Messages),
		Tools:     toSDKTools(req.Tools),
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfAuto: &anthropic.ToolChoiceAutoParam{DisableParallelToolUse: anthropic.Bool(true)},
		},
		Thinking: anthropic.ThinkingConfigParamUnion{OfDisabled: &anthropic.ThinkingConfigDisabledParam{}},
		// A second cache breakpoint, auto-placed on the last message block. Within
		// a turn's up-to-6 round-trips TurnContext is fixed and the transcript
		// only grows, so each round-trip reads the prior cache and extends it —
		// the robust win. (Cross-turn it hits only when the same user replies,
		// since the per-turn context block precedes the messages.) Harmless no-op
		// while the prefix is below the model's minimum cacheable length.
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}
}

// systemBlocks renders the system prompt as SDK blocks. The stable preamble
// carries a cache_control breakpoint, so the prompt prefix (tools render before
// system, so they're included) is cached across the turn's round-trips and
// across turns in a thread; the per-turn context follows it uncached. Anthropic
// only caches a prefix once it exceeds the model's minimum cacheable length, so
// on a short prompt the breakpoint is a harmless no-op.
func systemBlocks(req *Request) []anthropic.TextBlockParam {
	blocks := make([]anthropic.TextBlockParam, 0, 2)
	if req.SystemStable != "" {
		blocks = append(blocks, anthropic.TextBlockParam{
			Text:         req.SystemStable,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		})
	}
	if req.SystemPerTurn != "" {
		blocks = append(blocks, anthropic.TextBlockParam{Text: req.SystemPerTurn})
	}
	return blocks
}

// toSDKTools converts domain tool specs to SDK tool params.
func toSDKTools(specs []ToolSpec) []anthropic.ToolUnionParam {
	tools := make([]anthropic.ToolUnionParam, 0, len(specs))
	for _, s := range specs {
		props := s.Schema
		if props == nil {
			props = map[string]any{}
		}
		tool := anthropic.ToolParam{
			Name:        s.Name,
			Description: anthropic.String(s.Description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: props,
				Required:   s.Required,
			},
		}
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &tool})
	}
	return tools
}

// toSDKMessages rebuilds the SDK message history from domain messages,
// preserving tool_use and tool_result blocks so the model sees a valid
// transcript across turns.
func toSDKMessages(msgs []Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		if m.Role == roleAssistant {
			if block := assistantBlocks(m); len(block) > 0 {
				out = append(out, anthropic.NewAssistantMessage(block...))
			}
			continue
		}
		// roleUser: either tool results or a plain text message.
		if len(m.ToolResults) > 0 {
			blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.ToolResults))
			for _, tr := range m.ToolResults {
				blocks = append(blocks, anthropic.NewToolResultBlock(tr.ToolUseID, tr.Content, tr.IsError))
			}
			out = append(out, anthropic.NewUserMessage(blocks...))
			continue
		}
		out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Text)))
	}
	return out
}

// assistantBlocks renders an assistant turn's text + tool_use blocks.
func assistantBlocks(m *Message) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.ToolCalls)+1)
	if m.Text != "" {
		blocks = append(blocks, anthropic.NewTextBlock(m.Text))
	}
	for _, tc := range m.ToolCalls {
		var input any = tc.Input
		if len(tc.Input) == 0 {
			input = map[string]any{}
		}
		blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Name))
	}
	return blocks
}

// fromSDKMessage flattens an SDK response into the domain [Response].
func fromSDKMessage(msg *anthropic.Message) Response {
	var text strings.Builder
	var calls []ToolCall
	for i := range msg.Content {
		switch v := msg.Content[i].AsAny().(type) {
		case anthropic.TextBlock:
			text.WriteString(v.Text)
		case anthropic.ToolUseBlock:
			calls = append(calls, ToolCall{
				ID:    v.ID,
				Name:  v.Name,
				Input: append(json.RawMessage(nil), v.Input...),
			})
		}
	}
	return Response{
		Text:       text.String(),
		ToolCalls:  calls,
		StopReason: string(msg.StopReason),
		Usage: Usage{
			InputTokens:              msg.Usage.InputTokens,
			OutputTokens:             msg.Usage.OutputTokens,
			CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
		},
	}
}
