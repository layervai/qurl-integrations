package internal

// Tests for the agent "working on it" reaction ack (#661): added 👀 on the triggering
// message once the turn is committed without blocking the model turn, cleared on EVERY
// exit (success / error / panic), best-effort (a reaction failure never fails the
// turn), nil-seam no-op, and NOT added for turns dropped at the disabled/rate-limit
// gates (the ack means "I'm working on this", so it must only appear for turns that
// actually run).

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

type reactionCall struct{ teamID, enterpriseID, channel, timestamp, name string }

type recordingReactions struct {
	mu                sync.Mutex
	adds, removes     []reactionCall
	addErr, removeErr error
}

func (r *recordingReactions) Add(_ context.Context, teamID, enterpriseID, channelID, timestamp, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adds = append(r.adds, reactionCall{teamID, enterpriseID, channelID, timestamp, name})
	return r.addErr
}

func (r *recordingReactions) Remove(_ context.Context, teamID, enterpriseID, channelID, timestamp, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removes = append(r.removes, reactionCall{teamID, enterpriseID, channelID, timestamp, name})
	return r.removeErr
}

func (r *recordingReactions) snapshot() (adds, removes []reactionCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]reactionCall(nil), r.adds...), append([]reactionCall(nil), r.removes...)
}

type blockingOrderedReactions struct {
	mu         sync.Mutex
	events     []string
	addStarted chan struct{}
	releaseAdd chan struct{}
	release    sync.Once
}

func newBlockingOrderedReactions() *blockingOrderedReactions {
	return &blockingOrderedReactions{
		addStarted: make(chan struct{}),
		releaseAdd: make(chan struct{}),
	}
}

func (r *blockingOrderedReactions) Add(ctx context.Context, _, _, _, _, _ string) error {
	close(r.addStarted)
	select {
	case <-r.releaseAdd:
	case <-ctx.Done():
		return ctx.Err()
	}
	r.record("add")
	return nil
}

func (r *blockingOrderedReactions) Remove(_ context.Context, _, _, _, _, _ string) error {
	r.record("remove")
	return nil
}

func (r *blockingOrderedReactions) releaseAddCall() {
	r.release.Do(func() { close(r.releaseAdd) })
}

func (r *blockingOrderedReactions) snapshotEvents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *blockingOrderedReactions) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

type signalingAgentLLM struct {
	started chan struct{}
	once    sync.Once
}

func (s *signalingAgentLLM) Complete(context.Context, *agent.Request) (agent.Response, error) {
	s.once.Do(func() { close(s.started) })
	return agent.Response{Text: "ack does not block me", StopReason: "end_turn"}, nil
}

func newAckHandler(t *testing.T, rec ReactionPort, llm agent.LLM) (*Handler, *[]capturedReply, *sync.Mutex) {
	t.Helper()
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            llm,
		AgentStore:          store,
		PostMessage:         post,
		AgentDefaultEnabled: true,
		Reactions:           rec,
	})
	t.Cleanup(h.Wait)
	return h, posts, mu
}

// wantAck is the reaction the agent puts on the app_mention from appMentionBody:
// team T1, no enterprise, channel C1, message ts 100.1, emoji "eyes".
var wantAck = reactionCall{teamID: "T1", enterpriseID: "", channel: "C1", timestamp: "100.1", name: agentAckReaction}

const testAgentStillWorksReply = "still works"

func TestAgentAck_AddedAndClearedOnSuccess(t *testing.T) {
	rec := &recordingReactions{}
	h, _, _ := newAckHandler(t, rec, fakeAgentLLM{reply: "You can reach staging."})
	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("Ev1")))
	h.Wait()

	adds, removes := rec.snapshot()
	if len(adds) != 1 || adds[0] != wantAck {
		t.Fatalf("add = %+v, want one %+v", adds, wantAck)
	}
	if len(removes) != 1 || removes[0] != wantAck {
		t.Fatalf("remove = %+v, want one %+v (cleared when the reply posts)", removes, wantAck)
	}
}

