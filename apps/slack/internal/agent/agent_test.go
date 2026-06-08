package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// scriptedLLM returns a pre-baked sequence of responses, one per Complete call,
// and captures every request it received so tests can assert on what the loop
// fed back to the model.
type scriptedLLM struct {
	responses []Response
	calls     int
	captured  []Request
}

func (s *scriptedLLM) Complete(_ context.Context, req Request) (Response, error) {
	s.captured = append(s.captured, req)
	if s.calls >= len(s.responses) {
		return Response{}, errors.New("scriptedLLM: no more responses")
	}
	r := s.responses[s.calls]
	s.calls++
	return r, nil
}

// fakeBackend returns canned strings and records which reads were attempted.
type fakeBackend struct {
	resources    string
	aliases      string
	resolve      string
	quota        string
	err          error
	resolveCalls []string
}

func (f *fakeBackend) ListResources(_ context.Context, _ *TurnContext) (string, error) {
	return f.resources, f.err
}

func (f *fakeBackend) ListAliases(_ context.Context, _ *TurnContext) (string, error) {
	return f.aliases, f.err
}

func (f *fakeBackend) ResolveToken(_ context.Context, _ *TurnContext, token string) (string, error) {
	f.resolveCalls = append(f.resolveCalls, token)
	return f.resolve, f.err
}

func (f *fakeBackend) Quota(_ context.Context, _ *TurnContext) (string, error) {
	return f.quota, f.err
}

func toolResp(name string, input map[string]any) Response {
	raw, _ := json.Marshal(input)
	return Response{
		ToolCalls:  []ToolCall{{ID: "tu_" + name, Name: name, Input: raw}},
		StopReason: "tool_use",
	}
}

func textResp(s string) Response {
	return Response{Text: s, StopReason: "end_turn"}
}

// Shared test literals, hoisted to satisfy goconst (it counts test files too).
const (
	testChannel   = "oncall"
	testSlug      = "staging"
	testSlugSigil = "$" + testSlug
)

func testCtx() (context.Context, *TurnContext) {
	return context.Background(), &TurnContext{
		TeamID:        "T1",
		ChannelID:     "C1",
		ChannelName:   testChannel,
		UserID:        "U1",
		CallerIsAdmin: true,
	}
}

