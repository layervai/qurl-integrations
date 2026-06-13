package internal

// Tests for the native assistant-pane "thinking…" status (container Slice 2): set on a
// DM (message.im / pane) turn before the LLM call and (by Slack) auto-cleared when the
// reply posts; NOT set for an app_mention (channel) turn; im-only + best-effort behind
// the AssistantThreads seam (nil = no-op, a failure never fails the turn); and on the
// SAME thread the reply lands on (the auto-clear precondition).

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

func newStatusHandler(t *testing.T, seam AssistantThreadsPort, rec ReactionPort, llm agent.LLM) (*Handler, *[]capturedReply, *sync.Mutex) {
	t.Helper()
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            llm,
		AgentStore:          store,
		PostMessage:         post,
		AgentDefaultEnabled: true,
		AssistantThreads:    seam,
		Reactions:           rec,
	})
	t.Cleanup(h.Wait)
	return h, posts, mu
}

func TestAgentStatus_SetForPaneTurnOnReplyThread(t *testing.T) {
	fake := &fakeAssistantThreads{}
	rec := &recordingReactions{}
	h, posts, mu := newStatusHandler(t, fake, rec, fakeAgentLLM{reply: "You can reach staging."})

	// dmMessageBody: message.im, channel D1, ts 100.2, no thread_ts → root ts 100.2.
	h.handleEvent(httptest.NewRecorder(), []byte(dmMessageBody("EvStatus")))
	h.Wait()

	statuses := fake.statusCalls()
	if len(statuses) != 1 {
		t.Fatalf("a pane (im) turn must set exactly one status, got %d", len(statuses))
	}
	st := statuses[0]
	if st.channelID != "D1" || st.threadTS != "100.2" || st.status != agentThinkingStatus {
		t.Fatalf("status = %+v, want channel D1 / thread 100.2 / %q", st, agentThinkingStatus)
	}
	adds, removes := rec.snapshot()
	if len(adds) != 0 || len(removes) != 0 {
		t.Fatalf("pane (im) turn must use status only, got reaction adds=%+v removes=%+v", adds, removes)
	}

	// The status thread MUST equal the reply thread, or Slack's auto-clear (which fires
	// when the app posts into the thread) never clears the "thinking…" indicator.
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].threadTS != st.threadTS {
		t.Fatalf("reply must post on the status thread (auto-clear precondition); reply=%+v status.thread=%s", *posts, st.threadTS)
	}
}

func TestAgentStatus_NotSetForAppMention(t *testing.T) {
	// app_mention is a channel message, not an assistant thread — setStatus has nothing
	// to scope to there, so the channel @-mention path keeps the 👀 ack and sets no status.
	fake := &fakeAssistantThreads{}
	rec := &recordingReactions{}
	h, _, _ := newStatusHandler(t, fake, rec, fakeAgentLLM{reply: "ok"})

	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvMention")))
	h.Wait()

	if got := fake.statusCalls(); len(got) != 0 {
		t.Fatalf("app_mention (channel) turn must not set a pane status, got %+v", got)
	}
	adds, removes := rec.snapshot()
	if len(adds) != 1 || adds[0] != wantAck || len(removes) != 1 || removes[0] != wantAck {
		t.Fatalf("app_mention (channel) turn must use reaction only, got adds=%+v removes=%+v", adds, removes)
	}
}

func TestAgentStatus_NilSeamIsNoOp(t *testing.T) {
	// AssistantThreads unwired: the im turn still runs and replies, with no status and
	// no panic.
	h, posts, mu := newStatusHandler(t, nil, nil, fakeAgentLLM{reply: "ok"})

	h.handleEvent(httptest.NewRecorder(), []byte(dmMessageBody("EvNilStatus")))
	h.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 {
		t.Fatalf("nil AssistantThreads seam must still post the reply, got %d", len(*posts))
	}
}

func TestAgentStatus_BestEffortDoesNotFailTurn(t *testing.T) {
	// setStatus is still cosmetic: a failure must be visible at Warn without failing
	// the turn or dropping its reply.
	fake := &fakeAssistantThreads{statusErr: errors.New("no assistant thread")}
	h, posts, mu := newStatusHandler(t, fake, nil, fakeAgentLLM{reply: "still works"})

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h.processAgentEvent(context.Background(), logger,
		env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "what can I reach?"))

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != "still works" {
		t.Fatalf("a failing setStatus must not fail the turn; reply = %+v", *posts)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "agent: set assistant pane status failed") {
		t.Fatalf("a failing setStatus must log at Warn; log = %s", logs)
	}
}
