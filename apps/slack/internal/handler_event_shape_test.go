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

// These tests fence handleEvent's field-type tolerance, which is scoped by
// CONSEQUENCE: a single Slack field arriving as an unexpected JSON type must
// not cost a conversation turn, and must never drive the workspace purge.
//
// The two halves are inseparable. Tolerating drift everywhere would trade a
// silent drop for a silent MISFIRE on the uninstall cascade, which is strictly
// worse — and is not hypothetical: three separate fields can redirect that
// purge rather than merely withhold it (see the lifecycle tests below).

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
	if field, ok := jsonFieldTypeDrift(err); !ok || field != "event.bot_id" {
		t.Fatalf("mistyped field classified as field=%q drift=%v, want the bot_id path", field, ok)
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
	if _, ok := jsonFieldTypeDrift(syntaxErr); ok {
		t.Fatalf("body-level parse failure classified as field drift: %v", syntaxErr)
	}

	// A mismatch against the WHOLE document is an UnmarshalTypeError too, but it
	// names no field and populates nothing — classifying it as field drift would
	// make the log claim "routing on the fields that decoded" when none did.
	var bareEnv slackEventEnvelope
	bareErr := json.Unmarshal([]byte(`["not","an","object"]`), &bareEnv)
	if field, ok := jsonFieldTypeDrift(bareErr); ok {
		t.Fatalf("whole-document mismatch classified as drift on field %q: %v", field, bareErr)
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
			// Counting replies is not enough: a bug that refused every turn as
			// unsupported media would also post exactly one. Assert it is the
			// real answer.
			if want := agentLLMReplyWithDisclaimer(testAgentReachStagingReply); (*posts)[0].text != want {
				t.Fatalf("drifted %s got reply %q, want the model's answer %q", tt.name, (*posts)[0].text, want)
			}
		})
	}
}

// TestHandleEvent_SyntaxErrorIsNotTreatedAsDrift is the other side of the
// tolerance: a body that never parsed dispatches nothing AND is not reported as
// drift.
//
// The reply-count half alone is vacuous, which is worth spelling out because it
// looks like the obvious assertion. A syntax error leaves the envelope zero, so
// "no replies" holds no matter which branch runs — misclassifying it would not
// move that number at all. The log line is the only place the classification is
// observable, so that is what has to be asserted.
func TestHandleEvent_SyntaxErrorIsNotTreatedAsDrift(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

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

	got := logBuf.String()
	if strings.Contains(got, "type drift") {
		t.Fatalf("unparseable body reported as field drift, which claims fields decoded when none did: %q", got)
	}
	// The abort branch logs at Warn, not Debug: this path discards an event
	// permanently (the 200 stops Slack redelivering), so it is the failure that
	// most needs to be visible in prod.
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "event JSON parse failed") {
		t.Fatalf("parse failure must be visible at Warn; got %q", got)
	}

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

