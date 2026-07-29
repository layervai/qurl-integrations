package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testAgentReachStagingReply = "You can reach staging."
	testAgentStillWorksReply   = "still works"
	testAgentStopEndTurn       = "end_turn"
	testAgentStopToolUse       = "tool_use"
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

func TestProcessAgentEvent_LiteralHelpBypassesLLM(t *testing.T) {
	tests := []struct {
		name string
		env  *slackEventEnvelope
	}{
		{
			name: "channel mention",
			env:  env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> help"),
		},
		{
			name: "agent chat is case insensitive",
			env:  env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "  HELP  "),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
			post, posts, mu := capturingPostMessage()
			h := NewHandler(Config{
				AgentLLM:            panicAgentLLM{},
				AgentStore:          store,
				PostMessage:         post,
				AgentDefaultEnabled: true,
			})

			h.processAgentEvent(context.Background(), slog.Default(), tt.env)

			mu.Lock()
			defer mu.Unlock()
			if len(*posts) != 1 || (*posts)[0].text != agentHelpReply {
				t.Fatalf("literal help should post one deterministic usage reply, got %+v", *posts)
			}
		})
	}
}

func TestProcessAgentEvent_HelpPrefixUsesNormalAgentPath(t *testing.T) {
	llm := &scriptedHandlerAgentLLM{responses: []agent.Response{{
		Text:       testAgentStillWorksReply,
		StopReason: testAgentStopEndTurn,
	}}}
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            llm,
		AgentStore:          store,
		PostMessage:         post,
		AgentDefaultEnabled: true,
	})

	h.processAgentEvent(context.Background(), slog.Default(),
		env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> help me"))

	if llm.calls != 1 {
		t.Fatalf("non-literal help should reach the normal agent path once, got %d calls", llm.calls)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentLLMReplyWithDisclaimer(testAgentStillWorksReply) {
		t.Fatalf("non-literal help should return the agent result, got %+v", *posts)
	}
}

func TestAgentHasExplicitNonHTTPSProtectURL(t *testing.T) {
	cases := map[string]bool{
		"Protect javascript:alert(1) as $bad.":  true,
		"protect http://example.com as $docs":   true,
		"Protect <http://example.com> as $docs": true,
		"Protect https://example.com as $docs":  false,
		"Protect $docs as $shared":              false,
		"Protect example.com:8080 as $local":    false,
		"Protect example.com:8080/path as $x":   false,
		"Protect javascript:alert(1)":           true,
		"How do I protect javascript: URLs?":    false,
	}
	for message, want := range cases {
		if got := agentHasExplicitNonHTTPSProtectURL(message); got != want {
			t.Errorf("agentHasExplicitNonHTTPSProtectURL(%q) = %v, want %v", message, got, want)
		}
	}
}

func TestAgentHasExplicitInvalidSetAlias(t *testing.T) {
	cases := map[string]bool{
		"Set alias $Prod_Admin!!! to $staging-api.": true,
		"set alias $prod_admin to $staging-api":     true,
		"Set alias $prod-admin to $staging-api":     false,
		"Set alias for $prod-admin to staging":      false,
		"Set alias prod-admin to $staging-api":      false,
		"How do I set alias $Bad_ID?":               false,
	}
	for message, want := range cases {
		if got := agentHasExplicitInvalidSetAlias(message); got != want {
			t.Errorf("agentHasExplicitInvalidSetAlias(%q) = %v, want %v", message, got, want)
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
	chReplySubtype := func(text, threadTS, subtype string) *slackEventEnvelope {
		e := chReply(text, threadTS)
		e.Event.Subtype = subtype
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
		{"channel thread reply, followups off", chReply("hi", agentPoolTestThreadTS), false, false},
		{"channel thread reply, followups on", chReply("hi", agentPoolTestThreadTS), true, true},
		{"top-level channel message, followups off", chReply("hi", ""), false, false},
		{"top-level channel message, followups on", chReply("hi", ""), true, false},
		{"channel thread reply empty text, followups on", chReply("   ", agentPoolTestThreadTS), true, false},
		{"thread_broadcast channel thread reply, followups off", chReplySubtype("hi", agentPoolTestThreadTS, slackMessageSubtypeThreadBroadcast), false, false},
		{"thread_broadcast channel thread reply, followups on", chReplySubtype("hi", agentPoolTestThreadTS, slackMessageSubtypeThreadBroadcast), true, true},
		{"thread_broadcast top-level channel message, followups on", chReplySubtype("hi", "", slackMessageSubtypeThreadBroadcast), true, false},
		{"thread_broadcast dm ignored", env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", slackMessageSubtypeThreadBroadcast, "hi"), true, false},
		{"other channel thread subtype ignored", chReplySubtype("hi", agentPoolTestThreadTS, "message_changed"), true, false},
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
	var historyErr error
	h := &Handler{cfg: Config{
		AgentThreadHistory: func(_ context.Context, _, _, channelID, threadTS, _ string) ([]AgentThreadMessage, error) {
			if historyErr != nil {
				return nil, historyErr
			}
			if channelID == "C9" && threadTS == agentPoolTestThreadTS {
				return []AgentThreadMessage{
					{UserID: "U1", Text: "question", TS: agentPoolTestThreadTS},
					{AppID: "A1", Text: "answer", TS: "100.1"},
				}, nil
			}
			return nil, nil
		},
	}}
	ctx, log := context.Background(), slog.Default()

	reply := func(channel, threadTS string) *slackEventEnvelope {
		e := env(slackEventTypeMessage, "channel", "U2", "", "", "follow-up")
		e.APIAppID = "A1"
		e.Event.Channel = channel
		e.Event.ThreadTS = threadTS
		e.Event.TS = "100.2"
		return e
	}
	gateDrop := func(e *slackEventEnvelope) bool {
		dropped, _ := h.agentChannelFollowupDropped(ctx, log, e, agentEventPartition(e))
		return dropped
	}

	joined := reply("C9", agentPoolTestThreadTS)
	switch dropped, pre := h.agentChannelFollowupDropped(ctx, log, joined, agentEventPartition(joined)); {
	case dropped:
		t.Fatal("a reply in a thread the agent joined must not be dropped")
	case pre == nil:
		t.Fatal("an admitted follow-up must return the live transcript for reuse")
	case len(pre.history) != 2:
		t.Fatalf("preloaded history = %#v, want one completed exchange", pre.history)
	}
	if !gateDrop(reply("C9", "999.0")) {
		t.Fatal("a reply in a thread the agent never joined must be dropped")
	}
	if !gateDrop(reply("C-other", agentPoolTestThreadTS)) {
		t.Fatal("a reply in another channel's thread must be dropped")
	}

	mention := env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi")
	if dropped, pre := h.agentChannelFollowupDropped(ctx, log, mention, agentEventPartition(mention)); dropped || pre != nil {
		t.Fatal("an @mention is not a channel follow-up")
	}
	dm := env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "hi")
	if dropped, pre := h.agentChannelFollowupDropped(ctx, log, dm, agentEventPartition(dm)); dropped || pre != nil {
		t.Fatal("a DM is not a channel follow-up")
	}

	historyErr = errors.New("Slack read down")
	if !gateDrop(joined) {
		t.Fatal("a follow-up whose live history lookup errors must be dropped")
	}
}

func TestAgentEventKeys(t *testing.T) {
	gridTeam := &slackEventEnvelope{
		TeamID:       "T1",
		EnterpriseID: "E9",
		Authorizations: []slackEventAuthorization{{
			EnterpriseID:        "E9",
			TeamID:              "T1",
			IsEnterpriseInstall: false,
		}},
		Event: slackInnerEvent{Channel: "C1", TS: "100.1"},
	}
	if agentEventPartition(gridTeam) != "T1" {
		t.Errorf("grid workspace install partition should be team id")
	}
	gridOrg := &slackEventEnvelope{
		TeamID:       "T1",
		EnterpriseID: "E9",
		Authorizations: []slackEventAuthorization{{
			EnterpriseID:        "E9",
			IsEnterpriseInstall: true,
		}},
		Event: slackInnerEvent{Channel: "C1", TS: "100.1"},
	}
	if agentEventPartition(gridOrg) != "E9" {
		t.Errorf("grid org install partition should be enterprise id")
	}
	partialGridOrg := &slackEventEnvelope{
		TeamID:       "T1",
		EnterpriseID: "E9",
		Authorizations: []slackEventAuthorization{{
			IsEnterpriseInstall: true,
		}},
		Event: slackInnerEvent{Channel: "C1", TS: "100.1"},
	}
	if agentEventPartition(partialGridOrg) != "E9" {
		t.Errorf("partial grid org install partition should fall back to envelope enterprise id")
	}
	legacyGrid := &slackEventEnvelope{TeamID: "T1", EnterpriseID: "E9", Event: slackInnerEvent{Channel: "C1", TS: "100.1"}}
	if agentEventPartition(legacyGrid) != "T1" {
		t.Errorf("legacy grid payload should use team-id fallback to match lifecycle purge")
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
	// standard Markdown (see TestDeliverAgentResult_RoutesByDialect), not through here
	// — so a non-proposal result reaching this function renders the error reply.
	if got := agentReplyText(&agent.Result{Reply: "hello"}); got != agentErrorReply {
		t.Errorf("non-proposal result renders the error fallback (answers go via standard Markdown), got %q", got)
	}
	prop := agentReplyText(&agent.Result{Proposal: &agent.Proposal{Summary: "Protect $x."}})
	if !strings.Contains(prop, "isn't enabled yet") || !strings.Contains(prop, "Protect $x.") {
		t.Errorf("proposal preview = %q", prop)
	}
	if !strings.HasSuffix(prop, agentLLMReplyDisclaimer) {
		t.Errorf("LLM-distilled proposal preview must carry the disclaimer, got %q", prop)
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
	return agent.Response{Text: f.reply, StopReason: testAgentStopEndTurn}, nil
}

type scriptedHandlerAgentLLM struct {
	responses []agent.Response
	captured  []*agent.Request
	calls     int
}

func (s *scriptedHandlerAgentLLM) Complete(_ context.Context, req *agent.Request) (agent.Response, error) {
	s.captured = append(s.captured, req)
	if s.calls >= len(s.responses) {
		return agent.Response{}, errors.New("scriptedHandlerAgentLLM: no more responses")
	}
	r := s.responses[s.calls]
	s.calls++
	return r, nil
}

// panicAgentLLM panics mid-turn to exercise processAgentEvent's panic safety-net.
type panicAgentLLM struct{}

func (panicAgentLLM) Complete(context.Context, *agent.Request) (agent.Response, error) {
	panic("boom in the model call")
}

// memAgentDDB is a minimal in-memory DynamoDBClient for AgentStore: GetItem,
// create-if-absent PutItem, Query, UpdateItem, and unconditional DeleteItem.
type memAgentDDB struct {
	mu        sync.Mutex
	items     map[string]map[string]ddbtypes.AttributeValue
	getErr    error // when set, GetItem fails
	updateErr error // when set, UpdateItem (turn-rate counter) fails
	getCalls  int
	putErr    error // when set, PutItem fails
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
	f.getCalls++
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
	if f.putErr != nil {
		return nil, f.putErr
	}
	k := memKey(in.Item)
	_, present := f.items[k]
	if cond := aws.ToString(in.ConditionExpression); cond != "" && present {
		return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("conditional check failed")}
	}
	f.items[k] = in.Item
	return &dynamodb.PutItemOutput{}, nil
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

func (f *memAgentDDB) DeleteItem(_ context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, memKey(in.Key))
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
	// markdown records whether the reply arrived on the standard-Markdown seam
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

type capturedEphemeral struct {
	channel, threadTS, userID, text string
}

// capturingPostEphemeral returns a PostEphemeralFunc that records every ephemeral, plus
// the slice + mutex to read them after the async workers drain.
func capturingPostEphemeral() (PostEphemeralFunc, *[]capturedEphemeral, *sync.Mutex) {
	var mu sync.Mutex
	var posts []capturedEphemeral
	fn := func(_ context.Context, _, _, channel, threadTS, userID, text string) error {
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, capturedEphemeral{channel: channel, threadTS: threadTS, userID: userID, text: text})
		return nil
	}
	return fn, &posts, &mu
}

// capturingPostMarkdownMessage records standard-Markdown replies into the SAME slice
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

func threadBroadcastBody(eventID, ts, threadTS string) string {
	return `{"type":"event_callback","team_id":"T1","api_app_id":"A1","event_id":"` + eventID + `",` +
		`"event":{"type":"message","subtype":"` + slackMessageSubtypeThreadBroadcast + `",` +
		`"channel_type":"channel","user":"U2","channel":"C1","ts":"` + ts + `",` +
		`"thread_ts":"` + threadTS + `","text":"more please"}}`
}

func TestHandleEvent_AgentReplies(t *testing.T) {
	h, posts, mu := newAgentEventHandler(t, testAgentReachStagingReply)
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
	if got.channel != "C1" || got.threadTS != "100.1" || got.text != agentLLMReplyWithDisclaimer(testAgentReachStagingReply) {
		t.Fatalf("reply = %+v", got)
	}
}

func TestHandleEvent_AgentEchoedResourceDescriptionEscapesSlackControls(t *testing.T) {
	const (
		resourceID  = "r_agent_inert"
		description = "Deploy room <!channel> and <@U12345678>"
	)
	names := defaultTestTableNames()
	adminStore := newStoreFromFake(t, newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{
		names.channelPolicy: {
			seedChannelPolicySet("T1", "C1", "deploy", []string{resourceID}),
		},
	}), names, nil)
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != testResourcesPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		respondQURLEnvelope(t, w, []map[string]any{{
			testKeyResourceID:  resourceID,
			testKeyDescription: description,
			testKeyType:        client.ResourceTypeURL,
		}})
	}))
	t.Cleanup(qurlSrv.Close)

	llm := &scriptedHandlerAgentLLM{responses: []agent.Response{
		{
			ToolCalls:  []agent.ToolCall{{ID: "tool_list", Name: "list_resources", Input: json.RawMessage(`{}`)}},
			StopReason: testAgentStopToolUse,
		},
		{
			Text:       "I found " + description,
			StopReason: testAgentStopEndTurn,
		},
	}}
	t.Setenv("QURL_API_KEY", "test-key")
	agentStore := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	textPost, posts, mu := capturingPostMessage()
	mdPost := capturingPostMarkdownMessage(posts, mu)
	h := NewHandler(Config{
		AgentLLM:            llm,
		AgentStore:          agentStore,
		AdminStore:          adminStore,
		AuthProvider:        &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		NewClient:           func(apiKey string) *client.Client { return client.New(qurlSrv.URL, apiKey, client.WithRetry(0)) },
		PostMessage:         textPost,
		PostMarkdownMessage: mdPost,
		AgentDefaultEnabled: true,
	})
	t.Cleanup(h.Wait)

	w := httptest.NewRecorder()
	h.handleEvent(w, []byte(appMentionBody("EvDescriptionControls")))
	if w.Code != 200 {
		t.Fatalf("ack code = %d", w.Code)
	}
	h.Wait()

	if len(llm.captured) != 2 {
		t.Fatalf("expected list_resources then final answer calls, got %d", len(llm.captured))
	}
	var sawRawToolResult bool
	for _, m := range llm.captured[1].Messages {
		for _, tr := range m.ToolResults {
			if strings.Contains(tr.Content, description) {
				sawRawToolResult = true
			}
		}
	}
	if !sawRawToolResult {
		t.Fatalf("model did not receive the raw resource description in tool results: %+v", llm.captured[1].Messages)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 {
		t.Fatalf("expected one reply, got %d: %+v", len(*posts), *posts)
	}
	got := (*posts)[0]
	if !got.markdown {
		t.Fatalf("free-text answer should use the standard-Markdown seam, got %+v", got)
	}
	want := agentLLMReplyWithDisclaimer(`I found Deploy room \<!channel> and \<@U12345678>`)
	if got.text != want {
		t.Fatalf("reply = %q, want escaped visible controls %q", got.text, want)
	}
}

func TestHandleEvent_AgentRepliesToThreadBroadcastFollowup(t *testing.T) {
	mem := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: mem, TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:              fakeAgentLLM{reply: "still here"},
		AgentStore:            store,
		PostMessage:           post,
		AgentChannelFollowups: true,
		AgentDefaultEnabled:   true,
		AgentThreadHistory: func(context.Context, string, string, string, string, string) ([]AgentThreadMessage, error) {
			return []AgentThreadMessage{
				{UserID: "U1", Text: "question", TS: agentPoolTestThreadTS},
				{AppID: "A1", Text: "answer", TS: "100.1"},
			}, nil
		},
	})
	t.Cleanup(h.Wait)

	w := httptest.NewRecorder()
	h.handleEvent(w, []byte(threadBroadcastBody("EvBroadcast", "101.0", agentPoolTestThreadTS)))
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
	if got.channel != "C1" || got.threadTS != agentPoolTestThreadTS || got.text != agentLLMReplyWithDisclaimer("still here") {
		t.Fatalf("thread_broadcast follow-up reply = %+v", got)
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

func TestHandleEvent_HistoryFailurePostsError(t *testing.T) {
	// conversations.replies fails after the dedupe marker is committed: the user
	// must get an error reply, not silence (Slack won't retry — we acked 200).
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            fakeAgentLLM{reply: "unused"},
		AgentStore:          store,
		PostMessage:         post,
		AgentDefaultEnabled: true,
		AgentThreadHistory: func(context.Context, string, string, string, string, string) ([]AgentThreadMessage, error) {
			return nil, errors.New("Slack read down")
		},
	})
	t.Cleanup(h.Wait)
	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvLF")))
	h.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentErrorReply {
		t.Fatalf("history failure should post one error reply, got %+v", *posts)
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
		{"turn succeeded", fakeAgentLLM{reply: testAgentReachStagingReply}, agentLLMReplyWithDisclaimer(testAgentReachStagingReply)},
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

func TestProcessAgentEvent_RejectsExplicitNonHTTPSProtectURLBeforeLLM(t *testing.T) {
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            panicAgentLLM{},
		AgentStore:          store,
		PostMessage:         post,
		AgentDefaultEnabled: true,
	})

	h.processAgentEvent(context.Background(), slog.Default(),
		env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> Protect javascript:alert(1) as $bad."))

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentInvalidProtectURLReply {
		t.Fatalf("invalid URL should be rejected once before the LLM runs, got %+v", *posts)
	}
	if strings.Contains((*posts)[0].text, "javascript:") {
		t.Fatalf("invalid URL reply must not echo the attacker-controlled target: %q", (*posts)[0].text)
	}
}

func TestProcessAgentEvent_AllowsHTTPSProtectURLThroughToLLM(t *testing.T) {
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            fakeAgentLLM{reply: testAgentStillWorksReply},
		AgentStore:          store,
		PostMessage:         post,
		AgentDefaultEnabled: true,
	})

	h.processAgentEvent(context.Background(), slog.Default(),
		env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> Protect https://example.com as $docs."))

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentLLMReplyWithDisclaimer(testAgentStillWorksReply) {
		t.Fatalf("HTTPS protect request should follow the normal agent path, got %+v", *posts)
	}
}

