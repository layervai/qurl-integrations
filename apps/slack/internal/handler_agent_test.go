package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"sort"
	"strconv"
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
	// chReply builds a channel message carrying a thread_ts (a thread reply); an empty
	// threadTS is a top-level channel message.
	chReply := func(text, threadTS string) *slackEventEnvelope {
		e := env(slackEventTypeMessage, "channel", "U2", "", "", text)
		e.Event.ThreadTS = threadTS
		return e
	}
	tests := []struct {
		name      string
		env       *slackEventEnvelope
		followups bool
		want      bool
	}{
		// @mentions and DMs are deliberate addresses — admitted regardless of the flag.
		{"app_mention human", env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi"), false, true},
		{"app_mention still works with followups on", env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi"), true, true},
		{"dm human", env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "hi"), false, true},
		{"bot message ignored", env(slackEventTypeAppMention, "channel", "U2", "B9", "", "<@U12345678> hi"), false, false},
		{"subtype (edit/system) ignored", env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "message_changed", "hi"), false, false},
		{"authorless ignored", env(slackEventTypeAppMention, "channel", "", "", "", "<@U12345678> hi"), false, false},
		{"mention with empty text ignored", env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678>   "), false, false},
		{"other event type ignored", env("reaction_added", "channel", "U2", "", "", "x"), false, false},

		// Channel follow-ups: a thread reply is admitted ONLY when the flag is on; a
		// top-level channel message is never admitted (no un-addressed chatter).
		{"channel thread reply, followups off", chReply("hi", "100.0"), false, false},
		{"channel thread reply, followups on", chReply("hi", "100.0"), true, true},
		{"top-level channel message, followups off", chReply("hi", ""), false, false},
		{"top-level channel message, followups on", chReply("hi", ""), true, false},
		{"channel thread reply empty text, followups on", chReply("   ", "100.0"), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldDispatchAgentEvent(tt.env, tt.followups); got != tt.want {
				t.Fatalf("shouldDispatchAgentEvent = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAgentChannelFollowupDropped(t *testing.T) {
	fake := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: fake, TableName: "agent_state"}
	h := &Handler{cfg: Config{AgentStore: store}}
	ctx, log := context.Background(), slog.Default()

	reply := func(channel, threadTS string) *slackEventEnvelope {
		e := env(slackEventTypeMessage, "channel", "U2", "", "", "follow-up")
		e.Event.Channel = channel
		e.Event.ThreadTS = threadTS
		return e
	}

	// Seed a transcript for a thread the agent already joined.
	joined := reply("C9", "100.0")
	part := agentEventPartition(joined)
	blob, err := json.Marshal([]agent.Message{{}})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := store.SaveConversation(ctx, part, agentEventThreadKey(joined), blob, 0); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	// gateDrop runs the gate and returns just the drop decision, for the cases that only
	// care whether the reply is admitted or dropped (not the reused transcript).
	gateDrop := func(e *slackEventEnvelope, partition string) bool {
		dropped, _ := h.agentChannelFollowupDropped(ctx, log, e, partition)
		return dropped
	}

	// A reply in the joined thread continues it AND hands back the loaded transcript, so
	// the turn reuses it instead of reading DynamoDB a second time (the #712 double-read fix).
	switch dropped, pre := h.agentChannelFollowupDropped(ctx, log, joined, part); {
	case dropped:
		t.Fatal("a reply in a thread the agent joined must NOT be dropped")
	case pre == nil:
		t.Fatal("an admitted follow-up must return the preloaded transcript for reuse")
	case len(pre.history) != 1:
		t.Fatalf("preloaded history = %d msgs, want 1 (the seeded transcript)", len(pre.history))
	}
	if !gateDrop(reply("C9", "999.0"), part) {
		t.Fatal("a reply in a thread the agent never joined must be dropped")
	}
	// Same thread_ts but a different channel is a different thread → dropped.
	if !gateDrop(reply("C-other", "100.0"), part) {
		t.Fatal("a reply in another channel's thread must be dropped")
	}

	// Non-follow-ups are never dropped here — @mentions and DMs are deliberate
	// addresses handled without the history gate (and get no preloaded transcript).
	mention := env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi")
	if dropped, pre := h.agentChannelFollowupDropped(ctx, log, mention, agentEventPartition(mention)); dropped || pre != nil {
		t.Fatal("an @mention is not a channel follow-up; must not be dropped or preloaded")
	}
	dm := env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "hi")
	if dropped, pre := h.agentChannelFollowupDropped(ctx, log, dm, agentEventPartition(dm)); dropped || pre != nil {
		t.Fatal("a DM is not a channel follow-up; must not be dropped or preloaded")
	}

	// A joined thread whose stored transcript is corrupt/undecodable reads back as no
	// history (loadAgentHistory starts fresh on a decode error), so the follow-up
	// fail-closed drops — "no DECODABLE transcript", not merely "never joined".
	corrupt := reply("C-corrupt", "300.0")
	if err := store.SaveConversation(ctx, part, agentEventThreadKey(corrupt), []byte("not valid json"), 0); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	if !gateDrop(corrupt, part) {
		t.Fatal("a follow-up whose transcript can't be decoded must be dropped (fail closed)")
	}

	// Fail closed: when the transcript lookup itself errors we can't confirm the thread
	// is the agent's, so the reply is dropped (and stays silent) rather than answered.
	fake.getErr = errors.New("ddb read down")
	if !gateDrop(joined, part) {
		t.Fatal("a follow-up whose transcript lookup errors must be dropped (fail closed)")
	}
	fake.getErr = nil
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
	// agentReplyText renders the mrkdwn text-seam reply: the escaped proposal preview,
	// else the error fallback. The agent's own free-text answer is delivered as
	// markdown_text (see TestDeliverAgentResult_RoutesByDialect), not through here — so
	// a non-proposal result reaching this function renders the error reply.
	if got := agentReplyText(&agent.Result{Reply: "hello"}); got != agentErrorReply {
		t.Errorf("non-proposal result renders the error fallback (answers go via markdown_text), got %q", got)
	}
	prop := agentReplyText(&agent.Result{Proposal: &agent.Proposal{Summary: "Protect $x."}})
	if !strings.Contains(prop, "isn't enabled yet") || !strings.Contains(prop, "Protect $x.") {
		t.Errorf("proposal preview = %q", prop)
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
	mu        sync.Mutex
	items     map[string]map[string]ddbtypes.AttributeValue
	getErr    error // when set, GetItem (conversation load) fails
	updateErr error // when set, UpdateItem (turn-rate counter) fails
	// putCalls counts PutItem calls (conversation saves) so a test can assert the
	// conflict-retry path attempts exactly one extra write, never a loop.
	putCalls int
	// forceConflicts makes the next N PutItems return a version conflict
	// regardless of the stored version — the only way to deterministically force a
	// SECOND conflict (a passive CAS fake can't, since no writer slips in between a
	// synchronous reload and retry).
	forceConflicts int
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
	f.putCalls++
	if f.forceConflicts > 0 {
		f.forceConflicts--
		return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("forced conflict")}
	}
	k := memKey(in.Item)
	existing, present := f.items[k]
	if cond := aws.ToString(in.ConditionExpression); cond != "" && !memEvalSaveCond(cond, existing, present, in.ExpressionAttributeValues) {
		return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("conditional check failed")}
	}
	f.items[k] = in.Item
	return &dynamodb.PutItemOutput{}, nil
}

// memEvalSaveCond models the two PutItem condition shapes AgentStore emits:
// `attribute_not_exists(pk)` (single-term create guard, MarkEventSeen/pending) and
// `attribute_not_exists(pk) OR conv_version = :ev` (SaveConversation's optimistic
// concurrency). Mirrors agentFakeDDB.evalCond in the slackdata package so the
// version race can be driven faithfully at the handler level.
func memEvalSaveCond(cond string, existing map[string]ddbtypes.AttributeValue, present bool, vals map[string]ddbtypes.AttributeValue) bool {
	if !present {
		return true // attribute_not_exists(pk) holds
	}
	if !strings.Contains(cond, " OR ") {
		return false // single-term create guard, row already present
	}
	want, ok := vals[":ev"].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return false
	}
	cur, ok := existing["conv_version"].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return false
	}
	return cur.Value == want.Value
}

