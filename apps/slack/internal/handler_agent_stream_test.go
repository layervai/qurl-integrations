package internal

// Tests for the pane reply streamer (streaming delivery PR2): the finalization contracts
// the turn path depends on — lazy-start, coalescing, the no-double-post invariant (a streamed
// reply is delivered by the stream, a proposal still posts its card, an error finalizes the
// partial), and the synthetic-reply reconcile. The streaming LLM path itself is exercised in
// the agent package (the streamingLLM seam is unexported), so here we drive the streamer's
// onDelta/finalize directly through a recording stream port.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
)

type recordingStreamPort struct {
	startErr   error
	appendErr  error
	startCalls int
	appends    []string
	stops      int
}

func (r *recordingStreamPort) StartStream(context.Context, string, string, string, string, string) (string, error) {
	r.startCalls++
	if r.startErr != nil {
		return "", r.startErr
	}
	return "stream.1", nil
}

func (r *recordingStreamPort) AppendStream(_ context.Context, _, _, _, _, markdownText string) error {
	if r.appendErr != nil {
		return r.appendErr
	}
	r.appends = append(r.appends, markdownText)
	return nil
}

func (r *recordingStreamPort) StopStream(context.Context, string, string, string, string) error {
	r.stops++
	return nil
}

func (r *recordingStreamPort) appended() string { return strings.Join(r.appends, "") }

func newTestStreamer(port AgentStreamPort) *agentReplyStreamer {
	return &agentReplyStreamer{
		ctx: context.Background(), baseCtx: context.Background(), log: slog.Default(), port: port,
		teamID: "T1", channelID: "D1", threadTS: "100.1", userID: "U1",
	}
}