func TestProcessAgentEvent_RejectsExplicitInvalidAliasBeforeLLM(t *testing.T) {
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            panicAgentLLM{},
		AgentStore:          store,
		PostMessage:         post,
		AgentDefaultEnabled: true,
	})

	h.processAgentEvent(context.Background(), slog.Default(),
		env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> Set alias $Prod_Admin!!! to $staging-api."))

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentInvalidAliasReply {
		t.Fatalf("invalid alias should be rejected once before the LLM runs, got %+v", *posts)
	}
	if strings.Contains((*posts)[0].text, "Prod_Admin") {
		t.Fatalf("invalid alias reply must not echo the attacker-controlled alias: %q", (*posts)[0].text)
	}
}

func TestProcessAgentEvent_AllowsValidAliasThroughToLLM(t *testing.T) {
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            fakeAgentLLM{reply: testAgentStillWorksReply},
		AgentStore:          store,
		PostMessage:         post,
		AgentDefaultEnabled: true,
	})

	h.processAgentEvent(context.Background(), slog.Default(),
		env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> Set alias $prod-admin to $staging-api."))

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentLLMReplyWithDisclaimer(testAgentStillWorksReply) {
		t.Fatalf("valid alias request should follow the normal agent path, got %+v", *posts)
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

func TestLoadAgentThreadHistory_ReconstructsOnlyCompletedQURLExchanges(t *testing.T) {
	h := NewHandler(Config{
		AgentThreadHistory: func(_ context.Context, teamID, enterpriseID, channelID, threadTS, oldestTS string) ([]AgentThreadMessage, error) {
			if teamID != "T1" || enterpriseID != "" || channelID != "C1" || threadTS != agentPoolTestThreadTS || oldestTS != "70.000000" {
				t.Fatalf("history request = team %q enterprise %q channel %q thread %q oldest %q", teamID, enterpriseID, channelID, threadTS, oldestTS)
			}
			return []AgentThreadMessage{
				{UserID: "U1", Text: "<@U12345678> first question", TS: agentPoolTestThreadTS},
				{AppID: "A1", BotID: "B1", UserID: "UQURL", Text: "first answer", TS: "100.1"},
				{BotID: "BOTHER", UserID: "UOTHER", Text: "other bot", TS: "100.2"},
				{UserID: "U1", Text: "follow-up", TS: "100.3"},
				{AppID: "A1", BotID: "B1", UserID: "UQURL", Text: "second answer", TS: "100.4"},
				{UserID: "U1", Text: "current inbound turn", TS: "1870.5"},
			}, nil
		},
	})
	e := env(slackEventTypeMessage, "channel", "U1", "", "", "current inbound turn")
	e.APIAppID = "A1"
	e.Authorizations = []slackEventAuthorization{{UserID: "UQURL"}}
	e.Event.ThreadTS = agentPoolTestThreadTS
	e.Event.TS = "1870.5"

	history, joined, err := h.loadAgentThreadHistory(context.Background(), e)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	if !joined {
		t.Fatal("qURL app response should mark the thread joined")
	}
	want := []agent.Message{
		{Role: "user", Text: "first question"},
		{Role: "assistant", Text: "first answer"},
		{Role: "user", Text: "follow-up"},
		{Role: "assistant", Text: "second answer"},
	}
	if !reflect.DeepEqual(history, want) {
		t.Fatalf("history = %#v, want %#v", history, want)
	}
}

func TestLoadAgentThreadHistory_DropsIncompleteTail(t *testing.T) {
	h := NewHandler(Config{
		AgentThreadHistory: func(context.Context, string, string, string, string, string) ([]AgentThreadMessage, error) {
			return []AgentThreadMessage{
				{UserID: "U1", Text: "first", TS: agentPoolTestThreadTS},
				{AppID: "A1", Text: "answer", TS: "100.1"},
				{UserID: "U1", Text: "unanswered", TS: "100.2"},
			}, nil
		},
	})
	e := env(slackEventTypeMessage, "channel", "U1", "", "", "current")
	e.APIAppID = "A1"
	e.Event.ThreadTS = agentPoolTestThreadTS
	e.Event.TS = "100.3"

	history, joined, err := h.loadAgentThreadHistory(context.Background(), e)
	if err != nil || !joined {
		t.Fatalf("load history: joined=%v err=%v", joined, err)
	}
	want := []agent.Message{{Role: "user", Text: "first"}, {Role: "assistant", Text: "answer"}}
	if !reflect.DeepEqual(history, want) {
		t.Fatalf("history = %#v, want %#v", history, want)
	}
}

func TestLoadAgentThreadHistory_BlockOnlyOwnReplyMarksThreadJoined(t *testing.T) {
	h := NewHandler(Config{
		AgentThreadHistory: func(context.Context, string, string, string, string, string) ([]AgentThreadMessage, error) {
			return []AgentThreadMessage{{AppID: "A1", TS: "100.1"}}, nil
		},
	})
	e := env(slackEventTypeMessage, "channel", "U1", "", "", "current")
	e.APIAppID = "A1"
	e.Event.ThreadTS = agentPoolTestThreadTS
	e.Event.TS = "100.2"

	history, joined, err := h.loadAgentThreadHistory(context.Background(), e)
	if err != nil || !joined || len(history) != 0 {
		t.Fatalf("block-only qURL reply: history=%#v joined=%v err=%v", history, joined, err)
	}
}

func TestAgentHistoryOldestTS(t *testing.T) {
	if got := agentHistoryOldestTS("1800.123456"); got != "0.000000" {
		t.Fatalf("oldest = %q, want 0.000000", got)
	}
	if got := agentHistoryOldestTS("3600.123456"); got != "1800.000000" {
		t.Fatalf("oldest = %q, want 1800.000000", got)
	}
	before := time.Now().Add(-agentHistoryWindow).Unix()
	got, err := strconv.ParseInt(strings.TrimSuffix(agentHistoryOldestTS("invalid"), ".000000"), 10, 64)
	after := time.Now().Add(-agentHistoryWindow).Unix()
	if err != nil || got < before || got > after {
		t.Fatalf("invalid oldest = %d err=%v, want current time minus window in [%d,%d]", got, err, before, after)
	}
}

func TestProcessAgentEvent_DoesNotPersistSlackTranscript(t *testing.T) {
	fake := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: fake, TableName: "agent_state"}
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            fakeAgentLLM{reply: "done"},
		AgentStore:          store,
		PostMessage:         post,
		AgentDefaultEnabled: true,
		AgentThreadHistory: func(context.Context, string, string, string, string, string) ([]AgentThreadMessage, error) {
			return nil, nil
		},
	})

	h.processAgentEvent(context.Background(), slog.Default(),
		env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi"))

	for key, item := range fake.items {
		if strings.Contains(key, "|conv#") {
			t.Fatalf("agent turn persisted Slack transcript at %q: %#v", key, item)
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
	// The agent's free-text answer delivers via the standard-Markdown seam (parity
	// with the streaming pane) after masked-link hardening; a proposal preview stays
	// on the escaped mrkdwn text seam. The confirm card flow is OFF here (no
	// PostMessageBlocks), so a proposal falls through to the text preview rather
	// than a card.
	textPost, posts, mu := capturingPostMessage()
	mdPost := capturingPostMarkdownMessage(posts, mu)
	h := NewHandler(Config{PostMessage: textPost, PostMarkdownMessage: mdPost})
	e := env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi")

	h.deliverAgentResult(slog.Default(), e, "100.1", &agent.Result{Reply: "Use **bold** and [click](https://evil.example)"})
	h.deliverAgentResult(slog.Default(), e, "100.1", &agent.Result{Proposal: &agent.Proposal{Summary: "Protect $x."}})

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 2 {
		t.Fatalf("want 2 posts, got %d: %+v", len(*posts), *posts)
	}
	// Free-text answer: standard-Markdown seam, with masked links neutralized before
	// Slack renders the Markdown.
	if !(*posts)[0].markdown {
		t.Errorf("free-text answer should post on the standard-Markdown seam, got mrkdwn: %+v", (*posts)[0])
	}
	wantReply := agentLLMReplyWithDisclaimer("Use **bold** and click (https://evil.example)")
	if (*posts)[0].text != wantReply {
		t.Errorf("free-text answer body = %q, want masked link revealed", (*posts)[0].text)
	}
	// Proposal preview: escaped mrkdwn text seam, never standard Markdown
	// (injection defense).
	if (*posts)[1].markdown {
		t.Errorf("proposal preview should post on the mrkdwn text seam, got standard Markdown: %+v", (*posts)[1])
	}
	if !strings.HasPrefix((*posts)[1].text, agentProposalPreviewPrefix) {
		t.Errorf("proposal preview = %q, want the preview prefix", (*posts)[1].text)
	}
	if !strings.HasSuffix((*posts)[1].text, agentLLMReplyDisclaimer) {
		t.Errorf("proposal preview = %q, want the generated-content disclaimer", (*posts)[1].text)
	}
}

func TestAgentLLMReplyDisclaimer_IsMarkdownHardeningInvariant(t *testing.T) {
	if got := hardenAgentMarkdown(agentLLMReplyDisclaimer); got != agentLLMReplyDisclaimer {
		t.Fatalf("one-shot hardener changed the trusted footer: %q", got)
	}
	if got := hardenAgentMarkdownForStreamReconcile(agentLLMReplyDisclaimer); got != agentLLMReplyDisclaimer {
		t.Fatalf("stream-reconcile hardener changed the trusted footer: %q", got)
	}
}

func TestDeliverAgentResult_MalformedMarkdownCannotAbsorbDisclaimer(t *testing.T) {
	textPost, posts, mu := capturingPostMessage()
	mdPost := capturingPostMarkdownMessage(posts, mu)
	h := NewHandler(Config{PostMessage: textPost, PostMarkdownMessage: mdPost})
	e := env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi")
	reply := "An unclosed `code span"

	h.deliverAgentResult(slog.Default(), e, "100.1", &agent.Result{Reply: reply})

	mu.Lock()
	defer mu.Unlock()
	want := agentLLMReplyWithDisclaimer(hardenAgentMarkdown(reply))
	if len(*posts) != 1 || (*posts)[0].text != want {
		t.Fatalf("want malformed reply hardened before the intact footer, got %+v", *posts)
	}
}

func TestDeliverAgentResult_MarkdownSeamFallsBackToText(t *testing.T) {
	// With the markdown seam unwired, the free-text answer still delivers — on the
	// mrkdwn text seam (degraded rendering), not dropped. Masked links are still
	// neutralized before the fallback.
	textPost, posts, mu := capturingPostMessage()
	h := NewHandler(Config{PostMessage: textPost})
	e := env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi")

	h.deliverAgentResult(slog.Default(), e, "100.1", &agent.Result{Reply: "plain [answer](https://evil.example) for <@U12345678> <!channel>"})

	mu.Lock()
	defer mu.Unlock()
	want := agentLLMReplyWithDisclaimer(`plain answer (https://evil.example) for \<@U12345678> \<!channel>`)
	if len(*posts) != 1 || (*posts)[0].text != want {
		t.Fatalf("want the answer delivered via the text seam, got %+v", *posts)
	}
}