// UpdateItem fakes only the one shape BumpTurnCount emits — "ADD turn_count :one SET
// #ttl = :ttl" (ttl is a DynamoDB reserved word, so it's aliased via #ttl) — applying
// the number ADD and the SET, then returning UPDATED_NEW. A no-op stub would make the
// rate-limit tests vacuously pass (count always 0), so it actually mutates; anything
// other than that exact shape errors loudly.
func (f *memAgentDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if expr := aws.ToString(in.UpdateExpression); expr != "ADD turn_count :one SET #ttl = :ttl" {
		return nil, fmt.Errorf("memAgentDDB.UpdateItem: unsupported expression %q", expr)
	}
	k := memKey(in.Key)
	item, present := f.items[k]
	if !present {
		item = map[string]ddbtypes.AttributeValue{}
		for kk, vv := range in.Key {
			item[kk] = vv
		}
	}
	newVal := memNumberValue(item["turn_count"]) + memNumberValue(in.ExpressionAttributeValues[":one"])
	item["turn_count"] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(newVal, 10)}
	item["ttl"] = in.ExpressionAttributeValues[":ttl"]
	f.items[k] = item
	return &dynamodb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{
		"turn_count": item["turn_count"],
		"ttl":        item["ttl"],
	}}, nil
}

func memNumberValue(av ddbtypes.AttributeValue) int64 {
	n, ok := av.(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0
	}
	v, _ := strconv.ParseInt(n.Value, 10, 64)
	return v
}

