package internal

// Handler-level tests for the per-user / per-team agent-turn rate limit (PR6b): the
// gate in processAgentEvent, fail-open on a counter error, user-first containment of
// the shared team counter, and the disabled (0) no-op. The store-level BumpTurnCount
// contract is fenced in slackdata; here we drive the real AgentStore over the
// in-memory fake so the counter actually increments across turns.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/observability"
)

// rateLimitTurnReply is the fake LLM's reply, distinct from agentRateLimitedReply so
// a test can tell a turn that RAN from one that was capped.
const rateLimitTurnReply = "ok"

func rateLimitEventBody(eventID, userID, ts string) string {
	return `{"type":"event_callback","team_id":"T1","event_id":"` + eventID + `",` +
		`"event":{"type":"app_mention","user":"` + userID + `","channel":"C1","ts":"` + ts + `","text":"<@U12345678> hi"}}`
}

func newRateLimitHandler(t *testing.T, mem *memAgentDDB, userLimit, teamLimit int) (*Handler, *[]capturedReply, *sync.Mutex) {
	t.Helper()
	// Pin the store clock so every turn lands in one rate window (the sk is keyed on
	// the window start) — otherwise a run crossing an hour boundary would split the
	// count across two items.
	store := &slackdata.AgentStore{Client: mem, TableName: "agent_state", Now: func() time.Time { return fixedNow }}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:                    fakeAgentLLM{reply: rateLimitTurnReply},
		AgentStore:                  store,
		PostMessage:                 post,
		AgentDefaultEnabled:         true,
		AgentMaxTurnsPerUserPerHour: userLimit,
		AgentMaxTurnsPerTeamPerHour: teamLimit,
	})
	t.Cleanup(h.Wait)
	return h, posts, mu
}

// fireTurn delivers one event and drains its async worker, so the counter increments
// are sequential and the assertions deterministic.
func fireTurn(t *testing.T, h *Handler, body string) {
	t.Helper()
	h.handleEvent(httptest.NewRecorder(), []byte(body))
	h.Wait()
}

// rateSK builds the counter sort key for the pinned window (mirrors BumpTurnCount).
func rateSK(scope string) string {
	return "rate#" + scope + "#" + strconv.FormatInt(fixedNow.UTC().Truncate(time.Hour).Unix(), 10)
}

func (f *memAgentDDB) turnCount(t *testing.T, pk, sk string) int64 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return memNumberValue(f.items[pk+"|"+sk]["turn_count"])
}

func (f *memAgentDDB) hasRateItems() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.items {
		if strings.Contains(k, "|rate#") { // memKey is "pk|sk"; rate items have sk "rate#…"
			return true
		}
	}
	return false
}

