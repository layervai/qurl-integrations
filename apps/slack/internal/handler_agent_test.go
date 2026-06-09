package internal

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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

type fakeAgentLLM struct{ reply string }

func (f fakeAgentLLM) Complete(context.Context, *agent.Request) (agent.Response, error) {
	return agent.Response{Text: f.reply, StopReason: "end_turn"}, nil
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
