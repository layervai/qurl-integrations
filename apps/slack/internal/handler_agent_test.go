package internal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

func TestStripBotMention(t *testing.T) {
	// Realistic Slack ids (8-63 id chars), matching the mention-id grammar.
	cases := map[string]string{
		"<@U12345678> protect staging":                  "protect staging",
		"<@W1234ABCD|qurl> hi there":                    "hi there",
		"   <@U12345678>   spaced  ":                    "spaced",
		"no mention here":                               "no mention here",
		"<@U12345678> <@U87654321> only strips leading": "<@U87654321> only strips leading",
		"<@U1> too short to be an id":                   "<@U1> too short to be an id",
	}
	for in, want := range cases {
		if got := stripBotMention(in); got != want {
			t.Errorf("stripBotMention(%q) = %q, want %q", in, got, want)
		}
	}
}

func env(eventType, channelType, user, botID, subtype, text string) *slackEventEnvelope {
	return &slackEventEnvelope{
		Type: "event_callback", TeamID: "T1", EventID: "Ev1",
		Event: slackInnerEvent{
			Type: eventType, ChannelType: channelType, User: user,
			BotID: botID, Subtype: subtype, Text: text, Channel: "C1", TS: "100.1",
		},
	}
}

func TestShouldDispatchAgentEvent(t *testing.T) {
	tests := []struct {
		name string
		env  *slackEventEnvelope
		want bool
	}{
		{"app_mention human", env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi"), true},
		{"dm human", env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "hi"), true},
		{"channel message (not mention) ignored", env(slackEventTypeMessage, "channel", "U2", "", "", "hi"), false},
		{"bot message ignored", env(slackEventTypeAppMention, "channel", "U2", "B9", "", "<@U12345678> hi"), false},
		{"subtype (edit/system) ignored", env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "message_changed", "hi"), false},
		{"authorless ignored", env(slackEventTypeAppMention, "channel", "", "", "", "<@U12345678> hi"), false},
		{"mention with empty text ignored", env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678>   "), false},
		{"other event type ignored", env("reaction_added", "channel", "U2", "", "", "x"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldDispatchAgentEvent(tt.env); got != tt.want {
				t.Fatalf("shouldDispatchAgentEvent = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAgentEventKeys(t *testing.T) {
	grid := &slackEventEnvelope{TeamID: "T1", EnterpriseID: "E9", Event: slackInnerEvent{Channel: "C1", TS: "100.1"}}
	if agentEventPartition(grid) != "E9" {
		t.Errorf("grid partition should be enterprise id")
	}
	noGrid := &slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{Channel: "C1", TS: "100.1"}}
	if agentEventPartition(noGrid) != "T1" {
		t.Errorf("non-grid partition should be team id")
	}
	// Reply roots under the thread when present, else the message ts.
	threaded := &slackInnerEvent{TS: "100.1", ThreadTS: "90.0"}
	if agentEventRootTS(threaded) != "90.0" {
		t.Errorf("root ts should follow thread_ts")
	}
	if agentEventRootTS(&slackInnerEvent{TS: "100.1"}) != "100.1" {
		t.Errorf("root ts should fall back to ts")
	}
	if got := agentEventThreadKey(noGrid); got != "C1:100.1" {
		t.Errorf("thread key = %q", got)
	}
}

func TestAgentReplyText(t *testing.T) {
	if got := agentReplyText(&agent.Result{Reply: "hello"}); got != "hello" {
		t.Errorf("reply = %q", got)
	}
	prop := agentReplyText(&agent.Result{Proposal: &agent.Proposal{Summary: "Protect $x."}})
	if !strings.Contains(prop, "isn't enabled yet") || !strings.Contains(prop, "Protect $x.") {
		t.Errorf("proposal preview = %q", prop)
	}
	if got := agentReplyText(&agent.Result{Reply: "   "}); got != agentErrorReply {
		t.Errorf("blank reply should fall back to the error reply, got %q", got)
	}
	// A proposal with a blank summary would render as a dangling bullet; it must
	// fall back to the error reply like the blank-Reply case.
	if got := agentReplyText(&agent.Result{Proposal: &agent.Proposal{Summary: "  "}}); got != agentErrorReply {
		t.Errorf("blank proposal summary should fall back to the error reply, got %q", got)
	}
	// The LLM-distilled proposal summary posts as mrkdwn in the preview, so it must
	// be escaped (a masked link can't surface) — consistent with the confirm card.
	if got := agentReplyText(&agent.Result{Proposal: &agent.Proposal{Summary: "Protect <http://evil|x>."}}); strings.ContainsAny(got, "<>") {
		t.Errorf("proposal preview must escape mrkdwn (no raw <>), got %q", got)
	}
}

func TestAgentEnabled(t *testing.T) {
	llm := fakeAgentLLM{}
	store := &slackdata.AgentStore{}
	post := func(context.Context, string, string, string, string, string) error { return nil }
	full := Config{AgentLLM: llm, AgentStore: store, PostMessage: post}
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"fully wired", full, true},
		{"killed", Config{AgentLLM: llm, AgentStore: store, PostMessage: post, AgentDisabled: true}, false},
		{"no llm", Config{AgentStore: store, PostMessage: post}, false},
		{"no store", Config{AgentLLM: llm, PostMessage: post}, false},
		{"no post", Config{AgentLLM: llm, AgentStore: store}, false},
	}
	for _, c := range cases {
		h := &Handler{cfg: c.cfg}
		if got := h.agentEnabled(); got != c.want {
			t.Errorf("%s: agentEnabled = %v, want %v", c.name, got, c.want)
		}
	}
}

// --- integration: handleEvent → agent turn → reply ---

type fakeAgentLLM struct {
	reply string
	err   error // when set, the turn fails (mimics a Complete/round-trip error)
}

func (f fakeAgentLLM) Complete(context.Context, *agent.Request) (agent.Response, error) {
	if f.err != nil {
		return agent.Response{}, f.err
	}
	return agent.Response{Text: f.reply, StopReason: "end_turn"}, nil
}

// panicAgentLLM panics mid-turn to exercise processAgentEvent's panic safety-net.
type panicAgentLLM struct{}

func (panicAgentLLM) Complete(context.Context, *agent.Request) (agent.Response, error) {
	panic("boom in the model call")
}

// memAgentDDB is a minimal in-memory DynamoDBClient for AgentStore: GetItem +
// conditional PutItem (attribute_not_exists / version match).
type memAgentDDB struct {
	mu     sync.Mutex
	items  map[string]map[string]ddbtypes.AttributeValue
	getErr error // when set, GetItem (conversation load) fails
}

func newMemAgentDDB() *memAgentDDB {
	return &memAgentDDB{items: map[string]map[string]ddbtypes.AttributeValue{}}
}

func memKey(m map[string]ddbtypes.AttributeValue) string {
	pk, _ := m["pk"].(*ddbtypes.AttributeValueMemberS)
	sk, _ := m["sk"].(*ddbtypes.AttributeValueMemberS)
	return pk.Value + "|" + sk.Value
}

func (f *memAgentDDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if item, ok := f.items[memKey(in.Key)]; ok {
		return &dynamodb.GetItemOutput{Item: item}, nil
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (f *memAgentDDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := memKey(in.Item)
	_, present := f.items[k]
	if cond := aws.ToString(in.ConditionExpression); cond != "" && present && !strings.Contains(cond, " OR ") {
		return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("exists")}
	}
	f.items[k] = in.Item
	return &dynamodb.PutItemOutput{}, nil
}

func (f *memAgentDDB) UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	return &dynamodb.UpdateItemOutput{}, nil
}

func (f *memAgentDDB) DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	return &dynamodb.DeleteItemOutput{}, nil
}

func (f *memAgentDDB) Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	return &dynamodb.QueryOutput{}, nil
}

