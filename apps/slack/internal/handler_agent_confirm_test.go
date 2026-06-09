package internal

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// --- pure-unit coverage ---

func TestAdminGatedFor(t *testing.T) {
	gated := map[agent.ActionKind]bool{
		agent.ActionGet:              false,
		agent.ActionRevoke:           true,
		agent.ActionSetAlias:         true,
		agent.ActionUnsetAlias:       true,
		agent.ActionProtectConnector: true,
		agent.ActionProtectURL:       true,
		agent.ActionKind("mystery"):  true, // unknown kinds fail closed (gated)
	}
	for kind, want := range gated {
		if got := adminGatedFor(kind); got != want {
			t.Errorf("adminGatedFor(%q) = %v, want %v", kind, got, want)
		}
	}
}

func TestNewPendingActionID(t *testing.T) {
	a, err := newPendingActionID()
	if err != nil || len(a) != 32 {
		t.Fatalf("id = %q (len %d) err=%v, want 32 hex chars", a, len(a), err)
	}
	b, _ := newPendingActionID()
	if a == b {
		t.Fatal("two ids must differ (unguessable nonce)")
	}
}

func TestAgentConfirmEnabled(t *testing.T) {
	llm := fakeAgentLLM{}
	store := &slackdata.AgentStore{}
	post := func(context.Context, string, string, string, string, string) error { return nil }
	blocks := func(context.Context, string, string, string, string, []any, string) error { return nil }
	full := Config{AgentLLM: llm, AgentStore: store, PostMessage: post, PostMessageBlocks: blocks, AgentConfirmEnabled: true}
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"fully wired + flag on", full, true},
		{"flag off", Config{AgentLLM: llm, AgentStore: store, PostMessage: post, PostMessageBlocks: blocks}, false},
		{"no blocks seam", Config{AgentLLM: llm, AgentStore: store, PostMessage: post, AgentConfirmEnabled: true}, false},
		{"agent disabled", Config{AgentLLM: llm, AgentStore: store, PostMessage: post, PostMessageBlocks: blocks, AgentConfirmEnabled: true, AgentDisabled: true}, false},
		{"read-only agent off (no llm)", Config{AgentStore: store, PostMessage: post, PostMessageBlocks: blocks, AgentConfirmEnabled: true}, false},
	}
	for _, c := range cases {
		if got := (&Handler{cfg: c.cfg}).agentConfirmEnabled(); got != c.want {
			t.Errorf("%s: agentConfirmEnabled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBuildAgentConfirmBlocks(t *testing.T) {
	blocks := buildAgentConfirmBlocks("Revoke $staging.", "abc123")
	raw, _ := json.Marshal(blocks)
	s := string(raw)
	for _, want := range []string{"Revoke $staging.", agentConfirmApproveActionID, agentConfirmRejectActionID, "abc123"} {
		if !strings.Contains(s, want) {
			t.Errorf("confirm blocks missing %q: %s", want, s)
		}
	}
	// The LLM-distilled summary must render as plain_text (no mrkdwn), so an
	// injected masked link can't surface next to the Approve button.
	if strings.Contains(s, "mrkdwn") {
		t.Errorf("confirm card must not render any mrkdwn (injection surface): %s", s)
	}
}

// --- harness for propose + click orchestration ---

// blocksRecorder captures PostMessageBlocks calls (the confirm card).
type blocksCall struct {
	channel, threadTS, fallback string
	blocks                      []any
}

type blocksRecorder struct{ calls []blocksCall }

func (b *blocksRecorder) fn() PostMessageBlocksFunc {
	return func(_ context.Context, _, _, channel, threadTS string, blocks []any, fallback string) error {
		b.calls = append(b.calls, blocksCall{channel, threadTS, fallback, blocks})
		return nil
	}
}

type confirmHarness struct {
	h       *Handler
	store   *slackdata.AgentStore
	mem     *memAgentDDB
	blocks  *blocksRecorder
	posts   *[]capturedReply
	respURL string
	bodies  *capturedResponseURL
}

// newConfirmHarness wires a confirm-flow Handler: a real AgentStore over an
// in-memory DDB (Put/Load/Claim), an AdminStore over a fakeDDB (CheckAdmin +
// channel-scoped resolve — left with NO channel policies so the cores return a
// userError before any qURL-client call), capturing PostMessage/PostMessageBlocks,
// and a recording response_url server. adminUserID, when non-empty, is seeded as
// the only workspace admin for team T1.
func newConfirmHarness(t *testing.T, adminUserID string) *confirmHarness {
	t.Helper()
	mem := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: mem, TableName: "agent_state", Now: func() time.Time { return fixedNow }}

	names := defaultTestTableNames()
	var seed map[string][]map[string]ddbtypes.AttributeValue
	if adminUserID != "" {
		seed = map[string][]map[string]ddbtypes.AttributeValue{
			names.workspace: {seedWorkspaceAdmin("T1", "Uowner", adminUserID, fixedNow)},
		}
	}
	adminStore := newStoreFromFake(t, newFakeDDB(t, names, seed), names, nil)

	postText, posts, _ := capturingPostMessage()
	blocks := &blocksRecorder{}
	h := NewHandler(Config{
		AgentLLM:            fakeAgentLLM{reply: "x"},
		AgentStore:          store,
		AdminStore:          adminStore,
		PostMessage:         postText,
		PostMessageBlocks:   blocks.fn(),
		AgentConfirmEnabled: true,
	})
	h.validateResponseURLFn = url.Parse
	t.Cleanup(h.Wait)

	bodies := &capturedResponseURL{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies.record(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	return &confirmHarness{h: h, store: store, mem: mem, blocks: blocks, posts: posts, respURL: srv.URL, bodies: bodies}
}

func (hc *confirmHarness) seedPending(t *testing.T, pa *pendingAction) string {
	t.Helper()
	id, err := newPendingActionID()
	if err != nil {
		t.Fatalf("id: %v", err)
	}
	blob, _ := json.Marshal(pa)
	// All confirm-flow tests run under team T1 (the partition the click also reads).
	if err := hc.store.PutPendingAction(context.Background(), "T1", id, blob); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	return id
}

// SK prefixes the slackdata package writes (unexported there); the test inspects
// the in-memory table directly to assert claim/pending state.
const (
	testPendSKPrefix      = "pend#"
	testPendClaimSKPrefix = "pendclaim#"
)

func (hc *confirmHarness) claimed(id string) bool {
	_, ok := hc.mem.items["T1|"+testPendClaimSKPrefix+id]
	return ok
}

func confirmPayload(teamID, channelID, userID, responseURL, id string) *interactionPayload {
	p := &interactionPayload{Type: "block_actions", ResponseURL: responseURL, TriggerID: "trig"}
	p.Team.ID = teamID
	p.Channel.ID = channelID
	p.User.ID = userID
	p.Actions = []interactionAction{{ActionID: agentConfirmApproveActionID, Value: id}}
	return p
}

// parseResponse decodes a captured response_url body into its replace_original
// flag and text. An ephemeral denial has replace_original absent/false; a terminal
// card replacement has it true.
func parseResponse(t *testing.T, body []byte) (replaceOriginal bool, text string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode response body %q: %v", body, err)
	}
	ro, _ := m[respFieldReplaceOriginal].(bool)
	txt, _ := m[respFieldText].(string)
	return ro, txt
}

func revokePending() *pendingAction {
	return &pendingAction{Action: agent.ActionRevoke, Token: "staging", ChannelID: "C1"}
}

// --- click orchestration / security invariants ---

func TestConfirm_ExpiredIsGracefulEphemeral(t *testing.T) {
	hc := newConfirmHarness(t, "Uadmin")
	// No pending action stored → load misses.
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, "ghost"), "ghost", true)

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ro || !strings.Contains(text, "expired") {
		t.Fatalf("expired should be an ephemeral 'expired' reply, got replace=%v text=%q", ro, text)
	}
	if hc.claimed("ghost") {
		t.Fatal("a missing pending action must not be claimed")
	}
}

