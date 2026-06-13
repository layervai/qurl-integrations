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

func newStatusHandler(t *testing.T, seam AssistantThreadsPort, rec ReactionPort, llm agent.LLM, exclusiveAcks bool) (*Handler, *[]capturedReply, *sync.Mutex) {
	t.Helper()
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:                  llm,
		AgentStore:                store,
		PostMessage:               post,
		AgentDefaultEnabled:       true,
		AgentSurfaceExclusiveAcks: exclusiveAcks,
		AssistantThreads:          seam,
		Reactions:                 rec,
	})
	t.Cleanup(h.Wait)
	return h, posts, mu
}

var wantDMSurfaceAck = reactionCall{teamID: "T1", enterpriseID: "", channel: "D1", timestamp: "100.2", name: agentAckReaction}

func TestAgentStatus_SetForPaneTurnOnReplyThread(t *testing.T) {
	fake := &fakeAssistantThreads{}
	rec := &recordingReactions{}
	h, posts, mu := newStatusHandler(t, fake, rec, fakeAgentLLM{reply: testAgentReachStagingReply}, true)

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

func TestAgentStatus_DefaultPaneTurnOnReplyThread(t *testing.T) {
	fake := &fakeAssistantThreads{}
	rec := &recordingReactions{}
	h, posts, mu := newStatusHandler(t, fake, rec, fakeAgentLLM{reply: testAgentReachStagingReply}, false)

	// Default/pre-pane mode still attempts native status in addition to the reaction
	// fallback, so it must keep the same auto-clear precondition as exclusive mode.
	h.handleEvent(httptest.NewRecorder(), []byte(dmMessageBody("EvDefaultStatus")))
	h.Wait()

	statuses := fake.statusCalls()
	if len(statuses) != 1 {
		t.Fatalf("a default pane (im) turn must set exactly one status, got %d", len(statuses))
	}
	st := statuses[0]
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].threadTS != st.threadTS {
		t.Fatalf("default reply must post on the status thread (auto-clear precondition); reply=%+v status.thread=%s", *posts, st.threadTS)
	}
	adds, removes := rec.snapshot()
	if len(adds) != 1 || adds[0] != wantDMSurfaceAck || len(removes) != 1 || removes[0] != wantDMSurfaceAck {
		t.Fatalf("default pane (im) turn must also clear the reaction fallback, got adds=%+v removes=%+v", adds, removes)
	}
}

func TestAgentStatus_NotSetForAppMention(t *testing.T) {
	// app_mention is a channel message, not an assistant thread — setStatus has nothing
	// to scope to there, so the channel @-mention path keeps the eyes ack and sets no status.
	fake := &fakeAssistantThreads{}
	rec := &recordingReactions{}
	h, _, _ := newStatusHandler(t, fake, rec, fakeAgentLLM{reply: "ok"}, true)

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
	h, posts, mu := newStatusHandler(t, nil, nil, fakeAgentLLM{reply: "ok"}, true)

	h.handleEvent(httptest.NewRecorder(), []byte(dmMessageBody("EvNilStatus")))
	h.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 {
		t.Fatalf("nil AssistantThreads seam must still post the reply, got %d", len(*posts))
	}
}