func (f *memAgentDDB) DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	return &dynamodb.DeleteItemOutput{}, nil
}

// Query models the one shape ListAuditEntries emits: pk equality + begins_with(sk),
// honoring ScanIndexForward + Limit. (Other AgentStore reads are point GetItems.)
func (f *memAgentDDB) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vals := in.ExpressionAttributeValues
	pkv, _ := vals[":pk"].(*ddbtypes.AttributeValueMemberS)
	prefv, _ := vals[":prefix"].(*ddbtypes.AttributeValueMemberS)
	var matched []map[string]ddbtypes.AttributeValue
	for _, item := range f.items {
		pk, _ := item["pk"].(*ddbtypes.AttributeValueMemberS)
		sk, _ := item["sk"].(*ddbtypes.AttributeValueMemberS)
		if pk == nil || sk == nil || pkv == nil || pk.Value != pkv.Value {
			continue
		}
		if prefv != nil && !strings.HasPrefix(sk.Value, prefv.Value) {
			continue
		}
		matched = append(matched, item)
	}
	sort.Slice(matched, func(i, j int) bool {
		si := matched[i]["sk"].(*ddbtypes.AttributeValueMemberS).Value
		sj := matched[j]["sk"].(*ddbtypes.AttributeValueMemberS).Value
		if in.ScanIndexForward != nil && !*in.ScanIndexForward {
			return si > sj
		}
		return si < sj
	})
	if in.Limit != nil && int(*in.Limit) < len(matched) {
		matched = matched[:*in.Limit]
	}
	return &dynamodb.QueryOutput{Items: matched}, nil
}

type capturedReply struct {
	channel, threadTS, text string
	// markdown records whether the reply arrived on the markdown_text seam
	// (PostMarkdownMessage) rather than the mrkdwn text seam (PostMessage).
	markdown bool
}

// capturingPostMessage returns a PostMessageFunc that records every reply, plus
// the slice + mutex to read them after the async workers drain.
func capturingPostMessage() (PostMessageFunc, *[]capturedReply, *sync.Mutex) {
	var mu sync.Mutex
	var posts []capturedReply
	fn := func(_ context.Context, _, _, channel, threadTS, text string) error {
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, capturedReply{channel: channel, threadTS: threadTS, text: text})
		return nil
	}
	return fn, &posts, &mu
}

// capturingPostMarkdownMessage records markdown_text replies into the SAME slice
// (tagged markdown:true), so a test can assert which seam delivered a reply.
func capturingPostMarkdownMessage(posts *[]capturedReply, mu *sync.Mutex) PostMessageFunc {
	return func(_ context.Context, _, _, channel, threadTS, text string) error {
		mu.Lock()
		defer mu.Unlock()
		*posts = append(*posts, capturedReply{channel: channel, threadTS: threadTS, text: text, markdown: true})
		return nil
	}
}