func TestConfirm_NonAdminCannotExecuteOrClaim(t *testing.T) {
	hc := newConfirmHarness(t, "Uadmin") // Uadmin is the admin; the clicker is not
	id := hc.seedPending(t, revokePending())
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uother", hc.respURL, id), id, true)

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ro || !strings.Contains(strings.ToLower(text), "admin-only") {
		t.Fatalf("non-admin must get an ephemeral admin-only denial, got replace=%v text=%q", ro, text)
	}
	if hc.claimed(id) {
		t.Fatal("a denied non-admin click must NOT claim the pending action (an admin can still approve)")
	}
}

func TestConfirm_AdminCheckErrorFailsClosed(t *testing.T) {
	// No workspace row seeded at all → CheckAdmin errors / returns not-bound; the
	// gated click must be denied (fail-closed), not executed.
	hc := newConfirmHarness(t, "")
	id := hc.seedPending(t, revokePending())
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true)

	ro, _ := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ro {
		t.Fatal("a fail-closed admin check must deny ephemerally, not replace the card")
	}
	if hc.claimed(id) {
		t.Fatal("fail-closed denial must not claim")
	}
}

func TestConfirm_AdminApproveExecutesAndReplaces(t *testing.T) {
	hc := newConfirmHarness(t, "Uadmin")
	id := hc.seedPending(t, revokePending())
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true)

	ro, _ := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if !ro {
		t.Fatal("an authorized approve must replace the card (terminal)")
	}
	if !hc.claimed(id) {
		t.Fatal("an executed approve must have claimed the pending action")
	}
}

