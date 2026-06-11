package internal

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// fakeAssistantThreads records SetTitle / SetSuggestedPrompts / SetStatus calls.
// statusErr, when set, makes SetStatus fail (to prove the turn is best-effort).
type fakeAssistantThreads struct {
	mu        sync.Mutex
	titles    []assistantTitleCall
	prompts   []assistantPromptsCall
	statuses  []assistantStatusCall
	statusErr error
}

type assistantTitleCall struct{ channelID, threadTS, title string }
type assistantPromptsCall struct {
	channelID, threadTS string
	prompts             []SuggestedPrompt
}
type assistantStatusCall struct{ channelID, threadTS, status string }

func (f *fakeAssistantThreads) SetTitle(_ context.Context, _, _, channelID, threadTS, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.titles = append(f.titles, assistantTitleCall{channelID, threadTS, title})
	return nil
}

func (f *fakeAssistantThreads) SetSuggestedPrompts(_ context.Context, _, _, channelID, threadTS string, prompts []SuggestedPrompt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, assistantPromptsCall{channelID, threadTS, prompts})
	return nil
}

func (f *fakeAssistantThreads) SetStatus(_ context.Context, _, _, channelID, threadTS, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, assistantStatusCall{channelID, threadTS, status})
	return f.statusErr
}

func (f *fakeAssistantThreads) statusCalls() []assistantStatusCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]assistantStatusCall(nil), f.statuses...)
}

func assistantThreadStartedBody(channelID, threadTS string) string {
	return assistantEventBody(slackEventTypeAssistantThreadStarted, "EvAssist", channelID, threadTS, "C9")
}

// assistantEventBody builds an assistant_thread_started / _context_changed event_callback
// for the given pane channel + thread and context channel. An empty contextChannel omits
// context.channel_id (a pane opened with no channel in view).
func assistantEventBody(eventType, eventID, channelID, threadTS, contextChannel string) string {
	ctx := ""
	if contextChannel != "" {
		ctx = `,"context":{"channel_id":"` + contextChannel + `","team_id":"T1"}`
	}
	return `{"type":"event_callback","team_id":"T1","event_id":"` + eventID + `",` +
		`"event":{"type":"` + eventType + `","assistant_thread":{"user_id":"U2",` +
		`"channel_id":"` + channelID + `","thread_ts":"` + threadTS + `"` + ctx + `}}}`
}

func newAssistantHandler(t *testing.T, seam AssistantThreadsPort) *Handler {
	t.Helper()
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, _, _ := capturingPostMessage()
	// AgentDefaultEnabled so the per-workspace gate (workspaceAgentEnabled) is open;
	// the workspace-off case is its own test.
	h := NewHandler(Config{AgentLLM: fakeAgentLLM{reply: "x"}, AgentStore: store, PostMessage: post, AssistantThreads: seam, AgentDefaultEnabled: true})
	t.Cleanup(h.Wait)
	return h
}

func TestAssistantStarterPrompts(t *testing.T) {
	// Slack allows up to 4 suggested prompts; keep 2-4, each with a label + message.
	if n := len(assistantStarterPrompts); n < 2 || n > 4 {
		t.Fatalf("want 2-4 starter prompts, got %d", n)
	}
	for _, p := range assistantStarterPrompts {
		if p.Title == "" || p.Message == "" {
			t.Fatalf("each prompt needs a title + message: %+v", p)
		}
	}
	if assistantThreadTitle == "" {
		t.Fatal("assistant thread title must be set")
	}
}

func TestHandleEvent_AssistantThreadStarted(t *testing.T) {
	fake := &fakeAssistantThreads{}
	h := newAssistantHandler(t, fake)

	h.handleEvent(httptest.NewRecorder(), []byte(assistantThreadStartedBody("D1", "1700000000.000100")))
	h.Wait()

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.titles) != 1 {
		t.Fatalf("want one setTitle, got %d", len(fake.titles))
	}
	if got := fake.titles[0]; got.channelID != "D1" || got.threadTS != "1700000000.000100" || got.title != assistantThreadTitle {
		t.Fatalf("setTitle = %+v", got)
	}
	if len(fake.prompts) != 1 {
		t.Fatalf("want one setSuggestedPrompts, got %d", len(fake.prompts))
	}
	if got := fake.prompts[0]; got.channelID != "D1" || got.threadTS != "1700000000.000100" || len(got.prompts) != len(assistantStarterPrompts) {
		t.Fatalf("setSuggestedPrompts = %+v", got)
	}
}

func TestAssistantThreadStarted_NilSeamIsNoOp(t *testing.T) {
	// AssistantThreads unwired: the event is accepted (200 by handleEvent) and
	// silently dropped — no panic, nothing scheduled.
	h := newAssistantHandler(t, nil)
	h.handleEvent(httptest.NewRecorder(), []byte(assistantThreadStartedBody("D1", "100.1")))
	h.Wait()
}

func TestAssistantThreadStarted_WorkspaceDisabledSkipped(t *testing.T) {
	// The "Agents & AI Apps" toggle is app-level, so the pane can open in a workspace
	// that hasn't opted into conversation mode (AgentDefaultEnabled off, no per-
	// workspace override). Don't set prompts there — a clicked prompt's turn would be
	// dropped by the same workspaceAgentEnabled gate, so live-looking prompts that do
	// nothing are worse than none.
	fake := &fakeAssistantThreads{}
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{AgentLLM: fakeAgentLLM{reply: "x"}, AgentStore: store, PostMessage: post, AssistantThreads: fake, AgentDefaultEnabled: false})
	t.Cleanup(h.Wait)

	h.handleEvent(httptest.NewRecorder(), []byte(assistantThreadStartedBody("D1", "100.1")))
	h.Wait()

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.titles) != 0 || len(fake.prompts) != 0 {
		t.Fatalf("workspace not opted into conversation mode must not get prompts: titles=%d prompts=%d", len(fake.titles), len(fake.prompts))
	}
}

func TestAssistantThreadStarted_MissingThreadFieldsSkipped(t *testing.T) {
	// A malformed assistant_thread (no channel/thread) can't be addressed — skip it
	// rather than post to an empty channel/thread.
	fake := &fakeAssistantThreads{}
	h := newAssistantHandler(t, fake)
	h.handleEvent(httptest.NewRecorder(), []byte(assistantThreadStartedBody("", "")))
	h.Wait()

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.titles) != 0 || len(fake.prompts) != 0 {
		t.Fatalf("missing channel/thread must skip the seam: titles=%d prompts=%d", len(fake.titles), len(fake.prompts))
	}
}
