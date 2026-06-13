package internal

// Tests for the agent reply streamer (streaming delivery PR2): the finalization contracts
// the turn path depends on — lazy-start, coalescing, the no-double-post invariant (a streamed
// reply is delivered by the stream, a proposal still posts its card, an error finalizes the
// partial), and the synthetic-reply reconcile. The streaming LLM path itself is exercised in
// the agent package (the streamingLLM seam is unexported), so here we drive the streamer's
// onDelta/finalize directly through a recording stream port.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

const (
	testAgentStreamThreadTS      = "100.1"
	testChannelMentionStreamTS   = "200.1"
	testAgentStreamStateTable    = "agent_state"
	testAgentStreamEndTurn       = "end_turn"
	testAgentStreamToolUse       = "tool_use"
	testAgentStreamProposeRevoke = "propose_revoke"
	testAgentStreamInstalledTeam = "T_installed"
	testAgentStreamRemoteTeam    = "T_remote"
)

type recordingStreamPort struct {
	startErr   error
	appendErr  error
	startCalls int
	starts     []AgentStreamStart
	appends    []string
	stops      int
}

func (r *recordingStreamPort) StartStream(_ context.Context, start *AgentStreamStart) (string, error) {
	r.startCalls++
	if start != nil {
		r.starts = append(r.starts, *start)
	}
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
		teamID: "T1", channelID: "D1", threadTS: testAgentStreamThreadTS, recipientTeamID: "T1", userID: "U1",
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

func TestNewAgentReplyStreamer_ChannelMentionUsesRecipientTeam(t *testing.T) {
	port := &recordingStreamPort{}
	h := NewHandler(Config{AgentStream: port})
	e := env(slackEventTypeAppMention, "channel", "U_remote", "", "", "<@U12345678> hi")
	e.TeamID = testAgentStreamInstalledTeam
	e.EnterpriseID = "E_grid"
	e.Event.UserTeam = testAgentStreamRemoteTeam
	e.Event.Channel = "C_shared"
	e.Event.TS = testChannelMentionStreamTS

	s := h.newAgentReplyStreamer(context.Background(), slog.Default(), e, agentEventRootTS(&e.Event))
	if s == nil {
		t.Fatal("channel app_mention should create a streamer")
	}
	s.onDelta("hello channel")
	if port.startCalls != 1 {
		t.Fatalf("startStream calls = %d, want 1", port.startCalls)
	}
	got := port.starts[0]
	want := AgentStreamStart{
		TeamID:          testAgentStreamInstalledTeam,
		EnterpriseID:    "E_grid",
		ChannelID:       "C_shared",
		ThreadTS:        testChannelMentionStreamTS,
		RecipientTeamID: testAgentStreamRemoteTeam,
		RecipientUserID: "U_remote",
	}
	if got != want {
		t.Fatalf("startStream target = %+v, want %+v", got, want)
	}
}

func TestAgentStreamRecipientTeamID_Fallbacks(t *testing.T) {
	e := env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi")
	e.TeamID = testAgentStreamInstalledTeam
	e.Event.UserTeam = testAgentStreamRemoteTeam
	e.Event.SourceTeam = "T_source"
	if got := agentStreamRecipientTeamID(e); got != testAgentStreamRemoteTeam {
		t.Fatalf("user_team should take precedence over source_team, got %q", got)
	}

	e.Event.UserTeam = ""
	if got := agentStreamRecipientTeamID(e); got != "T_source" {
		t.Fatalf("source_team should be the second-choice recipient team, got %q", got)
	}

	e.Event.SourceTeam = ""
	if got := agentStreamRecipientTeamID(e); got != testAgentStreamInstalledTeam {
		t.Fatalf("team_id should be the same-workspace recipient-team fallback, got %q", got)
	}
}

func TestNewAgentReplyStreamer_ChannelMentionMissingRecipientTeamFallsBack(t *testing.T) {
	h := NewHandler(Config{AgentStream: &recordingStreamPort{}})
	e := env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi")
	e.TeamID = ""
	if s := h.newAgentReplyStreamer(context.Background(), slog.Default(), e, agentEventRootTS(&e.Event)); s != nil {
		t.Fatal("a channel stream without any recipient team must fall back to the posted path")
	}
}

func TestNewAgentReplyStreamer_KeepsPanePathAndSkipsChannelFollowups(t *testing.T) {
	port := &recordingStreamPort{}
	h := NewHandler(Config{AgentStream: port})
	dm := env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "hi")
	dm.Event.UserTeam = testAgentStreamRemoteTeam
	s := h.newAgentReplyStreamer(context.Background(), slog.Default(), dm, agentEventRootTS(&dm.Event))
	if s == nil {
		t.Fatal("message.im pane turns must still stream")
	}
	s.onDelta("pane reply")
	if got := port.starts[0].RecipientTeamID; got != "T1" {
		t.Fatalf("pane turns should keep using the event team as recipient team, got %q", got)
	}
	followup := env(slackEventTypeMessage, "channel", "U2", "", "", "follow-up")
	followup.Event.ThreadTS = "100.0"
	if s := h.newAgentReplyStreamer(context.Background(), slog.Default(), followup, agentEventRootTS(&followup.Event)); s != nil {
		t.Fatal("non-mention channel follow-ups are outside #706 and should keep the post path")
	}
}

