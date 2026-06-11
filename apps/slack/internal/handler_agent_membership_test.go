package internal

// Tests for the membership-gated assistant-pane read scope (container Slice 3b): a pane
// (DM) turn scopes its reads to the channel the user opened the pane from ONLY when the user
// is a confirmed member of it — so a non-member (e.g. previewing a public channel) can't
// enumerate that channel's qURL topology through the pane. Fail-closed: no context, a
// non-member, a check error, or no seam → the un-scoped DM. The scoped signal is observable
// via ResolveChannelName being asked to name the context channel (only a scoped turn does).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

type recordingMembership struct {
	mu     sync.Mutex
	checks [][2]string // {channelID, userID}
	member bool
	err    error
}

func (m *recordingMembership) fn(_ context.Context, _, _, channelID, userID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checks = append(m.checks, [2]string{channelID, userID})
	return m.member, m.err
}

func (m *recordingMembership) checked(channelID, userID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.checks {
		if c[0] == channelID && c[1] == userID {
			return true
		}
	}
	return false
}

func (m *recordingMembership) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.checks)
}

type recordingResolve struct {
	mu       sync.Mutex
	channels []string
	name     string
}

func (r *recordingResolve) fn(_ context.Context, _, _, channelID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels = append(r.channels, channelID)
	return r.name, nil
}

func (r *recordingResolve) resolved(channelID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.channels {
		if c == channelID {
			return true
		}
	}
	return false
}

func newScopeHandler(t *testing.T, membership ChannelMembershipFunc, resolve ResolveChannelNameFunc) (*Handler, *slackdata.AgentStore) {
	t.Helper()
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            fakeAgentLLM{reply: "ok"},
		AgentStore:          store,
		PostMessage:         post,
		AgentDefaultEnabled: true,
		ChannelMembership:   membership,
		ResolveChannelName:  resolve,
	})
	t.Cleanup(h.Wait)
	return h, store
}

// dmMessageBody is channel D1, ts 100.2, user U2 → pane thread key D1:100.2.
func seedPaneContext(t *testing.T, store *slackdata.AgentStore, contextChannel string) {
	t.Helper()
	if err := store.PutThreadContext(context.Background(), "T1", "D1:100.2", contextChannel); err != nil {
		t.Fatalf("seed context: %v", err)
	}
}

func TestPaneScope_MemberScopesToContextChannel(t *testing.T) {
	mem := &recordingMembership{member: true}
	res := &recordingResolve{name: "general"}
	h, store := newScopeHandler(t, mem.fn, res.fn)
	seedPaneContext(t, store, "C9") // the user opened the pane from C9 (Slice 3a persisted it)

	h.handleEvent(httptest.NewRecorder(), []byte(dmMessageBody("EvScope")))
	h.Wait()

	if !mem.checked("C9", "U2") {
		t.Fatal("membership must be checked for the context channel + the pane user")
	}
	if !res.resolved("C9") {
		t.Fatal("a member's pane turn must operate on (and resolve the name of) the context channel C9")
	}
}

func TestPaneScope_NonMemberFallsBackToDM(t *testing.T) {
	mem := &recordingMembership{member: false}
	res := &recordingResolve{name: "general"}
	h, store := newScopeHandler(t, mem.fn, res.fn)
	seedPaneContext(t, store, "C9")

	h.handleEvent(httptest.NewRecorder(), []byte(dmMessageBody("EvNonMember")))
	h.Wait()

	if !mem.checked("C9", "U2") {
		t.Fatal("membership should still be checked for the context channel")
	}
	if res.resolved("C9") {
		t.Fatal("a non-member's pane must NOT scope to C9 (no topology leak) — it stays on the DM")
	}
}

func TestPaneScope_MembershipErrorFailsClosed(t *testing.T) {
	mem := &recordingMembership{err: errors.New("slack down")}
	res := &recordingResolve{name: "general"}
	h, store := newScopeHandler(t, mem.fn, res.fn)
	seedPaneContext(t, store, "C9")

	h.handleEvent(httptest.NewRecorder(), []byte(dmMessageBody("EvMemErr")))
	h.Wait()

	if res.resolved("C9") {
		t.Fatal("a membership-check error must fail closed (no scope), never leak C9's topology")
	}
}