// TestHandleEvent_DriftNeverDrivesTheWorkspacePurge is the highest-stakes
// guarantee in this change, and the reason the tolerance is scoped by
// consequence rather than applied uniformly.
//
// Every row is a signature-valid teardown that a uniform tolerance WOULD have
// routed, and three of them would have been routed WRONG — this is not a
// precaution against a hypothetical. Each one is annotated with what it would
// have done, because the temptation to "just tolerate everything" is exactly
// what these rows exist to answer.
//
// The rows are not all held up by the same guard, by design. Deleting the
// refusal in handleEvent turns the first two red; the tokens.bot row stays green
// because isBotTokensRevokedEvent independently refuses to count an entry that
// names no token. That is defense in depth, not redundancy — the tokens.bot
// shape can also arrive with NO decode error, where the refusal never applies.
func TestHandleEvent_DriftNeverDrivesTheWorkspacePurge(t *testing.T) {
	const otherWorkspace = testEnterpriseID
	tests := []struct {
		name string
		body string
		// wouldHaveDone records the behavior a uniform tolerance produces, so a
		// future reader can see the cost of relaxing this instead of guessing.
		wouldHaveDone string
	}{
		{
			name: "team_id drift, with an enterprise_id to fall back to",
			body: `{"type":"event_callback","team_id":9,"enterprise_id":"` + otherWorkspace + `","api_app_id":"A1","event_id":"EvDriftTeam","event":{"type":"app_uninstalled"}}`,
			// The nastiest of the three: resolveSlackEventPartitions does not
			// SKIP an unresolvable team id, it SUBSTITUTES enterprise_id. For an
			// identity field, "drifted" and "absent" have different right
			// answers, and the fallback silently picks the wrong one.
			wouldHaveDone: "fully purged the ENTERPRISE partition for a workspace-level uninstall",
		},
		{
			name: "is_enterprise_install drift on a Grid org teardown",
			body: `{"type":"event_callback","team_id":"` + testAdminTeamID + `","enterprise_id":"` + otherWorkspace + `","api_app_id":"A1","event_id":"EvDriftGrid",` +
				`"authorizations":[{"enterprise_id":"` + otherWorkspace + `","team_id":"` + testAdminTeamID + `","is_enterprise_install":"true"}],` +
				`"event":{"type":"app_uninstalled"}}`,
			// The counterexample to "every routing case selects on a string":
			// this one selects on a BOOL, whose zero value is a different and
			// more destructive branch.
			wouldHaveDone: "flipped the org teardown onto the workspace branch — over-deleting the team and never purging the org",
		},
		{
			name: "tokens.bot element drift",
			body: `{"type":"event_callback","team_id":"` + testAdminTeamID + `","api_app_id":"A1","event_id":"EvDriftTok",` +
				`"event":{"type":"tokens_revoked","tokens":{"bot":[{"id":"B1"}]}}}`,
			// A slice is SIZED before its elements decode, so a mistyped element
			// still occupies a slot. len(Bot) > 0 was true here.
			wouldHaveDone: "fabricated a full purge from a payload that named no token",
		},
		{
			// The row a future reader is most likely to find surprising, and the
			// reason it is here: NOTHING about this payload's purge scope is in
			// doubt — team_id is clean and event_time is not even read by the
			// resolver. It is still refused, because narrowing the gate to
			// "fields the scope reads" cannot be done soundly: only the FIRST
			// mismatch is reported while every mismatched field is zeroed, so a
			// payload drifting an ignorable field first would look scope-clean
			// while team_id had already been emptied.
			name: "scope-irrelevant drift on an otherwise clean teardown",
			body: `{"type":"event_callback","team_id":"` + testAdminTeamID + `","api_app_id":"A1","event_id":"EvDriftIrrelevant",` +
				`"event_time":"1700000000","event":{"type":"app_uninstalled"}}`,
			wouldHaveDone: "purged the correct workspace — this row is the accepted COST of the refusal, not a bug it prevents",
		},
		{
			name: "inner event is a string, not an object",
			body: `{"type":"event_callback","team_id":"` + testAdminTeamID + `","api_app_id":"A1","event_id":"EvDriftA","event":"app_uninstalled"}`,
			// This one is safe under either policy — env.Event stays zero, so no
			// teardown is even recognized. Kept as the contrast case.
			wouldHaveDone: "nothing; env.Event stays zero so no teardown is recognized",
		},
		{
			name:          "envelope type drifted away from event_callback",
			body:          `{"type":123,"team_id":"` + testAdminTeamID + `","api_app_id":"A1","event_id":"EvDriftB","event":{"type":"app_uninstalled"}}`,
			wouldHaveDone: "nothing; every PURGE route is gated on env.Type, which drift zeroes — but see TestHandleEvent_DriftedEnvelopeTypeStillNamesTheLostTeardown: this is the case that would otherwise vanish unannounced",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, provider, ts := newLifecycleTestHandler(t)
			// Seed the OTHER workspace too, so a misdirected purge has something
			// real to destroy rather than silently no-op'ing on an absent row.
			ts.seedWorkspace(t, otherWorkspace, testAdminOwnerID, testAdminUserID, testWorkspaceConfiguredAt)
			seedLifecycleAgentState(t, h.cfg.AgentStore, otherWorkspace)

			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, tt.body, tt.body))
			if w.Code != http.StatusOK {
				t.Fatalf("ack code = %d, want 200", w.Code)
			}
			h.Wait()

			if provider.deleteStateCalls != 0 {
				t.Fatalf("drifted teardown deleted %d workspace_state rows (targets %v), want 0 — a uniform tolerance %s",
					provider.deleteStateCalls, provider.deleteStateWorkspaceIDs, tt.wouldHaveDone)
			}
			for _, workspaceID := range []string{testAdminTeamID, otherWorkspace} {
				assertLifecycleAgentStatePresent(t, h.cfg.AgentStore, workspaceID)
				if _, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), workspaceID); err != nil {
					t.Fatalf("workspace %q destroyed by a drifted teardown: %v — a uniform tolerance %s", workspaceID, err, tt.wouldHaveDone)
				}
			}
		})
	}
}