type capturedReply struct {
	channel, threadTS, text string
}

// capturingPostMessage returns a PostMessageFunc that records every reply, plus
// the slice + mutex to read them after the async workers drain.
func capturingPostMessage() (PostMessageFunc, *[]capturedReply, *sync.Mutex) {
	var mu sync.Mutex
	var posts []capturedReply
	fn := func(_ context.Context, _, _, channel, threadTS, text string) error {
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, capturedReply{channel, threadTS, text})
		return nil
	}
	return fn, &posts, &mu
}

func newAgentEventHandler(t *testing.T, reply string) (*Handler, *[]capturedReply, *sync.Mutex) {
	t.Helper()
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{AgentLLM: fakeAgentLLM{reply: reply}, AgentStore: store, PostMessage: post})
	t.Cleanup(h.Wait)
	return h, posts, mu
}

func appMentionBody(eventID string) string {
	return `{"type":"event_callback","team_id":"T1","event_id":"` + eventID + `",` +
		`"event":{"type":"app_mention","user":"U2","channel":"C1","ts":"100.1","text":"<@U12345678> what can I reach?"}}`
}

func TestHandleEvent_AgentReplies(t *testing.T) {
	h, posts, mu := newAgentEventHandler(t, "You can reach staging.")
	w := httptest.NewRecorder()
	h.handleEvent(w, []byte(appMentionBody("Ev1")))
	if w.Code != 200 {
		t.Fatalf("ack code = %d", w.Code)
	}
	h.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 {
		t.Fatalf("expected exactly one reply, got %d", len(*posts))
	}
	got := (*posts)[0]
	if got.channel != "C1" || got.threadTS != "100.1" || got.text != "You can reach staging." {
		t.Fatalf("reply = %+v", got)
	}
}