func newAgentEventHandler(t *testing.T, reply string) (*Handler, *[]capturedReply, *sync.Mutex) {
	t.Helper()
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	mdPost := capturingPostMarkdownMessage(posts, mu)
	h := NewHandler(Config{AgentLLM: fakeAgentLLM{reply: reply}, AgentStore: store, PostMessage: post, PostMarkdownMessage: mdPost, AgentDefaultEnabled: true})
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

// dmMessageBody builds a DM (message.im) event_callback — a human DM to the agent,
// which dispatches but carries no channel name to resolve.
func dmMessageBody(eventID string) string {
	return `{"type":"event_callback","team_id":"T1","event_id":"` + eventID + `",` +
		`"event":{"type":"message","channel_type":"im","user":"U2","channel":"D1","ts":"100.2","text":"what can I reach?"}}`
}

func TestHandleEvent_AgentResolvesChannelNameSkippingDMs(t *testing.T) {
	var mu sync.Mutex
	var resolved []string
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM: fakeAgentLLM{reply: "ok"}, AgentStore: store, PostMessage: post, AgentDefaultEnabled: true,
		ResolveChannelName: func(_ context.Context, _, _, channelID string) (string, error) {
			mu.Lock()
			resolved = append(resolved, channelID)
			mu.Unlock()
			return "general", nil
		},
	})
	t.Cleanup(h.Wait)

	// A channel @mention resolves the channel name for the prompt.
	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvCh")))
	h.Wait()
	mu.Lock()
	if len(resolved) != 1 || resolved[0] != "C1" {
		mu.Unlock()
		t.Fatalf("a channel mention should resolve its channel name (C1), got %v", resolved)
	}
	mu.Unlock()

	// A DM has no channel name → resolution is skipped; describeChannel uses the id.
	h.handleEvent(httptest.NewRecorder(), []byte(dmMessageBody("EvDM")))
	h.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(resolved) != 1 {
		t.Fatalf("a DM must not resolve a channel name, got %v", resolved)
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
	h := NewHandler(Config{AgentLLM: fakeAgentLLM{reply: "unused"}, AgentStore: store, PostMessage: post, AgentDefaultEnabled: true})
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
				posts = append(posts, capturedReply{channel: channel, threadTS: threadTS, text: text})
				return nil
			}
			h := NewHandler(Config{AgentLLM: c.llm, AgentStore: store, PostMessage: post, AgentDefaultEnabled: true})

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
		AgentLLM:            fakeAgentLLM{err: errors.New("model 500")},
		AgentStore:          store,
		PostMessage:         post,
		AgentDefaultEnabled: true,
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
	h := NewHandler(Config{AgentLLM: panicAgentLLM{}, AgentStore: store, PostMessage: post, AgentDefaultEnabled: true})

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
	h.saveAgentHistory(slog.Default(), "T1", "C1:1", history, nil, 0)

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
		h.saveAgentHistory(slog.Default(), "T1", "C1:9", history, nil, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("saveAgentHistory hung on a single oversized turn (byte-guard loop did not terminate)")
	}
}

// assertTranscriptWellFormed checks the agent-API invariants that trimAgentHistory
// + agent.Run maintain and that the #666 conflict-merge must not break: (i) the
// transcript does not begin with an orphaned tool_results message, and (ii) every
// assistant tool_use is answered by the immediately following user message,
// covering exactly those tool-use IDs. It deliberately does NOT assert role
// alternation: a turn ending in a proposal or the iteration cap ends with a
// user{ToolResults} message and the next turn opens with user{Text}, so user→user
// adjacency is normal and API-valid.
func assertTranscriptWellFormed(t *testing.T, msgs []agent.Message) {
	t.Helper()
	if len(msgs) == 0 {
		return
	}
	if len(msgs[0].ToolResults) > 0 {
		t.Fatalf("transcript head is an orphaned tool_results message: %+v", msgs[0])
	}
	for i := range msgs {
		if len(msgs[i].ToolCalls) == 0 {
			continue
		}
		if i+1 >= len(msgs) {
			t.Fatalf("assistant tool_use at msg %d has no following tool_results", i)
		}
		want := make(map[string]bool, len(msgs[i].ToolCalls))
		for _, call := range msgs[i].ToolCalls {
			want[call.ID] = true
		}
		got := make(map[string]bool, len(msgs[i+1].ToolResults))
		for _, tr := range msgs[i+1].ToolResults {
			got[tr.ToolUseID] = true
		}
		for id := range want {
			if !got[id] {
				t.Fatalf("tool_use %q (msg %d) has no matching tool_result in msg %d", id, i, i+1)
			}
		}
		for id := range got {
			if !want[id] {
				t.Fatalf("tool_result %q (msg %d) has no matching tool_use in msg %d", id, i+1, i)
			}
		}
	}
}