// TestHandleEvent_DroppedTeardownSaysSoAtWarn pins the other half of refusing
// the purge. Dropping a teardown silently is the failure mode this whole PR
// exists to remove, so the refusal has to be louder than what it replaces — the
// operator has to be able to find the workspace and clean it up by hand.
func TestHandleEvent_DroppedTeardownSaysSoAtWarn(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	h, _, _ := newLifecycleTestHandler(t)
	body := `{"type":"event_callback","team_id":9,"enterprise_id":"` + testEnterpriseID + `","api_app_id":"A1","event_id":"EvDropWarn","event":{"type":"app_uninstalled"}}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	h.Wait()
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}

	got := logBuf.String()
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "NOT purged") {
		t.Fatalf("a refused teardown must say so at Warn; got %q", got)
	}
	// And ONLY that line. The generic drift line says we are "routing on the
	// fields that decoded", which directly contradicts refusing to route — two
	// Warns telling an operator opposite things about one event is worse than
	// either alone.
	if strings.Contains(got, "drift tolerated") {
		t.Fatalf("refusal emitted the generic drift line too, which claims the opposite: %q", got)
	}
	// Enough to act on: which teardown, which field moved, and whether there was
	// an id to chase. The ids themselves are not logged in the clear here, which
	// matches the surrounding lifecycle lines.
	for _, want := range []string{"event_type=app_uninstalled", "drift_field=team_id", "has_enterprise_id=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal log missing %q, got %q", want, got)
		}
	}
}

// TestIsBotTokensRevokedEvent_RequiresANamedToken fences a teardown trigger that
// can fire on a payload revoking nothing.
//
// The array is SIZED before its elements are decoded, so a null or mistyped
// entry still occupies a slot — and `[null]` reaches that state with NO decode
// error at all, which means no amount of care at the routing layer would catch
// it. This is why the check is "names a token", not "the list is non-empty".
func TestIsBotTokensRevokedEvent_RequiresANamedToken(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"a real bot token", `{"type":"tokens_revoked","tokens":{"bot":["B123"]}}`, true},
		{"one real token among empties", `{"type":"tokens_revoked","tokens":{"bot":["","B123"]}}`, true},
		{"null element decodes clean but names nothing", `{"type":"tokens_revoked","tokens":{"bot":[null]}}`, false},
		{"empty string names nothing", `{"type":"tokens_revoked","tokens":{"bot":[""]}}`, false},
		{"whitespace names nothing", `{"type":"tokens_revoked","tokens":{"bot":["   "]}}`, false},
		{"mistyped element still occupies a slot", `{"type":"tokens_revoked","tokens":{"bot":[{"id":"B1"}]}}`, false},
		{"empty array", `{"type":"tokens_revoked","tokens":{"bot":[]}}`, false},
		{"tokens drifted to a string (allocates a zero struct)", `{"type":"tokens_revoked","tokens":"revoked"}`, false},
		{"user-token revoke is not a teardown", `{"type":"tokens_revoked","tokens":{"oauth":["U123"]}}`, false},
		{"wrong event type", `{"type":"app_mention","tokens":{"bot":["B123"]}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var event slackInnerEvent
			// The decode error is deliberately ignored: these shapes are exactly
			// the ones that produce one, and the point is what the guard does
			// with the struct that results.
			_ = json.Unmarshal([]byte(tt.raw), &event)
			if got := isBotTokensRevokedEvent(&event); got != tt.want {
				t.Fatalf("isBotTokensRevokedEvent(%s) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestHandleEvent_CleanUninstallStillPurges is the control for the refusal
// above: the drift gate must not have broken the ordinary teardown it sits in
// front of. Without this, "refuse on drift" could silently become "refuse".
func TestHandleEvent_CleanUninstallStillPurges(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)

	body := appUninstalledBody(testAdminTeamID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	if provider.deleteStateCalls != 1 || provider.deleteStateWorkspaceID != testAdminTeamID {
		t.Fatalf("clean uninstall purged %d rows targeting %q, want 1 targeting %q",
			provider.deleteStateCalls, provider.deleteStateWorkspaceID, testAdminTeamID)
	}
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testAdminTeamID)

	_, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), testAdminTeamID)
	var ae *slackdata.Error
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
		t.Fatalf("ListAdmins after clean purge: err = %v, want 404 *Error", err)
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

// TestHandleEvent_DriftWarnLatchesPerField pins the flood control, which until
// now rested only on prose. A schema change drifts EVERY event, so an unlatched
// Warn would bury the operator at request volume in exactly the incident it
// exists to report.
func TestHandleEvent_DriftWarnLatchesPerField(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	// Debug so the demoted repeats are visible; at Warn they would be invisible
	// either way and the test could not tell dropping from demoting.
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	h, _, _ := newAgentEventHandler(t, testAgentReachStagingReply)
	mention := func(eventID string) string {
		return `{"type":"event_callback","team_id":"T1","event_id":"` + eventID + `",` +
			`"event":{"type":"app_mention","user":"U2","channel":"C1","ts":"100.1","text":"<@U12345678> hi"},"event_time":"nope"}`
	}
	// Distinct event ids: same drifted FIELD, different deliveries, so dedupe
	// cannot be what suppresses the second line.
	h.handleEvent(httptest.NewRecorder(), []byte(mention("EvLatch1")))
	h.handleEvent(httptest.NewRecorder(), []byte(mention("EvLatch2")))
	h.handleEvent(httptest.NewRecorder(), []byte(mention("EvLatch3")))
	h.Wait()

	got := logBuf.String()
	if warns := strings.Count(got, `level=WARN msg="event JSON field type drift`); warns != 1 {
		t.Fatalf("same drifted field logged %d Warns across 3 events, want exactly 1: %q", warns, got)
	}
	// Demoted, not dropped — the repeats stay recoverable at Debug.
	if debugs := strings.Count(got, `level=DEBUG msg="event JSON field type drift`); debugs != 2 {
		t.Fatalf("repeat drifts logged %d Debug lines, want 2 (demoted, not discarded): %q", debugs, got)
	}

	// A DIFFERENT field is news again, and must not be swallowed by the latch.
	logBuf.Reset()
	h.handleEvent(httptest.NewRecorder(), []byte(`{"type":"event_callback","team_id":"T1","event_id":"EvLatch4",`+
		`"event":{"type":"app_mention","user":"U2","channel":"C1","ts":"100.1","text":"<@U12345678> hi","bot_id":42}}`))
	h.Wait()
	if !strings.Contains(logBuf.String(), "level=WARN") || !strings.Contains(logBuf.String(), "drift_field=event.bot_id") {
		t.Fatalf("a newly-drifted field must Warn on its own first sighting; got %q", logBuf.String())
	}
}

// TestHandleEvent_DriftedEnvelopeTypeStillNamesTheLostTeardown closes the one
// observability hole the refusal would otherwise leave. When the ENVELOPE type
// is the field that drifted, no purge route can match — so without this the
// event would exit under the generic "routing on the fields that decoded" line,
// and an operator would never learn a teardown had been dropped.
func TestHandleEvent_DriftedEnvelopeTypeStillNamesTheLostTeardown(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	h, provider, _ := newLifecycleTestHandler(t)
	body := `{"type":123,"team_id":"` + testAdminTeamID + `","api_app_id":"A1","event_id":"EvLostTeardown","event":{"type":"app_uninstalled"}}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	h.Wait()
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}

	got := logBuf.String()
	if !strings.Contains(got, "NOT purged") || !strings.Contains(got, "event_type=app_uninstalled") {
		t.Fatalf("a teardown lost to envelope-type drift must be named, not left to the generic line; got %q", got)
	}
	// Still refused, of course — the wider recognition is for the log only.
	if provider.deleteStateCalls != 0 {
		t.Fatalf("recognizing the teardown must not purge it: %d deletes", provider.deleteStateCalls)
	}
}
