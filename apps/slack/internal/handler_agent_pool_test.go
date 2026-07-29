package internal

// #712/#719: channel thread follow-ups pass through a short gate pool and then a
// SEPARATE bounded turn pool from the main turn pool (h.sem), so a busy channel's
// message.channels firehose can't saturate the pool that @mention/DM/slash/interaction
// work shares, and gate reads can't spend all long-running follow-up turn slots.

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

const agentPoolTestThreadTS = "100.0"

// followupEventBody is a channel thread REPLY (message + thread_ts, channel_type != im,
// no subtype) — what isAgentChannelFollowup admits onto the follow-up pool.
func followupEventBody(eventID, ts, threadTS string) string {
	return `{"type":"event_callback","team_id":"T1","api_app_id":"A1","event_id":"` + eventID + `",` +
		`"event":{"type":"message","channel_type":"channel","user":"U2","channel":"C1",` +
		`"ts":"` + ts + `","thread_ts":"` + threadTS + `","text":"more please"}}`
}

type countingThreadHistory struct {
	mu    sync.Mutex
	calls int
}

func (h *countingThreadHistory) read(_ context.Context, _, _, _, threadTS, _ string) ([]AgentThreadMessage, error) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	if threadTS != agentPoolTestThreadTS {
		return nil, nil
	}
	return []AgentThreadMessage{
		{UserID: "U1", Text: "question", TS: agentPoolTestThreadTS},
		{AppID: "A1", Text: "answer", TS: "100.1"},
	}, nil
}

func (h *countingThreadHistory) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

type blockingAgentLLM struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingAgentLLM) Complete(ctx context.Context, _ *agent.Request) (agent.Response, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return agent.Response{Text: "ok", StopReason: "end_turn"}, nil
	case <-ctx.Done():
		return agent.Response{}, ctx.Err()
	}
}

func waitForHistoryCalls(t *testing.T, history *countingThreadHistory, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := history.callCount(); got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("history calls = %d, want at least %d", history.callCount(), want)
}

func TestAgentEventFollowupPoolIsolation(t *testing.T) {
	newH := func(t *testing.T, mem *memAgentDDB) (*Handler, *countingThreadHistory, *[]capturedReply, *sync.Mutex) {
		t.Helper()
		store := &slackdata.AgentStore{Client: mem, TableName: "agent_state", Now: func() time.Time { return fixedNow }}
		history := &countingThreadHistory{}
		post, posts, mu := capturingPostMessage()
		h := NewHandler(Config{
			AgentLLM:                       fakeAgentLLM{reply: "ok"},
			AgentStore:                     store,
			PostMessage:                    post,
			AgentChannelFollowups:          true,
			AgentDefaultEnabled:            true,
			MaxConcurrentAsync:             1,
			MaxConcurrentFollowupAsync:     1,
			MaxConcurrentFollowupGateAsync: 1,
			AgentThreadHistory:             history.read,
		})
		t.Cleanup(h.Wait)
		return h, history, posts, mu
	}
	postCount := func(posts *[]capturedReply, mu *sync.Mutex) int {
		mu.Lock()
		defer mu.Unlock()
		return len(*posts)
	}

	t.Run("saturated follow-up gate pool drops before history read but not @mentions", func(t *testing.T) {
		mem := newMemAgentDDB()
		h, history, posts, mu := newH(t, mem)
		h.followupGateSem <- struct{}{} // hold the only gate slot (never released)

		// Follow-up -> full gate pool -> dropped before the transcript read.
		fireTurn(t, h, followupEventBody("Ev0", "101.0", agentPoolTestThreadTS))
		if got := history.callCount(); got != 0 {
			t.Fatalf("history calls = %d, want 0 — saturated gate must drop before reading Slack", got)
		}

		// @mention -> main pool (free) -> full turn -> one post.
		fireTurn(t, h, rateLimitEventBody("Ev1", "U2", "200.0"))
		if got := postCount(posts, mu); got != 1 {
			t.Fatalf("posts = %d, want 1 — the @mention must still run while the follow-up gate is saturated", got)
		}
	})

	t.Run("saturated follow-up turn pool drops follow-ups but not @mentions", func(t *testing.T) {
		mem := newMemAgentDDB()
		h, _, posts, mu := newH(t, mem)
		h.followupSem <- struct{}{} // hold the only follow-up turn slot (never released)

		// Follow-up -> gate admits -> full follow-up turn pool -> dropped (no turn, no post).
		fireTurn(t, h, followupEventBody("Ev2", "101.0", agentPoolTestThreadTS))
		// @mention → main pool (free) → full turn → one post.
		fireTurn(t, h, rateLimitEventBody("Ev3", "U2", "200.0"))

		if got := postCount(posts, mu); got != 1 {
			t.Fatalf("posts = %d, want 1 — the @mention must still run on the main pool while the follow-up turn pool is saturated", got)
		}
	})

	t.Run("saturated main pool drops @mentions but not follow-ups", func(t *testing.T) {
		mem := newMemAgentDDB()
		h, _, posts, mu := newH(t, mem)
		h.sem <- struct{}{} // hold the only main slot

		// @mention → full main pool → dropped (no post).
		fireTurn(t, h, rateLimitEventBody("Ev4", "U2", "200.0"))
		// Follow-up → follow-up pool (free) → admitted by the gate → full turn → one post.
		fireTurn(t, h, followupEventBody("Ev5", "101.0", agentPoolTestThreadTS))

		if got := postCount(posts, mu); got != 1 {
			t.Fatalf("posts = %d, want 1 — the follow-up must still run on its own pool while the main pool is saturated", got)
		}
	})
}

func TestAgentEventFollowupGateReleasesBeforeTurn(t *testing.T) {
	mem := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: mem, TableName: "agent_state", Now: func() time.Time { return fixedNow }}
	history := &countingThreadHistory{}

	release := make(chan struct{})
	releaseOnce := sync.Once{}
	llm := &blockingAgentLLM{started: make(chan struct{}), release: release}
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:                       llm,
		AgentStore:                     store,
		PostMessage:                    post,
		AgentChannelFollowups:          true,
		AgentDefaultEnabled:            true,
		MaxConcurrentAsync:             1,
		MaxConcurrentFollowupAsync:     1,
		MaxConcurrentFollowupGateAsync: 1,
		AgentThreadHistory:             history.read,
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		h.Wait()
	})

	h.handleEvent(httptest.NewRecorder(), []byte(followupEventBody("EvGate1", "101.0", agentPoolTestThreadTS)))
	select {
	case <-llm.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first follow-up turn did not start")
	}
	firstGateReads := history.callCount()

	// While the first admitted turn is still blocked in the LLM, a reply in another
	// thread must still be able to take the gate slot, read, and drop. If the gate
	// semaphore were held for the whole turn, this second delivery would be dropped
	// before the Slack history read below.
	h.handleEvent(httptest.NewRecorder(), []byte(followupEventBody("EvGate2", "201.0", "200.0")))
	waitForHistoryCalls(t, history, firstGateReads+1)

	releaseOnce.Do(func() { close(release) })
	h.Wait()
}
