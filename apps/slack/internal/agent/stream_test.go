package agent

// Tests for the agent-loop streaming core (response-streaming PR1): when a per-turn
// stream sink is set (WithStreamSink) AND the LLM implements streamingLLM, Run
// forwards the assistant's text deltas to the sink as they're generated, while still
// returning the identical Result a non-streaming turn would. The defining property —
// and the one a careless refactor breaks — is that the sink observes a propose_*
// round's narration even though Run returns a Proposal (not a Reply): the text streams
// before the tool_use block arrives, and the stream is append-only, so there's no
// suppressing it. These tests pin that contract before the Slack delivery half (PR2)
// is built against it.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// streamingFakeLLM implements both LLM and streamingLLM. It replays one response per
// round (like scriptedLLM) and, on StreamComplete, emits the response's Text to onText
// in small chunks before returning the whole Response — modeling how the SDK surfaces
// tokens. The two counters let a test assert WHICH path Run took (Complete vs stream).
type streamingFakeLLM struct {
	responses     []Response
	idx           int
	completeCalls int
	streamCalls   int
}

func (s *streamingFakeLLM) next() (Response, error) {
	if s.idx >= len(s.responses) {
		return Response{}, errors.New("streamingFakeLLM: no more responses")
	}
	r := s.responses[s.idx]
	s.idx++
	return r, nil
}

func (s *streamingFakeLLM) Complete(_ context.Context, _ *Request) (Response, error) {
	s.completeCalls++
	return s.next()
}

func (s *streamingFakeLLM) StreamComplete(_ context.Context, _ *Request, onText func(string)) (Response, error) {
	s.streamCalls++
	r, err := s.next()
	if err != nil {
		return Response{}, err
	}
	if onText != nil {
		for _, chunk := range chunkText(r.Text, 4) {
			onText(chunk)
		}
	}
	return r, nil
}