func TestPaneScope_NoContextFallsBack(t *testing.T) {
	mem := &recordingMembership{member: true}
	res := &recordingResolve{name: "general"}
	h, _ := newScopeHandler(t, mem.fn, res.fn) // no context seeded

	h.handleEvent(httptest.NewRecorder(), []byte(dmMessageBody("EvNoCtx")))
	h.Wait()

	if mem.count() != 0 {
		t.Fatal("with no stored context there's nothing to scope to — skip the membership check")
	}
	if res.resolved("C9") {
		t.Fatal("no context → no scope")
	}
}

func TestPaneScope_NilMembershipSeamFallsBack(t *testing.T) {
	res := &recordingResolve{name: "general"}
	h, store := newScopeHandler(t, nil, res.fn) // membership seam not wired
	seedPaneContext(t, store, "C9")

	h.handleEvent(httptest.NewRecorder(), []byte(dmMessageBody("EvNilMem")))
	h.Wait()

	if res.resolved("C9") {
		t.Fatal("without the membership seam the pane can't be scoped — it stays on the DM")
	}
}

func TestResolveChannelMembership_Caching(t *testing.T) {
	logd := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	if NewHandler(Config{}).resolveChannelMembership(ctx, logd, "T1", "", "C9", "U2") {
		t.Fatal("nil seam must be false (fail-closed)")
	}

	// A definitive answer (member=true) is cached: the seam is hit once.
	calls := 0
	h := NewHandler(Config{ChannelMembership: func(_ context.Context, _, _, _, _ string) (bool, error) { calls++; return true, nil }})
	for range 2 {
		if !h.resolveChannelMembership(ctx, logd, "T1", "", "C9", "U2") {
			t.Fatal("want member")
		}
	}
	if calls != 1 {
		t.Fatalf("a definitive answer must be cached; seam hit %d times", calls)
	}

	// An error of ANY kind (a stable missing_scope, a transient 5xx, a ctx timeout) is NOT
	// cached: it fails closed AND re-checks next turn, so one blip can't lock a member out of
	// scope for the whole TTL.
	ecalls := 0
	he := NewHandler(Config{ChannelMembership: func(_ context.Context, _, _, _, _ string) (bool, error) {
		ecalls++
		return false, errors.New("missing_scope")
	}})
	for range 2 {
		if he.resolveChannelMembership(ctx, logd, "T1", "", "C9", "U2") {
			t.Fatal("an error must fail closed")
		}
	}
	if ecalls != 2 {
		t.Fatalf("an error must NOT be cached (re-checked next turn); seam hit %d times", ecalls)
	}
}

func TestPaneScope_AppMentionSkipsMembershipCheck(t *testing.T) {
	// An @mention (channel) turn must never run the pane membership gate — scoping is for the
	// assistant pane (im) only. Even with a context row present under the same thread key,
	// the im-type gate keeps the channel path out.
	mem := &recordingMembership{member: true}
	res := &recordingResolve{name: "general"}
	h, store := newScopeHandler(t, mem.fn, res.fn)
	// appMentionBody: channel C1, ts 100.1. Seed a context under that thread key anyway.
	if err := store.PutThreadContext(context.Background(), "T1", "C1:100.1", "C9"); err != nil {
		t.Fatal(err)
	}

	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvMention")))
	h.Wait()

	if mem.count() != 0 {
		t.Fatalf("an app_mention turn must not run the membership gate, got %d checks", mem.count())
	}
}

