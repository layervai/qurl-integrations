package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// These tests fence handleEvent's field-type tolerance: a single Slack field
// arriving as an unexpected JSON type must not cost the whole event, and the
// partially-decoded envelope it produces must not be able to reach anything it
// could not have reached with that field simply absent.
//
// The two halves are inseparable. Tolerating drift without the second half
// would trade a silent drop for a silent misfire on the uninstall cascade,
// which is strictly worse.

// mistypedDriftEventID is the event id shared by the drift fixtures. Dedupe is
// per-partition and these handlers are per-test, so one constant is fine.
const mistypedDriftEventID = "EvDrift"

// TestSlackEnvelopeDriftPopulatesTheRestOfTheStruct pins the encoding/json
// behavior the whole change rests on, so a future toolchain that "fixed" it
// (aborting the decode on the first type mismatch) fails here rather than
// quietly turning every drifted event back into a silent drop.
//
// It also pins the far less obvious half: a SYNTAX error leaves the struct
// completely zero, because encoding/json validates the document before
// decoding any of it. That is what makes "tolerate one, abort the other" safe
// rather than arbitrary — there is genuinely nothing to route on in the second
// case.
func TestSlackEnvelopeDriftPopulatesTheRestOfTheStruct(t *testing.T) {
	const drifted = `{"type":"event_callback","team_id":"T1","event":{"type":"app_mention","user":"U2","channel":"C1","ts":"100.1","text":"hi","bot_id":42}}`

	var env slackEventEnvelope
	err := json.Unmarshal([]byte(drifted), &env)
	if err == nil {
		t.Fatal("a mistyped field must still report an error; the tolerance is a routing decision, not a claim the payload was clean")
	}
	if !eventEnvelopeTypeDrift(err) {
		t.Fatalf("mistyped field classified as a body-level parse failure: %v", err)
	}
	if env.Type != "event_callback" || env.Event.Type != "app_mention" || env.Event.User != "U2" || env.Event.Text != "hi" {
		t.Fatalf("sibling fields lost alongside the drifted one: %+v", env)
	}
	if env.Event.BotID != "" {
		t.Fatalf("drifted string field = %q, want the zero value — every guard below depends on drift never producing a WRONG value", env.Event.BotID)
	}

	// Trailing garbage after a complete, well-typed object: the object never
	// reaches the struct at all.
	var syntaxEnv slackEventEnvelope
	syntaxErr := json.Unmarshal([]byte(`{"type":"event_callback","event":{"type":"app_uninstalled"}} trailing`), &syntaxEnv)
	if eventEnvelopeTypeDrift(syntaxErr) {
		t.Fatalf("body-level parse failure classified as field drift: %v", syntaxErr)
	}
	if !reflect.DeepEqual(syntaxEnv, slackEventEnvelope{}) {
		t.Fatalf("syntax error left a partially-populated envelope: %+v — the abort branch assumes there is nothing to route on", syntaxEnv)
	}
}