func TestSaveAgentHistory_ConflictMergesAndRetries(t *testing.T) {
	// A concurrent turn won the version race. saveAgentHistory must reload the
	// winner's transcript, graft this turn's delta on top, and retry once — losing
	// neither turn and keeping the merged transcript well-formed. This is the
	// guard-#5 proof: the merge seam joins the winner's tool-using turn (ending in
	// a tool_results message) to this turn's tool-using turn.
	fake := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: fake, TableName: "agent_state"}
	h := NewHandler(Config{AgentStore: store})
	ctx := context.Background()
	const part, thread = "T1", "C1:1"

	mustMarshal := func(m []agent.Message) []byte {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	// Base transcript both racing turns loaded (stored at conv_version 1).
	base := []agent.Message{
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "hello"},
	}
	if err := store.SaveConversation(ctx, part, thread, mustMarshal(base), 0); err != nil {
		t.Fatalf("seed base: %v", err)
	}
	// The winner: base + a proposal turn ending in a tool_results message (the
	// proposal path), so the merge seam follows tool_results with this turn's
	// leading user message — the user→user adjacency the checker must allow.
	winnerDelta := []agent.Message{
		{Role: "user", Text: "revoke staging"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "p1", Name: "propose_revoke"}}},
		{Role: "user", ToolResults: []agent.ToolResult{{ToolUseID: "p1", Content: "proposed"}}},
	}
	winnerFull := append(append([]agent.Message{}, base...), winnerDelta...)
	if err := store.SaveConversation(ctx, part, thread, mustMarshal(winnerFull), 1); err != nil {
		t.Fatalf("seed winner: %v", err)
	}

	// This turn loaded base at conv_version 1 and produced its own tool-using turn.
	myDelta := []agent.Message{
		{Role: "user", Text: "what can I reach"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "t2", Name: "list_resources"}}},
		{Role: "user", ToolResults: []agent.ToolResult{{ToolUseID: "t2", Content: "r_1"}}},
		{Role: "assistant", Text: "You can reach r_1"},
	}
	myFull := append(append([]agent.Message{}, base...), myDelta...)

	fake.putCalls = 0 // ignore the two seeding writes
	h.saveAgentHistory(slog.Default(), part, thread, myFull, myDelta, 1)

	// Exactly one extra write: the conflicting save + one merged retry.
	if fake.putCalls != 2 {
		t.Fatalf("expected 2 writes (conflict + one retry), got %d", fake.putCalls)
	}
	storedBlob, _, err := store.LoadConversation(ctx, part, thread)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var merged []agent.Message
	if err := json.Unmarshal(storedBlob, &merged); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}
	assertTranscriptWellFormed(t, merged)

	// The stored transcript is the winner's full transcript with this turn's delta
	// appended in order — neither turn lost, the winner's turn kept in place.
	if len(merged) != len(winnerFull)+len(myDelta) {
		t.Fatalf("merged length %d, want %d (winner + delta)", len(merged), len(winnerFull)+len(myDelta))
	}
	if merged[len(base)].Text != "revoke staging" {
		t.Fatalf("winner's turn not preserved at the seam: %+v", merged[len(base)])
	}
	if merged[len(winnerFull)].Text != "what can I reach" {
		t.Fatalf("this turn's delta not grafted after the winner: %+v", merged[len(winnerFull)])
	}
	if last := merged[len(merged)-1]; last.Text != "You can reach r_1" {
		t.Fatalf("this turn's reply not preserved: %+v", last)
	}
}

