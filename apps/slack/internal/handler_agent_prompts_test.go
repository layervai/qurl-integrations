package internal

// Tests for channel-aware first-run starter prompts (container Slice 3c): when a pane is
// opened from a channel whose name resolves, the starter prompts NAME that channel ("What
// can I reach in #general?"); otherwise (no context channel, or no channels:read scope) they
// fall back to the generic DM-safe set. Leak-free — the prompt only names the channel the
// user is already viewing; the scoped answer stays membership-gated at turn time (Slice 3b).

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

func newPromptsHandler(t *testing.T, fake *fakeAssistantThreads, resolve ResolveChannelNameFunc) *Handler {
	t.Helper()
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            fakeAgentLLM{reply: "x"},
		AgentStore:          store,
		PostMessage:         post,
		AssistantThreads:    fake,
		ResolveChannelName:  resolve,
		AgentDefaultEnabled: true,
	})
	t.Cleanup(h.Wait)
	return h
}

func TestAssistantPrompts_ChannelAwareWhenContextResolves(t *testing.T) {
	fake := &fakeAssistantThreads{}
	res := &recordingResolve{name: "general"}
	h := newPromptsHandler(t, fake, res.fn)

	// started, opened from context channel C9.
	h.handleEvent(httptest.NewRecorder(), []byte(assistantEventBody(slackEventTypeAssistantThreadStarted, "Ev1", "D1", "100.1", "C9")))
	h.Wait()

	if !res.resolved("C9") {
		t.Fatal("the context channel's name must be resolved to build channel-aware prompts")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.prompts) != 1 {
		t.Fatalf("want one setSuggestedPrompts, got %d", len(fake.prompts))
	}
	got := fake.prompts[0].prompts
	if len(got) != 4 {
		t.Fatalf("channel-aware set is 4 prompts (3 channel-aware + capability), got %d: %+v", len(got), got)
	}
	// Both channel-aware messages (reach + get-access) name #general; the capability
	// onboarding starter is kept last so a new user isn't worse off.
	if !strings.Contains(got[0].Message, "#general") || !strings.Contains(got[1].Message, "#general") {
		t.Fatalf("both channel-aware prompts must name #general, got %+v", got)
	}
	if got[len(got)-1].Message != "What can you help me with?" {
		t.Fatalf("channel-aware set must keep the capability starter last, got %+v", got)
	}
}

func TestAssistantPrompts_GenericWhenContextNameUnresolved(t *testing.T) {
	fake := &fakeAssistantThreads{}
	res := &recordingResolve{name: ""} // e.g. no channels:read scope → empty name
	h := newPromptsHandler(t, fake, res.fn)

	h.handleEvent(httptest.NewRecorder(), []byte(assistantEventBody(slackEventTypeAssistantThreadStarted, "Ev1", "D1", "100.1", "C9")))
	h.Wait()

	// The fallback came from an ATTEMPTED resolve of C9 that returned empty — not an
	// accidental short-circuit before the lookup.
	if !res.resolved("C9") {
		t.Fatal("an unresolved context channel must still attempt the resolve of C9")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.prompts) != 1 {
		t.Fatalf("want one setSuggestedPrompts, got %d", len(fake.prompts))
	}
	got := fake.prompts[0].prompts
	if got[0].Message != assistantStarterPrompts[0].Message {
		t.Fatalf("an unresolved context channel must fall back to the generic prompts, got %+v", got)
	}
	for _, p := range got {
		if strings.Contains(p.Message, "#") {
			t.Fatalf("generic fallback must name no channel, got %q", p.Message)
		}
	}
}

func TestAssistantPrompts_GenericWhenNoContextChannel(t *testing.T) {
	fake := &fakeAssistantThreads{}
	res := &recordingResolve{name: "general"} // would resolve, but there's no context channel to resolve
	h := newPromptsHandler(t, fake, res.fn)

	// started with NO context channel.
	h.handleEvent(httptest.NewRecorder(), []byte(assistantEventBody(slackEventTypeAssistantThreadStarted, "Ev1", "D1", "100.1", "")))
	h.Wait()

	if len(res.channels) != 0 {
		t.Fatalf("with no context channel, the name resolve must be skipped, got %v", res.channels)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.prompts) != 1 || fake.prompts[0].prompts[0].Message != assistantStarterPrompts[0].Message {
		t.Fatalf("no context channel → generic prompts, got %+v", fake.prompts)
	}
}