func TestPaneContextChannel_RefreshesTTLOnScope(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state", Now: func() time.Time { return now }, ConversationTTL: 30 * time.Minute}
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM: fakeAgentLLM{reply: "x"}, AgentStore: store, PostMessage: post, AgentDefaultEnabled: true,
		ChannelMembership: func(_ context.Context, _, _, _, _ string) (bool, error) { return true, nil },
	})
	logd := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	if err := store.PutThreadContext(ctx, "T1", "D1:100.2", "C9"); err != nil {
		t.Fatal(err)
	}

	now = now.Add(20 * time.Minute) // within the original 30m TTL
	env := &slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{
		Type: slackEventTypeMessage, ChannelType: slackChannelTypeIM, Channel: "D1", TS: "100.2", User: "U2",
	}}
	if c := h.paneContextChannel(ctx, logd, env); c != "C9" {
		t.Fatalf("a member's pane turn must scope to C9, got %q", c)
	}

	now = now.Add(20 * time.Minute) // 40m since the seed — past the ORIGINAL 30m TTL
	if _, found, _ := store.GetThreadContext(ctx, "T1", "D1:100.2"); !found {
		t.Fatal("a scoped turn must refresh the context TTL so channel-awareness outlives the original window")
	}
}

// proposingLLM emits one propose_revoke tool call so a full turn yields a Proposal (and,
// with the confirm flow enabled, a pending action) — used to pin that a scoped pane turn's
// mutation still anchors to the DM, not the read-scoped operating channel.
type proposingLLM struct {
	mu    sync.Mutex
	calls int
}

func (p *proposingLLM) Complete(_ context.Context, _ *agent.Request) (agent.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 {
		return agent.Response{
			ToolCalls:  []agent.ToolCall{{ID: "p1", Name: "propose_revoke", Input: json.RawMessage(`{"token":"staging"}`)}},
			StopReason: "tool_use",
		}, nil
	}
	return agent.Response{Text: "done", StopReason: "end_turn"}, nil
}

func TestPaneScope_ScopedProposalAnchorsMutationToDM(t *testing.T) {
	// THE security invariant of this slice: scoping a pane turn's READS to the context
	// channel must NOT widen the MUTATION target. A pane scoped to C9 that proposes a revoke
	// must still anchor the pending action to the pane DM (env.Event.Channel = D1) — the
	// confirm/click path re-resolves against that channel (which has no qURL policies), so
	// the read-scope override can never let the pane mutate a channel it only reads. This
	// pins that operatingChannel never leaks from the turn into the confirm path.
	mem := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: mem, TableName: "agent_state"}
	post, _, _ := capturingPostMessage()
	blocks := &blocksRecorder{}
	h := NewHandler(Config{
		AgentLLM:            &proposingLLM{},
		AgentStore:          store,
		PostMessage:         post,
		PostMessageBlocks:   blocks.fn(),
		AgentConfirmEnabled: true,
		AgentDefaultEnabled: true,
		ChannelMembership:   func(_ context.Context, _, _, _, _ string) (bool, error) { return true, nil },
	})
	t.Cleanup(h.Wait)
	if err := store.PutThreadContext(context.Background(), "T1", "D1:100.2", "C9"); err != nil {
		t.Fatal(err)
	}

	h.handleEvent(httptest.NewRecorder(), []byte(dmMessageBody("EvScopedProp")))
	h.Wait()

	// Find the single stored pending action (pend#<id> under team T1) and read its channel.
	var pendID string
	for k := range mem.items {
		if id, ok := strings.CutPrefix(k, "T1|"+testPendSKPrefix); ok {
			pendID = id
		}
	}
	if pendID == "" {
		t.Fatal("the scoped proposal turn must store a pending action")
	}
	blob, found, err := store.LoadPendingAction(context.Background(), "T1", pendID)
	if err != nil || !found {
		t.Fatalf("load pending action: found=%v err=%v", found, err)
	}
	var pa pendingAction
	if err := json.Unmarshal(blob, &pa); err != nil {
		t.Fatal(err)
	}
	if pa.ChannelID != "D1" {
		t.Fatalf("a scoped pane's mutation must anchor to the DM (D1), got %q — the read-scope override must NOT widen the action target", pa.ChannelID)
	}
}
