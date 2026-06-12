package internal

// #712: channel thread follow-ups ride a SEPARATE bounded pool (h.followupSem) from the
// main turn pool (h.sem), so a busy channel's message.channels firehose can't saturate
// the pool that @mention/DM/slash/interaction work shares. These tests pin that isolation
// both directions by sizing each pool to 1, manually holding the single slot of one pool,
// and asserting the OTHER path still runs a full (fake-LLM) turn — one captured post is
// the "work ran" signal; a dropped event posts nothing.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// followupEventBody is a channel thread REPLY (message + thread_ts, channel_type != im,
// no subtype) — what isAgentChannelFollowup admits onto the follow-up pool.
func followupEventBody(eventID, ts, threadTS string) string {
	return `{"type":"event_callback","team_id":"T1","event_id":"` + eventID + `",` +
		`"event":{"type":"message","channel_type":"channel","user":"U2","channel":"C1",` +
		`"ts":"` + ts + `","thread_ts":"` + threadTS + `","text":"more please"}}`
}

// seedAgentThread writes a one-message transcript so the follow-up gate admits a reply in
// channel/threadTS (partition "T1" — team, no enterprise).
func seedAgentThread(t *testing.T, mem *memAgentDDB, channel, threadTS string) {
	t.Helper()
	store := &slackdata.AgentStore{Client: mem, TableName: "agent_state", Now: func() time.Time { return fixedNow }}
	blob, err := json.Marshal([]agent.Message{{}})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := store.SaveConversation(context.Background(), "T1", agentThreadKey(channel, threadTS), blob, 0); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
}

func TestAgentEventFollowupPoolIsolation(t *testing.T) {
	newH := func(t *testing.T, mem *memAgentDDB) (*Handler, *[]capturedReply, *sync.Mutex) {
		t.Helper()
		store := &slackdata.AgentStore{Client: mem, TableName: "agent_state", Now: func() time.Time { return fixedNow }}
		post, posts, mu := capturingPostMessage()
		h := NewHandler(Config{
			AgentLLM:                   fakeAgentLLM{reply: "ok"},
			AgentStore:                 store,
			PostMessage:                post,
			AgentChannelFollowups:      true,
			AgentDefaultEnabled:        true,
			MaxConcurrentAsync:         1,
			MaxConcurrentFollowupAsync: 1,
		})
		t.Cleanup(h.Wait)
		return h, posts, mu
	}
	postCount := func(posts *[]capturedReply, mu *sync.Mutex) int {
		mu.Lock()
		defer mu.Unlock()
		return len(*posts)
	}

	t.Run("saturated follow-up pool drops follow-ups but not @mentions", func(t *testing.T) {
		mem := newMemAgentDDB()
		h, posts, mu := newH(t, mem)
		seedAgentThread(t, mem, "C1", "100.0") // the follow-up's thread, so the gate would admit it
		h.followupSem <- struct{}{}            // hold the only follow-up slot (never released)

		// Follow-up → full follow-up pool → dropped (no turn, no post). fireTurn's Wait is a
		// no-op for the dropped event: runOnPool returns synchronously before spawning a worker.
		fireTurn(t, h, followupEventBody("Ev1", "101.0", "100.0"))
		// @mention → main pool (free) → full turn → one post.
		fireTurn(t, h, rateLimitEventBody("Ev2", "U2", "200.0"))

		if got := postCount(posts, mu); got != 1 {
			t.Fatalf("posts = %d, want 1 — the @mention must still run on the main pool while the follow-up pool is saturated", got)
		}
	})

	t.Run("saturated main pool drops @mentions but not follow-ups", func(t *testing.T) {
		mem := newMemAgentDDB()
		h, posts, mu := newH(t, mem)
		seedAgentThread(t, mem, "C1", "100.0")
		h.sem <- struct{}{} // hold the only main slot

		// @mention → full main pool → dropped (no post).
		fireTurn(t, h, rateLimitEventBody("Ev3", "U2", "200.0"))
		// Follow-up → follow-up pool (free) → admitted by the gate → full turn → one post.
		fireTurn(t, h, followupEventBody("Ev4", "101.0", "100.0"))

		if got := postCount(posts, mu); got != 1 {
			t.Fatalf("posts = %d, want 1 — the follow-up must still run on its own pool while the main pool is saturated", got)
		}
	})
}