func TestConfirm_NonAdminCanApproveGet(t *testing.T) {
	// get is NOT admin-gated, so any channel member may Approve a pending get
	// (privilege-parity with a typed /qurl get) — the non-admin clicker reaches
	// execute and the card is replaced (terminal), unlike the admin-gated revoke a
	// non-admin can't run. This drives the ActionGet branch's Command construction
	// (SubcmdGet + token + reason flag) through executeAgentAction; that the reason
	// then reaches the mint's Create.Reason is covered in handler_get_test.
	hc := newConfirmHarness(t, "Uadmin") // Uadmin is the only admin; the clicker is NOT
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionGet, Token: "staging", Reason: "on-call", ChannelID: "C1"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uother", hc.respURL, id), id, true)

	ro, _ := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if !ro {
		t.Fatal("a non-admin Approve of a get must execute and replace the card (get is not admin-gated)")
	}
	if !hc.claimed(id) {
		t.Fatal("an executed get approve must claim the pending action")
	}
}

func TestConfirm_ConsumeOnceNoDoubleExecute(t *testing.T) {
	hc := newConfirmHarness(t, "Uadmin")
	id := hc.seedPending(t, revokePending())
	payload := confirmPayload("T1", "C1", "Uadmin", hc.respURL, id)

	hc.h.processAgentConfirm(context.Background(), slog.Default(), payload, id, true)
	_ = hc.bodies.waitForBody(t, 2*time.Second)
	// Second click on the same id: already claimed → ephemeral "already handled".
	hc.h.processAgentConfirm(context.Background(), slog.Default(), payload, id, true)

	bodies := hc.waitForN(t, 2)
	ro, text := parseResponse(t, bodies[1])
	if ro || !strings.Contains(strings.ToLower(text), "already handled") {
		t.Fatalf("second approve must be an ephemeral 'already handled', got replace=%v text=%q", ro, text)
	}
}