func chunkStr(s string, n int) []string {
	var out []string
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

func TestAgentStreamer_NoDeltas_NotHandled(t *testing.T) {
	port := &recordingStreamPort{}
	s := newTestStreamer(port)
	// No deltas streamed (e.g. a no-narration proposal, or a turn that emits no text).
	if s.finalizeReply(&agent.Result{Reply: "hi"}) {
		t.Fatal("no stream opened → the caller must deliver (finalizeReply must be false)")
	}
	if port.startCalls != 0 || port.stops != 0 {
		t.Fatalf("no stream should have opened, got start=%d stop=%d", port.startCalls, port.stops)
	}
}

func TestAgentStreamer_NormalReply_StreamsCoalescedAndStops(t *testing.T) {
	port := &recordingStreamPort{}
	s := newTestStreamer(port)
	const reply = "You can reach the staging dashboard in this channel right now."
	for _, ch := range chunkStr(reply, 8) { // small deltas → coalesced
		s.onDelta(ch)
	}
	if !s.finalizeReply(&agent.Result{Reply: reply}) {
		t.Fatal("a streamed reply must be delivered by the stream (finalizeReply true)")
	}
	if port.startCalls != 1 || port.stops != 1 {
		t.Fatalf("expected one start + one stop, got start=%d stop=%d", port.startCalls, port.stops)
	}
	if port.appended() != reply {
		t.Fatalf("appends must reassemble the reply\n got: %q\nwant: %q", port.appended(), reply)
	}
	if len(port.appends) >= len(reply) {
		t.Fatalf("coalescing should yield far fewer appends than deltas, got %d", len(port.appends))
	}
}

func TestAgentStreamer_SyntheticReply_AppendedNotDoubled(t *testing.T) {
	port := &recordingStreamPort{}
	s := newTestStreamer(port)
	// Intermediate narration streams, but Result.Reply is a synthesized message the deltas
	// never carried (the iteration-cap fallback).
	s.onDelta("Let me look into that. ")
	s.flush(context.Background())
	const capMsg = "I wasn't able to work that out — could you rephrase?"
	if !s.finalizeReply(&agent.Result{Reply: capMsg}) {
		t.Fatal("a reply turn must be delivered by the stream")
	}
	if !strings.Contains(port.appended(), capMsg) {
		t.Fatalf("a synthesized reply absent from the stream must be appended, got %q", port.appended())
	}
	// ...but a reply the stream DID carry is not appended again.
	port2 := &recordingStreamPort{}
	s2 := newTestStreamer(port2)
	const streamed = "You can reach staging-dash."
	s2.onDelta(streamed)
	s2.finalizeReply(&agent.Result{Reply: streamed})
	if c := strings.Count(port2.appended(), streamed); c != 1 {
		t.Fatalf("a streamed reply must not be doubled, got it %d times: %q", c, port2.appended())
	}
}

func TestAgentStreamer_Proposal_StopsButCallerPostsCard(t *testing.T) {
	port := &recordingStreamPort{}
	s := newTestStreamer(port)
	s.onDelta("Sure — I'll revoke that token; confirm below.")
	if s.finalizeReply(&agent.Result{Proposal: &agent.Proposal{Action: agent.ActionRevoke}}) {
		t.Fatal("a proposal must NOT be marked delivered — the caller still posts the confirm card")
	}
	if port.stops != 1 {
		t.Fatalf("the narration stream must still be stopped, got stops=%d", port.stops)
	}
}

func TestAgentStreamer_ErrorAfterDeltas_FinalizesPartial(t *testing.T) {
	port := &recordingStreamPort{}
	s := newTestStreamer(port)
	s.onDelta("Here's what I fou")
	if !s.finalizeError() {
		t.Fatal("a live stream owns the error finalization (no double error post → finalizeError true)")
	}
	if port.stops != 1 {
		t.Fatalf("the partial stream must be stopped, got stops=%d", port.stops)
	}
}

func TestAgentStreamer_ErrorAfterBroken_CallerPostsError(t *testing.T) {
	// The stream broke mid-turn (an append failed), THEN the turn errored. finalizeError must
	// return false so the caller posts the error over the truncated partial — symmetric with the
	// successful-turn broken path — rather than leaving the user reading a half-message as final.
	port := &recordingStreamPort{appendErr: errors.New("appendStream 500")}
	s := newTestStreamer(port)
	for _, ch := range chunkStr("Looking that up across the workspace, one moment…", 8) { // crosses threshold → breaks
		s.onDelta(ch)
	}
	if s.finalizeError() {
		t.Fatal("a turn that errors after the stream broke must fall back to a posted error (finalizeError false)")
	}
	if port.startCalls != 1 || port.stops != 1 {
		t.Fatalf("the stream opened and must still be stopped, got start=%d stop=%d", port.startCalls, port.stops)
	}
}

func TestAgentStreamer_ErrorBeforeDeltas_CallerPosts(t *testing.T) {
	port := &recordingStreamPort{}
	s := newTestStreamer(port)
	if s.finalizeError() {
		t.Fatal("with no stream open, the caller posts the error (finalizeError must be false)")
	}
	if port.startCalls != 0 || port.stops != 0 {
		t.Fatalf("no stream should have opened, got start=%d stop=%d", port.startCalls, port.stops)
	}
}

func TestAgentStreamer_StartFailure_FallsBackToPost(t *testing.T) {
	port := &recordingStreamPort{startErr: errors.New("startStream 500")}
	s := newTestStreamer(port)
	s.onDelta("hello ") // startStream fails → broken
	s.onDelta("world")  // no-op (broken)
	if s.finalizeReply(&agent.Result{Reply: "hello world"}) {
		t.Fatal("a failed startStream must fall back to the posted reply (finalizeReply false)")
	}
	if port.startCalls != 1 || len(port.appends) != 0 || port.stops != 0 {
		t.Fatalf("on start failure expect 1 start attempt, no appends, no stop; got start=%d appends=%d stop=%d",
			port.startCalls, len(port.appends), port.stops)
	}
}

func TestAgentStreamer_AppendFailureMidStream_FallsBackToPost(t *testing.T) {
	port := &recordingStreamPort{appendErr: errors.New("appendStream 500")}
	s := newTestStreamer(port)
	const reply = "The staging dashboard is reachable from this channel right now."
	for _, ch := range chunkStr(reply, 8) { // crosses the coalesce threshold → a mid-stream append
		s.onDelta(ch)
	}
	// The append broke the stream, so the caller must post the full reply (no truncated bubble).
	if s.finalizeReply(&agent.Result{Reply: reply}) {
		t.Fatal("a mid-stream append failure must fall back to the posted reply (finalizeReply false)")
	}
	if port.startCalls != 1 {
		t.Fatalf("the stream must have opened once, got start=%d", port.startCalls)
	}
	if port.stops != 1 {
		t.Fatalf("a broken stream must still be stopped so its spinner clears, got stops=%d", port.stops)
	}
	if len(port.appends) != 0 {
		t.Fatalf("a failing append records nothing, got %v", port.appends)
	}
}

func TestAgentStreamer_AppendFailureAtFinalize_FallsBackToPost(t *testing.T) {
	// A reply UNDER the coalesce threshold never flushes during onDelta, so the only append is
	// the buffered tail at finalize. If THAT fails, the stream is healthy until finalize — this
	// is the path the post-stop broken-check in finalizeReply exists for (deleting it would
	// return Proposal==nil here and truncate the reply).
	port := &recordingStreamPort{appendErr: errors.New("appendStream 500")}
	s := newTestStreamer(port)
	const reply = "All set."
	s.onDelta(reply)
	if s.finalizeReply(&agent.Result{Reply: reply}) {
		t.Fatal("a finalize-time append failure must fall back to the posted reply (finalizeReply false)")
	}
	if port.startCalls != 1 || port.stops != 1 {
		t.Fatalf("the stream opened and must still be stopped, got start=%d stop=%d", port.startCalls, port.stops)
	}
	if len(port.appends) != 0 {
		t.Fatalf("the only (failing) append records nothing, got %v", port.appends)
	}
}
