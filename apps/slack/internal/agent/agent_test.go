package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// scriptedLLM returns a pre-baked sequence of responses, one per Complete call,
// and captures every request it received so tests can assert on what the loop
// fed back to the model.
type scriptedLLM struct {
	responses []Response
	calls     int
	captured  []*Request
}

func (s *scriptedLLM) Complete(_ context.Context, req *Request) (Response, error) {
	s.captured = append(s.captured, req)
	if s.calls >= len(s.responses) {
		return Response{}, errors.New("scriptedLLM: no more responses")
	}
	r := s.responses[s.calls]
	s.calls++
	return r, nil
}

// blockingLLM blocks the first call until its context expires — an upstream that
// outruns the round's ration — then answers the final round normally.
type blockingLLM struct {
	final Response
	calls int
}

func (b *blockingLLM) Complete(ctx context.Context, _ *Request) (Response, error) {
	b.calls++
	if b.calls == 1 {
		<-ctx.Done()
		return Response{}, ctx.Err()
	}
	return b.final, nil
}

// deadlineCapturingBackend records the context deadline a read tool was handed,
// so the sub-budget can be asserted without waiting for it to elapse.
type deadlineCapturingBackend struct {
	fakeBackend
	deadline time.Time
}

func (d *deadlineCapturingBackend) ListResources(ctx context.Context, _ *TurnContext) (string, error) {
	d.deadline, _ = ctx.Deadline()
	return "x", nil
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
	testReason    = "incident 412"
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

func TestRun_ProposeInspectStopsForConfirmation(t *testing.T) {
	// A summary request resolves to propose_inspect: the loop must STOP and hand back
	// an ActionInspect proposal. Fetching the page mints a qURL (a grant), so it is
	// gated on a human Confirm — never a read the model performs on its own, and the
	// backend is never touched during the turn.
	llm := &scriptedLLM{responses: []Response{
		toolResp(toolProposeInspect, map[string]any{fieldToken: "$dashboard", fieldReason: "onboarding"}),
	}}
	backend := &fakeBackend{}
	a := New(llm, backend)

	ctx, tc := testCtx()
	res, history, err := a.Run(ctx, tc, nil, "what's on $dashboard?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Proposal == nil || res.Proposal.Action != ActionInspect {
		t.Fatalf("expected an inspect proposal, got %+v", res.Proposal)
	}
	if res.Proposal.Token != "dashboard" {
		t.Fatalf("inspect proposal token = %q, want dashboard", res.Proposal.Token)
	}
	if len(backend.resolveCalls) != 0 {
		t.Fatalf("propose_inspect must not touch the backend before confirmation, got %+v", backend.resolveCalls)
	}
	assertWellFormed(t, history)
}

func TestRun_ProposeAlongsideReadInSameTurn_KeepsHistoryValid(t *testing.T) {
	// A single assistant turn carrying both a read call and a propose call.
	// Parallel tool use is disabled in production, but the loop must still
	// produce a well-formed transcript: the read call needs a tool_result too,
	// not just the proposal ack.
	readInput, _ := json.Marshal(map[string]any{})
	propInput, _ := json.Marshal(map[string]any{fieldToken: testSlug})
	llm := &scriptedLLM{responses: []Response{{
		ToolCalls: []ToolCall{
			{ID: "tu_read", Name: toolListResources, Input: readInput},
			{ID: "tu_prop", Name: toolProposeGet, Input: propInput},
		},
		StopReason: "tool_use",
	}}}
	ctx, tc := testCtx()
	res, history, err := New(llm, &fakeBackend{resources: "staging-dash (r_1)"}).Run(ctx, tc, nil, "list then get")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Proposal == nil {
		t.Fatalf("expected a proposal")
	}
	assertWellFormed(t, history) // both tu_read and tu_prop must have tool_results
	// The read result must be the backend output, not dropped.
	last := history[len(history)-1]
	var sawRead bool
	for _, tr := range last.ToolResults {
		if tr.ToolUseID == "tu_read" && tr.Content == "staging-dash (r_1)" {
			sawRead = true
		}
	}
	if !sawRead {
		t.Fatalf("read tool result was dropped when proposing: %+v", last)
	}
}

func TestRun_CallAfterProposeIsDrained(t *testing.T) {
	// A turn where the propose comes FIRST and a read follows it. The loop stops
	// at the proposal, but the trailing read still needs a tool_result (a
	// synthetic ack) — and it must NOT be executed against the backend.
	propInput, _ := json.Marshal(map[string]any{fieldToken: testSlug})
	readInput, _ := json.Marshal(map[string]any{})
	llm := &scriptedLLM{responses: []Response{{
		ToolCalls: []ToolCall{
			{ID: "tu_prop", Name: toolProposeGet, Input: propInput},
			{ID: "tu_read", Name: toolListResources, Input: readInput},
		},
		StopReason: "tool_use",
	}}}
	backend := &fakeBackend{resources: "should-not-be-read"}
	ctx, tc := testCtx()
	res, history, err := New(llm, backend).Run(ctx, tc, nil, "go")
	if err != nil || res.Proposal == nil {
		t.Fatalf("expected a proposal: %v / %+v", err, res)
	}
	assertWellFormed(t, history) // tu_prop AND tu_read both need results
	// The trailing read must be drained as an ack, never executed.
	last := history[len(history)-1]
	for _, tr := range last.ToolResults {
		if tr.ToolUseID == "tu_read" && tr.Content != proposalAckResult {
			t.Fatalf("trailing read should be a synthetic ack, got %q", tr.Content)
		}
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

func TestRun_ProposeProtectConnector_CarriesReason(t *testing.T) {
	llm := &scriptedLLM{responses: []Response{
		toolResp(toolProposeProtectConnector, map[string]any{
			fieldAlias:  "prod",
			fieldEnv:    "kubernetes",
			fieldPort:   8443,
			fieldReason: testReason,
		}),
	}}
	ctx, tc := testCtx()
	res, history, err := New(llm, &fakeBackend{}).Run(ctx, tc, nil, "protect the prod connector for "+testReason)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Proposal == nil {
		t.Fatalf("expected a proposal, got reply %q", res.Reply)
	}
	p := res.Proposal
	if p.Action != ActionProtectConnector || p.Alias != "prod" || p.Env != "kubernetes" || p.Port != "8443" || p.Reason != testReason {
		t.Fatalf("proposal = %+v", p)
	}
	if !p.AdminGated {
		t.Fatalf("protect-connector must be admin-gated")
	}
	assertWellFormed(t, history)
}

func TestRun_AccumulatesUsageAcrossRoundTrips(t *testing.T) {
	// Two round-trips (a read, then an answer); their usage must sum into the
	// turn's Result so the Slack layer can log/observe cache effectiveness.
	r1 := toolResp(toolListResources, map[string]any{})
	r1.Usage = Usage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 80}
	r2 := textResp("here you go")
	r2.Usage = Usage{InputTokens: 120, OutputTokens: 20, CacheReadInputTokens: 110}
	llm := &scriptedLLM{responses: []Response{r1, r2}}
	ctx, tc := testCtx()
	res, _, err := New(llm, &fakeBackend{resources: "x"}).Run(ctx, tc, nil, "list")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Usage.InputTokens != 220 || res.Usage.OutputTokens != 30 || res.Usage.CacheReadInputTokens != 190 {
		t.Fatalf("usage not summed across round-trips: %+v", res.Usage)
	}
}

func TestRun_EmptyModelReply_FallsBack(t *testing.T) {
	// Model returns neither text nor tool calls — must not post an empty message.
	llm := &scriptedLLM{responses: []Response{{StopReason: "end_turn"}}}
	ctx, tc := testCtx()
	res, _, err := New(llm, &fakeBackend{}).Run(ctx, tc, nil, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Proposal != nil || res.Reply != iterationCapMessage {
		t.Fatalf("expected the fallback reply, got proposal=%v reply=%q", res.Proposal, res.Reply)
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

func TestRun_IterationCapFinalizesWithAModelAnswer(t *testing.T) {
	// A model that never stops asking for reads must be cut off at the cap — but
	// what it already gathered is worth more than a canned "couldn't work it out",
	// so the turn spends one final tool-free round saying so.
	llm := &scriptedLLM{responses: []Response{
		toolResp(toolListResources, map[string]any{}),
		toolResp(toolListResources, map[string]any{}),
		textResp("Here's what I could confirm so far."),
	}}
	ctx, tc := testCtx()
	res, _, err := New(llm, &fakeBackend{resources: "x"}, WithMaxIterations(2)).Run(ctx, tc, nil, "loop")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reply != "Here's what I could confirm so far." {
		t.Fatalf("expected the final round's answer, got %q", res.Reply)
	}
	if res.Cutoff != CutoffIterations {
		t.Fatalf("Cutoff = %q, want %q", res.Cutoff, CutoffIterations)
	}
	if llm.calls != 3 {
		t.Fatalf("expected 2 gathering rounds plus 1 final round, got %d model calls", llm.calls)
	}
	assertGatheringThenFinal(t, llm.captured)
}

func TestRun_TurnBudgetFinalizesInsteadOfTimingOut(t *testing.T) {
	// The turn deadline is already inside the finalization reserve, so there is no
	// budget for a gathering round. The old loop would have started one anyway,
	// blown the deadline, and surfaced an error the caller renders as "that took
	// longer than I could handle". It must answer instead.
	llm := &scriptedLLM{responses: []Response{textResp("I couldn't finish checking, here's what I know.")}}
	_, tc := testCtx()
	ctx, cancel := context.WithTimeout(context.Background(), finalAnswerReserve/2)
	defer cancel()

	res, _, err := New(llm, &fakeBackend{}).Run(ctx, tc, nil, "give me access to $docs")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reply != "I couldn't finish checking, here's what I know." {
		t.Fatalf("expected the reserved final answer, got %q", res.Reply)
	}
	if res.Cutoff != CutoffBudget {
		t.Fatalf("Cutoff = %q, want %q", res.Cutoff, CutoffBudget)
	}
	if llm.calls != 1 {
		t.Fatalf("expected exactly the final round, got %d model calls", llm.calls)
	}
	if !llm.captured[0].TextOnly {
		t.Fatal("the final round must forbid tool calls")
	}
}

func TestRun_SlowRoundFallsBackToTheReservedFinalAnswer(t *testing.T) {
	// One round-trip that outruns its own ration (an overloaded upstream retrying
	// with backoff inside a single call) must not fail the turn: the reserve was
	// held back precisely so the turn can still answer.
	llm := &blockingLLM{final: textResp("Here's what I have.")}
	_, tc := testCtx()
	// Deadline just past the reserve, so exactly one gathering round is funded —
	// with a ration short enough to expire in test time.
	ctx, cancel := context.WithTimeout(context.Background(), finalAnswerReserve+250*time.Millisecond)
	defer cancel()

	res, _, err := New(llm, &fakeBackend{}).Run(ctx, tc, nil, "give me access to $docs")
	if err != nil {
		t.Fatalf("Run must degrade to an answer, not fail: %v", err)
	}
	if res.Reply != "Here's what I have." {
		t.Fatalf("expected the reserved final answer, got %q", res.Reply)
	}
	if res.Cutoff != CutoffBudget {
		t.Fatalf("Cutoff = %q, want %q", res.Cutoff, CutoffBudget)
	}
	if ctx.Err() != nil {
		t.Fatal("the turn deadline should still be intact: the reserve was not spent on the slow round")
	}
}

func TestRun_ReadToolGetsItsOwnSubBudget(t *testing.T) {
	// A single backend read must not be able to spend the turn. The read is handed
	// a context whose deadline is strictly tighter than the turn's, and never
	// reaches into the finalization reserve.
	llm := &scriptedLLM{responses: []Response{
		toolResp(toolListResources, map[string]any{}),
		textResp("done"),
	}}
	backend := &deadlineCapturingBackend{}
	_, tc := testCtx()
	turnBudget := finalAnswerReserve + maxToolCallBudget + time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), turnBudget)
	defer cancel()

	if _, _, err := New(llm, backend).Run(ctx, tc, nil, "what can I access?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if backend.deadline.IsZero() {
		t.Fatal("the read tool received a context with no deadline")
	}
	turnDeadline, _ := ctx.Deadline()
	if !backend.deadline.Before(turnDeadline) {
		t.Fatal("the read tool's deadline must be tighter than the turn's")
	}
	if got := time.Until(backend.deadline); got > maxToolCallBudget {
		t.Fatalf("read budget %v exceeds the %v cap", got, maxToolCallBudget)
	}
}

func TestRun_FinalAnswerIgnoresUnexpectedToolCalls(t *testing.T) {
	// TextOnly forbids tool calls, but a model or transport that returned one
	// anyway must not leave a tool_use with no matching tool_result in the
	// transcript — that would poison every later turn in the thread.
	llm := &scriptedLLM{responses: []Response{
		{Text: "partial answer", ToolCalls: []ToolCall{{ID: "tu_x", Name: toolListResources}}, StopReason: "tool_use"},
	}}
	_, tc := testCtx()
	ctx, cancel := context.WithTimeout(context.Background(), finalAnswerReserve/2)
	defer cancel()

	res, history, err := New(llm, &fakeBackend{}).Run(ctx, tc, nil, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reply != "partial answer" {
		t.Fatalf("expected the final round's text, got %q", res.Reply)
	}
	for i := range history {
		if len(history[i].ToolCalls) > 0 {
			t.Fatalf("final round recorded a tool_use with no tool_result: %+v", history[i])
		}
	}
}

func TestRun_FinalAnswerFallsBackWhenTheModelSaysNothing(t *testing.T) {
	llm := &scriptedLLM{responses: []Response{textResp("   ")}}
	_, tc := testCtx()
	ctx, cancel := context.WithTimeout(context.Background(), finalAnswerReserve/2)
	defer cancel()

	res, _, err := New(llm, &fakeBackend{}).Run(ctx, tc, nil, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reply != iterationCapMessage {
		t.Fatalf("a blank final answer must fall back to the ask-again reply, got %q", res.Reply)
	}
}

// assertGatheringThenFinal pins the shape of a finalized turn: every gathering
// round offers tools, and only the last round forbids them and carries the
// final-answer directive.
func assertGatheringThenFinal(t *testing.T, captured []*Request) {
	t.Helper()
	if len(captured) < 2 {
		t.Fatalf("expected at least one gathering round plus a final round, got %d", len(captured))
	}
	for i, req := range captured[:len(captured)-1] {
		if req.TextOnly {
			t.Fatalf("gathering round %d must be able to call tools", i)
		}
		if strings.Contains(req.SystemPerTurn, finalAnswerDirective) {
			t.Fatalf("gathering round %d must not carry the final-answer directive", i)
		}
	}
	final := captured[len(captured)-1]
	if !final.TextOnly {
		t.Fatal("the final round must forbid tool calls")
	}
	if !strings.Contains(final.SystemPerTurn, finalAnswerDirective) {
		t.Fatal("the final round must carry the final-answer directive")
	}
	if len(final.Tools) == 0 {
		t.Fatal("the final round must still DEFINE tools: the Messages API rejects a tool_use transcript with none")
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

	// EXACT-PREFIX invariant: handler_agent.go's saveAgentHistory computes this
	// turn's delta as history[len(prior):] and, on a concurrent-save conflict,
	// re-applies just that delta onto the winner's transcript. That is correct only
	// if Run returns the input history UNCHANGED as a prefix. If Run ever rewrites
	// or reorders prior history, this assertion must fail loudly — otherwise the
	// delta logic would silently corrupt persisted transcripts.
	for i := range prior {
		if !reflect.DeepEqual(history[i], prior[i]) {
			t.Fatalf("Run did not preserve prior history as an exact prefix at index %d: got %+v want %+v", i, history[i], prior[i])
		}
	}
	// The delta begins with the user message Run prepends — the clean turn boundary
	// the conflict-merge grafts onto the winner's transcript.
	if d := history[priorLen]; d.Role != roleUser || d.Text != "follow-up" {
		t.Fatalf("delta does not begin with the user message: %+v", d)
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