func TestConfirm_RejectIsGatedAndCancels(t *testing.T) {
	// Reject of an admin-gated action is gated like approve: a non-admin can't
	// cancel an admin's pending action.
	hc := newConfirmHarness(t, "Uadmin")
	id := hc.seedPending(t, revokePending())
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uother", hc.respURL, id), id, false)
	ro, _ := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ro || hc.claimed(id) {
		t.Fatal("a non-admin reject of a gated action must be denied ephemerally and not claim")
	}

	// An admin reject cancels (claims + replaces with Canceled).
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, false)
	bodies := hc.waitForN(t, 2)
	ro, text := parseResponse(t, bodies[1])
	if !ro || !strings.Contains(strings.ToLower(text), "canceled") {
		t.Fatalf("admin reject should replace with Canceled, got replace=%v text=%q", ro, text)
	}
	if !hc.claimed(id) {
		t.Fatal("a reject must claim so a racing approve can't also fire")
	}
}

func TestConfirm_ChannelMismatchRefused(t *testing.T) {
	hc := newConfirmHarness(t, "Uadmin")
	id := hc.seedPending(t, revokePending()) // stored ChannelID = C1
	// Click arrives with a different channel.
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "Cother", "Uadmin", hc.respURL, id), id, true)
	ro, _ := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ro || hc.claimed(id) {
		t.Fatal("a channel-mismatched click must be refused ephemerally and not claim")
	}
}

func (hc *confirmHarness) waitForN(t *testing.T, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		hc.bodies.mu.Lock()
		got := len(hc.bodies.bodies)
		if got >= n {
			out := make([][]byte, n)
			copy(out, hc.bodies.bodies[:n])
			hc.bodies.mu.Unlock()
			return out
		}
		hc.bodies.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("wanted %d response bodies, got %d", n, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- propose side (postAgentConfirm) ---

func TestPostAgentConfirm_StoresPendingAndPostsCard(t *testing.T) {
	hc := newConfirmHarness(t, "")
	prop := &agent.Proposal{Action: agent.ActionRevoke, Token: "staging", Summary: "Revoke $staging.", Reason: "cleanup"}
	env := &slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{Channel: "C1", User: "U2", TS: "100.1"}}

	hc.h.postAgentConfirm(slog.Default(), env, "100.1", prop)

	// LLM-never-executes: proposing only stores + posts a card — the text-reply
	// seam is untouched, and (covered elsewhere) no mutation core runs at propose.
	if len(hc.blocks.calls) != 1 {
		t.Fatalf("want exactly one confirm card, got %d", len(hc.blocks.calls))
	}
	if n := len(*hc.posts); n != 0 {
		t.Fatalf("propose must not post a text reply when the card succeeds, got %d", n)
	}
	// A pending action was stored under the TEAM id.
	pend := 0
	for k := range hc.mem.items {
		if strings.HasPrefix(k, "T1|"+testPendSKPrefix) {
			pend++
		}
	}
	if pend != 1 {
		t.Fatalf("want one stored pending action under team T1, got %d", pend)
	}
}

func TestConfirmExecutable_LockstepWithExecute(t *testing.T) {
	// confirmExecutable, adminGatedFor, and executeAgentAction's switch enumerate
	// ActionKinds independently (all fail-safe), so a future PR4b/PR4c kind could be
	// added to confirmExecutable (card shown) but forgotten in executeAgentAction
	// (falls to the "unsupported" default — a live Approve that no-ops). Pin the
	// invariant: every kind confirmExecutable green-lights is actually handled.
	hc := newConfirmHarness(t, "Uadmin")
	payload := confirmPayload("T1", "C1", "Uadmin", hc.respURL, "x")
	allKinds := []agent.ActionKind{
		agent.ActionGet, agent.ActionRevoke, agent.ActionSetAlias,
		agent.ActionUnsetAlias, agent.ActionProtectConnector, agent.ActionProtectURL,
	}
	handled := 0
	for _, kind := range allKinds {
		if !confirmExecutable(kind) {
			continue
		}
		handled++
		got := hc.h.executeAgentAction(context.Background(), slog.Default(),
			&pendingAction{Action: kind, Token: "staging", ChannelID: "C1"}, payload)
		if got == agentConfirmUnsupportedReply {
			t.Errorf("kind %q is confirmExecutable but executeAgentAction returns unsupported (half-wired)", kind)
		}
	}
	if handled == 0 {
		t.Fatal("expected at least one executable kind")
	}
}

func TestDeliverAgentResult_GatesCardToExecutableKinds(t *testing.T) {
	// The card must render ONLY for an executable proposal with the flag on; a
	// deferred-kind proposal (or flag off, or a plain reply) falls back to the text
	// preview — so a not-yet-wired action never shows a misleading Approve button.
	env := &slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{Channel: "C1", User: "U2", TS: "100.1"}}
	cases := []struct {
		name      string
		result    agent.Result
		confirmOn bool
		wantCard  bool
	}{
		{"executable + flag on → card", agent.Result{Proposal: &agent.Proposal{Action: agent.ActionRevoke, Summary: "Revoke $x."}}, true, true},
		{"deferred + flag on → preview", agent.Result{Proposal: &agent.Proposal{Action: agent.ActionSetAlias, Summary: "Bind $x."}}, true, false},
		{"executable + flag off → preview", agent.Result{Proposal: &agent.Proposal{Action: agent.ActionRevoke, Summary: "Revoke $x."}}, false, false},
		{"plain reply → preview", agent.Result{Reply: "hello"}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hc := newConfirmHarness(t, "")
			hc.h.cfg.AgentConfirmEnabled = c.confirmOn
			res := c.result
			hc.h.deliverAgentResult(slog.Default(), env, "100.1", &res)

			gotCard := len(hc.blocks.calls) == 1
			gotPreview := len(*hc.posts) == 1
			if gotCard != c.wantCard {
				t.Fatalf("card posted = %v, want %v", gotCard, c.wantCard)
			}
			if gotPreview == c.wantCard { // exactly one of card/preview must fire
				t.Fatalf("expected exactly one of card(%v)/preview(%v)", gotCard, gotPreview)
			}
		})
	}
}