func TestAgentAck_AddIsAsyncAndClearWaitsBeforeRemove(t *testing.T) {
	rec := newBlockingOrderedReactions()
	defer rec.releaseAddCall()

	llmStarted := make(chan struct{})
	h, _, _ := newAckHandler(t, rec, &signalingAgentLLM{started: llmStarted})
	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvAsyncAck")))

	select {
	case <-rec.addStarted:
	case <-time.After(time.Second):
		t.Fatal("reaction add did not start")
	}

	select {
	case <-llmStarted:
	case <-time.After(time.Second):
		t.Fatal("LLM turn did not start while reactions.add was still blocked")
	}

	rec.releaseAddCall()
	h.Wait()

	if got, want := rec.snapshotEvents(), []string{"add", "remove"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reaction order = %v, want %v", got, want)
	}
}

func TestAgentAck_ClearedOnTurnError(t *testing.T) {
	rec := &recordingReactions{}
	h, _, _ := newAckHandler(t, rec, fakeAgentLLM{err: errors.New("model 500")})
	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvErr")))
	h.Wait()

	adds, removes := rec.snapshot()
	if len(adds) != 1 || len(removes) != 1 {
		t.Fatalf("a failed turn must still add then clear the ack, got adds=%d removes=%d", len(adds), len(removes))
	}
}

func TestAgentAck_ClearedOnPanic(t *testing.T) {
	rec := &recordingReactions{}
	h, _, _ := newAckHandler(t, rec, panicAgentLLM{})
	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvPanic")))
	h.Wait()

	adds, removes := rec.snapshot()
	if len(adds) != 1 || len(removes) != 1 {
		t.Fatalf("a panicking turn must still add then clear the ack (deferred clear runs before the recover), got adds=%d removes=%d", len(adds), len(removes))
	}
}

func TestAgentAck_BestEffortDoesNotFailTurn(t *testing.T) {
	rec := &recordingReactions{addErr: errors.New("reaction down"), removeErr: errors.New("reaction down")}
	h, posts, mu := newAckHandler(t, rec, fakeAgentLLM{reply: testAgentStillWorksReply})
	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvBestEffort")))
	h.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != testAgentStillWorksReply {
		t.Fatalf("a failing reaction must not fail the turn; reply = %+v", *posts)
	}
}

func TestAgentAck_NilSeamIsNoOp(t *testing.T) {
	// newAgentEventHandler wires no Reactions seam — the turn must run and reply with
	// no ack and no panic.
	h, posts, mu := newAgentEventHandler(t, "ok")
	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvNil")))
	h.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 {
		t.Fatalf("nil reactions seam should still post the reply, got %d", len(*posts))
	}
}

func TestAgentAck_NotAddedForRateLimitedTurn(t *testing.T) {
	// The ack sits AFTER the rate-limit gate, so a dropped (rate-limited) turn gets no
	// 👀 — the ack only marks turns that actually run.
	rec := &recordingReactions{}
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state", Now: func() time.Time { return fixedNow }}
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:                    fakeAgentLLM{reply: "ok"},
		AgentStore:                  store,
		PostMessage:                 post,
		AgentDefaultEnabled:         true,
		AgentMaxTurnsPerUserPerHour: 1, // 2nd turn is rate-limited
		Reactions:                   rec,
	})
	t.Cleanup(h.Wait)

	for i, ts := range []string{"200.1", "200.2"} {
		h.handleEvent(httptest.NewRecorder(), []byte(rateLimitEventBody("EvRL"+strconv.Itoa(i), "U2", ts)))
		h.Wait()
	}

	adds, removes := rec.snapshot()
	if len(adds) != 1 || len(removes) != 1 {
		t.Fatalf("only the first (within-cap) turn should be acked; got adds=%d removes=%d", len(adds), len(removes))
	}
}