// chunkText splits s into fixed-size rune chunks so the fake emits several deltas for
// any multi-character text; the concatenation of the chunks is exactly s.
func chunkText(s string, size int) []string {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	var out []string
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

// recordingSink captures the deltas a turn streamed, in order.
type recordingSink struct {
	deltas []string
}

func (r *recordingSink) fn(delta string) { r.deltas = append(r.deltas, delta) }
func (r *recordingSink) joined() string  { return strings.Join(r.deltas, "") }

// proposeRevokeRespWithText is a single round that narrates AND calls propose_revoke —
// the case where streaming surfaces text the non-streaming path drops.
func proposeRevokeRespWithText(text, token string) Response {
	raw, _ := json.Marshal(map[string]any{fieldToken: token})
	return Response{
		Text:       text,
		ToolCalls:  []ToolCall{{ID: "tu_propose", Name: toolProposeRevoke, Input: raw}},
		StopReason: "tool_use",
	}
}

// readRespWithText is a non-terminal round that narrates AND calls a read tool.
func readRespWithText(text, toolName string) Response {
	return Response{
		Text:       text,
		ToolCalls:  []ToolCall{{ID: "tu_" + toolName, Name: toolName, Input: json.RawMessage(`{}`)}},
		StopReason: "tool_use",
	}
}

func TestRun_Streaming_ForwardsFinalReplyDeltas(t *testing.T) {
	const reply = "You can reach the staging dashboard in this channel."
	llm := &streamingFakeLLM{responses: []Response{{
		Text:       reply,
		StopReason: "end_turn",
		Usage:      Usage{InputTokens: 11, OutputTokens: 7},
	}}}
	sink := &recordingSink{}
	ctx, tc := testCtx()

	res, _, err := New(llm, &fakeBackend{}, WithStreamSink(sink.fn)).Run(ctx, tc, nil, "what can I reach?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Streamed: the sink saw the reply arrive in multiple deltas, and they reassemble
	// to exactly the reply Run returns.
	if llm.streamCalls != 1 || llm.completeCalls != 0 {
		t.Fatalf("expected the streaming path (1 stream, 0 complete), got stream=%d complete=%d", llm.streamCalls, llm.completeCalls)
	}
	if len(sink.deltas) < 2 {
		t.Fatalf("expected the reply to stream in multiple deltas, got %d: %q", len(sink.deltas), sink.deltas)
	}
	if sink.joined() != reply {
		t.Fatalf("streamed deltas must reassemble to the reply\n got: %q\nwant: %q", sink.joined(), reply)
	}
	if res.Reply != reply {
		t.Fatalf("Result.Reply = %q, want %q", res.Reply, reply)
	}
	// The streaming path must still carry usage (cost/cache observability depends on it).
	if res.Usage.InputTokens != 11 || res.Usage.OutputTokens != 7 {
		t.Fatalf("streaming dropped usage: got %+v", res.Usage)
	}
}

// The defining contract: a propose_* round's narration streams to the sink, yet Run
// returns the Proposal (never a Reply). PR2's finalization relies on exactly this.
func TestRun_Streaming_ProposeRoundStreamsNarrationThenProposal(t *testing.T) {
	const narration = "Sure — I'll revoke that token; confirm below."
	llm := &streamingFakeLLM{responses: []Response{
		proposeRevokeRespWithText(narration, "analytics"),
	}}
	sink := &recordingSink{}
	ctx, tc := testCtx()

	res, _, err := New(llm, &fakeBackend{}, WithStreamSink(sink.fn)).Run(ctx, tc, nil, "kill the analytics token")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// (a) the narration reached the sink, live, before the proposal was known...
	if sink.joined() != narration {
		t.Fatalf("propose-round narration must stream\n got: %q\nwant: %q", sink.joined(), narration)
	}
	// ...and (b) Run still returns the Proposal with no Reply — streaming changed only
	// delivery of the narration, not the turn's outcome.
	if res.Proposal == nil || res.Proposal.Action != ActionRevoke {
		t.Fatalf("expected an ActionRevoke proposal, got %+v", res.Proposal)
	}
	if res.Reply != "" {
		t.Fatalf("a proposing turn must not also carry a Reply, got %q", res.Reply)
	}
}

// A propose_* round with no narration streams nothing — so PR2's lazy-start (open the
// stream on the first delta) opens no stream at all, and the user just gets the card.
func TestRun_Streaming_NoNarrationPropose_EmitsNoDeltas(t *testing.T) {
	llm := &streamingFakeLLM{responses: []Response{
		toolResp(toolProposeRevoke, map[string]any{fieldToken: "analytics"}),
	}}
	sink := &recordingSink{}
	ctx, tc := testCtx()

	res, _, err := New(llm, &fakeBackend{}, WithStreamSink(sink.fn)).Run(ctx, tc, nil, "revoke analytics")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.deltas) != 0 {
		t.Fatalf("a no-narration propose round must stream nothing, got %q", sink.deltas)
	}
	if res.Proposal == nil {
		t.Fatalf("expected a proposal, got %+v", res)
	}
}

// Across a read-then-answer turn, every round goes through the stream path, but only
// the terminal round carries text — so the sink receives exactly the final reply.
func TestRun_Streaming_MultiRound_StreamsTerminalReply(t *testing.T) {
	const reply = "You can reach one connector here: staging-dash."
	llm := &streamingFakeLLM{responses: []Response{
		toolResp(toolListResources, map[string]any{}), // round 1: read, no text
		{Text: reply, StopReason: "end_turn"},         // round 2: the answer
	}}
	sink := &recordingSink{}
	ctx, tc := testCtx()

	res, _, err := New(llm, &fakeBackend{resources: "staging-dash (r_1)"}, WithStreamSink(sink.fn)).Run(ctx, tc, nil, "what's here?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if llm.streamCalls != 2 {
		t.Fatalf("both rounds should take the stream path, got streamCalls=%d", llm.streamCalls)
	}
	if sink.joined() != reply {
		t.Fatalf("sink should hold exactly the terminal reply\n got: %q\nwant: %q", sink.joined(), reply)
	}
	if res.Reply != reply {
		t.Fatalf("Result.Reply = %q, want %q", res.Reply, reply)
	}
}

// An intermediate read round that NARRATES (text) and calls a tool, followed by a
// terminal reply round, streams BOTH rounds' text — in order — pinning the documented
// "the sink observes EVERY round's text" contract (WithStreamSink). This is the case
// PR2's finalization must reason about: text from semantically distinct rounds
// (intermediate narration + the final reply) concatenated into one stream. A future
// refactor that streamed only the terminal round would silently break this.
func TestRun_Streaming_IntermediateNarrationThenReply_StreamsBoth(t *testing.T) {
	const narration = "Let me check what's reachable here. "
	const reply = "You can reach staging-dash."
	llm := &streamingFakeLLM{responses: []Response{
		readRespWithText(narration, toolListResources), // round 1: narrate + read
		{Text: reply, StopReason: "end_turn"},          // round 2: terminal reply
	}}
	sink := &recordingSink{}
	ctx, tc := testCtx()

	res, _, err := New(llm, &fakeBackend{resources: "staging-dash (r_1)"}, WithStreamSink(sink.fn)).Run(ctx, tc, nil, "what's here?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.joined() != narration+reply {
		t.Fatalf("sink must hold both rounds' text in order\n got: %q\nwant: %q", sink.joined(), narration+reply)
	}
	// Result.Reply is still only the terminal round's text — the stream shows more than
	// the persisted reply when an intermediate round narrates.
	if res.Reply != reply {
		t.Fatalf("Result.Reply = %q, want the terminal round's text %q", res.Reply, reply)
	}
}

// A sink set against an LLM that does NOT implement streamingLLM falls back to Complete
// cleanly: no panic, no deltas, identical Result.
func TestRun_NonStreamingLLM_WithSink_FallsBackToComplete(t *testing.T) {
	const reply = "Nothing in this channel matches that."
	llm := &scriptedLLM{responses: []Response{textResp(reply)}}
	sink := &recordingSink{}
	ctx, tc := testCtx()

	res, _, err := New(llm, &fakeBackend{}, WithStreamSink(sink.fn)).Run(ctx, tc, nil, "find x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.deltas) != 0 {
		t.Fatalf("a non-streaming LLM must not drive the sink, got %q", sink.deltas)
	}
	if res.Reply != reply {
		t.Fatalf("Result.Reply = %q, want %q", res.Reply, reply)
	}
}

// Without a sink, a streaming-capable LLM still uses Complete — streaming activates
// only when a sink is wired, so every existing (sink-less) caller is unaffected.
func TestRun_StreamingLLM_NoSink_UsesComplete(t *testing.T) {
	llm := &streamingFakeLLM{responses: []Response{textResp("hello")}}
	ctx, tc := testCtx()

	res, _, err := New(llm, &fakeBackend{}).Run(ctx, tc, nil, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if llm.streamCalls != 0 || llm.completeCalls != 1 {
		t.Fatalf("no sink → Complete only, got stream=%d complete=%d", llm.streamCalls, llm.completeCalls)
	}
	if res.Reply != "hello" {
		t.Fatalf("Result.Reply = %q, want %q", res.Reply, "hello")
	}
}