func TestAgentStatus_DefaultNilSeamKeepsReactionFallback(t *testing.T) {
	// If the AssistantThreads seam is unwired before the exclusive rollout flips on,
	// the reaction fallback remains the visible working-on-it cue.
	rec := &recordingReactions{}
	h, posts, mu := newStatusHandler(t, nil, rec, fakeAgentLLM{reply: "ok"}, false)

	h.handleEvent(httptest.NewRecorder(), []byte(dmMessageBody("EvDefaultNilStatus")))
	h.Wait()

	adds, removes := rec.snapshot()
	if len(adds) != 1 || adds[0] != wantDMSurfaceAck || len(removes) != 1 || removes[0] != wantDMSurfaceAck {
		t.Fatalf("default nil-seam pane turn must keep reaction fallback, got adds=%+v removes=%+v", adds, removes)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 {
		t.Fatalf("default nil AssistantThreads seam must still post the reply, got %d", len(*posts))
	}
}

func TestAgentStatus_BestEffortDoesNotFailTurn(t *testing.T) {
	// setStatus is still cosmetic: a failure must be visible at Warn without failing
	// the turn or dropping its reply.
	fake := &fakeAssistantThreads{statusErr: errors.New("no assistant thread")}
	rec := &recordingReactions{}
	h, posts, mu := newStatusHandler(t, fake, rec, fakeAgentLLM{reply: testAgentStillWorksReply}, true)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h.processAgentEvent(context.Background(), logger,
		env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "what can I reach?"))

	adds, removes := rec.snapshot()
	if len(adds) != 0 || len(removes) != 0 {
		t.Fatalf("exclusive pane status failure must not fall back to reactions, got adds=%+v removes=%+v", adds, removes)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != testAgentStillWorksReply {
		t.Fatalf("a failing setStatus must not fail the turn; reply = %+v", *posts)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, `level=WARN msg="agent: set assistant pane status failed in exclusive mode"`) {
		t.Fatalf("a failing setStatus must log at Warn; log = %s", logs)
	}
}

func TestAgentStatus_DefaultPaneTurnKeepsReactionFallback(t *testing.T) {
	// Until the pane manifest/smoke gate flips QURL_AGENT_SURFACE_EXCLUSIVE_ACKS, an
	// im turn keeps the old reaction fallback and logs setStatus failures at Debug.
	fake := &fakeAssistantThreads{statusErr: errors.New("no assistant thread")}
	rec := &recordingReactions{}
	h, posts, mu := newStatusHandler(t, fake, rec, fakeAgentLLM{reply: testAgentStillWorksReply}, false)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.processAgentEvent(context.Background(), logger,
		env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "what can I reach?"))

	adds, removes := rec.snapshot()
	if len(adds) != 1 || adds[0] != wantAck || len(removes) != 1 || removes[0] != wantAck {
		t.Fatalf("default pane (im) turn must keep reaction fallback, got adds=%+v removes=%+v", adds, removes)
	}
	if got := fake.statusCalls(); len(got) != 1 {
		t.Fatalf("default pane (im) turn must still attempt setStatus, got %+v", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != testAgentStillWorksReply {
		t.Fatalf("a failing setStatus must not fail the turn; reply = %+v", *posts)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, `level=DEBUG msg="agent: set assistant pane status failed (best-effort)"`) {
		t.Fatalf("default pane setStatus failure must log at Debug; log = %s", logs)
	}
	if strings.Contains(logs, `level=WARN msg="agent: set assistant pane status failed`) {
		t.Fatalf("default pane setStatus failure must not log that status failure at Warn; log = %s", logs)
	}
}

func TestAgentStatus_DefaultPaneReactionClearedOnStatusPanic(t *testing.T) {
	// Default/pre-pane mode adds the reaction fallback before attempting native status.
	// If that later status path panics, the reaction cleanup must already be registered.
	fake := &fakeAssistantThreads{panicOnSetStatus: true}
	rec := &recordingReactions{}
	h, posts, mu := newStatusHandler(t, fake, rec, fakeAgentLLM{reply: testAgentStillWorksReply}, false)

	h.processAgentEvent(context.Background(), slog.Default(),
		env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "what can I reach?"))

	adds, removes := rec.snapshot()
	if len(adds) != 1 || len(removes) != 1 {
		t.Fatalf("a panicking default pane status path must still add then clear the reaction fallback, got adds=%d removes=%d", len(adds), len(removes))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentErrorReply {
		t.Fatalf("a panicking default pane status path must post the error reply; reply = %+v", *posts)
	}
}

func TestAgentStatus_ExclusivePaneNoReactionOnStatusPanic(t *testing.T) {
	// Exclusive/post-pane mode has no reaction fallback. If native status panics, the
	// top-level recover must still post the error reply without adding or clearing a
	// reaction.
	fake := &fakeAssistantThreads{panicOnSetStatus: true}
	rec := &recordingReactions{}
	h, posts, mu := newStatusHandler(t, fake, rec, fakeAgentLLM{reply: testAgentStillWorksReply}, true)

	h.processAgentEvent(context.Background(), slog.Default(),
		env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "what can I reach?"))

	adds, removes := rec.snapshot()
	if len(adds) != 0 || len(removes) != 0 {
		t.Fatalf("a panicking exclusive pane status path must not touch reaction fallback, got adds=%d removes=%d", len(adds), len(removes))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentErrorReply {
		t.Fatalf("a panicking exclusive pane status path must post the error reply; reply = %+v", *posts)
	}
}
