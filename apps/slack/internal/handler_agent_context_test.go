package internal

// Tests for assistant-pane context persistence (container Slice 3a): the channel a user
// opened the pane FROM (assistant_thread.context.channel_id) is stored — on both
// assistant_thread_started and assistant_thread_context_changed — under the pane thread
// key, for a later turn to scope its reads to. Workspace-gated + best-effort; depends on
// the AgentStore, not the AssistantThreads seam.

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

func newContextHandler(t *testing.T, seam AssistantThreadsPort, defaultEnabled bool) (*Handler, *slackdata.AgentStore) {
	t.Helper()
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            fakeAgentLLM{reply: "x"},
		AgentStore:          store,
		PostMessage:         post,
		AssistantThreads:    seam,
		AgentDefaultEnabled: defaultEnabled,
	})
	t.Cleanup(h.Wait)
	return h, store
}

func TestAssistantThreadStarted_PersistsContext(t *testing.T) {
	fake := &fakeAssistantThreads{}
	h, store := newContextHandler(t, fake, true)

	h.handleEvent(httptest.NewRecorder(), []byte(assistantEventBody(slackEventTypeAssistantThreadStarted, "Ev1", "D1", "100.1", "C9")))
	h.Wait()

	ch, found, err := store.GetThreadContext(context.Background(), "T1", "D1:100.1")
	if err != nil || !found || ch != "C9" {
		t.Fatalf("started must persist context.channel_id under the pane thread key: ch=%q found=%v err=%v", ch, found, err)
	}
	// First-run UX still runs alongside the persist.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.titles) != 1 || len(fake.prompts) != 1 {
		t.Fatalf("started should still set title+prompts: titles=%d prompts=%d", len(fake.titles), len(fake.prompts))
	}
}

func TestAssistantThreadContextChanged_PersistsUpdatedContext(t *testing.T) {
	fake := &fakeAssistantThreads{}
	h, store := newContextHandler(t, fake, true)

	// Opened viewing C9, then the user switches to C2 while the pane is open.
	h.handleEvent(httptest.NewRecorder(), []byte(assistantEventBody(slackEventTypeAssistantThreadStarted, "Ev1", "D1", "100.1", "C9")))
	h.Wait()
	h.handleEvent(httptest.NewRecorder(), []byte(assistantEventBody(slackEventTypeAssistantThreadContextChanged, "Ev2", "D1", "100.1", "C2")))
	h.Wait()

	if ch, found, _ := store.GetThreadContext(context.Background(), "T1", "D1:100.1"); !found || ch != "C2" {
		t.Fatalf("context_changed must overwrite the stored context: ch=%q found=%v", ch, found)
	}
	// context_changed carries no first-run UX — only the title/prompts from started.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.titles) != 1 || len(fake.prompts) != 1 {
		t.Fatalf("context_changed must not re-run first-run UX: titles=%d prompts=%d", len(fake.titles), len(fake.prompts))
	}
}

func TestAssistantContext_PersistsEvenWithNilSeam(t *testing.T) {
	// Persistence depends on the AgentStore, not the AssistantThreads seam: a started
	// event with no seam wired still stores the context (only title/prompts are skipped).
	h, store := newContextHandler(t, nil, true)

	h.handleEvent(httptest.NewRecorder(), []byte(assistantEventBody(slackEventTypeAssistantThreadStarted, "Ev1", "D1", "100.1", "C9")))
	h.Wait()

	if ch, found, _ := store.GetThreadContext(context.Background(), "T1", "D1:100.1"); !found || ch != "C9" {
		t.Fatalf("context must persist even with a nil seam: ch=%q found=%v", ch, found)
	}
}

func TestAssistantContext_WorkspaceDisabledSkipsPersist(t *testing.T) {
	fake := &fakeAssistantThreads{}
	h, store := newContextHandler(t, fake, false) // workspace not opted into conversation mode

	h.handleEvent(httptest.NewRecorder(), []byte(assistantEventBody(slackEventTypeAssistantThreadStarted, "Ev1", "D1", "100.1", "C9")))
	h.Wait()

	if _, found, _ := store.GetThreadContext(context.Background(), "T1", "D1:100.1"); found {
		t.Fatal("a workspace not opted into conversation mode must not persist pane context")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.titles) != 0 || len(fake.prompts) != 0 {
		t.Fatalf("disabled workspace must also skip first-run UX: titles=%d prompts=%d", len(fake.titles), len(fake.prompts))
	}
}

func TestAssistantContext_MissingContextChannelSkipsPersist(t *testing.T) {
	// A pane opened with no channel in view (no context.channel_id): nothing to scope to,
	// so no context is stored — but the first-run UX still runs.
	fake := &fakeAssistantThreads{}
	h, store := newContextHandler(t, fake, true)

	h.handleEvent(httptest.NewRecorder(), []byte(assistantEventBody(slackEventTypeAssistantThreadStarted, "Ev1", "D1", "100.1", "")))
	h.Wait()

	if _, found, _ := store.GetThreadContext(context.Background(), "T1", "D1:100.1"); found {
		t.Fatal("no context.channel_id → nothing to persist")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.titles) != 1 || len(fake.prompts) != 1 {
		t.Fatalf("missing context must not block first-run UX: titles=%d prompts=%d", len(fake.titles), len(fake.prompts))
	}
}
