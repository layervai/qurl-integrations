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
	"github.com/layervai/qurl-integrations/shared/observability"
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
	// chReplyIn is chReply for a non-"channel" channel_type — a private channel or a
	// group DM, both of which take the same arm of the filter.
	chReplyIn := func(channelType, text, threadTS string) *slackEventEnvelope {
		e := env(slackEventTypeMessage, channelType, "U2", "", "", text)
		e.Event.ThreadTS = threadTS
		return e
	}
	chReplySubtype := func(text, threadTS, subtype string) *slackEventEnvelope {
		e := chReply(text, threadTS)
		e.Event.Subtype = subtype
		return e
	}
	// mentionInThread is the recovery route the limitation reply names: an @mention
	// posted inside a channel thread. It arrives as its own event type, so it never
	// meets the channel branch's upload drop.
	mentionInThread := func(text, threadTS string) *slackEventEnvelope {
		e := env(slackEventTypeAppMention, "channel", "U2", "", "", text)
		e.Event.ThreadTS = threadTS
		return e
	}
	withFile := func(e *slackEventEnvelope) *slackEventEnvelope {
		e.Event.Files = filesFromJSON(t, `[{"id":"F1"}]`)
		return e
	}
	// withRawFiles attaches a files value straight from the wire, for shapes the
	// decoder cannot count. It goes through the real decoder, so `{}` lands as
	// present-but-uncountable rather than hand-built struct state.
	withRawFiles := func(e *slackEventEnvelope, raw string) *slackEventEnvelope {
		e.Event.Files = filesFromJSON(t, raw)
		return e
	}
	// withBot / withApp / withoutUser restamp an otherwise-admissible event as one
	// the author guard rejects, so the rows below can pin that the guard runs BEFORE
	// the channel-upload branch — i.e. none of the three shapes reaches the demand
	// log.
	withBot := func(e *slackEventEnvelope) *slackEventEnvelope {
		e.Event.BotID = "B9"
		return e
	}
	withApp := func(e *slackEventEnvelope) *slackEventEnvelope {
		e.Event.AppID = "A1"
		return e
	}
	withoutUser := func(e *slackEventEnvelope) *slackEventEnvelope {
		e.Event.User = ""
		return e
	}
	withBotAuthorization := func(e *slackEventEnvelope, userID string, isBot bool) *slackEventEnvelope {
		e.Authorizations = []slackEventAuthorization{{UserID: userID, IsBot: isBot}}
		return e
	}
	// wantDrop is the refusal REASON, checked on every row including admitted ones
	// (where agentDropSilent is what an admitted event carries). It is what the caller
	// logs off, so a row that stops reporting agentDropChannelUpload is an upload that
	// silently stops counting as demand — invisible in `want` alone, since both
	// reasons produce the same false.
	tests := []struct {
		name      string
		env       *slackEventEnvelope
		followups bool
		want      bool
		wantDrop  agentDispatchDrop
	}{
		// @mentions and DMs are deliberate addresses — admitted regardless of the flag.
		{"app_mention human", env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi"), false, true, agentDropSilent},
		{"app_mention still works with followups on", env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678> hi"), true, true, agentDropSilent},
		{"dm human", env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "hi"), false, true, agentDropSilent},
		{"bot message ignored", env(slackEventTypeAppMention, "channel", "U2", "B9", "", "<@U12345678> hi"), false, false, agentDropSilent},
		{"subtype (edit/system) ignored", env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "message_changed", "hi"), false, false, agentDropSilent},
		{"authorless ignored", env(slackEventTypeAppMention, "channel", "", "", "", "<@U12345678> hi"), false, false, agentDropSilent},
		// The authorless guard runs before the type/subtype switch, so it covers an
		// upload too. Load-bearing for agentEventMediaNoticeKey: an empty user would
		// collapse the latch key to "channel:" — one shared bucket that silences every
		// member after the channel's first upload.
		{"authorless upload ignored", withFile(env(slackEventTypeMessage, slackChannelTypeIM, "", "", slackMessageSubtypeFileShare, "")), false, false, agentDropSilent},
		{"mention with empty text ignored", env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678>   "), false, false, agentDropSilent},
		{"file-only mention admitted for limitation reply", withFile(env(slackEventTypeAppMention, "channel", "U2", "", "", "<@U12345678>")), false, true, agentDropSilent},
		{"file-only dm admitted for limitation reply", withFile(env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "")), false, true, agentDropSilent},
		// file_share is admissible now, so the ONLY thing stopping a bot's upload from
		// drawing a reply (and looping) is the author/bot guard ahead of the subtype
		// switch. That guard is not in this PR's diff, which is exactly why it is
		// pinned here.
		{"bot file upload ignored (author guard, not the subtype filter)", withFile(env(slackEventTypeMessage, slackChannelTypeIM, "U2", "B1", slackMessageSubtypeFileShare, "")), false, false, agentDropSilent},
		{"authorless file upload ignored", withFile(env(slackEventTypeMessage, slackChannelTypeIM, "", "", slackMessageSubtypeFileShare, "")), false, false, agentDropSilent},
		{"me_message dm ignored (named in the policy doc, not admitted)", env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "me_message", "hi"), false, false, agentDropSilent},
		{"file_share dm admitted for limitation reply", withFile(env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", slackMessageSubtypeFileShare, "")), false, true, agentDropSilent},
		{"file_share dm without files still admitted (subtype is proof)", env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", slackMessageSubtypeFileShare, ""), false, true, agentDropSilent},
		{"file_share subtype on a mention admitted", withFile(env(slackEventTypeAppMention, "channel", "U2", "", slackMessageSubtypeFileShare, "<@U12345678>")), false, true, agentDropSilent},
		{"non-upload subtype on a mention ignored", env(slackEventTypeAppMention, "channel", "U2", "", "message_changed", "<@U12345678> hi"), false, false, agentDropSilent},
		{"other event type ignored", env("reaction_added", "channel", "U2", "", "", "x"), false, false, agentDropSilent},

		// Channel follow-ups: a TEXT thread reply is admitted ONLY when the flag is on;
		// a top-level channel message is never admitted (no un-addressed chatter).
		{"channel thread reply, followups off", chReply("hi", agentPoolTestThreadTS), false, false, agentDropSilent},
		{"channel thread reply, followups on", chReply("hi", agentPoolTestThreadTS), true, true, agentDropSilent},
		{"top-level channel message, followups off", chReply("hi", ""), false, false, agentDropSilent},
		{"top-level channel message, followups on", chReply("hi", ""), true, false, agentDropSilent},
		{"top-level channel file ignored", withFile(chReply("", "")), true, false, agentDropChannelUpload},
		{"channel thread reply empty text, followups on", chReply("   ", agentPoolTestThreadTS), true, false, agentDropSilent},

		// Channel uploads: never admitted, on any channel path. This is what keeps a
		// thread upload off followupGateSem and out of conversations.replies, so the
		// rows below are the whole cost fix — a regression here is silent, because the
		// admitted event still ends in the same limitation reply.
		{"channel thread file reply dropped before the gate", withFile(chReply("", agentPoolTestThreadTS)), true, false, agentDropChannelUpload},
		{"file_share channel thread dropped before the gate", withFile(chReplySubtype("", agentPoolTestThreadTS, slackMessageSubtypeFileShare)), true, false, agentDropChannelUpload},
		{"file_share channel thread reply without files dropped (subtype is proof)", chReplySubtype("", agentPoolTestThreadTS, slackMessageSubtypeFileShare), true, false, agentDropChannelUpload},
		{"file_share top-level channel file ignored", withFile(chReplySubtype("", "", slackMessageSubtypeFileShare)), true, false, agentDropChannelUpload},
		// A caption does not buy the message back in. The turn is refused wholesale
		// either way (see processAgentEventWithAdmission), so the text was never the
		// reason it would have been answered.
		{"captioned channel thread file reply dropped", withFile(chReply("more please", agentPoolTestThreadTS)), true, false, agentDropChannelUpload},
		{"thread_broadcast channel thread file reply dropped", withFile(chReplySubtype("look", agentPoolTestThreadTS, slackMessageSubtypeThreadBroadcast)), true, false, agentDropChannelUpload},
		// Captioned AND file_share with no files array. The row above it proves the
		// subtype is honored when the text is empty; this one proves it independently of
		// the empty-text guard at the end of the function, which cannot reach a message
		// that HAS text.
		{"captioned file_share channel thread reply without files dropped", chReplySubtype("here you go", agentPoolTestThreadTS, slackMessageSubtypeFileShare), true, false, agentDropChannelUpload},
		// A files value the decoder cannot count reads as present, so this pure-TEXT
		// follow-up is refused as an upload. That is the intended fail-toward-refusal
		// posture (see slackEventFiles.UnmarshalJSON). The member still gets no reply,
		// so the reason is what carries the "the agent refused my message" report out —
		// this row is the one where the refusal may be WRONG, and agentDropSilent here
		// would leave the reporting member and on-call both with nothing.
		{"channel thread text reply with an uncountable files value dropped", withRawFiles(chReply("and revoke it too", agentPoolTestThreadTS), `{}`), true, false, agentDropChannelUpload},
		// Regression pin on the flag-off path. `want` alone cannot distinguish an
		// unconditional drop from a flag-conditioned one — the gate below drops this row
		// either way when the flag is off — but the reason can, and it must not depend
		// on the flag: an upload is demand whether or not follow-ups have shipped, and
		// the flag is dark today, so a flag-conditioned reason would report zero for as
		// long as the signal is the only thing telling us to build file support.
		{"channel thread file reply dropped with followups off too (flag gate drops it anyway)", withFile(chReply("", agentPoolTestThreadTS)), false, false, agentDropChannelUpload},
		// The author guard runs ahead of the upload branch, so none of these becomes
		// demand: bot/app traffic is not a member asking for file support, and an
		// authorless upload has no user_id to join a complaint to.
		{"bot channel thread upload ignored, and not counted as demand", withBot(withFile(chReply("", agentPoolTestThreadTS))), true, false, agentDropSilent},
		{"app channel thread upload ignored, and not counted as demand", withApp(withFile(chReply("", agentPoolTestThreadTS))), true, false, agentDropSilent},
		{"authorless channel thread upload ignored, and not counted as demand", withoutUser(withFile(chReply("", agentPoolTestThreadTS))), true, false, agentDropSilent},
		// An edit is not an upload arriving. The subtype guard runs first, so a
		// message_changed carrying files stays out of the count — otherwise every edit
		// of an old upload would re-report it as new demand.
		{"edited channel message with files ignored, and not counted as demand", withFile(chReplySubtype("hi", agentPoolTestThreadTS, "message_changed")), true, false, agentDropSilent},
		// A message/file_share event that mentions this bot is the twin of an
		// app_mention event for the same upload. The admitted mention counts it, so the
		// twin must stay silent or the demand total counts one member action twice.
		{"mid-caption mentioned channel upload twin is not counted twice", withBotAuthorization(withFile(chReply("please review <@U12345678> this", agentPoolTestThreadTS)), "U12345678", true), true, false, agentDropSilent},
		{"upload mentioning another user is still counted", withBotAuthorization(withFile(chReply("<@U87654321>", agentPoolTestThreadTS)), "U12345678", true), true, false, agentDropChannelUpload},
		{"non-bot authorization does not suppress the upload count", withBotAuthorization(withFile(chReply("<@U12345678>", agentPoolTestThreadTS)), "U12345678", false), true, false, agentDropChannelUpload},
		// The arm is every non-IM channel_type, not just "channel": a private channel and
		// a group DM land here too. Called out because the "people talking to each other"
		// rationale is weakest in an mpim, where the bot was deliberately invited — worth
		// revisiting when the flag ships, but the cost argument is unchanged.
		{"private channel thread file reply dropped", withFile(chReplyIn(slackChannelTypeGroup, "", agentPoolTestThreadTS)), true, false, agentDropChannelUpload},
		{"group DM thread file reply dropped", withFile(chReplyIn(slackChannelTypeMPIM, "", agentPoolTestThreadTS)), true, false, agentDropChannelUpload},
		{"private channel thread text reply still admitted", chReplyIn(slackChannelTypeGroup, "hi", agentPoolTestThreadTS), true, true, agentDropSilent},
		// ...and the route out stays open: an @mention is a different event type, so it
		// carries an upload through from inside a channel thread. This is the only
		// channel upload the surface still answers, and the limitation text names it.
		{"file-only mention inside a channel thread still admitted", withFile(mentionInThread("<@U12345678>", agentPoolTestThreadTS)), true, true, agentDropSilent},

		{"thread_broadcast channel thread reply, followups off", chReplySubtype("hi", agentPoolTestThreadTS, slackMessageSubtypeThreadBroadcast), false, false, agentDropSilent},
		{"thread_broadcast channel thread reply, followups on", chReplySubtype("hi", agentPoolTestThreadTS, slackMessageSubtypeThreadBroadcast), true, true, agentDropSilent},
		{"thread_broadcast top-level channel message, followups on", chReplySubtype("hi", "", slackMessageSubtypeThreadBroadcast), true, false, agentDropSilent},
		{"thread_broadcast dm ignored", env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", slackMessageSubtypeThreadBroadcast, "hi"), true, false, agentDropSilent},
		{"other channel thread subtype ignored", chReplySubtype("hi", agentPoolTestThreadTS, "message_changed"), true, false, agentDropSilent},
	}
	dropNames := map[agentDispatchDrop]string{
		agentDropSilent:        "agentDropSilent",
		agentDropChannelUpload: "agentDropChannelUpload",
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, drop := shouldDispatchAgentEvent(tt.env, tt.followups)
			if got != tt.want {
				t.Fatalf("shouldDispatchAgentEvent = %v, want %v", got, tt.want)
			}
			if drop != tt.wantDrop {
				t.Fatalf("shouldDispatchAgentEvent drop = %v, want %v", dropNames[drop], dropNames[tt.wantDrop])
			}
		})
	}
}

