package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

type recordingHomePublish struct {
	calls []recordedHomePublish
}

type recordedHomePublish struct {
	userID string
	blocks []any
}

func (r *recordingHomePublish) fn(_ context.Context, _, _, userID string, blocks []any) error {
	r.calls = append(r.calls, recordedHomePublish{userID: userID, blocks: blocks})
	return nil
}

func newHomeHandler(t *testing.T, store *slackdata.AgentStore, pub AppHomePublishFunc) *Handler {
	t.Helper()
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:            fakeAgentLLM{reply: "x"},
		AgentStore:          store,
		PostMessage:         post,
		AppHomePublish:      pub,
		AgentDefaultEnabled: true,
	})
	t.Cleanup(h.Wait)
	return h
}

// blocksContain serializes a Home view's blocks and reports whether want appears.
// HTML escaping is disabled so a Slack mention like "<#C1>" matches literally (Slack
// decodes the < the production marshal would emit, so the wire is unaffected).
func blocksContain(t *testing.T, blocks []any, want string) bool {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(blocks); err != nil {
		t.Fatalf("encode blocks: %v", err)
	}
	return strings.Contains(buf.String(), want)
}

func TestBuildAgentHomeView_EmptyState(t *testing.T) {
	blocks := buildAgentHomeView(nil)
	if len(blocks) != 4 { // header + intro + divider + empty-state
		t.Fatalf("empty view should have 4 blocks, got %d", len(blocks))
	}
	if !blocksContain(t, blocks, agentHomeEmpty) {
		t.Fatal("empty view must carry the empty-state copy")
	}
}

func TestBuildAgentHomeView_ListsEntriesWithLabels(t *testing.T) {
	entries := []slackdata.AuditEntry{
		{Actor: "U1", Action: string(agent.ActionRevoke), Target: "billing", Channel: "C1", Outcome: "revoked the qURL and its links", UnixSec: 1_700_000_100},
		{Actor: "U1", Action: string(agent.ActionGet), Target: "staging", Channel: "C1", UnixSec: 1_700_000_000},
	}
	blocks := buildAgentHomeView(entries)
	if len(blocks) != 5 { // 3 chrome + 2 entries
		t.Fatalf("expected 5 blocks, got %d", len(blocks))
	}
	// Neutral action label (not a success claim) + target.
	if !blocksContain(t, blocks, "Revoke") || !blocksContain(t, blocks, "billing") {
		t.Fatal("the revoke entry must render its neutral label + target")
	}
	if !blocksContain(t, blocks, "Get access") || !blocksContain(t, blocks, "staging") {
		t.Fatal("the get entry must render its neutral label + target")
	}
	// The captured Outcome (the real result) is rendered, so a failure would read honestly.
	if !blocksContain(t, blocks, "revoked the qURL and its links") {
		t.Fatal("the entry must render its captured Outcome")
	}
	// Channel renders as a Slack mention, not the raw id text.
	if !blocksContain(t, blocks, "<#C1>") {
		t.Fatal("the channel must render as a <#…> mention")
	}
}

// The Home view is a public-echo surface: a stored Target/Reason that's partly
// LLM-distilled must render INERT — escaped exactly as the confirm card escapes it.
func TestAgentHomeEntryText_EscapesInjectedEcho(t *testing.T) {
	e := slackdata.AuditEntry{
		Actor:   "U1",
		Action:  string(agent.ActionSetAlias),
		Target:  "ev`il",  // a raw backtick would break out of the code span
		Reason:  "*boom*", // raw asterisks would bold the surrounding text
		Channel: "C1",
		UnixSec: 1_700_000_000,
	}
	got := agentHomeEntryText(&e)
	if strings.Contains(got, "ev`il") {
		t.Fatalf("a backtick in the target must be escaped, got %q", got)
	}
	if !strings.Contains(got, "evˊil") {
		t.Fatalf("the target backtick must be replaced by the code escaper, got %q", got)
	}
	if strings.Contains(got, "*boom*") {
		t.Fatalf("asterisks in the reason must be escaped, got %q", got)
	}
	if !strings.Contains(got, "∗boom∗") {
		t.Fatalf("the reason asterisks must be replaced by the text escaper, got %q", got)
	}
}

func TestHandleAppHomeOpened_PublishesViewerActions(t *testing.T) {
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	if err := store.PutAuditEntry(context.Background(), "T1", &slackdata.AuditEntry{
		Actor: "U1", Action: string(agent.ActionRevoke), Target: "analytics", Channel: "C1",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pub := &recordingHomePublish{}
	h := newHomeHandler(t, store, pub.fn)

	h.handleAppHomeOpened(&slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{Type: slackEventTypeAppHomeOpened, User: "U1", Tab: homeTabName}})
	h.Wait()

	if len(pub.calls) != 1 {
		t.Fatalf("a home-tab open should publish once, got %d", len(pub.calls))
	}
	if pub.calls[0].userID != "U1" {
		t.Fatalf("must publish to the opening user, got %q", pub.calls[0].userID)
	}
	if !blocksContain(t, pub.calls[0].blocks, "analytics") {
		t.Fatal("the published view must list the user's own action")
	}
}

func TestHandleAppHomeOpened_IgnoresNonHomeTabAndEmptyUser(t *testing.T) {
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	pub := &recordingHomePublish{}
	h := newHomeHandler(t, store, pub.fn)

	// A "messages" tab open carries no review surface; an empty user is malformed.
	h.handleAppHomeOpened(&slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{Type: slackEventTypeAppHomeOpened, User: "U1", Tab: "messages"}})
	h.handleAppHomeOpened(&slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{Type: slackEventTypeAppHomeOpened, User: "", Tab: homeTabName}})
	h.Wait()

	if len(pub.calls) != 0 {
		t.Fatalf("neither a non-home tab nor an empty user should publish, got %d", len(pub.calls))
	}
}

func TestRecordAgentAudit_PersistsForApprover(t *testing.T) {
	store := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	h := newHomeHandler(t, store, (&recordingHomePublish{}).fn)

	var payload interactionPayload
	payload.Team.ID, payload.Channel.ID, payload.User.ID = "T1", "C1", "Uadmin"
	pa := &pendingAction{Action: agent.ActionRevoke, Token: "metrics", Reason: "cleanup"}
	h.recordAgentAudit(context.Background(), slog.Default(), &payload, pa, "revoked $metrics")

	got, err := store.ListAuditEntries(context.Background(), "T1", "Uadmin", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the executed action must be recorded for the approver, got %d", len(got))
	}
	if got[0].Action != string(agent.ActionRevoke) || got[0].Target != "metrics" || got[0].Outcome != "revoked $metrics" {
		t.Fatalf("recorded entry mismatch: %+v", got[0])
	}
}

func TestRecordAgentAudit_NilStoreSafe(t *testing.T) {
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{PostMessage: post}) // no AgentStore
	var payload interactionPayload
	payload.Team.ID, payload.User.ID = "T1", "U1"
	// Must not panic with a nil store.
	h.recordAgentAudit(context.Background(), slog.Default(), &payload, &pendingAction{Action: agent.ActionGet, Token: "x"}, "ok")
}