func TestHandleEvent_DedupesRetries(t *testing.T) {
	h, posts, mu := newAgentEventHandler(t, "hello")
	// Same event_id delivered twice (a Slack retry).
	for range 2 {
		h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvDup")))
	}
	h.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 {
		t.Fatalf("a retried event must reply once, got %d", len(*posts))
	}
}

// agentEventBody builds an app_mention event_callback with a controllable
// event_id, message ts, and (optional) thread_ts — for exercising the dedupe key.
func agentEventBody(eventID, ts, threadTS string) string {
	tt := ""
	if threadTS != "" {
		tt = `,"thread_ts":"` + threadTS + `"`
	}
	return `{"type":"event_callback","team_id":"T1","event_id":"` + eventID + `",` +
		`"event":{"type":"app_mention","user":"U2","channel":"C1","ts":"` + ts + `"` + tt + `,"text":"<@U12345678> hi"}}`
}

func TestHandleEvent_DedupeKeyedOnMessageIdentity(t *testing.T) {
	// Dedupe keys on (channel, the message's own ts), so:
	//   - one message delivered as two events (e.g. app_mention + message.im, both
	//     subscribed) — DISTINCT event_ids, same ts → ONE reply; and
	//   - two DIFFERENT messages in one thread (shared thread_ts, distinct own ts)
	//     → TWO replies. The key is the message's own ts, NOT the thread root, so
	//     threaded follow-ups aren't dropped — this row guards against keying the
	//     dedupe on the conversation/thread id by mistake.
	cases := []struct {
		name     string
		a, b     string
		wantReps int
	}{
		{"one message, two event ids", agentEventBody("EvA", "200.1", ""), agentEventBody("EvB", "200.1", ""), 1},
		{"threaded follow-ups", agentEventBody("Ev1", "300.1", "300.0"), agentEventBody("Ev2", "300.2", "300.0"), 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, posts, mu := newAgentEventHandler(t, "hi")
			h.handleEvent(httptest.NewRecorder(), []byte(c.a))
			h.handleEvent(httptest.NewRecorder(), []byte(c.b))
			h.Wait()

			mu.Lock()
			defer mu.Unlock()
			if len(*posts) != c.wantReps {
				t.Fatalf("%s: want %d replies, got %d", c.name, c.wantReps, len(*posts))
			}
		})
	}
}

func TestHandleEvent_LoadFailurePostsError(t *testing.T) {
	// LoadConversation fails after the dedupe marker is committed: the user must
	// get an error reply, not silence (Slack won't retry — we acked 200).
	fake := newMemAgentDDB()
	fake.getErr = errors.New("ddb read down") // GetItem (load) fails; PutItem (dedupe) still succeeds
	store := &slackdata.AgentStore{Client: fake, TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{AgentLLM: fakeAgentLLM{reply: "unused"}, AgentStore: store, PostMessage: post})
	t.Cleanup(h.Wait)
	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvLF")))
	h.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentErrorReply {
		t.Fatalf("load failure should post one error reply, got %+v", *posts)
	}
}