// filesFromJSON decodes a raw Slack `files` value the way an inbound event would,
// so table rows exercise the real decoder instead of hand-built struct state.
func filesFromJSON(t *testing.T, raw string) slackEventFiles {
	t.Helper()
	var f slackEventFiles
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("decoding files %s: %v", raw, err)
	}
	return f
}

// TestSlackEventFilesDecodesTolerantly pins the property that keeps a shape
// surprise from silently eating the whole message. handleEvent treats ANY envelope
// decode error as "log at Debug, ack 200, dispatch nothing", so a files value that
// fails to decode would drop the event — including an ordinary text turn that
// merely carried the field. Every shape here must therefore decode without error,
// and anything unrecognized must still read as an attachment so detection fails
// toward refusing rather than toward answering past a file.
func TestSlackEventFilesDecodesTolerantly(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantPresent bool
		wantCount   int
	}{
		{"one file", `[{"id":"F1"}]`, true, 1},
		{"two files", `[{"id":"F1"},{"id":"F2"}]`, true, 2},
		{"empty array is not an upload", `[]`, false, 0},
		{"explicit null is not an upload", `null`, false, 0},
		{"non-object element still counts", `["F1"]`, true, 1},
		// The shapes below are not ones Slack sends today. They are here because the
		// cost of guessing wrong is the whole event vanishing, not a miscount.
		{"object instead of array is an uncountable upload", `{"id":"F1"}`, true, 0},
		{"string instead of array is an uncountable upload", `"F1"`, true, 0},
		{"number instead of array is an uncountable upload", `7`, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f slackEventFiles
			if err := json.Unmarshal([]byte(tt.raw), &f); err != nil {
				t.Fatalf("files %s must not fail the decode, got %v", tt.raw, err)
			}
			if f.present != tt.wantPresent || f.count != tt.wantCount {
				t.Fatalf("files %s decoded to present=%v count=%d, want present=%v count=%d",
					tt.raw, f.present, f.count, tt.wantPresent, tt.wantCount)
			}
		})
	}
}

// TestSlackEventFilesSurvivesEnvelopeDecode is the end-to-end half of
// TestSlackEventFilesDecodesTolerantly: it pins that a surprising files shape does
// not fail the ENCLOSING envelope decode, which is the failure that would make the
// message disappear. A plain []json.RawMessage field fails this test.
func TestSlackEventFilesSurvivesEnvelopeDecode(t *testing.T) {
	tests := []struct {
		name string
		// field is spliced into the event object, so the absent case can omit the key
		// entirely — which is a different path from an explicit null.
		field       string
		wantPresent bool
	}{
		{"object instead of array", `,"files":{"id":"F1"}`, true},
		{"string instead of array", `,"files":"F1"`, true},
		{"number instead of array", `,"files":7`, true},
		{"explicit null", `,"files":null`, false},
		{"empty array", `,"files":[]`, false},
		{"field absent entirely", ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := eventCallbackBody("EvShape", `{"type":"message","channel_type":"im","user":"U2","channel":"D1","ts":"500.1","text":"what can I access?"`+tt.field+`}`)
			var env slackEventEnvelope
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("files %q must not fail the envelope decode, got %v", tt.field, err)
			}
			if env.Event.Text != "what can I access?" || env.Event.User != "U2" {
				t.Fatalf("envelope fields lost alongside files %q: %+v", tt.field, env.Event)
			}
			// Surviving the decode is only half of it: an uncountable shape must still
			// read as an attachment, or the tolerance would turn into a silent
			// answer-past-the-file instead of a silent drop.
			if env.Event.Files.present != tt.wantPresent {
				t.Fatalf("files %q decoded to present=%v, want %v", tt.field, env.Event.Files.present, tt.wantPresent)
			}
		})
	}
}

// TestSlackEventFilesIsDecodeOnly pins that an event cannot be re-marshaled, and
// shows why that matters. count/present are unexported, so default marshaling emits
// `{}` — and `{}` is exactly the shape this type's UnmarshalJSON classifies as an
// uncountable attachment. A round trip would therefore turn every event, textual
// ones included, into a refused media turn. The guard makes that loud.
func TestSlackEventFilesIsDecodeOnly(t *testing.T) {
	var f slackEventFiles
	if err := json.Unmarshal([]byte(`{}`), &f); err != nil {
		t.Fatalf("decoding `{}`: %v", err)
	}
	if !f.present {
		t.Fatal("precondition: `{}` must read as an uncountable attachment for this hazard to exist")
	}
	if _, err := json.Marshal(slackInnerEvent{Text: "what can I access?"}); err == nil {
		t.Fatal("marshaling an inner event must fail: a round trip would refuse every turn")
	}
}

