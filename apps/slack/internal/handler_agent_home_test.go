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

const injectedBacktickAlias = "ev`il"

type recordingHomePublish struct {
	calls []recordedHomePublish
}

type recordedHomePublish struct {
	userID string
	blocks []any
}

func auditSuccess(v bool) *bool { return &v }

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
	if len(blocks) != 6 { // header + intro + AI-disclosure + support + divider + empty-state
		t.Fatalf("empty view should have 6 blocks, got %d", len(blocks))
	}
	if !blocksContain(t, blocks, agentHomeEmpty) {
		t.Fatal("empty view must carry the empty-state copy")
	}
	if !blocksContain(t, blocks, agentAIDisclosureShort) {
		t.Fatal("Home view must carry the AI disclosure")
	}
	if !blocksContain(t, blocks, agentHomeSupport) {
		t.Fatal("Home view must carry the qURL support link")
	}
}

func TestBuildAgentHomeView_ListsEntriesWithLabels(t *testing.T) {
	entries := []slackdata.AuditEntry{
		{Actor: "U1", Action: string(agent.ActionRevoke), Target: "billing", Channel: "C1", Reason: "stale resource cleanup", Outcome: "Revoked `$billing` and all its qURLs.", Result: "Resource and all of its qURLs were revoked.", ResultSuccess: auditSuccess(true), UnixSec: 1_700_000_100},
		{Actor: "U1", Action: string(agent.ActionProtectURL), Target: "https://docs.example.com", Channel: "C1", Result: "URL protection did not complete because the alias is already bound in this channel; the URL resource is ready.", ResultSuccess: auditSuccess(false), UnixSec: 1_700_000_050},
		{Actor: "U1", Action: string(agent.ActionGet), Target: "staging", Channel: "C1", UnixSec: 1_700_000_000},
	}
	blocks := buildAgentHomeView(entries)
	if len(blocks) != 8 { // 5 chrome (header + intro + disclosure + support + divider) + 3 entries
		t.Fatalf("expected 8 blocks, got %d", len(blocks))
	}
	// Neutral action label (not a success claim) + target.
	if !blocksContain(t, blocks, "Revoke") || !blocksContain(t, blocks, "billing") {
		t.Fatal("the revoke entry must render its neutral label + target")
	}
	if !blocksContain(t, blocks, "Get access") || !blocksContain(t, blocks, "staging") {
		t.Fatal("the get entry must render its neutral label + target")
	}
	if !blocksContain(t, blocks, "Succeeded:") || !blocksContain(t, blocks, "Resource and all of its qURLs were revoked.") {
		t.Fatal("a successful structured result must render cleanly")
	}
	if !blocksContain(t, blocks, "Failed:") || !blocksContain(t, blocks, "URL protection did not complete because the alias is already bound") {
		t.Fatal("a failed structured result must render as a failure")
	}
	if blocksContain(t, blocks, "ˊ") || blocksContain(t, blocks, "∗") {
		t.Fatal("clean structured results must not render degraded substitute glyphs")
	}
	// The audit reason renders (escaped); the formatted Outcome does NOT — its escaped
	// backticks would read degraded, and it is captured in the record only.
	if !blocksContain(t, blocks, "stale resource cleanup") {
		t.Fatal("the entry must render its reason")
	}
	if blocksContain(t, blocks, "Revoked `$billing`") || blocksContain(t, blocks, "Revoked ˊ$billingˊ") {
		t.Fatal("the formatted Outcome must not be echoed in the summary view")
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
		Actor:         "U1",
		Action:        string(agent.ActionSetAlias),
		Target:        injectedBacktickAlias,                              // a raw backtick would break out of the code span
		Reason:        "*boom* <@U99999999> <https://evil.example|click>", // raw mrkdwn would bold, mention, or link
		Result:        "*done* <now>",                                     // stored result still escapes before mrkdwn display
		ResultSuccess: auditSuccess(true),
		Channel:       "C1",
		UnixSec:       1_700_000_000,
	}
	got := agentHomeEntryText(&e)
	if strings.Contains(got, injectedBacktickAlias) {
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
	if strings.Contains(got, "<@U99999999>") || strings.Contains(got, "<https://evil.example|click>") {
		t.Fatalf("reason mentions and masked links must be escaped, got %q", got)
	}
	if !strings.Contains(got, "&lt;@U99999999&gt;") || !strings.Contains(got, "&lt;https://evil.example|click&gt;") {
		t.Fatalf("reason mention/link syntax must render inert, got %q", got)
	}
	if strings.Contains(got, "*done* <now>") {
		t.Fatalf("result text must be escaped, got %q", got)
	}
	if !strings.Contains(got, "∗done∗ &lt;now&gt;") {
		t.Fatalf("the result must be escaped by the text escaper, got %q", got)
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
	res := newAttributedActionResult(true, "Revoked `$metrics`.", "Resource and all of its qURLs were revoked.")
	h.recordAgentAudit(context.Background(), slog.Default(), &payload, pa, &res)

	got, err := store.ListAuditEntries(context.Background(), "T1", "Uadmin", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the executed action must be recorded for the approver, got %d", len(got))
	}
	if got[0].Action != string(agent.ActionRevoke) || got[0].Target != "metrics" || got[0].Outcome != "Revoked `$metrics`." {
		t.Fatalf("recorded entry mismatch: %+v", got[0])
	}
	if got[0].Result != "Resource and all of its qURLs were revoked." || got[0].ResultSuccess == nil || !*got[0].ResultSuccess {
		t.Fatalf("recorded result mismatch: %+v", got[0])
	}
}

func TestRecordAgentAudit_NilStoreSafe(t *testing.T) {
	post, _, _ := capturingPostMessage()
	h := NewHandler(Config{PostMessage: post}) // no AgentStore
	var payload interactionPayload
	payload.Team.ID, payload.User.ID = "T1", "U1"
	// Must not panic with a nil store.
	res := newAttributedActionResult(true, "ok", "Access link was sent privately to the approver.")
	h.recordAgentAudit(context.Background(), slog.Default(), &payload, &pendingAction{Action: agent.ActionGet, Token: "x"}, &res)
}