func TestSaveAgentHistory_ReloadFailureDropsTurn(t *testing.T) {
	// On conflict, if reloading the winner's transcript fails hard, drop the turn —
	// never graft this turn's delta onto a garbage base, never clobber the winner.
	fake := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: fake, TableName: "agent_state"}
	h := NewHandler(Config{AgentStore: store})
	ctx := context.Background()
	const part, thread = "T1", "C1:1"

	winner := []agent.Message{{Role: "user", Text: "winner"}, {Role: "assistant", Text: "won"}}
	wb, err := json.Marshal(winner)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := store.SaveConversation(ctx, part, thread, wb, 0); err != nil { // stores conv_version 1
		t.Fatalf("seed winner: %v", err)
	}
	fake.getErr = errors.New("ddb unavailable") // the reload (GetItem) now fails
	fake.putCalls = 0

	myDelta := []agent.Message{{Role: "user", Text: "mine"}, {Role: "assistant", Text: "reply"}}
	h.saveAgentHistory(slog.Default(), part, thread, myDelta, myDelta, 0) // version 0 conflicts with stored 1

	if fake.putCalls != 1 {
		t.Fatalf("expected exactly 1 write (the conflicting save, no merged retry), got %d", fake.putCalls)
	}
	fake.getErr = nil
	storedBlob, _, err := store.LoadConversation(ctx, part, thread)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !bytes.Equal(storedBlob, wb) {
		t.Fatalf("winner's transcript was clobbered: %s", storedBlob)
	}
}

func TestSaveAgentHistory_CorruptReloadDropsTurn(t *testing.T) {
	// On conflict, if the winner's stored blob can't be decoded, drop the turn
	// rather than graft onto a garbage base or overwrite the (maybe-recoverable)
	// winner blob with only this turn's delta.
	fake := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: fake, TableName: "agent_state"}
	h := NewHandler(Config{AgentStore: store})
	ctx := context.Background()
	const part, thread = "T1", "C1:1"

	if err := store.SaveConversation(ctx, part, thread, []byte("{not json"), 0); err != nil { // corrupt winner at conv_version 1
		t.Fatalf("seed corrupt: %v", err)
	}
	fake.putCalls = 0

	myDelta := []agent.Message{{Role: "user", Text: "mine"}, {Role: "assistant", Text: "reply"}}
	h.saveAgentHistory(slog.Default(), part, thread, myDelta, myDelta, 0) // conflicts with stored 1

	if fake.putCalls != 1 {
		t.Fatalf("expected exactly 1 write (no merged retry on corrupt reload), got %d", fake.putCalls)
	}
	storedBlob, _, err := store.LoadConversation(ctx, part, thread)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if string(storedBlob) != "{not json" {
		t.Fatalf("corrupt winner blob was overwritten: %s", storedBlob)
	}
}

func TestSaveAgentHistory_RetriesAtMostOnce(t *testing.T) {
	// If the merged retry ALSO loses a version race, drop the turn after one retry.
	// The save path never loops — an unbounded race would pin the worker.
	fake := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: fake, TableName: "agent_state"}
	h := NewHandler(Config{AgentStore: store})
	const part, thread = "T1", "C1:1"

	fake.forceConflicts = 2 // the first save AND the merged retry both conflict
	myDelta := []agent.Message{{Role: "user", Text: "mine"}, {Role: "assistant", Text: "reply"}}
	h.saveAgentHistory(slog.Default(), part, thread, myDelta, myDelta, 0)

	if fake.putCalls != 2 {
		t.Fatalf("expected exactly 2 writes (initial + one retry, then stop), got %d", fake.putCalls)
	}
}

func TestSaveAgentHistory_MalformedDeltaHeadDropsTurn(t *testing.T) {
	// Defense-in-depth: if this turn's delta opens with a tool_results message
	// (a.Run never produces that today, but would if it ever stopped being
	// pure-append), the conflict-merge must drop rather than graft an orphaned
	// tool_result onto the winner's transcript — no reload, no merged retry.
	fake := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: fake, TableName: "agent_state"}
	h := NewHandler(Config{AgentStore: store})
	ctx := context.Background()
	const part, thread = "T1", "C1:1"

	winner := []agent.Message{{Role: "user", Text: "winner"}, {Role: "assistant", Text: "won"}}
	wb, err := json.Marshal(winner)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := store.SaveConversation(ctx, part, thread, wb, 0); err != nil { // stores conv_version 1
		t.Fatalf("seed winner: %v", err)
	}
	fake.putCalls = 0

	badDelta := []agent.Message{{Role: "user", ToolResults: []agent.ToolResult{{ToolUseID: "x", Content: "orphan"}}}}
	h.saveAgentHistory(slog.Default(), part, thread, badDelta, badDelta, 0) // conflicts with stored 1

	if fake.putCalls != 1 {
		t.Fatalf("expected exactly 1 write (the conflicting save, no merged retry), got %d", fake.putCalls)
	}
	storedBlob, _, err := store.LoadConversation(ctx, part, thread)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !bytes.Equal(storedBlob, wb) {
		t.Fatalf("winner's transcript was clobbered by a malformed delta: %s", storedBlob)
	}
}