// TestHandleEvent_MistypedFieldStillRoutes is the headline regression fence: an
// ordinary @mention that happens to carry one drifted field is still answered.
// Before this change the whole event was discarded at Debug with a 200 that
// stopped Slack retrying, so the user's question simply vanished.
func TestHandleEvent_MistypedFieldStillRoutes(t *testing.T) {
	// Each fixture is a valid app_mention carrying ONE extra field whose JSON
	// type is not the one we model — spliced into the envelope, or into the
	// inner event, at the two nesting depths a real payload can drift at. They
	// stand in for shape drift anywhere, which is the point: the fix is not
	// per-field.
	const mention = `"type":"app_mention","user":"U2","channel":"C1","ts":"100.1","text":"<@U12345678> what can I reach?"`
	tests := []struct {
		name string
		// envelopeField and innerField are appended to the envelope and the
		// inner event respectively; exactly one is set per row.
		envelopeField string
		innerField    string
	}{
		{name: "envelope scalar drift", envelopeField: `"event_time":"1700000000"`},
		{name: "envelope array drift", envelopeField: `"authorizations":"A1"`},
		{name: "envelope object drift", envelopeField: `"authorizations":{"team_id":"T1"}`},
		{name: "inner scalar drift", innerField: `"bot_id":42`},
		{name: "inner nested-object drift", innerField: `"assistant_thread":"nope"`},
		{name: "inner array drift", innerField: `"tokens":["B1"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, posts, mu := newAgentEventHandler(t, testAgentReachStagingReply)
			inner := mention
			if tt.innerField != "" {
				inner += "," + tt.innerField
			}
			body := `{"type":"event_callback","team_id":"T1","event_id":"` + mistypedDriftEventID + `","event":{` + inner + `}`
			if tt.envelopeField != "" {
				body += "," + tt.envelopeField
			}
			body += "}"

			w := httptest.NewRecorder()
			h.handleEvent(w, []byte(body))
			if w.Code != http.StatusOK {
				t.Fatalf("ack code = %d, want 200", w.Code)
			}
			h.Wait()

			mu.Lock()
			defer mu.Unlock()
			if len(*posts) != 1 {
				t.Fatalf("a drifted %s must not swallow the turn: got %d replies, want 1", tt.name, len(*posts))
			}
		})
	}
}

// TestHandleEvent_SyntaxErrorRoutesNothing is the other side of the tolerance:
// a body that never parsed still dispatches nothing. Without this, "tolerate
// decode errors" could quietly widen into routing on a zero envelope.
func TestHandleEvent_SyntaxErrorRoutesNothing(t *testing.T) {
	h, posts, mu := newAgentEventHandler(t, testAgentReachStagingReply)

	w := httptest.NewRecorder()
	// A complete, well-typed app_mention followed by trailing garbage — the
	// nastiest case, because the bytes the handler wants are all present and
	// valid, and only the document as a whole is not.
	h.handleEvent(w, []byte(appMentionBody("EvSyntax")+" trailing"))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 0 {
		t.Fatalf("unparseable body dispatched %d replies, want 0", len(*posts))
	}
}

// TestHandleEvent_TypeDriftIsLoggedAtWarn pins the observability half. The
// tolerance removes the only symptom shape drift used to have (events silently
// disappearing), so the log line is now the ONLY way an operator learns Slack's
// payload has moved — Debug is off in prod, which is where it was hiding before.
func TestHandleEvent_TypeDriftIsLoggedAtWarn(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	h, _, _ := newAgentEventHandler(t, testAgentReachStagingReply)
	body := `{"type":"event_callback","team_id":"T1","event_id":"EvWarn",` +
		`"event":{"type":"app_mention","user":"U2","channel":"C1","ts":"100.1","text":"<@U12345678> hi"},"event_time":"nope"}`
	h.handleEvent(httptest.NewRecorder(), []byte(body))
	h.Wait()

	got := logBuf.String()
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "type drift") {
		t.Fatalf("tolerated drift must be visible at Warn; got %q", got)
	}
	// The routing keys belong in the line: knowing WHICH event type drifted is
	// the difference between an actionable report and "something changed".
	for _, want := range []string{"envelope_type=event_callback", "inner_event_type=app_mention"} {
		if !strings.Contains(got, want) {
			t.Errorf("drift log missing %q, got %q", want, got)
		}
	}

	// A clean envelope must stay quiet, or the line becomes noise operators filter out.
	logBuf.Reset()
	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvClean")))
	h.Wait()
	if strings.Contains(logBuf.String(), "type drift") {
		t.Fatalf("clean envelope logged drift: %q", logBuf.String())
	}
}

// TestHandleEvent_MistypedFieldCannotFabricateLifecycleRouting is the
// highest-stakes guarantee in this change. Routing a partially-decoded envelope
// is only defensible if drift cannot INVENT an uninstall, so each fixture here
// is a payload that is one plausible decoder mistake away from wiping a live
// workspace, and none of them may purge anything.
func TestHandleEvent_MistypedFieldCannotFabricateLifecycleRouting(t *testing.T) {
	tests := []struct {
		name string
		body string
		why  string
	}{
		{
			name: "inner event is a string, not an object",
			body: `{"type":"event_callback","team_id":"` + testAdminTeamID + `","api_app_id":"A1","event_id":"EvDriftA","event":"app_uninstalled"}`,
			why:  "env.Event stays zero, so isLifecycleEvent sees no event type",
		},
		{
			name: "envelope type drifted away from event_callback",
			body: `{"type":123,"team_id":"` + testAdminTeamID + `","api_app_id":"A1","event_id":"EvDriftB","event":{"type":"app_uninstalled"}}`,
			why:  "every routing case is gated on env.Type, which drift zeroes",
		},
		{
			name: "tokens object is a string",
			// The trap: encoding/json ALLOCATES Tokens before it discovers the
			// value is a string, so a nil check alone would read this as a
			// revocation of every bot token.
			body: `{"type":"event_callback","team_id":"` + testAdminTeamID + `","api_app_id":"A1","event_id":"EvDriftC","event":{"type":"tokens_revoked","tokens":"revoked"}}`,
			why:  "isBotTokensRevokedEvent requires a listed bot token, not just a non-nil tokens object",
		},
		{
			name: "tokens.bot is a string, not an array",
			body: `{"type":"event_callback","team_id":"` + testAdminTeamID + `","api_app_id":"A1","event_id":"EvDriftD","event":{"type":"tokens_revoked","tokens":{"bot":"B123"}}}`,
			why:  "an uncountable bot list is not proof the bot token died",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, provider, _ := newLifecycleTestHandler(t)

			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, tt.body, tt.body))
			if w.Code != http.StatusOK {
				t.Fatalf("ack code = %d, want 200", w.Code)
			}
			h.Wait()

			if provider.deleteStateCalls != 0 {
				t.Fatalf("drifted payload triggered %d workspace_state deletes, want 0 (%s)", provider.deleteStateCalls, tt.why)
			}
			assertLifecycleAgentStatePresent(t, h.cfg.AgentStore, testAdminTeamID)
			if _, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), testAdminTeamID); err != nil {
				t.Fatalf("workspace mapping destroyed by a drifted payload: %v (%s)", err, tt.why)
			}
		})
	}
}

// TestHandleEvent_MistypedTeamIDPurgesNothingRatherThanTheWrongWorkspace pins
// the property that makes routing a partial envelope safe on the purge path:
// drift zeroes a string, and lifecycleWorkspaceIDs skips empty ids. So a real
// uninstall whose team_id drifted resolves to no target and is logged as such —
// it can never resolve to somebody else's workspace.
func TestHandleEvent_MistypedTeamIDPurgesNothingRatherThanTheWrongWorkspace(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)

	body := `{"type":"event_callback","team_id":9,"api_app_id":"A1","event_id":"EvDriftTeam","event":{"type":"app_uninstalled"}}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	if provider.deleteStateCalls != 0 {
		t.Fatalf("unaddressable uninstall deleted %d workspace_state rows, want 0", provider.deleteStateCalls)
	}
	assertLifecycleAgentStatePresent(t, h.cfg.AgentStore, testAdminTeamID)
}

// TestHandleEvent_UninstallSurvivesADriftedSiblingField is the reason the
// tolerance is worth its risk. A dropped uninstall is unrecoverable: handleEvent
// has already acked 200, Slack never redelivers, and the departed workspace's
// data stays behind — so one drifted field used to mean a permanent retention
// failure. Here the cascade runs on the fields that did decode.
func TestHandleEvent_UninstallSurvivesADriftedSiblingField(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)

	body := `{"type":"event_callback","team_id":"` + testAdminTeamID + `","api_app_id":"A1","event_id":"EvDriftUninstall",` +
		`"event_time":"1700000000","event":{"type":"app_uninstalled"}}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	if provider.deleteStateCalls != 1 {
		t.Fatalf("DeleteWorkspaceState calls = %d, want 1", provider.deleteStateCalls)
	}
	if provider.deleteStateWorkspaceID != testAdminTeamID {
		t.Fatalf("purged workspace = %q, want %q", provider.deleteStateWorkspaceID, testAdminTeamID)
	}
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testAdminTeamID)

	_, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), testAdminTeamID)
	var ae *slackdata.Error
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
		t.Fatalf("ListAdmins after drifted-field purge: err = %v, want 404 *Error", err)
	}
}

// TestShouldDispatchAgentEvent_OwnPostDroppedWithADriftedBotID covers the one
// guard that routing partial envelopes genuinely weakens. bot_id is the sole
// thing standing between the agent and answering its own reply, and drift
// zeroes it — so app_id carries the guard when it does. This is the failure
// that compounds rather than costing one turn: each self-reply is another
// message event, and the per-turn rate limiter is a cost backstop that fails
// open.
func TestShouldDispatchAgentEvent_OwnPostDroppedWithADriftedBotID(t *testing.T) {
	ownPost := func() *slackEventEnvelope {
		e := env(slackEventTypeMessage, slackChannelTypeIM, "U_BOT", "", "", "here's what you can reach")
		e.APIAppID = "A1"
		e.Event.AppID = "A1"
		return e
	}

	// bot_id intact: the original guard still does the work.
	withBotID := ownPost()
	withBotID.Event.BotID = "B9"
	if shouldDispatchAgentEvent(withBotID, false) {
		t.Fatal("own post with bot_id admitted")
	}

	// bot_id zeroed, exactly as a drifted `"bot_id": 42` decodes.
	if shouldDispatchAgentEvent(ownPost(), false) {
		t.Fatal("own post admitted once bot_id drifted away — this is the self-reply loop")
	}

	// A human is untouched: real people carry no app_id, so the added guard can
	// only ever drop, never silence a member.
	human := env(slackEventTypeMessage, slackChannelTypeIM, "U2", "", "", "what can I reach?")
	human.APIAppID = "A1"
	if !shouldDispatchAgentEvent(human, false) {
		t.Fatal("human DM dropped by the app_id guard")
	}

	// Another app's post is still judged on bot_id alone; app_id only speaks for
	// messages this app itself sent.
	otherApp := ownPost()
	otherApp.Event.AppID = "A_OTHER"
	if !shouldDispatchAgentEvent(otherApp, false) {
		t.Fatal("app_id guard fired for a different app's id")
	}
}

// TestHandleEvent_OwnPostWithDriftedBotIDIsNotAnswered is the end-to-end half of
// the unit test above, driven through the real decoder so the zeroed bot_id
// comes from encoding/json rather than from a hand-built struct.
func TestHandleEvent_OwnPostWithDriftedBotIDIsNotAnswered(t *testing.T) {
	h, posts, mu := newAgentEventHandler(t, testAgentReachStagingReply)

	body := `{"type":"event_callback","team_id":"T1","api_app_id":"A1","event_id":"EvSelfLoop",` +
		`"event":{"type":"message","channel_type":"im","user":"U_BOT","channel":"D1","ts":"100.3",` +
		`"text":"here's what you can reach","bot_id":42,"app_id":"A1"}}`
	w := httptest.NewRecorder()
	h.handleEvent(w, []byte(body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 0 {
		t.Fatalf("the agent answered its own post through a drifted bot_id: %d replies", len(*posts))
	}
}