type handlerStreamingLLM struct {
	responses []agent.Response
	err       error
	partial   string
	idx       int
}

func (s *handlerStreamingLLM) Complete(context.Context, *agent.Request) (agent.Response, error) {
	return agent.Response{}, errors.New("streaming test must use StreamComplete")
}

func (s *handlerStreamingLLM) StreamComplete(_ context.Context, _ *agent.Request, onText func(string)) (agent.Response, error) {
	if s.err != nil {
		if onText != nil && s.partial != "" {
			onText(s.partial)
		}
		return agent.Response{}, s.err
	}
	if s.idx >= len(s.responses) {
		return agent.Response{}, errors.New("handlerStreamingLLM: no more responses")
	}
	r := s.responses[s.idx]
	s.idx++
	if onText != nil {
		for _, ch := range chunkStr(r.Text, 8) {
			onText(ch)
		}
	}
	return r, nil
}

func newStreamingAgentHandler(llm agent.LLM, port AgentStreamPort, blocks PostMessageBlocksFunc) (*Handler, *[]capturedReply, *sync.Mutex) {
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: testAgentStreamStateTable}
	post, posts, mu := capturingPostMessage()
	mdPost := capturingPostMarkdownMessage(posts, mu)
	return NewHandler(Config{
		AgentLLM:            llm,
		AgentStore:          store,
		PostMessage:         post,
		PostMarkdownMessage: mdPost,
		PostMessageBlocks:   blocks,
		AgentStream:         port,
		AgentDefaultEnabled: true,
	}), posts, mu
}

func TestProcessAgentEvent_ChannelMentionStreamingSkipsReplyPost(t *testing.T) {
	const reply = "You can reach staging from this channel."
	port := &recordingStreamPort{}
	llm := &handlerStreamingLLM{responses: []agent.Response{{Text: reply, StopReason: testAgentStreamEndTurn}}}
	h, posts, mu := newStreamingAgentHandler(llm, port, nil)

	e := env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> what can I reach?")
	e.Event.UserTeam = "T_user"
	h.processAgentEvent(context.Background(), slog.Default(), e)

	if port.startCalls != 1 || port.stops != 1 || port.appended() != reply {
		t.Fatalf("channel mention should stream and stop once, got start=%d stop=%d appended=%q", port.startCalls, port.stops, port.appended())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 0 {
		t.Fatalf("streamed reply must not also post, got %+v", *posts)
	}
}

func TestProcessAgentEvent_ChannelMentionStreamingProposalStillPostsCard(t *testing.T) {
	port := &recordingStreamPort{}
	blocks := &blocksRecorder{}
	llm := &handlerStreamingLLM{responses: []agent.Response{{
		Text:       "I can revoke that token; confirm below.",
		ToolCalls:  []agent.ToolCall{{ID: "p1", Name: testAgentStreamProposeRevoke, Input: json.RawMessage(`{"token":"staging"}`)}},
		StopReason: testAgentStreamToolUse,
	}}}
	h, posts, mu := newStreamingAgentHandler(llm, port, blocks.fn())
	h.cfg.AgentConfirmEnabled = true

	e := env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> revoke staging")
	h.processAgentEvent(context.Background(), slog.Default(), e)

	if port.startCalls != 1 || port.stops != 1 {
		t.Fatalf("proposal narration should stream and stop once, got start=%d stop=%d", port.startCalls, port.stops)
	}
	if len(blocks.calls) != 1 {
		t.Fatalf("proposal must still post one confirm card, got %d", len(blocks.calls))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 0 {
		t.Fatalf("confirm-card proposal should not post a text fallback, got %+v", *posts)
	}
}

func TestProcessAgentEvent_ChannelMentionStreamingPartialErrorNoDoublePost(t *testing.T) {
	port := &recordingStreamPort{}
	llm := &handlerStreamingLLM{partial: "partial answer", err: errors.New("model stream failed")}
	h, posts, mu := newStreamingAgentHandler(llm, port, nil)

	e := env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> what can I reach?")
	h.processAgentEvent(context.Background(), slog.Default(), e)

	if port.startCalls != 1 || port.stops != 1 || port.appended() != "partial answer" {
		t.Fatalf("partial error should finalize the live stream, got start=%d stop=%d appended=%q", port.startCalls, port.stops, port.appended())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 0 {
		t.Fatalf("healthy partial stream owns the error outcome; got posted fallback %+v", *posts)
	}
}