func TestPostAgentConfirm_EscapesFallbackText(t *testing.T) {
	// The card section is plain_text, but the fallbackText is the message's
	// top-level mrkdwn notification text — an injected masked link must not survive
	// there either (it would show in the push/notification preview).
	hc := newConfirmHarness(t, "")
	prop := &agent.Proposal{Action: agent.ActionRevoke, Token: "x", Summary: "Revoke <http://evil|click here>."}
	env := &slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{Channel: "C1", User: "U2", TS: "100.1"}}
	hc.h.postAgentConfirm(slog.Default(), env, "100.1", prop)

	if len(hc.blocks.calls) != 1 {
		t.Fatalf("want one card, got %d", len(hc.blocks.calls))
	}
	if fb := hc.blocks.calls[0].fallback; strings.ContainsAny(fb, "<>") {
		t.Fatalf("fallback text must be mrkdwn-escaped (no raw <>): %q", fb)
	}
}

func TestPostAgentConfirm_BlankSummaryFallsBack(t *testing.T) {
	hc := newConfirmHarness(t, "")
	prop := &agent.Proposal{Action: agent.ActionRevoke, Token: "staging", Summary: "   "}
	env := &slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{Channel: "C1", User: "U2", TS: "100.1"}}
	hc.h.postAgentConfirm(slog.Default(), env, "100.1", prop)

	if len(hc.blocks.calls) != 0 {
		t.Fatal("a blank summary must not post a card")
	}
	if len(*hc.posts) != 1 || (*hc.posts)[0].text != agentErrorReply {
		t.Fatalf("blank summary should fall back to the error reply, got %+v", *hc.posts)
	}
}