func TestProcessAgentEvent_DeliversOnSpentTurnCtx(t *testing.T) {
	// A turn that exhausts agentTurnTimeout leaves the turn ctx canceled by the
	// time there's a reply to post. Delivery — the error reply, and the save +
	// success post — must ride a fresh context off h.baseCtx, NOT the spent turn
	// ctx: a post on a dead ctx fails instantly, and the user, whose @-mention was
	// already dedupe-committed and acked 200, would get silence. The ctx-aware post
	// below rejects an already-canceled ctx, so a regression to posting on the turn
	// ctx drops the reply here and fails the count assertion.
	cases := []struct {
		name string
		llm  fakeAgentLLM
		want string
	}{
		// The turn ctx is spent here, so a failed turn reads as transient (retry),
		// not the generic error copy — see TestProcessAgentEvent_GenericErrorCopy
		// for the live-ctx (capability) branch.
		{"turn failed", fakeAgentLLM{err: errors.New("turn deadline exceeded")}, agentTransientReply},
		{"turn succeeded", fakeAgentLLM{reply: "You can reach staging."}, "You can reach staging."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
			var mu sync.Mutex
			var posts []capturedReply
			post := func(ctx context.Context, _, _, channel, threadTS, text string) error {
				if err := ctx.Err(); err != nil { // pre-fix: posts riding the spent turn ctx land here
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				posts = append(posts, capturedReply{channel, threadTS, text})
				return nil
			}
			h := NewHandler(Config{AgentLLM: c.llm, AgentStore: store, PostMessage: post})

			// A spent turn ctx, exactly as the 90s budget elapsing would leave it.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			h.processAgentEvent(ctx, slog.Default(), env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> do it"))

			mu.Lock()
			defer mu.Unlock()
			if len(posts) != 1 || posts[0].text != c.want {
				t.Fatalf("spent turn ctx should still deliver %q, got %+v", c.want, posts)
			}
		})
	}
}

func TestProcessAgentEvent_GenericErrorCopy(t *testing.T) {
	// A turn that fails while its ctx is still live (a model/backend error within
	// budget, not a timeout) is a generic failure, not a transient one — the user
	// gets agentErrorReply, not the retry-flavored agentTransientReply.
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:    fakeAgentLLM{err: errors.New("model 500")},
		AgentStore:  store,
		PostMessage: post,
	})

	h.processAgentEvent(context.Background(), slog.Default(),
		env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> do it"))

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentErrorReply {
		t.Fatalf("in-budget failure should post the generic error reply, got %+v", *posts)
	}
}

func TestProcessAgentEvent_PanicPostsError(t *testing.T) {
	// A panic mid-turn — after dedupe is committed and 200 already acked, so Slack
	// won't retry — must not vanish: the safety-net recover posts the error reply
	// (and the panic must not escape processAgentEvent). We assert the panic was
	// logged as well as replied, so the test is specific to the recover path and
	// not satisfied by an ordinary in-budget error that also posts agentErrorReply.
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{AgentLLM: panicAgentLLM{}, AgentStore: store, PostMessage: post})

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	h.processAgentEvent(context.Background(), logger,
		env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> boom"))

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentErrorReply {
		t.Fatalf("a panicking turn should still post the error reply, got %+v", *posts)
	}
	if !strings.Contains(logBuf.String(), "agent: panic during turn") {
		t.Fatalf("panic safety-net must log the recovered panic; log = %s", logBuf.String())
	}
}