func TestAgentTurnLimit_PerUser(t *testing.T) {
	mem := newMemAgentDDB()
	h, posts, mu := newRateLimitHandler(t, mem, 2, 0) // user cap 2, team disabled
	for i, ts := range []string{"200.1", "200.2", "200.3"} {
		fireTurn(t, h, rateLimitEventBody("Ev"+strconv.Itoa(i), "U2", ts))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 3 {
		t.Fatalf("expected 3 replies, got %d: %+v", len(*posts), *posts)
	}
	wantReply := agentLLMReplyWithDisclaimer(rateLimitTurnReply)
	if (*posts)[0].text != wantReply || (*posts)[1].text != wantReply {
		t.Fatalf("first two turns (within cap) should run, got %+v", *posts)
	}
	if (*posts)[2].text != agentRateLimitedReply {
		t.Fatalf("3rd turn should be rate-limited, got %q", (*posts)[2].text)
	}
}

func TestAgentTurnLimit_PerTeamAcrossUsers(t *testing.T) {
	mem := newMemAgentDDB()
	h, posts, mu := newRateLimitHandler(t, mem, 0, 2) // user disabled, team cap 2
	for i, u := range []string{"U1", "U2", "U3"} {
		fireTurn(t, h, rateLimitEventBody("Ev"+strconv.Itoa(i), u, "30"+strconv.Itoa(i)+".1"))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 3 || (*posts)[2].text != agentRateLimitedReply {
		t.Fatalf("per-team cap should limit the 3rd turn even across distinct users, got %+v", *posts)
	}
}

func TestAgentTurnLimit_UserFirstContainsTeamCount(t *testing.T) {
	// user cap 1, team cap 5: a member over their own cap must NOT bump the shared
	// team counter, so one abuser can't burn the whole workspace's budget.
	mem := newMemAgentDDB()
	h, _, _ := newRateLimitHandler(t, mem, 1, 5)
	for i, ts := range []string{"400.1", "400.2", "400.3"} {
		fireTurn(t, h, rateLimitEventBody("Ev"+strconv.Itoa(i), "U2", ts))
	}
	if got := mem.turnCount(t, "T1", rateSK("team")); got != 1 {
		t.Fatalf("team counter = %d, want 1 (only the first within-cap turn bumps the team counter)", got)
	}
}

func TestAgentTurnLimit_FailsOpenOnCounterError(t *testing.T) {
	mem := newMemAgentDDB()
	mem.updateErr = errors.New("ddb down")
	h, posts, mu := newRateLimitHandler(t, mem, 1, 0)
	fireTurn(t, h, rateLimitEventBody("EvFO", "U2", "500.1"))
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentLLMReplyWithDisclaimer(rateLimitTurnReply) {
		t.Fatalf("a counter error must fail OPEN (turn runs), got %+v", *posts)
	}
}

func TestAgentTurnLimit_FailOpenLogContract(t *testing.T) {
	// qurl-integrations-infra#1065 filters this exact msg key/value for the
	// fail-open path introduced by qurl-integrations-infra#1055.
	const infraFilterFailOpenMsg = "agent: turn-rate counter failed; allowing turn (fail-open)"

	if agentTurnRateCounterFailOpenMsg != infraFilterFailOpenMsg {
		t.Fatalf("agentTurnRateCounterFailOpenMsg = %q, want %q", agentTurnRateCounterFailOpenMsg, infraFilterFailOpenMsg)
	}

	mem := newMemAgentDDB()
	mem.updateErr = errors.New("ddb down")
	store := &slackdata.AgentStore{Client: mem, TableName: "agent_state", Now: func() time.Time { return fixedNow }}
	h := &Handler{cfg: Config{AgentStore: store}}

	var buf bytes.Buffer
	log := slog.New(observability.NewRedactingJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if limited := h.overTurnLimit(context.Background(), log, "T1", "team", 1); limited {
		t.Fatal("counter errors must fail open")
	}

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("unmarshal fail-open log %q: %v", buf.String(), err)
	}
	if rec["msg"] != infraFilterFailOpenMsg {
		t.Fatalf("msg = %v, want %q", rec["msg"], infraFilterFailOpenMsg)
	}
	if rec["level"] != "WARN" {
		t.Fatalf("level = %v, want WARN", rec["level"])
	}
	if rec["scope"] != "team" {
		t.Fatalf("scope = %v, want team", rec["scope"])
	}
	if rec["team_id"] != "T1" {
		t.Fatalf("team_id = %v, want T1", rec["team_id"])
	}
	if got, _ := rec["error"].(string); !strings.Contains(got, "ddb down") {
		t.Fatalf("error = %v, want ddb down", rec["error"])
	}
}

func TestAgentTurnLimit_DisabledRunsEveryTurn(t *testing.T) {
	mem := newMemAgentDDB()
	h, posts, mu := newRateLimitHandler(t, mem, 0, 0) // both disabled (unlimited)
	for i, ts := range []string{"600.1", "600.2", "600.3", "600.4"} {
		fireTurn(t, h, rateLimitEventBody("Ev"+strconv.Itoa(i), "U2", ts))
	}
	mu.Lock()
	n := len(*posts)
	mu.Unlock()
	if n != 4 {
		t.Fatalf("disabled limits must run every turn, got %d replies", n)
	}
	if mem.hasRateItems() {
		t.Fatal("disabled limits must not write any rate-counter items (BumpTurnCount not called)")
	}
}