func TestRun_ReadThenAnswer_GroundsOnToolResult(t *testing.T) {
	llm := &scriptedLLM{responses: []Response{
		toolResp(toolListResources, map[string]any{}),
		textResp("You can reach the staging dashboard."),
	}}
	backend := &fakeBackend{resources: "staging-dash (r_123), active"}
	a := New(llm, backend)

	ctx, tc := testCtx()
	res, history, err := a.Run(ctx, tc, nil, "what can I reach here?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Proposal != nil {
		t.Fatalf("expected a text reply, got proposal %+v", res.Proposal)
	}
	if res.Reply != "You can reach the staging dashboard." {
		t.Fatalf("reply = %q", res.Reply)
	}

	// The second request must include the tool result so the model is grounded.
	if len(llm.captured) != 2 {
		t.Fatalf("expected 2 model calls, got %d", len(llm.captured))
	}
	last := llm.captured[1].Messages
	found := false
	for _, m := range last {
		for _, tr := range m.ToolResults {
			if tr.Content == backend.resources {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("tool result %q was not fed back to the model; messages=%+v", backend.resources, last)
	}

	// History must be a well-formed transcript: user, assistant(tool_use),
	// user(tool_result), assistant(text).
	assertWellFormed(t, history)
	if got := len(history); got != 4 {
		t.Fatalf("history len = %d, want 4: %+v", got, history)
	}
}

func TestRun_ProposeGet_StopsAndKeepsHistoryValid(t *testing.T) {
	llm := &scriptedLLM{responses: []Response{
		toolResp(toolProposeGet, map[string]any{fieldToken: testSlugSigil, fieldReason: "incident 412"}),
	}}
	a := New(llm, &fakeBackend{})

	ctx, tc := testCtx()
	res, history, err := a.Run(ctx, tc, nil, "give me access to staging for incident 412")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Proposal == nil {
		t.Fatalf("expected a proposal, got reply %q", res.Reply)
	}
	p := res.Proposal
	if p.Action != ActionGet || p.Token != testSlug || p.Reason != "incident 412" {
		t.Fatalf("proposal = %+v", p)
	}
	if p.AdminGated {
		t.Fatalf("get must not be admin-gated")
	}
	// Even though the loop stopped, the propose tool_use needs a tool_result so
	// the next turn in this thread is a valid request.
	assertWellFormed(t, history)
	last := history[len(history)-1]
	if last.Role != roleUser || len(last.ToolResults) != 1 || last.ToolResults[0].Content != proposalAckResult {
		t.Fatalf("expected a proposal-ack tool result, got %+v", last)
	}
}

func TestRun_ProposeRevoke_AdminGated(t *testing.T) {
	llm := &scriptedLLM{responses: []Response{
		toolResp(toolProposeRevoke, map[string]any{fieldToken: "analytics"}),
	}}
	ctx, tc := testCtx()
	res, _, err := New(llm, &fakeBackend{}).Run(ctx, tc, nil, "kill the analytics connector")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Proposal == nil || res.Proposal.Action != ActionRevoke || !res.Proposal.AdminGated {
		t.Fatalf("expected an admin-gated revoke proposal, got %+v", res.Proposal)
	}
}

func TestRun_ClarifyingQuestion(t *testing.T) {
	llm := &scriptedLLM{responses: []Response{textResp("Which one — $oncall or $on-call-eng?")}}
	ctx, tc := testCtx()
	res, _, err := New(llm, &fakeBackend{}).Run(ctx, tc, nil, "protect the dashboard")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Proposal != nil || !strings.Contains(res.Reply, "Which one") {
		t.Fatalf("expected a clarifying question, got %+v / %q", res.Proposal, res.Reply)
	}
}

func TestRun_MalformedProposal_FedBackForCorrection(t *testing.T) {
	llm := &scriptedLLM{responses: []Response{
		toolResp(toolProposeGet, map[string]any{}), // missing required token
		textResp("Which resource did you mean?"),
	}}
	ctx, tc := testCtx()
	res, _, err := New(llm, &fakeBackend{}).Run(ctx, tc, nil, "get me access")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reply != "Which resource did you mean?" {
		t.Fatalf("reply = %q", res.Reply)
	}
	// The model's second call must carry the error as a tool result.
	last := llm.captured[1].Messages
	sawErr := false
	for _, m := range last {
		for _, tr := range m.ToolResults {
			if tr.IsError && strings.Contains(tr.Content, "token is required") {
				sawErr = true
			}
		}
	}
	if !sawErr {
		t.Fatalf("expected the validation error fed back as an error tool result; messages=%+v", last)
	}
}

func TestRun_BackendError_SurfacedNotFatal(t *testing.T) {
	llm := &scriptedLLM{responses: []Response{
		toolResp(toolResolveToken, map[string]any{fieldToken: "$x"}),
		textResp("I couldn't resolve that — could you double-check the name?"),
	}}
	backend := &fakeBackend{err: errors.New("upstream down")}
	ctx, tc := testCtx()
	res, _, err := New(llm, backend).Run(ctx, tc, nil, "resolve $x")
	if err != nil {
		t.Fatalf("Run should not fail on a backend error: %v", err)
	}
	if res.Reply == "" || res.Proposal != nil {
		t.Fatalf("expected a graceful reply, got %+v / %q", res.Proposal, res.Reply)
	}
	if len(backend.resolveCalls) != 1 || backend.resolveCalls[0] != "x" {
		t.Fatalf("resolve should be called once with the $-stripped token; got %v", backend.resolveCalls)
	}
}

func TestRun_IterationCap(t *testing.T) {
	// Always ask for a read tool; the loop must give up after the cap rather
	// than spin forever.
	llm := &scriptedLLM{responses: []Response{
		toolResp(toolListResources, map[string]any{}),
		toolResp(toolListResources, map[string]any{}),
		toolResp(toolListResources, map[string]any{}),
	}}
	ctx, tc := testCtx()
	res, _, err := New(llm, &fakeBackend{resources: "x"}, WithMaxIterations(2)).Run(ctx, tc, nil, "loop")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reply != iterationCapMessage {
		t.Fatalf("expected the iteration-cap message, got %q", res.Reply)
	}
	if llm.calls != 2 {
		t.Fatalf("expected exactly 2 model calls at the cap, got %d", llm.calls)
	}
}

func TestRun_MissingDeps(t *testing.T) {
	var a Agent // zero value: no llm, no backend
	ctx, tc := testCtx()
	if _, _, err := a.Run(ctx, tc, nil, "hi"); !errors.Is(err, errMissingDeps) {
		t.Fatalf("expected errMissingDeps, got %v", err)
	}
}

func TestRun_AppendsToPriorHistoryWithoutMutatingInput(t *testing.T) {
	prior := []Message{
		{Role: roleUser, Text: "earlier question"},
		{Role: roleAssistant, Text: "earlier answer"},
	}
	priorLen := len(prior)
	llm := &scriptedLLM{responses: []Response{textResp("follow-up answer")}}
	ctx, tc := testCtx()
	_, history, err := New(llm, &fakeBackend{}).Run(ctx, tc, prior, "follow-up")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prior) != priorLen {
		t.Fatalf("Run mutated the caller's history slice")
	}
	if len(history) <= priorLen {
		t.Fatalf("expected history to grow; got %d", len(history))
	}
}

// assertWellFormed checks the transcript invariant the Anthropic API requires:
// every assistant tool_use has a matching tool_result in a following user turn.
func assertWellFormed(t *testing.T, history []Message) {
	t.Helper()
	resultIDs := map[string]bool{}
	for _, m := range history {
		for _, tr := range m.ToolResults {
			resultIDs[tr.ToolUseID] = true
		}
	}
	for _, m := range history {
		for _, tc := range m.ToolCalls {
			if !resultIDs[tc.ID] {
				t.Fatalf("tool_use %q (%s) has no matching tool_result", tc.ID, tc.Name)
			}
		}
	}
}