// TestAgentEventHasUpload pins upload detection on the event envelope, which is
// decided ahead of — and independently of — the text classifier below. The two
// signals back each other up, so each is covered on its own as well as together.
func TestAgentEventHasUpload(t *testing.T) {
	tests := []struct {
		name  string
		event slackInnerEvent
		want  bool
	}{
		{"files array alone", slackInnerEvent{Files: filesFromJSON(t, `[{"id":"F1"}]`)}, true},
		{"file_share subtype alone (files array absent)", slackInnerEvent{Subtype: slackMessageSubtypeFileShare}, true},
		{"both signals", slackInnerEvent{Subtype: slackMessageSubtypeFileShare, Files: filesFromJSON(t, `[{"id":"F1"}]`)}, true},
		// A long paste is converted by the Slack client into a snippet, so a purely
		// textual request arrives shaped exactly like an upload. It is refused: the
		// paste's text is in the file, not in this event, so answering the caption
		// would answer without the thing the caption refers to.
		{"long paste converted to a snippet", slackInnerEvent{Subtype: slackMessageSubtypeFileShare, Files: filesFromJSON(t, `[{"id":"F5","mode":"snippet","filetype":"text"}]`)}, true},
		{"files in a shape we could not count", slackInnerEvent{Files: filesFromJSON(t, `{"id":"F1"}`)}, true},
		{"empty files array is not an upload", slackInnerEvent{Files: filesFromJSON(t, `[]`)}, false},
		{"null files is not an upload", slackInnerEvent{Files: filesFromJSON(t, `null`)}, false},
		{"plain message is not an upload", slackInnerEvent{Text: "what can I reach?"}, false},
		{"a non-upload subtype is not an upload", slackInnerEvent{Subtype: "message_changed"}, false},
		// A canvas or file shared as a LINK carries neither signal: Slack sends an
		// unfurl, which slackInnerEvent does not decode. So the limitation never
		// fires for one, which is why agentUnsupportedMediaReply is scoped to
		// attachments rather than naming canvases outright.
		{"a linked canvas is not an upload", slackInnerEvent{Text: "what's in https://acme.slack.com/docs/T1/F2"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := tt.event
			if got := agentEventHasUpload(&event); got != tt.want {
				t.Fatalf("agentEventHasUpload = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSlackMessageHasUploadMatchesEventDetection pins the two upload classifiers
// against each other rather than restating expectations for one of them. The event
// path decides whether an upload's OWN turn is refused; SlackMessageHasUpload
// decides whether every LATER turn in that thread is told the attachment was there.
// A disagreement is not a miscount — it puts a caption back in front of the model
// stripped of the fact that it described a file.
func TestSlackMessageHasUploadMatchesEventDetection(t *testing.T) {
	shapes := []string{
		`[{"id":"F1"}]`, `[{"id":"F1"},{"id":"F2"}]`, `["F1"]`, `[]`, `null`,
		`{"id":"F1"}`, `"F1"`, `7`,
	}
	for _, raw := range shapes {
		for _, subtype := range []string{"", slackMessageSubtypeFileShare, "message_changed"} {
			// The event side decodes a whole envelope on purpose. Building it with
			// filesFromJSON would run the SAME top-level decode the seam uses, so the
			// comparison would be one code path against itself; the divergence worth
			// pinning is nested-struct-field decode vs top-level.
			body := eventCallbackBody("EvParity", `{"type":"message","channel_type":"im","user":"U2","channel":"D1","ts":"500.1","text":"x","subtype":"`+subtype+`","files":`+raw+`}`)
			var envelope slackEventEnvelope
			if err := json.Unmarshal([]byte(body), &envelope); err != nil {
				t.Fatalf("files %s must not fail the envelope decode: %v", raw, err)
			}
			want := agentEventHasUpload(&envelope.Event)
			if got := SlackMessageHasUpload(json.RawMessage(raw), subtype); got != want {
				t.Errorf("SlackMessageHasUpload(%s, %q) = %v, want %v to match the event path", raw, subtype, got, want)
			}
		}
	}
	// An absent files key has no event counterpart to compare against: the seam hands
	// over a nil RawMessage where the envelope decode never calls the Unmarshaler.
	if SlackMessageHasUpload(nil, "") {
		t.Error("a message with no files field and no subtype is not an upload")
	}
	if !SlackMessageHasUpload(nil, slackMessageSubtypeFileShare) {
		t.Error("file_share alone must still read as an upload, as it does on the event path")
	}
	// Routing through json.Unmarshal rather than calling the Unmarshaler directly is
	// what makes the byte-for-byte "null" check safe here too: the decoder strips the
	// padding either way. Called directly, a padded null would read as an attachment.
	if SlackMessageHasUpload(json.RawMessage(" null "), "") {
		t.Error("a padded null must still read as no attachment")
	}
	// No caller can produce this today — a RawMessage carries bytes the enclosing
	// decode already validated — but the branch is what makes the safe direction a
	// property of the code rather than of the comment above it. present=false is the
	// UNSAFE answer here: it replays a caption as ordinary text.
	if !SlackMessageHasUpload(json.RawMessage(`{"unterminated`), "") {
		t.Error("a value that cannot be decoded must read as an attachment we cannot count, not as no attachment")
	}
}

// TestAgentHistoryAttachmentNoteStatesTheBoundary pins the note's CONTENT, which
// every other test takes as a given: they build their expectation from the constant
// itself, so they pin the concatenation and not a word of what it says. Replacing
// the note with "[x]" — or with "[the attached file was read in full]", which says
// the opposite — passes all of them.
//
// The string is the mechanism here. It is the only thing standing between the model
// and reading "protect everything in this" as a complete instruction, so it gets the
// same treatment as agentUnsupportedMediaReply above: assert the load-bearing
// properties, not the prose.
func TestAgentHistoryAttachmentNoteStatesTheBoundary(t *testing.T) {
	note := agentHistoryAttachmentNote
	if !strings.HasPrefix(note, "[") || !strings.HasSuffix(note, "]") {
		t.Errorf("the note must be bracketed so it reads as an annotation rather than as the user's own prose; got %q", note)
	}
	// It has to say something arrived...
	if !strings.Contains(note, "attach") {
		t.Errorf("the note must name the thing that was attached, or it says nothing the model can act on; got %q", note)
	}
	// ...and, load-bearing, that its contents did NOT. A note that only says "there
	// was a file" leaves the model free to assume it read one.
	if !strings.Contains(note, "never reached you") {
		t.Errorf("the note must state that the contents did not arrive; got %q", note)
	}
	// Presence detection cannot tell a snippet from a PDF, and Slack turns a long
	// typed paste into a snippet — so a note that named only files would misdescribe
	// a user who typed a lot. This is the same scoping rule
	// TestUnsupportedMediaReplyOffersAReachableRoute enforces on the refusal.
	if !strings.Contains(note, "snippet") {
		t.Errorf("the note must cover the paste-converted-to-snippet case, which trips the same detection; got %q", note)
	}
	// "this message" would be wrong the moment appendVisibleAgentMessage merges the
	// annotated message with an adjacent one — the claim has to hold for the blob.
	if strings.Contains(note, "this message") {
		t.Errorf("the note must not say \"this message\": adjacent same-role messages merge into one turn; got %q", note)
	}
}

// TestSlackEventFilesNestedDecodeIsUnpadded pins the stdlib behavior the decoder's
// null check relies on: as a struct field, files arrives with the whitespace around
// it already stripped, so comparing the value against "null" byte-for-byte is safe.
// If encoding/json ever passed padding through, a null files field would read as an
// uncountable attachment and refuse an ordinary text turn.
func TestSlackEventFilesNestedDecodeIsUnpadded(t *testing.T) {
	for _, body := range []string{
		`{"files": null , "text":"x"}`,
		`{"files" :  null   ,"text":"x"}`,
		"{\"files\":\n\tnull\n\t,\"text\":\"x\"}",
	} {
		t.Run(body, func(t *testing.T) {
			var e slackInnerEvent
			if err := json.Unmarshal([]byte(body), &e); err != nil {
				t.Fatalf("decode failed: %v", err)
			}
			if e.Files.present {
				t.Fatalf("a null files field must not read as an attachment, got present=true")
			}
		})
	}
}

// TestAgentDeterministicReply pins the TEXT half of the switch: which wordings are
// answered without the model, and which fall through to it. Uploads are not in
// scope here by design — they are an envelope property, decided by the caller
// before this runs. That an upload BEATS every keyword below is pinned end-to-end
// by TestHandleEvent_UnsupportedMediaRepliesWithoutLLM's "upload captioned with a
// deterministic keyword" row, which exercises the real ordering rather than a
// reconstruction of it.
func TestAgentDeterministicReply(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string // "" means the turn should reach the model
	}{
		{"literal help", "help", agentHelpReply},
		{"help with different casing", "HELP", agentHelpReply},
		{"help with punctuation reaches the model", "help!", ""},
		{"explicit non-HTTPS protect URL", "protect http://example.com", agentInvalidProtectURLReply},
		{"explicit invalid set-alias", "set alias $Bad_Alias for $id", agentInvalidAliasReply},
		{"ordinary request reaches the model", "what can I reach?", ""},
		{"empty text reaches the model", "", ""},
		// The text half of the linked-canvas decision (envelope half in
		// TestAgentEventHasUpload). Matching Slack file permalinks here to close that
		// gap would refuse the whole turn — the upload branch returns before this
		// runs — so the second row, a legitimate propose_protect_url request against
		// a Slack-hosted https:// endpoint, would stop being answered too.
		{"a linked canvas reaches the model", "what's in https://acme.slack.com/docs/T1/F2", ""},
		{"protecting a Slack file URL reaches the model", "protect https://acme.slack.com/files/U1/F2/report.pdf as $report", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply, ok := agentDeterministicReply(tt.text)
			if tt.want == "" {
				if ok {
					t.Fatalf("want the turn to reach the model, got deterministic reply %q", reply)
				}
				return
			}
			if !ok || reply != tt.want {
				t.Fatalf("reply = %q ok = %v, want %q", reply, ok, tt.want)
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
	fileReply := reply("C9", "999.1")
	fileReply.Event.Files = filesFromJSON(t, `[{"id":"F1"}]`)
	if !gateDrop(fileReply) {
		t.Fatal("a file reply outside an agent thread must be dropped")
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
	// putErrSKPrefix scopes putErr to one item family, so a test can fail (say) the
	// media-notice latch while event dedupe still works. Empty means putErr applies
	// to every PutItem.
	putErrSKPrefix string
	// putSKs records the sk of every PutItem ATTEMPT, including ones the
	// create-if-absent condition rejects, so a test can assert which writes the
	// handler reached rather than only which ones stuck.
	putSKs []string
}

// putAttempts counts recorded PutItem attempts whose sk carries prefix.
func (f *memAgentDDB) putAttempts(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, sk := range f.putSKs {
		if strings.HasPrefix(sk, prefix) {
			n++
		}
	}
	return n
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
	// A missing/non-string sk is a caller bug, not a condition to model: surface it
	// as a readable error rather than panicking on a nil deref.
	sk, ok := in.Item["sk"].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("memAgentDDB.PutItem: item has no string sk: %v", in.Item)
	}
	f.putSKs = append(f.putSKs, sk.Value)
	if f.putErr != nil && strings.HasPrefix(sk.Value, f.putErrSKPrefix) {
		return nil, f.putErr
	}
	k := memKey(in.Item)
	existing, present := f.items[k]
	if cond := aws.ToString(in.ConditionExpression); cond != "" && present && !memCondWins(cond, existing, in.ExpressionAttributeValues) {
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

// memCondWins evaluates the conditions AgentStore emits against an item that is
// already present. Only putMarkerIfExpired's expiry clause can win there — the
// plain attribute_not_exists form never can. Modeled rather than stubbed: a fake
// that waved conditions through would make the media latch look reclaimable when
// it is not, and one that always failed them would hide the expiry branch.
func memCondWins(cond string, existing, values map[string]ddbtypes.AttributeValue) bool {
	if !strings.Contains(cond, "#ttl <= :now") {
		return false
	}
	// DynamoDB evaluates a comparison against a NONEXISTENT attribute as false, so
	// a marker carrying no ttl counts as live. Reading it as 0 (what a bare
	// memNumberValue would give) would invert that and let every write win.
	// "ttl" mirrors slackdata's unexported attrAgentTTL, which this package cannot
	// reference. If that constant's VALUE ever changes, this lookup silently misses
	// and the fake stops modeling the expiry branch — the latch tests would still
	// pass while no longer exercising reopen. Keep the two in step.
	if _, ok := existing["ttl"].(*ddbtypes.AttributeValueMemberN); !ok {
		return false
	}
	return memNumberValue(existing["ttl"]) <= memNumberValue(values[":now"])
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

// eventCallbackBody wraps a raw inner event in an event_callback envelope, for
// tests that need a shape the fixed-purpose builders above cannot express.
func eventCallbackBody(eventID, event string) string {
	return `{"type":"event_callback","team_id":"T1","api_app_id":"A1","event_id":"` + eventID + `","event":` + event + `}`
}

func botAuthorizedEventCallbackBody(eventID, event string) string {
	return `{"type":"event_callback","team_id":"T1","api_app_id":"A1","event_id":"` + eventID + `",` +
		`"authorizations":[{"user_id":"U12345678","is_bot":true}],"event":` + event + `}`
}

// TestUnsupportedMediaReplyOffersAReachableRoute pins the recovery route, not the
// prose. Presence detection refuses every upload and Slack turns a long paste
// into one, so a paste-shaped request is only recoverable somewhere other than
// this surface: copy that offered nothing but "send it as a message again" would
// loop the user back through the same conversion, with no way to be answered.
func TestUnsupportedMediaReplyOffersAReachableRoute(t *testing.T) {
	for _, want := range []string{"snippet", "`/qurl help`"} {
		if !strings.Contains(agentUnsupportedMediaReply, want) {
			t.Errorf("unsupported-media reply must mention %q so a converted paste has a route it can take; got %q", want, agentUnsupportedMediaReply)
		}
	}
}

// TestUnsupportedMediaReplyLeadsWithTheTextOnlyRule pins the promise to what
// agentEventHasUpload detects. A canvas shared as a LINK is never refused (see
// TestAgentDeterministicReply), so a reply that opens by listing refused media
// types teaches "canvases are refused" — a boundary this surface does not
// enforce, which is the broken promise the reply exists to avoid.
//
// The fix is order, not vocabulary: state the rule that actually holds (this
// surface reads a message's text) BEFORE naming any medium, so the takeaway
// generalizes to the linked shape instead of contradicting it. Both checks are
// positional so rewording stays free.
func TestUnsupportedMediaReplyLeadsWithTheTextOnlyRule(t *testing.T) {
	nouns := []string{"file", "image", "canvas"}
	firstNoun := len(agentUnsupportedMediaReply)
	for _, noun := range nouns {
		if i := strings.Index(agentUnsupportedMediaReply, noun); i >= 0 && i < firstNoun {
			firstNoun = i
		}
	}
	if firstNoun == len(agentUnsupportedMediaReply) {
		return // names no medium at all: nothing to over-promise.
	}
	rule := strings.Index(agentUnsupportedMediaReply, "text")
	if rule < 0 || rule > firstNoun {
		t.Errorf("unsupported-media reply must state the text-only rule before it names a medium, so a reader learns a boundary that also holds for a linked canvas; got %q", agentUnsupportedMediaReply)
	}
	if scope := strings.Index(agentUnsupportedMediaReply, "attach"); scope < 0 || scope > firstNoun {
		t.Errorf("unsupported-media reply must scope the media it names to attachments — presence detection refuses nothing else; got %q", agentUnsupportedMediaReply)
	}
}

func TestHandleEvent_UnsupportedMediaRepliesWithoutLLM(t *testing.T) {
	tests := []struct {
		name string
		body string
		// wantThread is where the reply must land: threaded under the upload itself,
		// never loose in the channel.
		wantThread string
	}{
		{
			name:       "file-only channel mention",
			body:       eventCallbackBody("EvFileOnly", `{"type":"app_mention","user":"U2","channel":"C1","ts":"400.1","text":"<@U12345678>","files":[{"id":"F1","mimetype":"image/png"}]}`),
			wantThread: "400.1",
		},
		{
			name:       "captioned DM file",
			body:       eventCallbackBody("EvFileCaption", `{"type":"message","subtype":"file_share","channel_type":"im","user":"U2","channel":"D1","ts":"400.2","text":"Please inspect this","files":[{"id":"F2","mimetype":"application/pdf"}]}`),
			wantThread: "400.2",
		},
		{
			// The media case runs ahead of the deterministic text short-circuits, so a
			// caption that reads as a keyword still never gets an answer that silently
			// ignores the attachment.
			name:       "upload captioned with a deterministic keyword",
			body:       eventCallbackBody("EvFileHelp", `{"type":"message","subtype":"file_share","channel_type":"im","user":"U2","channel":"D1","ts":"400.4","text":"help","files":[{"id":"F4"}]}`),
			wantThread: "400.4",
		},
		{
			// Slack converts a long paste into a snippet, so a request that the user
			// typed as text arrives as a captioned upload. The reply must still be the
			// limitation — and must name a route the user can actually take, since
			// retyping the paste reproduces the snippet.
			name:       "long paste converted to a snippet",
			body:       eventCallbackBody("EvSnippet", `{"type":"message","subtype":"file_share","channel_type":"im","user":"U2","channel":"D1","ts":"400.6","text":"protect all of these","files":[{"id":"F5","mode":"snippet","filetype":"text","name":"Untitled"}]}`),
			wantThread: "400.6",
		},
		{
			// A files value the decoder could not count still refuses. This is the shape
			// that would otherwise have taken the whole event down (see
			// TestSlackEventFilesSurvivesEnvelopeDecode) — here it reaches the reply.
			name:       "files in a shape we could not count",
			body:       eventCallbackBody("EvUncountable", `{"type":"message","subtype":"file_share","channel_type":"im","user":"U2","channel":"D1","ts":"400.7","text":"protect this","files":{"id":"F7"}}`),
			wantThread: "400.7",
		},
		{
			// A file_share whose files array never arrived is still an upload; the
			// subtype alone must keep it out of the silent-drop path.
			name:       "file_share with no files array",
			body:       eventCallbackBody("EvFileBare", `{"type":"message","subtype":"file_share","channel_type":"im","user":"U2","channel":"D1","ts":"400.5","text":""}`),
			wantThread: "400.5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
			post, posts, mu := capturingPostMessage()
			// Conversation history is read live from the Slack thread, so "kept out of
			// history" is pinned by never reaching the transcript seam at all.
			hist := &countingThreadHistory{}
			h := NewHandler(Config{
				AgentLLM: panicAgentLLM{}, AgentStore: store, PostMessage: post,
				AgentThreadHistory: hist.read, AgentDefaultEnabled: true,
			})
			t.Cleanup(h.Wait)

			h.handleEvent(httptest.NewRecorder(), []byte(tt.body))
			h.Wait()

			if calls := hist.callCount(); calls != 0 {
				t.Fatalf("unsupported media should not read conversation history, got %d lookups", calls)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(*posts) != 1 || (*posts)[0].text != agentUnsupportedMediaReply {
				t.Fatalf("unsupported media should post one deterministic reply without calling the LLM, got %+v", *posts)
			}
			if got := (*posts)[0].threadTS; got != tt.wantThread {
				t.Fatalf("reply threadTS = %q, want %q (the limitation must thread under the upload)", got, tt.wantThread)
			}
		})
	}
}

// TestHandleEvent_ChannelThreadUploadStaysSilent is the reply this surface gives up,
// stated as a test so it is a decision and not a regression. With channel follow-ups
// ON and a thread the agent HAS joined — the single case where an un-mentioned channel
// upload would have drawn the limitation — nothing is read, nothing is posted, and no
// marker is written. The member's route is the @mention the limitation text names, and
// TestHandleEvent_MentionedChannelUploadAnswersOnceViaTheMentionOnly holds that open.
//
// Paying for the reply instead would mean routing every thread upload through the
// admission gate, because "did we join this thread?" IS this history read. Slack stamps
// file_share on essentially every message-with-files, so that would let any member of
// any channel the bot is in spend a followupGateSem slot and a conversations.replies
// call with no @mention — and that pool's saturation path drops legitimate text
// follow-ups.
func TestHandleEvent_ChannelThreadUploadStaysSilent(t *testing.T) {
	mem := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: mem, TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	hist := &countingThreadHistory{}
	h := NewHandler(Config{
		AgentLLM: panicAgentLLM{}, AgentStore: store, PostMessage: post,
		AgentThreadHistory:    hist.read,
		AgentChannelFollowups: true, AgentDefaultEnabled: true,
	})
	t.Cleanup(h.Wait)

	// A TEXT thread reply in the same thread, on the same handler, is the positive
	// control. Every assertion below is otherwise a zero, and a test that only asserts
	// zeros passes for the wrong reason the moment the premise decays — a flipped
	// default, a new pre-dispatch gate, a Config field this literal forgets. The control
	// fails FIRST in all of those, so the uploads' silence stays attributable to the
	// upload guard rather than assumed. (Verified: without it, this test still passes
	// with AgentChannelFollowups or AgentDefaultEnabled set to false.)
	//
	// "help" is a deterministic reply, so the control never reaches panicAgentLLM.
	h.handleEvent(httptest.NewRecorder(), []byte(eventCallbackBody("EvChanControl", `{"type":"message","channel_type":"channel","user":"U2","channel":"C1","thread_ts":"`+agentPoolTestThreadTS+`","ts":"500.0","text":"help"}`)))
	// Bare upload, captioned upload, and a file_share whose files array never arrived —
	// all three are channel thread replies in the same agent thread, and none is
	// addressed to the agent.
	for _, body := range []string{
		eventCallbackBody("EvChanFile", `{"type":"message","subtype":"file_share","channel_type":"channel","user":"U2","channel":"C1","thread_ts":"`+agentPoolTestThreadTS+`","ts":"500.1","text":"","files":[{"id":"F1"}]}`),
		eventCallbackBody("EvChanFileCaption", `{"type":"message","channel_type":"channel","user":"U2","channel":"C1","thread_ts":"`+agentPoolTestThreadTS+`","ts":"500.2","text":"here you go","files":[{"id":"F2"}]}`),
		eventCallbackBody("EvChanFileBare", `{"type":"message","subtype":"file_share","channel_type":"channel","user":"U2","channel":"C1","thread_ts":"`+agentPoolTestThreadTS+`","ts":"500.3","text":""}`),
	} {
		h.handleEvent(httptest.NewRecorder(), []byte(body))
	}
	h.Wait()

	// One history read, one dedupe marker, one reply — all the control's. The three
	// uploads contribute nothing to any of the three counts.
	if calls := hist.callCount(); calls != 1 {
		t.Fatalf("history lookups = %d, want 1 (the control's; a channel upload reads none)", calls)
	}
	if got := mem.putAttempts("evt#"); got != 1 {
		t.Fatalf("dedupe attempts = %d, want 1 (the control's; a channel upload never reaches the marker)", got)
	}
	// The media latch is the sharpest of the three: it is written only by a turn that
	// reached the upload branch, so a nonzero count means an upload was answered.
	if got := mem.putAttempts("media#"); got != 0 {
		t.Fatalf("media latch attempts = %d, want 0 (no channel upload should reach the notice)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentHelpReply {
		t.Fatalf("only the control should be answered, got %+v", *posts)
	}
}

// mediaUploadBody builds one deliberate upload. Each call is a DISTINCT message —
// its own event id and its own ts — so every one clears event dedupe on its own.
// That is the burst shape: dedupe cannot cap it, which is why the notice latch has
// to.
func mediaUploadBody(eventID, channelType, channel, user, ts string) string {
	return eventCallbackBody(eventID, `{"type":"message","subtype":"file_share","channel_type":"`+channelType+
		`","user":"`+user+`","channel":"`+channel+`","ts":"`+ts+`","text":"","files":[{"id":"F`+ts+`"}]}`)
}

// newMediaNoticeHandler wires a handler whose only reachable reply is the media
// notice: panicAgentLLM fails the test loudly if a turn ever reaches the model.
func newMediaNoticeHandler(t *testing.T, mem *memAgentDDB) (*Handler, *[]capturedReply, *sync.Mutex) {
	t.Helper()
	return newMediaNoticeHandlerAt(t, mem, nil)
}

// newMediaNoticeHandlerAt is newMediaNoticeHandler with the store clock pinned, for
// the tests that step past the notice window. A nil now keeps the store default.
func newMediaNoticeHandlerAt(t *testing.T, mem *memAgentDDB, now func() time.Time) (*Handler, *[]capturedReply, *sync.Mutex) {
	t.Helper()
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM: panicAgentLLM{}, PostMessage: post, AgentDefaultEnabled: true,
		AgentStore: &slackdata.AgentStore{Client: mem, TableName: "agent_state", Now: now},
	})
	t.Cleanup(h.Wait)
	return h, posts, mu
}

// TestMediaMarkerShapeMatchesFake closes the one coupling memCondWins cannot
// express in code. It reads the literals "ttl" and "media#" because slackdata's
// attrAgentTTL and mediaNoticeSKPrefix are unexported and cross-package — and a
// change to either VALUE would silently stop the fake from modeling the expiry
// branch, leaving every latch test green while none of them still exercised
// reopen. Drive the real store once and assert the shape, so that drift fails
// loudly here instead of going quiet everywhere.
func TestMediaMarkerShapeMatchesFake(t *testing.T) {
	mem := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: mem, TableName: "agent_state"}
	if _, err := store.MarkMediaNoticeSent(context.Background(), "T1", "C1:U2"); err != nil {
		t.Fatalf("MarkMediaNoticeSent: %v", err)
	}

	mem.mu.Lock()
	defer mem.mu.Unlock()
	item, ok := mem.items["T1|media#C1:U2"]
	if !ok {
		keys := make([]string, 0, len(mem.items))
		for k := range mem.items {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf(`no marker at "T1|media#C1:U2" — the sk prefix memCondWins keys on has drifted. wrote: %v`, keys)
	}
	if _, ok := item["ttl"]; !ok {
		t.Fatal(`marker carries no "ttl" attribute — memCondWins reads that key, so the expiry branch is no longer modeled`)
	}
}

// TestHandleEvent_UnsupportedMediaNoticeCapsBurst is the point of the latch: one
// member dragging in a pile of files must cost one reply, not one per file.
//
// Every upload here is a top-level DM message, which is what a drag-and-drop burst
// actually produces — and top-level messages carry no thread_ts, so agentEventRootTS
// resolves the "thread" to each message's OWN ts. A latch keyed on the thread would
// therefore be unique per upload and cap nothing; this test fails against that key.
func TestHandleEvent_UnsupportedMediaNoticeCapsBurst(t *testing.T) {
	mem := newMemAgentDDB()
	h, posts, mu := newMediaNoticeHandler(t, mem)

	for i, ts := range []string{"700.1", "700.2", "700.3", "700.4", "700.5"} {
		fireTurn(t, h, mediaUploadBody("EvBurst"+strconv.Itoa(i), slackChannelTypeIM, "D1", "U2", ts))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 {
		t.Fatalf("a 5-file burst must draw ONE notice, got %d: %+v", len(*posts), *posts)
	}
	if (*posts)[0].text != agentUnsupportedMediaReply {
		t.Fatalf("reply = %q, want the media reply", (*posts)[0].text)
	}
	// The notice threads under the upload that won the latch. It is "700.1" here only
	// because fireTurn drains each event before the next; production dispatches the
	// pool concurrently, so in a real burst the winner — and therefore the message
	// the notice hangs under — is whichever upload gets there first. That is fine:
	// the reply is identical either way. What must hold is that it threads under an
	// upload at all rather than landing loose.
	if got := (*posts)[0].threadTS; got != "700.1" {
		t.Fatalf("reply threadTS = %q, want %q (the notice must answer the upload that won the latch)", got, "700.1")
	}
	// Every upload still deduped and still reached the latch: the cap is the latch's
	// doing, not an accident of some earlier gate swallowing the burst.
	if got := mem.putAttempts("evt#"); got != 5 {
		t.Fatalf("dedupe writes = %d, want 5 (each upload is its own message)", got)
	}
	if got := mem.putAttempts("media#"); got != 5 {
		t.Fatalf("latch attempts = %d, want 5 (every upload must consult the latch)", got)
	}
}

// TestHandleEvent_UnsupportedMediaNoticeReopensAfterWindow is the end-to-end half
// of TestMarkMediaNoticeSent_WindowReopensOnExpiry: the cap must be a pause, not a
// mute. memAgentDDB never reaps, so the marker written by the first burst is still
// sitting there when the second one arrives — only the write-time TTL comparison
// can let it through.
func TestHandleEvent_UnsupportedMediaNoticeReopensAfterWindow(t *testing.T) {
	now := fixedNow
	mem := newMemAgentDDB()
	h, posts, mu := newMediaNoticeHandlerAt(t, mem, func() time.Time { return now })

	fireTurn(t, h, mediaUploadBody("EvWin1", slackChannelTypeIM, "D1", "U2", "740.1"))
	fireTurn(t, h, mediaUploadBody("EvWin2", slackChannelTypeIM, "D1", "U2", "740.2"))
	now = now.Add(6 * time.Minute) // past defaultMediaNoticeTTL
	fireTurn(t, h, mediaUploadBody("EvWin3", slackChannelTypeIM, "D1", "U2", "740.3"))

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 2 {
		t.Fatalf("want 2 notices (one per window), got %d: %+v", len(*posts), *posts)
	}
	for i, want := range []string{"740.1", "740.3"} {
		if got := (*posts)[i].threadTS; got != want {
			t.Fatalf("notice %d answered %q, want %q", i, got, want)
		}
	}
}

// TestHandleEvent_UnsupportedMediaNoticeScope pins that the cap is narrow enough to
// keep its promise: everyone who has not just been told the limitation still hears
// it. A latch keyed on the channel alone (or on the workspace) fails this.
func TestHandleEvent_UnsupportedMediaNoticeScope(t *testing.T) {
	mem := newMemAgentDDB()
	h, posts, mu := newMediaNoticeHandler(t, mem)

	fireTurn(t, h, mediaUploadBody("EvScope1", slackChannelTypeIM, "D1", "U2", "710.1"))
	// Same member, same conversation — suppressed.
	fireTurn(t, h, mediaUploadBody("EvScope2", slackChannelTypeIM, "D1", "U2", "710.2"))
	// A different member in the same conversation was never told.
	fireTurn(t, h, mediaUploadBody("EvScope3", slackChannelTypeIM, "D1", "U3", "710.3"))
	// The same member in a different conversation.
	fireTurn(t, h, mediaUploadBody("EvScope4", slackChannelTypeIM, "D2", "U2", "710.4"))

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 3 {
		t.Fatalf("want 3 notices (U2/D1, U3/D1, U2/D2), got %d: %+v", len(*posts), *posts)
	}
	for i, want := range []string{"710.1", "710.3", "710.4"} {
		if got := (*posts)[i].threadTS; got != want {
			t.Fatalf("notice %d answered %q, want %q", i, got, want)
		}
	}
}

// TestHandleEvent_UnsupportedMediaNoticeFailsOpen: the latch is a volume cap, not a
// correctness guard, so a store blip must not turn an upload back into the silence
// this notice exists to replace. Contrast the dedupe write, which fails CLOSED.
func TestHandleEvent_UnsupportedMediaNoticeFailsOpen(t *testing.T) {
	mem := newMemAgentDDB()
	mem.putErr = errors.New("ddb down")
	mem.putErrSKPrefix = "media#" // dedupe still works; only the latch is broken
	h, posts, mu := newMediaNoticeHandler(t, mem)

	for i, ts := range []string{"720.1", "720.2"} {
		fireTurn(t, h, mediaUploadBody("EvOpen"+strconv.Itoa(i), slackChannelTypeIM, "D1", "U2", ts))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 2 {
		t.Fatalf("a broken latch must fall back to answering every upload, got %+v", *posts)
	}
	for i, p := range *posts {
		if p.text != agentUnsupportedMediaReply {
			t.Fatalf("reply %d = %q, want the media reply", i, p.text)
		}
	}
}

// TestHandleEvent_UnsupportedMediaNoticeIsMediaOnly keeps the latch off the other
// deterministic replies. They need typing an exact keyword, so they have no burst
// shape to cap — and capping "help" would silently swallow a real question.
func TestHandleEvent_UnsupportedMediaNoticeIsMediaOnly(t *testing.T) {
	mem := newMemAgentDDB()
	h, posts, mu := newMediaNoticeHandler(t, mem)

	for i, ts := range []string{"730.1", "730.2", "730.3"} {
		fireTurn(t, h, eventCallbackBody("EvHelp"+strconv.Itoa(i),
			`{"type":"message","channel_type":"im","user":"U2","channel":"D1","ts":"`+ts+`","text":"help"}`))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 3 {
		t.Fatalf("every help turn must be answered, got %d: %+v", len(*posts), *posts)
	}
	if got := mem.putAttempts("media#"); got != 0 {
		t.Fatalf("a non-media deterministic reply must not touch the media latch, got %d writes", got)
	}
}

// TestHandleEvent_UnsupportedMediaLogContract pins the demand-signal log. Mutation
// testing found that deleting this line, hardcoding files_visible to 0, or renaming
// the message all left the suite green — yet the whole justification for the line is
// that an operator query consumes it, exactly like the fail-open message pinned by
// TestAgentTurnLimit_FailOpenLogContract. It also pins the two bools that separate
// "Slack sent a subtype only" (benign) from "Slack changed the files shape" (the
// case where the refusal may be wrong), which is the distinction on-call needs.
func TestHandleEvent_UnsupportedMediaLogContract(t *testing.T) {
	// Spelled out, NOT referenced as agentUnsupportedMediaMsg: an operator query
	// consumes this exact string, so the literal is the contract and comparing the
	// const to itself would pin nothing (renaming it would keep the suite green —
	// which is one of the three mutants that motivated this test).
	//
	// Outcome-neutral on purpose: once repeats are suppressed, "replied with the
	// text-only limitation" is false for most of a burst. The other outcome is
	// covered by TestAgentUnsupportedMediaLogContract_SuppressedRepeat.
	const mediaLogMsg = "agent: unsupported media"

	if agentUnsupportedMediaMsg != mediaLogMsg {
		t.Fatalf("agentUnsupportedMediaMsg = %q, want %q", agentUnsupportedMediaMsg, mediaLogMsg)
	}

	tests := []struct {
		name             string
		event            string
		wantVisible      float64
		wantFieldPresent bool
		wantSubtype      bool
	}{
		{
			name:             "counted files",
			event:            `{"type":"message","subtype":"file_share","channel_type":"im","user":"U2","channel":"D1","ts":"600.1","text":"hi","files":[{"id":"F1"},{"id":"F2"}]}`,
			wantVisible:      2,
			wantFieldPresent: true,
			wantSubtype:      true,
		},
		{
			// Benign: Slack described the upload by subtype alone.
			name:        "subtype only",
			event:       `{"type":"message","subtype":"file_share","channel_type":"im","user":"U2","channel":"D1","ts":"600.2","text":"hi"}`,
			wantVisible: 0,
			wantSubtype: true,
		},
		{
			// The alertable pair: present but uncountable, and NO file_share subtype —
			// so this turn may have carried no attachment at all.
			name:             "uncountable shape",
			event:            `{"type":"message","channel_type":"im","user":"U2","channel":"D1","ts":"600.3","text":"hi","files":{"id":"F1"}}`,
			wantVisible:      0,
			wantFieldPresent: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
			post, _, _ := capturingPostMessage()
			h := NewHandler(Config{
				AgentLLM: panicAgentLLM{}, AgentStore: store, PostMessage: post,
				AgentDefaultEnabled: true,
			})
			t.Cleanup(h.Wait)

			var env slackEventEnvelope
			if err := json.Unmarshal([]byte(eventCallbackBody("EvLog", tt.event)), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			var buf bytes.Buffer
			log := slog.New(observability.NewRedactingJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
			h.processAgentEventWithAdmission(context.Background(), log, &env, "", nil, false)
			h.Wait()

			var rec map[string]any
			for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
				var candidate map[string]any
				if err := json.Unmarshal(line, &candidate); err != nil {
					continue
				}
				if candidate["msg"] == mediaLogMsg {
					rec = candidate
					break
				}
			}
			if rec == nil {
				t.Fatalf("no %q record; an operator query consumes this line, so it must be emitted. got %s", mediaLogMsg, buf.String())
			}
			if rec["level"] != "INFO" {
				t.Fatalf("level = %v, want INFO", rec["level"])
			}
			if rec["files_visible"] != tt.wantVisible {
				t.Fatalf("files_visible = %v, want %v", rec["files_visible"], tt.wantVisible)
			}
			if rec["files_field_present"] != tt.wantFieldPresent {
				t.Fatalf("files_field_present = %v, want %v", rec["files_field_present"], tt.wantFieldPresent)
			}
			if rec["file_share_subtype"] != tt.wantSubtype {
				t.Fatalf("file_share_subtype = %v, want %v", rec["file_share_subtype"], tt.wantSubtype)
			}
			if rec["user_id"] != "U2" {
				t.Fatalf("user_id = %v, want U2 (needed to join this record to a complaining user)", rec["user_id"])
			}
			// Each subtest gets a fresh store, so every one is a first upload.
			if rec["notice_posted"] != true {
				t.Fatalf("notice_posted = %v, want true (a fresh conversation must speak)", rec["notice_posted"])
			}
		})
	}
}

// TestAgentUnsupportedMediaLogContract_SuppressedRepeat is the other half of
// TestHandleEvent_UnsupportedMediaLogContract: capping the reply must not cap the
// COUNT. A suppressed upload still emits the same msg, so one exact $.msg filter
// totals real demand, and notice_posted is what separates spoke-from-stayed-quiet.
// Splitting the two outcomes across two msg strings would silently halve any
// "how much file demand is there?" query built on this line.
func TestAgentUnsupportedMediaLogContract_SuppressedRepeat(t *testing.T) {
	h := &Handler{cfg: Config{AgentStore: &slackdata.AgentStore{
		Client: newMemAgentDDB(), TableName: "agent_state",
	}}}
	env := &slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{
		Type: slackEventTypeMessage, ChannelType: slackChannelTypeIM,
		User: "U2", Channel: "D1", TS: "800.1",
		Files: filesFromJSON(t, `[{"id":"F1"},{"id":"F2"}]`),
	}}

	var buf bytes.Buffer
	log := slog.New(observability.NewRedactingJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if posted := h.claimMediaNotice(context.Background(), log, env, "T1"); !posted {
		t.Fatal("the first upload must win the latch")
	}
	env.Event.TS = "800.2" // a second, distinct upload in the same conversation
	if posted := h.claimMediaNotice(context.Background(), log, env, "T1"); posted {
		t.Fatal("the repeat must be suppressed")
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("every upload must log, suppressed or not; got %d lines: %q", len(lines), buf.String())
	}
	for i, wantPosted := range []bool{true, false} {
		var rec map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
			t.Fatalf("unmarshal %q: %v", lines[i], err)
		}
		// The literal again, for the same reason as in the sibling test.
		if rec["msg"] != "agent: unsupported media" {
			t.Fatalf("line %d msg = %v, want %q (both outcomes share one msg)", i, rec["msg"], "agent: unsupported media")
		}
		if rec["notice_posted"] != wantPosted {
			t.Fatalf("line %d notice_posted = %v, want %v", i, rec["notice_posted"], wantPosted)
		}
		// The suppressed line still carries the count, or the cap would erase demand.
		if got, _ := rec["files_visible"].(float64); got != 2 {
			t.Fatalf("line %d files_visible = %v, want 2", i, rec["files_visible"])
		}
		// A count, never an inventory: file ids and names are user content.
		if strings.Contains(lines[i], "F1") || strings.Contains(lines[i], "F2") {
			t.Fatalf("line %d leaked file ids: %s", i, lines[i])
		}
	}
}

// TestHandleEvent_ChannelUploadLogsWithoutDispatching is the observability half of
// the channel-upload drop: the member hears nothing, so the log line is the ONLY
// record that the upload happened at all. Two things ride on it — the count of
// "someone tried to send a file", which is the demand signal for building real file
// support, and the files_field_present=true / files_visible=0 pair on-call alerts on
// — and both would read as zero on this surface without it (see
// logAgentChannelUploadUnanswered).
//
// It goes through handleEvent rather than calling the emitter, because what can
// break is the WIRING: the drop happens in a pure filter that cannot log, so a
// refusal that stops being reported to the caller is invisible in
// TestShouldDispatchAgentEvent's verdict and invisible to the member by design.
// The negative cases are the other half — this must not become a line per message on
// the message.channels firehose, which is chatter, not demand.
func TestHandleEvent_ChannelUploadLogsWithoutDispatching(t *testing.T) {
	// Spelled out rather than referenced as agentUnsupportedMediaMsg, like the sibling
	// contract tests: an operator query consumes this exact string, and it is the SAME
	// string claimMediaNotice emits — one exact-$.msg filter has to total both.
	const mediaLogMsg = "agent: unsupported media"

	const joinedThread = agentPoolTestThreadTS
	tests := []struct {
		name             string
		followups        bool
		event            string
		wantLog          bool
		wantVisible      float64
		wantFieldPresent bool
		wantSubtype      bool
	}{
		{
			name:             "channel thread upload",
			followups:        true,
			event:            `{"type":"message","subtype":"file_share","channel_type":"channel","user":"U2","channel":"C1","thread_ts":"` + joinedThread + `","ts":"700.1","text":"","files":[{"id":"F1"}]}`,
			wantLog:          true,
			wantVisible:      1,
			wantFieldPresent: true,
			wantSubtype:      true,
		},
		{
			// Demand is not conditional on the flag, and the flag is dark: a line that
			// only fired with follow-ups on would report zero for exactly as long as
			// this count is the thing arguing for file support.
			name:             "channel thread upload with follow-ups off",
			followups:        false,
			event:            `{"type":"message","subtype":"file_share","channel_type":"channel","user":"U2","channel":"C1","thread_ts":"` + joinedThread + `","ts":"700.2","text":"","files":[{"id":"F1"},{"id":"F2"}]}`,
			wantLog:          true,
			wantVisible:      2,
			wantFieldPresent: true,
			wantSubtype:      true,
		},
		{
			// The alertable pair, on the surface that stopped raising it: a pure-TEXT
			// follow-up carrying a files value the decoder cannot count is refused as an
			// upload, and this is the one shape where that refusal may be WRONG.
			name:             "text follow-up with an uncountable files value",
			followups:        true,
			event:            `{"type":"message","channel_type":"channel","user":"U2","channel":"C1","thread_ts":"` + joinedThread + `","ts":"700.3","text":"and revoke it too","files":{"id":"F1"}}`,
			wantLog:          true,
			wantVisible:      0,
			wantFieldPresent: true,
		},
		{
			// Chatter, not demand. A top-level channel message is refused by a different
			// branch and must stay silent — this is the shape the firehose is made of.
			name:      "top-level channel chatter",
			followups: true,
			event:     `{"type":"message","channel_type":"channel","user":"U2","channel":"C1","ts":"700.4","text":"morning all"}`,
		},
		{
			// Admitted, then dropped downstream for not being the agent's thread. Also
			// not demand: nothing about it was an upload.
			name:      "text thread reply in a thread the agent never joined",
			followups: true,
			event:     `{"type":"message","channel_type":"channel","user":"U2","channel":"C1","thread_ts":"700.0","ts":"700.5","text":"more please"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
			post, posts, mu := capturingPostMessage()
			h := NewHandler(Config{
				AgentLLM: panicAgentLLM{}, AgentStore: store, PostMessage: post,
				AgentThreadHistory:    (&countingThreadHistory{}).read,
				AgentChannelFollowups: tt.followups, AgentDefaultEnabled: true,
			})
			t.Cleanup(h.Wait)

			// handleAgentEvent logs off the DEFAULT logger — the drop happens before any
			// per-turn logger exists — so capture that, through the production redacting
			// handler at the level a deployment actually ships.
			var buf bytes.Buffer
			prevLogger := slog.Default()
			slog.SetDefault(slog.New(observability.NewRedactingJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
			t.Cleanup(func() { slog.SetDefault(prevLogger) })

			h.handleEvent(httptest.NewRecorder(), []byte(eventCallbackBody("EvChanUpload", tt.event)))
			h.Wait()

			var rec map[string]any
			for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
				var candidate map[string]any
				if err := json.Unmarshal(line, &candidate); err != nil {
					continue
				}
				if candidate["msg"] == mediaLogMsg {
					rec = candidate
					break
				}
			}
			// Either way the member hears nothing: this surface refuses channel uploads
			// silently, and the log is what replaces the reply — never joins it.
			mu.Lock()
			gotPosts := len(*posts)
			mu.Unlock()
			if gotPosts != 0 {
				t.Fatalf("channel refusal should post nothing, got %d messages", gotPosts)
			}
			if !tt.wantLog {
				if rec != nil {
					t.Fatalf("chatter must not be counted as file demand, got %v", rec)
				}
				return
			}
			if rec == nil {
				t.Fatalf("no %q record; this line is the only trace a refused channel upload leaves. got %s", mediaLogMsg, buf.String())
			}
			if rec["level"] != "INFO" {
				t.Fatalf("level = %v, want INFO (Debug would not survive to the operator query)", rec["level"])
			}
			if rec["files_visible"] != tt.wantVisible {
				t.Fatalf("files_visible = %v, want %v", rec["files_visible"], tt.wantVisible)
			}
			if rec["files_field_present"] != tt.wantFieldPresent {
				t.Fatalf("files_field_present = %v, want %v", rec["files_field_present"], tt.wantFieldPresent)
			}
			if rec["file_share_subtype"] != tt.wantSubtype {
				t.Fatalf("file_share_subtype = %v, want %v", rec["file_share_subtype"], tt.wantSubtype)
			}
			if rec["user_id"] != "U2" {
				t.Fatalf("user_id = %v, want U2 (needed to join this record to a complaining member)", rec["user_id"])
			}
			// The discriminator: this upload drew no reply and ran no turn.
			if rec["channel_upload_unanswered"] != true {
				t.Fatalf("channel_upload_unanswered = %v, want true", rec["channel_upload_unanswered"])
			}
			// ...and NOT by overloading notice_posted, which on claimMediaNotice's line
			// means "counted, but a repeat we suppressed" — a conversation that did hear
			// the limitation. Emitting it here would merge two different outcomes.
			if _, ok := rec["notice_posted"]; ok {
				t.Fatalf("notice_posted must be absent; false there means a suppressed repeat, not silence: %v", rec)
			}
			// The identifying fields are the turn's own (see agentEventLogger): an
			// operator who finds this line pivots on them to the rest of the surface.
			if rec["channel_id"] != "C1" || rec["event_id"] != "EvChanUpload" || rec["surface"] != "agent" {
				t.Fatalf("record is missing the shared event identity: %v", rec)
			}
		})
	}
}

// TestHandleEvent_MentionedChannelUploadAnswersOnceViaTheMentionOnly covers the
// overlap Slack produces for ONE mentioned in-thread upload: an app_mention plus a
// message/file_share twin. The mention is the surface that answers it — the twin is a
// channel upload, dropped at the dispatch filter — so exactly one limitation reply
// goes out, and the twin costs nothing on the way to being dropped.
//
// This used to be a dedupe test, and it cannot be one any more: with the twin stopped
// at the filter, only one of the two events ever reaches the marker, so it could no
// longer fail on a broken dedupe key. That coverage moved to
// TestHandleEvent_OverlappingTextEventsDedupeOnChannelAndTS, which uses a TEXT thread
// reply — the shape where both events are still admissible.
func TestHandleEvent_MentionedChannelUploadAnswersOnceViaTheMentionOnly(t *testing.T) {
	mem := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: mem, TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	// countingThreadHistory would answer for agentPoolTestThreadTS with a completed
	// exchange whose reply belongs to app A1 — i.e. a thread the agent HAS joined, the
	// one case where the file_share twin would otherwise have been admitted and
	// answered. Its call count is the assertion: the twin must not reach it even then.
	//
	// AgentChannelFollowups: true is load-bearing. With it false the twin would be
	// dropped by the flag gate instead of the upload guard and this test would still
	// pass, proving nothing about the guard.
	hist := &countingThreadHistory{}
	h := NewHandler(Config{
		AgentLLM: panicAgentLLM{}, AgentStore: store, PostMessage: post,
		AgentThreadHistory:    hist.read,
		AgentChannelFollowups: true, AgentDefaultEnabled: true,
	})
	t.Cleanup(h.Wait)

	var logs bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(observability.NewRedactingJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	// The mention is deliberately mid-caption: Slack still emits app_mention for
	// this shape, so a leading-only twin detector would count the same upload twice.
	h.handleEvent(httptest.NewRecorder(), []byte(botAuthorizedEventCallbackBody("EvMentionFile", `{"type":"app_mention","user":"U2","channel":"C1","thread_ts":"`+agentPoolTestThreadTS+`","ts":"400.3","text":"please review <@U12345678> this","files":[{"id":"F3"}]}`)))
	h.handleEvent(httptest.NewRecorder(), []byte(botAuthorizedEventCallbackBody("EvShareFile", `{"type":"message","subtype":"file_share","channel_type":"channel","user":"U2","channel":"C1","thread_ts":"`+agentPoolTestThreadTS+`","ts":"400.3","text":"please review <@U12345678> this","files":[{"id":"F3"}]}`)))
	h.Wait()

	// The whole point of the filter drop: a channel upload buys no conversations.replies
	// read, so it also spends no followupGateSem slot — that pool stays available for the
	// legitimate text follow-ups its saturation path would otherwise drop.
	if calls := hist.callCount(); calls != 0 {
		t.Fatalf("channel upload should read no thread history, got %d lookups", calls)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentUnsupportedMediaReply {
		t.Fatalf("a mentioned in-thread upload should post one media reply, got %+v", *posts)
	}
	if got := (*posts)[0].threadTS; got != agentPoolTestThreadTS {
		t.Fatalf("reply threadTS = %q, want %q (an in-thread upload must answer in that thread)", got, agentPoolTestThreadTS)
	}
	// Only the mention reached the turn at all, so it alone consulted the marker and the
	// latch. Both counts are 1 — if the twin ever starts being admitted again, both go
	// to 2 here before anything user-visible changes.
	if got := mem.putAttempts("evt#"); got != 1 {
		t.Fatalf("dedupe attempts = %d, want 1 (the file_share twin should stop at the dispatch filter)", got)
	}
	if got := mem.putAttempts("media#"); got != 1 {
		t.Fatalf("latch attempts = %d, want 1 (only the mention should reach the notice latch)", got)
	}
	mediaLogs := 0
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) == nil && rec["msg"] == "agent: unsupported media" {
			mediaLogs++
			if rec["channel_upload_unanswered"] == true {
				t.Fatalf("message/file_share twin emitted a second demand record: %v", rec)
			}
		}
	}
	if mediaLogs != 1 {
		t.Fatalf("one mentioned upload must emit one demand record, got %d: %s", mediaLogs, logs.String())
	}
}

// TestHandleEvent_OverlappingTextEventsDedupeOnChannelAndTS holds the coverage the
// media overlap test above used to carry. Slack emits both app_mention and
// message.channels for one mentioned message; with channel follow-ups on, a TEXT
// thread reply is admissible under BOTH shapes, so both events reach dedupe and their
// shared message identity is the only thing that can collapse them.
//
// This rests on agentEventDedupeKey being channel+":"+ts. The two events differ in
// event_id AND in type, and share only channel and ts — so if the key ever keys on
// either of those instead, this goes red rather than quietly double-replying. The
// dedupe-attempt count is asserted too: it is what proves both events actually got
// that far, so a future filter change cannot hollow this test out silently.
func TestHandleEvent_OverlappingTextEventsDedupeOnChannelAndTS(t *testing.T) {
	mem := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: mem, TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM: fakeAgentLLM{reply: testAgentStillWorksReply}, AgentStore: store, PostMessage: post,
		AgentThreadHistory:    (&countingThreadHistory{}).read,
		AgentChannelFollowups: true, AgentDefaultEnabled: true,
	})
	t.Cleanup(h.Wait)

	h.handleEvent(httptest.NewRecorder(), []byte(eventCallbackBody("EvMentionText", `{"type":"app_mention","user":"U2","channel":"C1","thread_ts":"`+agentPoolTestThreadTS+`","ts":"401.3","text":"<@U12345678> more please"}`)))
	h.handleEvent(httptest.NewRecorder(), []byte(eventCallbackBody("EvChannelText", `{"type":"message","channel_type":"channel","user":"U2","channel":"C1","thread_ts":"`+agentPoolTestThreadTS+`","ts":"401.3","text":"<@U12345678> more please"}`)))
	h.Wait()

	if got := mem.putAttempts("evt#"); got != 2 {
		t.Fatalf("dedupe attempts = %d, want 2 — both events must REACH dedupe for this to be testing the key at all", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 || (*posts)[0].text != agentLLMReplyWithDisclaimer(testAgentStillWorksReply) {
		t.Fatalf("overlapping mention/channel events for one message should post one agent reply, got %+v", *posts)
	}
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

// misuseSuiteScoringWindow is the LLM-misuse suite's convention: a case whose
// expected result is a reply FAILS if no reply or confirm card appears within 90
// seconds. It is an external contract, mirrored here so the turn budget can be
// checked against it.
// TODO(upstream-contract): keep in lockstep with the misuse runbook's timeout
// convention ("Reply expected: fail if no reply or card appears within 90 seconds").
const misuseSuiteScoringWindow = 90 * time.Second

func TestAgentTurnBudgetDeliversInsideTheScoringWindow(t *testing.T) {
	// P15.2 failed on this arithmetic, not on a policy gap: the turn budget was
	// ALSO 90s, so a turn that spent it delivered at or after the moment the case
	// was already scored a failure. A turn that finalizes gracefully still has to
	// LAND in time. Keep real margin — the deploy that closes P15.2 depends on it.
	const requiredMargin = 10 * time.Second
	worstCase := agentTurnTimeout + agentDeliveryBudget
	if worstCase+requiredMargin > misuseSuiteScoringWindow {
		t.Fatalf("worst-case turn+delivery is %v; needs %v of margin inside the %v scoring window",
			worstCase, requiredMargin, misuseSuiteScoringWindow)
	}
	// The original sizing constraint still holds: a multi-tool turn needs more
	// room than a slash command, or legitimate turns get cut off mid-flight.
	if agentTurnTimeout <= asyncWorkTimeout {
		t.Fatalf("turn budget %v must exceed the %v slash-command budget", agentTurnTimeout, asyncWorkTimeout)
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

// agentHistoryTestFollowupTS is the inbound follow-up turn's ts in the
// attachment-history tests: later than every entry the stub returns, so it is the
// current turn (which Agent.Run adds) rather than part of the rebuilt transcript.
const agentHistoryTestFollowupTS = "100.2"

// TestLoadAgentThreadHistory_AnnotatesEarlierAttachmentTurn pins the text-only
// boundary ACROSS turns. An upload's own turn is refused outright, but the caption
// stays in the Slack thread and every later turn rebuilds that thread from
// conversations.replies. Replayed bare, "protect everything in this" reads as a
// complete instruction the model can act on when the next message says "ok do it" —
// a referent it never saw, presented as plain text. That is the same
// misrepresentation agentUnsupportedMediaReply exists to remove, one turn later.
//
// The note travels with the caption rather than relying on the refusal reply beside
// it: repeated uploads in one conversation are capped (claimMediaNotice) and post no
// reply at all, so a caption can outlive the reply that explained it — as long as a
// later exchange completes. A caption with no completed exchange after it is dropped
// by the pre-existing window rules instead, which is the other safe direction; see
// TestLoadAgentThreadHistory_UnrepliedAttachmentNeverReachesTheModel.
func TestLoadAgentThreadHistory_AnnotatesEarlierAttachmentTurn(t *testing.T) {
	const caption = "protect everything in this"
	h := NewHandler(Config{
		AgentThreadHistory: func(context.Context, string, string, string, string, string) ([]AgentThreadMessage, error) {
			return []AgentThreadMessage{
				{UserID: "U1", Text: "<@U12345678> " + caption, TS: agentPoolTestThreadTS, HasFiles: true},
				{AppID: "A1", Text: agentUnsupportedMediaReply, TS: "100.1"},
				// An ordinary text turn in the same thread, so the test also pins that
				// the note is CONDITIONAL. Without it, annotating every user message
				// unconditionally passes.
				{UserID: "U1", Text: "what can I reach?", TS: "100.15"},
				{AppID: "A1", Text: "the connectors I can see here", TS: "100.16"},
			}, nil
		},
	})
	e := env(slackEventTypeMessage, "channel", "U1", "", "", "ok do it")
	e.APIAppID = "A1"
	e.Event.ThreadTS = agentPoolTestThreadTS
	e.Event.TS = agentHistoryTestFollowupTS

	history, joined, err := h.loadAgentThreadHistory(context.Background(), e)
	if err != nil || !joined {
		t.Fatalf("load history: joined=%v err=%v", joined, err)
	}
	want := []agent.Message{
		{Role: "user", Text: caption + " " + agentHistoryAttachmentNote},
		{Role: "assistant", Text: agentUnsupportedMediaReply},
		{Role: "user", Text: "what can I reach?"},
		{Role: "assistant", Text: "the connectors I can see here"},
	}
	if !reflect.DeepEqual(history, want) {
		t.Fatalf("history = %#v, want %#v", history, want)
	}
}

// TestLoadAgentThreadHistory_KeepsFileOnlyTurnAndLeavesOwnRepliesAlone covers the
// two edges of the annotation. A file-only upload has no caption at all, so before
// the note it left model context entirely — the refusal reply beside it answered
// nothing, and a thread could read as if the user had never spoken. It becomes the
// note on its own instead of vanishing. An own reply is never annotated: this
// surface posts no files, so a HasFiles on one is Slack describing something we did
// not attach.
func TestLoadAgentThreadHistory_KeepsFileOnlyTurnAndLeavesOwnRepliesAlone(t *testing.T) {
	h := NewHandler(Config{
		AgentThreadHistory: func(context.Context, string, string, string, string, string) ([]AgentThreadMessage, error) {
			return []AgentThreadMessage{
				{UserID: "U1", TS: agentPoolTestThreadTS, HasFiles: true},
				// An upload whose only text is the @mention that addressed the agent.
				// On main it left history entirely (empty text after the strip); now it
				// reduces to the note. The strip/annotate order is immaterial only
				// because botMentionPattern is ^-anchored, so appending cannot disturb
				// the prefix it matches — an unanchored pattern would make this row the
				// one that notices.
				{UserID: "U1", Text: "<@U12345678>", TS: "100.05", HasFiles: true},
				{AppID: "A1", Text: "answer", TS: "100.1", HasFiles: true},
			}, nil
		},
	})
	e := env(slackEventTypeMessage, "channel", "U1", "", "", "ok do it")
	e.APIAppID = "A1"
	e.Event.ThreadTS = agentPoolTestThreadTS
	e.Event.TS = agentHistoryTestFollowupTS

	history, joined, err := h.loadAgentThreadHistory(context.Background(), e)
	if err != nil || !joined {
		t.Fatalf("load history: joined=%v err=%v", joined, err)
	}
	want := []agent.Message{
		// Both uploads merge into one user turn, so the note appears twice — the
		// count is the number of attachments, which is what it should be.
		{Role: "user", Text: agentHistoryAttachmentNote + "\n" + agentHistoryAttachmentNote},
		{Role: "assistant", Text: "answer"},
	}
	if !reflect.DeepEqual(history, want) {
		t.Fatalf("history = %#v, want %#v", history, want)
	}
}

// TestLoadAgentThreadHistory_UnrepliedAttachmentNeverReachesTheModel pins where
// the annotation STOPS mattering, so the note is not mistaken for a guarantee that
// every caption survives. Two pre-existing window rules outrank it, and both were
// already in place before uploads were refused at all:
//
//   - a thread with no completed exchange returns no history (lastAssistant < 0);
//   - a caption after the last qURL response is an incomplete tail and is dropped.
//
// #1045 caps repeated unsupported-media replies, so an upload really can sit in a
// thread with no reply beside it — which is exactly when a reader might expect the
// note to be doing the work. It is not: in both shapes the caption reaches the model
// neither annotated nor bare, which is the other safe direction. The value of pinning
// it is that a future change to the tail rules cannot quietly turn "dropped" into
// "replayed bare" without a red test.
func TestLoadAgentThreadHistory_UnrepliedAttachmentNeverReachesTheModel(t *testing.T) {
	const caption = "protect everything in this"
	load := func(t *testing.T, raw []AgentThreadMessage) []agent.Message {
		t.Helper()
		h := NewHandler(Config{
			AgentThreadHistory: func(context.Context, string, string, string, string, string) ([]AgentThreadMessage, error) {
				return raw, nil
			},
		})
		e := env(slackEventTypeMessage, "channel", "U1", "", "", "ok do it")
		e.APIAppID = "A1"
		e.Event.ThreadTS = agentPoolTestThreadTS
		e.Event.TS = agentHistoryTestFollowupTS

		history, _, err := h.loadAgentThreadHistory(context.Background(), e)
		if err != nil {
			t.Fatalf("load history: %v", err)
		}
		return history
	}
	// The two dropped shapes are checked by absence rather than by an expected
	// slice, so the assertion stays true to what actually matters: the caption is
	// gone in EVERY form, annotated or bare.
	mustNotMentionCaption := func(t *testing.T, history []agent.Message) {
		t.Helper()
		for _, msg := range history {
			if strings.Contains(msg.Text, caption) {
				t.Fatalf("an un-replied caption must not reach the model at all; history = %#v", history)
			}
		}
	}

	t.Run("capped upload with no completed exchange yields no history", func(t *testing.T) {
		got := load(t, []AgentThreadMessage{
			{UserID: "U1", Text: caption, TS: agentPoolTestThreadTS, HasFiles: true},
		})
		mustNotMentionCaption(t, got)
		if len(got) != 0 {
			t.Fatalf("history = %#v, want none", got)
		}
	})

	t.Run("capped upload trailing a completed exchange is dropped as an incomplete tail", func(t *testing.T) {
		got := load(t, []AgentThreadMessage{
			{UserID: "U1", Text: "what can I reach?", TS: agentPoolTestThreadTS},
			{AppID: "A1", Text: "answer", TS: "100.1"},
			{UserID: "U1", Text: caption, TS: "100.15", HasFiles: true},
		})
		mustNotMentionCaption(t, got)
		want := []agent.Message{
			{Role: "user", Text: "what can I reach?"},
			{Role: "assistant", Text: "answer"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("history = %#v, want %#v", got, want)
		}
	})

	// The contrast case, in the same test so the boundary is readable from one
	// place: once an exchange completes after it, the caption is back inside the
	// window — and it arrives annotated, merged into the adjacent user turn.
	t.Run("a caption followed by a completed exchange survives, annotated", func(t *testing.T) {
		got := load(t, []AgentThreadMessage{
			{UserID: "U1", Text: caption, TS: agentPoolTestThreadTS, HasFiles: true},
			{UserID: "U1", Text: "what can I reach?", TS: "100.1"},
			{AppID: "A1", Text: "answer", TS: "100.15"},
		})
		want := []agent.Message{
			{Role: "user", Text: caption + " " + agentHistoryAttachmentNote + "\nwhat can I reach?"},
			{Role: "assistant", Text: "answer"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("history = %#v, want %#v", got, want)
		}
	})
}

// TestProcessAgentEvent_EarlierAttachmentTurnReachesModelAnnotated is the
// end-to-end half: it asserts on what the LLM is actually handed, not on the
// rebuilt slice, so an annotation lost anywhere between loadAgentThreadHistory and
// the request still fails.
func TestProcessAgentEvent_EarlierAttachmentTurnReachesModelAnnotated(t *testing.T) {
	const caption = "protect everything in this"
	llm := &scriptedHandlerAgentLLM{responses: []agent.Response{{
		Text:       testAgentStillWorksReply,
		StopReason: testAgentStopEndTurn,
	}}}
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            llm,
		AgentStore:          &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"},
		PostMessage:         post,
		AgentDefaultEnabled: true,
		AgentThreadHistory: func(context.Context, string, string, string, string, string) ([]AgentThreadMessage, error) {
			return []AgentThreadMessage{
				{UserID: "U1", Text: caption, TS: agentPoolTestThreadTS, HasFiles: true},
				{AppID: "A1", Text: agentUnsupportedMediaReply, TS: "100.1"},
			}, nil
		},
	})

	e := env(slackEventTypeMessage, "im", "U1", "", "", "ok do it")
	e.APIAppID = "A1"
	e.Event.ThreadTS = agentPoolTestThreadTS
	e.Event.TS = agentHistoryTestFollowupTS
	h.processAgentEvent(context.Background(), slog.Default(), e)

	if llm.calls != 1 {
		t.Fatalf("follow-up should reach the model once, got %d calls", llm.calls)
	}
	// Exact equality, not Contains: a Contains against the note is vacuously true if
	// the note is ever emptied, which is precisely the change this test exists to catch.
	var annotated bool
	for _, msg := range llm.captured[0].Messages {
		if msg.Text == caption {
			t.Fatalf("an attachment's caption must never reach the model as plain text; messages = %#v", llm.captured[0].Messages)
		}
		if msg.Text == caption+" "+agentHistoryAttachmentNote {
			annotated = true
		}
	}
	if !annotated {
		t.Fatalf("the caption must survive alongside its note, not be dropped; messages = %#v", llm.captured[0].Messages)
	}
}

// TestAgentTurnCompleteLogsHistoryAttachments pins the operator field. Annotating
// history is silent where the event path is loud: an upload's own turn refuses
// visibly and logs an alertable pair (TestAgentUnsupportedMediaLogContract), while a
// rebuilt annotation changes what the model is told with nothing to look at after.
// This is the one record that says it happened, so the field has to be emitted and
// has to count.
func TestAgentTurnCompleteLogsHistoryAttachments(t *testing.T) {
	const turnCompleteMsg = "agent: turn complete"
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            fakeAgentLLM{reply: "done"},
		AgentStore:          &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"},
		PostMessage:         post,
		AgentDefaultEnabled: true,
		AgentThreadHistory: func(context.Context, string, string, string, string, string) ([]AgentThreadMessage, error) {
			return []AgentThreadMessage{
				{UserID: "U1", Text: "protect everything in this", TS: agentPoolTestThreadTS, HasFiles: true},
				{AppID: "A1", Text: agentUnsupportedMediaReply, TS: "100.1"},
				{UserID: "U1", Text: "what can I reach?", TS: "100.15"},
				// An assistant turn that quotes the note back. The model does see the
				// marker, so it can echo it; counting that would report an attachment
				// that never existed.
				{AppID: "A1", Text: "You sent " + agentHistoryAttachmentNote + " so I can't read it.", TS: "100.16"},
			}, nil
		},
	})
	t.Cleanup(h.Wait)

	var buf bytes.Buffer
	log := slog.New(observability.NewRedactingJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	e := env(slackEventTypeMessage, "im", "U1", "", "", "ok do it")
	e.APIAppID = "A1"
	e.Event.ThreadTS = agentPoolTestThreadTS
	e.Event.TS = agentHistoryTestFollowupTS
	h.processAgentEvent(context.Background(), log, e)
	h.Wait()

	var rec map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var candidate map[string]any
		if err := json.Unmarshal(line, &candidate); err != nil {
			continue
		}
		if candidate["msg"] == turnCompleteMsg {
			rec = candidate
			break
		}
	}
	if rec == nil {
		t.Fatalf("no %q record; got %s", turnCompleteMsg, buf.String())
	}
	if rec["history_attachments"] != float64(1) {
		t.Fatalf("history_attachments = %v, want 1 — one USER message told the model an attachment was there; an assistant turn quoting the note is not one", rec["history_attachments"])
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