func TestSaveAgentHistory_ByteGuard(t *testing.T) {
	fake := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: fake, TableName: "agent_state"}
	h := NewHandler(Config{AgentStore: store})

	// 12 turns of ~50KB each (~600KB) — well past maxPersistedBytes (350KB) even
	// though it's under the 40-message count cap.
	big := strings.Repeat("x", 50*1024)
	history := make([]agent.Message, 0, 24)
	for i := range 12 {
		history = append(history,
			agent.Message{Role: "user", Text: fmt.Sprintf("q%d", i)},
			agent.Message{Role: "assistant", Text: big},
		)
	}
	h.saveAgentHistory(slog.Default(), "T1", "C1:1", history, 0)

	item, ok := fake.items["T1|conv#C1:1"]
	if !ok {
		t.Fatalf("conversation not persisted")
	}
	stored := item["messages"].(*ddbtypes.AttributeValueMemberS).Value
	if stored == "" {
		t.Fatalf("expected the latest turn to be kept")
	}
	if len(stored) > maxPersistedBytes {
		t.Fatalf("persisted blob %d bytes exceeds the %d byte cap", len(stored), maxPersistedBytes)
	}
}

func TestSaveAgentHistory_SingleOversizedTurnDoesNotHang(t *testing.T) {
	// A single turn whose own content exceeds the byte cap has no turn boundary
	// to trim below, so the byte-guard loop must break (not spin forever) and
	// save oversized. Without the no-progress break this test would hang.
	fake := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: fake, TableName: "agent_state"}
	h := NewHandler(Config{AgentStore: store})

	history := []agent.Message{
		{Role: "user", Text: "do it"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "t1", Name: "list_resources"}}},
		{Role: "user", ToolResults: []agent.ToolResult{{ToolUseID: "t1", Content: strings.Repeat("y", 400*1024)}}},
		{Role: "assistant", Text: "done"},
	}
	done := make(chan struct{})
	go func() {
		h.saveAgentHistory(slog.Default(), "T1", "C1:9", history, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("saveAgentHistory hung on a single oversized turn (byte-guard loop did not terminate)")
	}
}

func TestTrimAgentHistory(t *testing.T) {
	// Short history is untouched.
	short := []agent.Message{{Role: "user", Text: "hi"}, {Role: "assistant", Text: "yo"}}
	if got := trimAgentHistory(short, 40); len(got) != 2 {
		t.Fatalf("short history should be unchanged, got %d", len(got))
	}

	// 15 four-message turns (user text, assistant tool_use, user tool_result,
	// assistant text) = 60 messages.
	long := make([]agent.Message, 0, 60)
	for i := range 15 {
		long = append(long,
			agent.Message{Role: "user", Text: fmt.Sprintf("q%d", i)},
			agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: fmt.Sprintf("t%d", i), Name: "list_resources"}}},
			agent.Message{Role: "user", ToolResults: []agent.ToolResult{{ToolUseID: fmt.Sprintf("t%d", i), Content: "x"}}},
			agent.Message{Role: "assistant", Text: fmt.Sprintf("a%d", i)},
		)
	}
	got := trimAgentHistory(long, 40)
	if len(got) > 40 {
		t.Fatalf("history not bounded: %d", len(got))
	}
	if !isUserTurnStart(&got[0]) {
		t.Fatalf("trimmed history must start at a user turn, got %+v", got[0])
	}
	// Well-formed: every kept tool_use has its tool_result and vice versa.
	results := map[string]bool{}
	for _, m := range got {
		for _, tr := range m.ToolResults {
			results[tr.ToolUseID] = true
		}
	}
	for _, m := range got {
		for _, c := range m.ToolCalls {
			if !results[c.ID] {
				t.Fatalf("tool_use %s lost its result after trim", c.ID)
			}
		}
	}
}

func TestHandleEvent_DisabledStaysSilent(t *testing.T) {
	h := NewHandler(Config{}) // nothing wired → conversation mode off
	t.Cleanup(h.Wait)
	w := httptest.NewRecorder()
	h.handleEvent(w, []byte(appMentionBody("Ev1")))
	if w.Code != 200 {
		t.Fatalf("must still ack 200, got %d", w.Code)
	}
	// url_verification still works.
	w2 := httptest.NewRecorder()
	h.handleEvent(w2, []byte(`{"type":"url_verification","challenge":"abc"}`))
	if !strings.Contains(w2.Body.String(), "abc") {
		t.Fatalf("url_verification challenge not echoed: %s", w2.Body.String())
	}
}