func TestAgentRunPreservedPrefix(t *testing.T) {
	base := []agent.Message{
		{Role: "user", Text: "q1"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "t1", Name: "list_resources"}}},
		{Role: "user", ToolResults: []agent.ToolResult{{ToolUseID: "t1", Content: "r"}}},
	}
	appended := append(append([]agent.Message{}, base...),
		agent.Message{Role: "user", Text: "q2"},
		agent.Message{Role: "assistant", Text: "a2"},
	)
	if !agentRunPreservedPrefix(base, appended) {
		t.Fatal("pure-append should preserve the prefix")
	}
	if !agentRunPreservedPrefix(nil, appended) {
		t.Fatal("empty loaded history is always a prefix")
	}
	if agentRunPreservedPrefix(base, base[:2]) {
		t.Fatal("a shorter transcript can't contain loaded as a prefix")
	}
	// Same length-or-longer, but a prefix element rewritten (a hypothetical
	// compaction). The length check alone would pass; the exact-prefix check must
	// not — this is the silent-corruption case the runtime guard exists to catch.
	rewritten := append([]agent.Message{}, appended...)
	rewritten[1] = agent.Message{Role: "assistant", Text: "compacted"}
	if agentRunPreservedPrefix(base, rewritten) {
		t.Fatal("a rewritten prefix element must fail the exact-prefix check")
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

func TestDeliverAgentResult_RoutesByDialect(t *testing.T) {
	// The agent's free-text answer delivers via markdown_text (standard Markdown,
	// parity with the streaming pane); a proposal preview stays on the escaped mrkdwn
	// text seam. The confirm card flow is OFF here (no PostMessageBlocks), so a
	// proposal falls through to the text preview rather than a card.
	textPost, posts, mu := capturingPostMessage()
	mdPost := capturingPostMarkdownMessage(posts, mu)
	h := NewHandler(Config{PostMessage: textPost, PostMarkdownMessage: mdPost})
	e := env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi")

	h.deliverAgentResult(slog.Default(), e, "100.1", &agent.Result{Reply: "Use **bold** here"})
	h.deliverAgentResult(slog.Default(), e, "100.1", &agent.Result{Proposal: &agent.Proposal{Summary: "Protect $x."}})

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 2 {
		t.Fatalf("want 2 posts, got %d: %+v", len(*posts), *posts)
	}
	// Free-text answer: markdown_text seam, body passed through verbatim (Slack's
	// parser renders the Markdown — we must not pre-mangle it).
	if !(*posts)[0].markdown {
		t.Errorf("free-text answer should post on the markdown_text seam, got mrkdwn: %+v", (*posts)[0])
	}
	if (*posts)[0].text != "Use **bold** here" {
		t.Errorf("free-text answer body = %q, want it verbatim", (*posts)[0].text)
	}
	// Proposal preview: escaped mrkdwn text seam, never markdown_text (injection defense).
	if (*posts)[1].markdown {
		t.Errorf("proposal preview should post on the mrkdwn text seam, got markdown_text: %+v", (*posts)[1])
	}
	if !strings.HasPrefix((*posts)[1].text, agentProposalPreviewPrefix) {
		t.Errorf("proposal preview = %q, want the preview prefix", (*posts)[1].text)
	}
}

func TestDeliverAgentResult_MarkdownSeamFallsBackToText(t *testing.T) {
	// With the markdown seam unwired, the free-text answer still delivers — on the
	// mrkdwn text seam (the pre-fix behavior), not dropped.
	textPost, posts, mu := capturingPostMessage()
	h := NewHandler(Config{PostMessage: textPost})
	e := env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi")

	h.deliverAgentResult(slog.Default(), e, "100.1", &agent.Result{Reply: "plain answer"})

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != "plain answer" {
		t.Fatalf("want the answer delivered via the text seam, got %+v", *posts)
	}
}
