package internal

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
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
	eph     *[]capturedEphemeral
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
	return newConfirmHarnessWithSeed(t, adminUserID, nil)
}

func newConfirmHarnessWithSeed(t *testing.T, adminUserID string, seed map[string][]map[string]ddbtypes.AttributeValue) *confirmHarness {
	t.Helper()
	mem := newMemAgentDDB()
	store := &slackdata.AgentStore{Client: mem, TableName: "agent_state", Now: func() time.Time { return fixedNow }}

	names := defaultTestTableNames()
	if adminUserID != "" {
		if seed == nil {
			seed = map[string][]map[string]ddbtypes.AttributeValue{}
		}
		seed[names.workspace] = append(seed[names.workspace], seedWorkspaceAdmin("T1", "Uowner", adminUserID, fixedNow))
	}
	adminStore := newStoreFromFake(t, newFakeDDB(t, names, seed), names, nil)

	// qURL client + alias store, so the alias/get cores can execute (the get/revoke
	// tests use an empty channel and short-circuit before the client; set-alias
	// resolves a slug through it). qurlBackendServer returns r_2 for slug "staging".
	t.Setenv("QURL_API_KEY", "test-key")
	qurlSrv := qurlBackendServer(t)

	postText, posts, _ := capturingPostMessage()
	postEph, eph, _ := capturingPostEphemeral()
	blocks := &blocksRecorder{}
	h := NewHandler(Config{
		AgentLLM:            fakeAgentLLM{reply: "x"},
		AgentStore:          store,
		AdminStore:          adminStore,
		AuthProvider:        &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		NewClient:           func(apiKey string) *client.Client { return client.New(qurlSrv.URL, apiKey, client.WithRetry(0)) },
		PostMessage:         postText,
		PostEphemeral:       postEph,
		PostMessageBlocks:   blocks.fn(),
		AgentConfirmEnabled: true,
		// Per-workspace toggle defaults ON in the harness (conversation mode is
		// enabled); the toggle-specific tests seed an explicit per-workspace value.
		AgentDefaultEnabled: true,
	})
	h.SetAliasStore(adminStore)
	h.validateResponseURLFn = url.Parse
	t.Cleanup(h.Wait)

	bodies := &capturedResponseURL{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies.record(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	return &confirmHarness{h: h, store: store, mem: mem, blocks: blocks, posts: posts, eph: eph, respURL: srv.URL, bodies: bodies}
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

// pendingID returns the id of the single pending action stored for teamID — for
// tests that need to load the snapshot postAgentConfirm generated internally.
func (hc *confirmHarness) pendingID(t *testing.T, teamID string) string {
	t.Helper()
	prefix := teamID + "|" + testPendSKPrefix
	var ids []string
	for k := range hc.mem.items {
		if strings.HasPrefix(k, prefix) {
			ids = append(ids, strings.TrimPrefix(k, prefix))
		}
	}
	if len(ids) != 1 {
		t.Fatalf("want exactly one pending action for %s, got %d", teamID, len(ids))
	}
	return ids[0]
}

func requireSingleAuditEntry(t *testing.T, hc *confirmHarness, userID string) slackdata.AuditEntry {
	t.Helper()
	got, err := hc.store.ListAuditEntries(context.Background(), "T1", userID, 10)
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want one audit entry for %s, got %d: %+v", userID, len(got), got)
	}
	return got[0]
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
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, "ghost"), "ghost", true, time.Now())

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
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uother", hc.respURL, id), id, true, time.Now())

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
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, time.Now())

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
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, time.Now())

	ro, _ := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if !ro {
		t.Fatal("an authorized approve must replace the card (terminal)")
	}
	if !hc.claimed(id) {
		t.Fatal("an executed approve must have claimed the pending action")
	}
}

func TestConfirm_GetApproveByAskerIsEphemeral(t *testing.T) {
	// get is NOT admin-gated but IS asker-only: the requesting member (Asker) may
	// Approve their own get, and its result is a one-time-use credential delivered
	// PRIVATELY to the clicker (== asker), never broadcast on the public card.
	hc := newConfirmHarness(t, "Uadmin") // Uadmin is the only admin; the asker/clicker is Uother
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionGet, Token: "staging", Reason: "on-call", Asker: "Uother", ChannelID: "C1", ThreadTS: "111.2"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uother", hc.respURL, id), id, true, time.Now())
	hc.h.Wait()

	// The asker (a non-admin) reaching execute proves get isn't admin-gated (it claimed).
	if !hc.claimed(id) {
		t.Fatal("the asker (non-admin) must be able to approve+execute their own get")
	}
	// In a channel the token-bearing detail goes to the clicker via a STANDALONE
	// chat.postEphemeral (scoped to them, threaded to the card) — NOT the response_url,
	// which the card-replace would overwrite.
	eph := *hc.eph
	if len(eph) != 1 {
		t.Fatalf("want exactly one channel ephemeral delivery, got %d: %+v", len(eph), eph)
	}
	if eph[0].channel != "C1" || eph[0].userID != "Uother" || eph[0].threadTS != "111.2" || eph[0].text == "" {
		t.Fatalf("channel ephemeral must target the channel/clicker/thread with the detail, got %+v", eph[0])
	}
	// Exactly one response_url POST — the neutral public card (replace_original true). This
	// get FAILS (empty-channel resolve), so the card is the FAILURE copy: never the old
	// unconditional "sent privately", and never an echo of the token-bearing detail.
	ro, card := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if !ro {
		t.Fatalf("the public card must replace the original, got ephemeral %q", card)
	}
	if !strings.Contains(card, agentConfirmGetFailedReply) {
		t.Fatalf("a failed get must show the neutral failure card, got %q", card)
	}
	if strings.Contains(card, "staging") || strings.Contains(card, eph[0].text) {
		t.Fatalf("public card leaked the get detail: %q", card)
	}
	hc.bodies.mu.Lock()
	n := len(hc.bodies.bodies)
	hc.bodies.mu.Unlock()
	if n != 1 {
		t.Fatalf("a channel get must POST only the card to response_url, got %d bodies", n)
	}
}

func TestConfirm_GetNonAskerDeniedThenAskerSucceeds(t *testing.T) {
	// The asker-only gate is BEFORE the claim: a non-asker click is denied and claims
	// NOTHING, so the asker can STILL approve. If the gate were after the claim, the
	// non-asker click would consume the pending get and permanently burn the asker's
	// request — a DoS. This sequential assertion is what fails if the gate ever moves
	// below the claim.
	hc := newConfirmHarness(t, "Uadmin")
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionGet, Token: "staging", Reason: "r", Asker: "Uasker", ChannelID: "C1"})

	// Non-asker Approve → denied ephemerally, nothing claimed.
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uother", hc.respURL, id), id, true, time.Now())
	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ro || text != agentConfirmGetNotAskerReply {
		t.Fatalf("non-asker get Approve must be denied ephemerally; replace=%v text=%q", ro, text)
	}
	if hc.claimed(id) {
		t.Fatal("a non-asker click must NOT claim the asker's get (it would burn the request)")
	}

	// The asker then approves and succeeds — proves the non-asker click didn't consume it.
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uasker", hc.respURL, id), id, true, time.Now())
	if !hc.claimed(id) {
		t.Fatal("the asker must still be able to approve after a non-asker was denied")
	}
}

func TestConfirm_GetNonAskerRejectDenied(t *testing.T) {
	// The asker-only gate covers Reject too: a non-asker can't dismiss the asker's get
	// card out from under them (claims nothing; the asker can still act, or the 10-min
	// TTL reaps an abandoned card).
	hc := newConfirmHarness(t, "Uadmin")
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionGet, Token: "staging", Asker: "Uasker", ChannelID: "C1"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uother", hc.respURL, id), id, false, time.Now())

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ro || text != agentConfirmGetNotAskerReply {
		t.Fatalf("non-asker get Reject must be denied ephemerally; replace=%v text=%q", ro, text)
	}
	if hc.claimed(id) {
		t.Fatal("a non-asker Reject must not claim")
	}
}

func TestConfirm_GetApproveInDMMintsAndDeliversLinkInThread(t *testing.T) {
	// This is the end-to-end success path #726 wanted: resolve `$staging`, mint a
	// resource-scoped qURL, deliver the resulting link via the in-DM PostMessage path
	// (not response_url, not chat.postEphemeral), and replace the public card with
	// neutral success copy that never echoes the credential.
	names := defaultTestTableNames()
	hc := newConfirmHarnessWithSeed(t, "Uadmin", map[string][]map[string]ddbtypes.AttributeValue{
		names.channelPolicy: {
			// No alias binding: this forces resolveTokenForGet through the tunnel-slug
			// fallback and then the qURL mint endpoint, rather than returning before the
			// client call.
			seedChannelPolicySet("T1", "D1", "", []string{testAgentGetResourceID}),
		},
	})
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionGet, Token: "staging", Reason: "incident follow-up", Asker: "Uasker", ChannelID: "D1", ThreadTS: "1700000000.5"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "D1", "Uasker", hc.respURL, id), id, true, time.Now())
	hc.h.Wait()

	if !hc.claimed(id) {
		t.Fatal("the asker must claim and execute their own get")
	}
	posts := *hc.posts
	if len(posts) != 1 {
		t.Fatalf("want exactly one in-DM PostMessage delivery, got %d: %+v", len(posts), posts)
	}
	if posts[0].channel != "D1" || posts[0].threadTS != "1700000000.5" {
		t.Fatalf("in-DM link delivery must target the card's channel+thread, got channel=%q thread=%q", posts[0].channel, posts[0].threadTS)
	}
	if !strings.Contains(posts[0].text, testAgentGetQURLLink) {
		t.Fatalf("in-DM delivery missing minted link %q: %q", testAgentGetQURLLink, posts[0].text)
	}
	if !strings.Contains(posts[0].text, "one-time use") || !strings.Contains(posts[0].text, "link expires in "+resourceLinkExpiryHuman) {
		t.Fatalf("in-DM delivery missing one-time-use/expiry copy: %q", posts[0].text)
	}
	if len(*hc.eph) != 0 {
		t.Fatalf("a 1:1 DM success must not use chat.postEphemeral, got %+v", *hc.eph)
	}

	bodies := hc.waitForN(t, 1)
	hc.bodies.mu.Lock()
	bodyCount := len(hc.bodies.bodies)
	hc.bodies.mu.Unlock()
	if bodyCount != 1 {
		t.Fatalf("a DM success must POST only the public card to response_url, got %d bodies", bodyCount)
	}
	ro, card := parseResponse(t, bodies[0])
	if !ro {
		t.Fatalf("the DM success card must replace the original, got ephemeral %q", card)
	}
	if !strings.Contains(card, agentConfirmGetDeliveredReply) {
		t.Fatalf("public card must show neutral delivery success, got %q", card)
	}
	if strings.Contains(card, testAgentGetQURLLink) || strings.Contains(card, "staging") {
		t.Fatalf("public card leaked the minted link or token: %q", card)
	}
}

func TestConfirm_GetApproveInDMDeliversInThread(t *testing.T) {
	// In a 1:1 DM the get's sensitive output (here the failure detail; a link on success)
	// must be delivered as a NORMAL message via PostMessage — ephemerals don't render in a
	// DM — and into the CARD'S OWN THREAD (pa.ThreadTS): the assistant pane is a threaded
	// view, so a top-level post would land out of sight (the original "I never saw the
	// link" bug). The public response_url carries only the neutral card; the token-bearing
	// detail never touches it.
	hc := newConfirmHarness(t, "Uadmin")
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionGet, Token: "staging", Reason: "r", Asker: "Uasker", ChannelID: "D1", ThreadTS: "1700000000.5"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "D1", "Uasker", hc.respURL, id), id, true, time.Now())
	hc.h.Wait()

	// The sensitive detail went via PostMessage, into the card's channel+thread.
	posts := *hc.posts
	if len(posts) != 1 {
		t.Fatalf("want exactly one in-DM PostMessage delivery, got %d: %+v", len(posts), posts)
	}
	if posts[0].channel != "D1" || posts[0].threadTS != "1700000000.5" {
		t.Fatalf("in-DM delivery must target the card's channel+thread, got channel=%q thread=%q", posts[0].channel, posts[0].threadTS)
	}
	if posts[0].text == "" {
		t.Fatal("in-DM delivery carried no detail text")
	}
	// Exactly one response_url POST — the card — and NO ephemeral (a DM ephemeral would
	// silently not render; that was the bug).
	hc.bodies.mu.Lock()
	n := len(hc.bodies.bodies)
	hc.bodies.mu.Unlock()
	if n != 1 {
		t.Fatalf("a DM get must POST only the card to response_url (no ephemeral), got %d bodies", n)
	}
	ro, card := parseResponse(t, hc.waitForN(t, 1)[0])
	if !ro {
		t.Fatalf("the DM card must replace the original, got ephemeral %q", card)
	}
	if strings.Contains(card, "staging") || strings.Contains(card, posts[0].text) {
		t.Fatalf("public card leaked the get detail: %q", card)
	}
}

func TestDeliverConfirmPrivate_RoutesBySurface(t *testing.T) {
	// The success payload (a one-time link) routes by surface: a normal in-thread message
	// in a 1:1 DM (PostMessage), a STANDALONE ephemeral in a channel (chat.postEphemeral,
	// NOT the click's response_url) — never both, so a one-time link never lands in shared
	// history and the card-replace can't overwrite it.
	const link = ":link: *qURL ready:* https://qurl.link/abc123 (one-time use)"

	t.Run("1:1 DM posts the link as a normal in-thread message", func(t *testing.T) {
		hc := newConfirmHarness(t, "")
		pa := &pendingAction{Action: agent.ActionGet, ChannelID: "D1", ThreadTS: "1700.9"}
		payload := confirmPayload("T1", "D1", "Uasker", hc.respURL, "id")
		if !hc.h.deliverConfirmPrivate(context.Background(), slog.Default(), pa, payload, link) {
			t.Fatal("DM delivery should report success")
		}
		hc.h.Wait()
		posts := *hc.posts
		if len(posts) != 1 || posts[0].channel != "D1" || posts[0].threadTS != "1700.9" || posts[0].text != link {
			t.Fatalf("the link must post to the DM channel+thread verbatim, got %+v", posts)
		}
		if len(*hc.eph) != 0 {
			t.Fatalf("a DM delivery must not use chat.postEphemeral, got %+v", *hc.eph)
		}
		hc.bodies.mu.Lock()
		n := len(hc.bodies.bodies)
		hc.bodies.mu.Unlock()
		if n != 0 {
			t.Fatalf("a DM delivery must not POST to response_url, got %d", n)
		}
	})

	t.Run("channel delivers the link as a standalone ephemeral scoped to the clicker", func(t *testing.T) {
		hc := newConfirmHarness(t, "")
		pa := &pendingAction{Action: agent.ActionGet, ChannelID: "C1", ThreadTS: "1700.7"}
		payload := confirmPayload("T1", "C1", "Uasker", hc.respURL, "id")
		if !hc.h.deliverConfirmPrivate(context.Background(), slog.Default(), pa, payload, link) {
			t.Fatal("channel delivery should report success")
		}
		hc.h.Wait()
		eph := *hc.eph
		if len(eph) != 1 || eph[0].channel != "C1" || eph[0].userID != "Uasker" || eph[0].threadTS != "1700.7" || eph[0].text != link {
			t.Fatalf("the link must post via chat.postEphemeral to the channel/clicker/thread verbatim, got %+v", eph)
		}
		if len(*hc.posts) != 0 {
			t.Fatalf("channel delivery must not use PostMessage, got %+v", *hc.posts)
		}
		hc.bodies.mu.Lock()
		n := len(hc.bodies.bodies)
		hc.bodies.mu.Unlock()
		if n != 0 {
			t.Fatalf("channel delivery must NOT touch the response_url (the card-replace would overwrite it), got %d bodies", n)
		}
	})

	t.Run("DM reports failure when PostMessage errors", func(t *testing.T) {
		h := NewHandler(Config{PostMessage: func(context.Context, string, string, string, string, string) error {
			return errors.New("boom")
		}})
		pa := &pendingAction{ChannelID: "D1", ThreadTS: "t"}
		payload := confirmPayload("T1", "D1", "Uasker", "", "id")
		if h.deliverConfirmPrivate(context.Background(), slog.Default(), pa, payload, link) {
			t.Fatal("a failing in-DM PostMessage must report delivery failure so the card stops claiming success")
		}
	})

	t.Run("DM reports failure when the PostMessage seam is nil", func(t *testing.T) {
		h := NewHandler(Config{})
		pa := &pendingAction{ChannelID: "D1"}
		payload := confirmPayload("T1", "D1", "Uasker", "", "id")
		if h.deliverConfirmPrivate(context.Background(), slog.Default(), pa, payload, link) {
			t.Fatal("a nil PostMessage seam must report delivery failure")
		}
	})

	t.Run("channel reports failure when chat.postEphemeral errors", func(t *testing.T) {
		h := NewHandler(Config{PostEphemeral: func(context.Context, string, string, string, string, string, string) error {
			return errors.New("user_not_in_channel")
		}})
		pa := &pendingAction{ChannelID: "C1", ThreadTS: "t"}
		payload := confirmPayload("T1", "C1", "Uasker", "", "id")
		if h.deliverConfirmPrivate(context.Background(), slog.Default(), pa, payload, link) {
			t.Fatal("a failing chat.postEphemeral must report delivery failure")
		}
	})

	t.Run("channel reports failure when the PostEphemeral seam is nil", func(t *testing.T) {
		h := NewHandler(Config{})
		pa := &pendingAction{ChannelID: "C1"}
		payload := confirmPayload("T1", "C1", "Uasker", "", "id")
		if h.deliverConfirmPrivate(context.Background(), slog.Default(), pa, payload, link) {
			t.Fatal("a nil PostEphemeral seam must report delivery failure")
		}
	})
}

func TestComposeConfirmCard(t *testing.T) {
	const (
		asker    = "Uasker"
		approver = "Uapprover"
		link     = ":link: qURL ready: https://qurl.link/abc"
	)
	cases := []struct {
		name      string
		res       actionResult
		delivered bool
		wantCard  string // the card must contain this
		notText   string // the card must NOT contain this (no leak)
	}{
		{
			// The central guarantee: mint succeeded (DeliveredReply + link) but the private
			// delivery failed → the card must stop claiming success and never echo the link.
			name:      "successful get whose delivery failed downgrades to delivery-failed",
			res:       actionResult{cardText: agentConfirmGetDeliveredReply, ephemeralText: link, attributed: true},
			delivered: false,
			wantCard:  agentConfirmGetDeliveryFailedReply,
			notText:   link,
		},
		{
			name:      "successful get delivered keeps the success card and never echoes the link",
			res:       actionResult{cardText: agentConfirmGetDeliveredReply, ephemeralText: link, attributed: true},
			delivered: true,
			wantCard:  agentConfirmGetDeliveredReply,
			notText:   link,
		},
		{
			name:      "failed get keeps the failure card even when its detail was not delivered",
			res:       actionResult{cardText: agentConfirmGetFailedReply, ephemeralText: ":warning: staging", attributed: true},
			delivered: false,
			wantCard:  agentConfirmGetFailedReply,
			notText:   "staging",
		},
		{
			name:      "non-get executed action is untouched by the delivery flag",
			res:       actionResult{cardText: "revoked $staging", attributed: true},
			delivered: true,
			wantCard:  "revoked $staging",
		},
		{
			name:      "pre-execution rejection stays byte-exact (unattributed)",
			res:       actionResult{cardText: agentConfirmInvalidAliasReply},
			delivered: true,
			wantCard:  agentConfirmInvalidAliasReply,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := composeConfirmCard(c.res, c.delivered, asker, approver)
			if !strings.Contains(got, c.wantCard) {
				t.Fatalf("card = %q, want to contain %q", got, c.wantCard)
			}
			if c.notText != "" && strings.Contains(got, c.notText) {
				t.Fatalf("card leaked %q: %q", c.notText, got)
			}
			if c.res.attributed && !strings.Contains(got, asker) {
				t.Fatalf("an executed (attributed) card must carry the attribution footer, got %q", got)
			}
			if !c.res.attributed && got != c.wantCard {
				t.Fatalf("an unattributed card must stay byte-exact, got %q want %q", got, c.wantCard)
			}
		})
	}
}

func TestPostAgentConfirm_SnapshotsAsker(t *testing.T) {
	// The pending snapshot must carry the asker (env.Event.User) so the click can
	// enforce asker-only for a get.
	hc := newConfirmHarness(t, "")
	prop := &agent.Proposal{Action: agent.ActionGet, Token: "staging", Reason: "r", Summary: "Get a link to $staging."}
	const threadTS = "100.1"
	env := &slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{Channel: "C1", User: "Uasker", TS: threadTS}}
	hc.h.postAgentConfirm(slog.Default(), env, threadTS, prop)

	blob, found, err := hc.store.LoadPendingAction(context.Background(), "T1", hc.pendingID(t, "T1"))
	if err != nil || !found {
		t.Fatalf("pending not stored: found=%v err=%v", found, err)
	}
	var pa pendingAction
	if err := json.Unmarshal(blob, &pa); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pa.Asker != "Uasker" {
		t.Fatalf("snapshot Asker = %q, want Uasker", pa.Asker)
	}
	// The thread the card is posted into is snapshotted too, so the get's link can be
	// delivered back into that exact thread (the assistant pane is a threaded view).
	if pa.ThreadTS != threadTS {
		t.Fatalf("snapshot ThreadTS = %q, want %q", pa.ThreadTS, threadTS)
	}
}

func TestConfirm_SetAliasOnApprove(t *testing.T) {
	// set-alias is admin-gated and direct-execute (mirrors revoke). An admin approve
	// runs the alias core in the click's channel and replaces the public card with
	// the (benign) result. The binding/resolution correctness is covered in
	// handler_alias_test; here we pin the confirm orchestration of the new case —
	// it claims, executes the real core (not the unsupported default), and replaces.
	hc := newConfirmHarness(t, "Uadmin")
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionSetAlias, Alias: "oncall", Target: "staging", ChannelID: "C1"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, time.Now())

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if !ro || !hc.claimed(id) {
		t.Fatalf("admin set-alias approve should execute (claim) and replace the card; replace=%v text=%q", ro, text)
	}
	if text == agentConfirmUnsupportedReply || text == "" {
		t.Fatalf("set-alias must run the alias core, not the unsupported default; got %q", text)
	}
}

func TestAgentConfirmAttributedCard(t *testing.T) {
	const body = "Revoked $staging."
	// asker != approver → both mentions + the agent name, body preserved as prefix.
	out := agentConfirmAttributedCard(body, "Uasker", "Uadmin")
	for _, want := range []string{"<@Uasker>", "<@Uadmin>", agentAttributionAgentName, "Requested by", "approved by"} {
		if !strings.Contains(out, want) {
			t.Fatalf("attributed card missing %q: %q", want, out)
		}
	}
	if !strings.HasPrefix(out, body) {
		t.Fatalf("attribution must preserve the result text as a prefix: %q", out)
	}
	// asker == approver (a get, or an admin approving their own request) → one
	// mention, no redundant "approved by".
	self := agentConfirmAttributedCard(body, "Uself", "Uself")
	if !strings.Contains(self, "<@Uself>") || strings.Contains(self, "approved by") {
		t.Fatalf("self-approved card should name one person without 'approved by': %q", self)
	}
	// Defensive empty asker → still marks the agent, with no dangling mention.
	none := agentConfirmAttributedCard(body, "", "Uadmin")
	if !strings.Contains(none, agentAttributionAgentName) || strings.Contains(none, "<@") {
		t.Fatalf("empty-asker card should mark the agent with no mention: %q", none)
	}
}

func TestConfirm_ExecutedCardCarriesAttribution(t *testing.T) {
	// An EXECUTED action (set-alias here, which runs the alias core) replaces the
	// public card with the result PLUS an on-behalf attribution footer: the asker who
	// requested it, the approver who clicked Approve, and the agent that performed it
	// (#662). Pre-execution rejections are NOT attributed — covered by
	// TestConfirm_AliasRejectsInvalidInput, whose cards stay byte-exact.
	hc := newConfirmHarness(t, "Uadmin")
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionSetAlias, Alias: "oncall", Target: "staging", ChannelID: "C1", Asker: "Uasker"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, time.Now())

	_, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	for _, want := range []string{"<@Uasker>", "<@Uadmin>", agentAttributionAgentName} {
		if !strings.Contains(text, want) {
			t.Fatalf("executed card missing attribution %q: %q", want, text)
		}
	}
	// Attribution augments the core result, never replaces it: the footer follows
	// the (non-empty) result, so "Requested by" can't be at the very start.
	if i := strings.Index(text, "Requested by"); i <= 0 {
		t.Fatalf("attribution must follow the core result, not be the whole card: %q", text)
	}
}

func TestConfirm_AliasRejectsInvalidInput(t *testing.T) {
	// The confirm card is public, so an LLM-distilled alias/target that's out of
	// grammar (backtick — fence break; bidi/zero-width control — spoofing; bad
	// charset) must be rejected with the GENERIC reply: never bound/cleared, never
	// echoed onto the card. Asserting exact equality to the generic const proves no
	// part of the injected value leaks (and doesn't couple to the reply wording).
	cases := []struct {
		name string
		pa   *pendingAction
	}{
		{"set-alias backtick alias", &pendingAction{Action: agent.ActionSetAlias, Alias: "ev`il", Target: "staging", ChannelID: "C1"}},
		{"set-alias backtick target", &pendingAction{Action: agent.ActionSetAlias, Alias: "oncall", Target: "ev`il", ChannelID: "C1"}},
		{"unset-alias bidi-control alias", &pendingAction{Action: agent.ActionUnsetAlias, Alias: "on\u202ecall", ChannelID: "C1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hc := newConfirmHarness(t, "Uadmin")
			id := hc.seedPending(t, c.pa)
			hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, time.Now())
			ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
			if !ro || text != agentConfirmInvalidAliasReply {
				t.Fatalf("invalid alias input must replace the card with the generic reply (no echo); replace=%v text=%q", ro, text)
			}
		})
	}
}

func TestConfirm_UnsetAliasOnApprove(t *testing.T) {
	hc := newConfirmHarness(t, "Uadmin")
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionUnsetAlias, Alias: "ghost", ChannelID: "C1"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, time.Now())

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if !ro || !hc.claimed(id) {
		t.Fatalf("admin unset-alias approve should execute and replace the card; replace=%v text=%q", ro, text)
	}
	if !strings.Contains(text, "ghost") { // unbound alias → "…`$ghost` is not bound…"
		t.Fatalf("unset-alias result should mention the alias, got %q", text)
	}
}

func TestConfirm_ProtectURLOnApprove(t *testing.T) {
	// protect-url is admin-gated and direct-execute (mirrors revoke/alias). An admin
	// approve validates the URL + channel alias through the slash grammar, creates
	// the URL resource, binds it as the channel alias in the click's channel, and
	// replaces the public card with the benign result. Bind correctness lives in
	// handler_expose_test; here we pin the confirm orchestration of the new case
	// (claim, run the real core, replace) and ensure the agent does not use the
	// older exact-target expose path.
	hc := newConfirmHarness(t, "Uadmin")
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionProtectURL, URL: "https://docs.example.com/handbook", Alias: "docs", ChannelID: "C1"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, time.Now())

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if !ro || !hc.claimed(id) {
		t.Fatalf("admin protect-url approve should execute (claim) and replace the card; replace=%v text=%q", ro, text)
	}
	if text == agentConfirmUnsupportedReply || text == agentConfirmInvalidProtectURLReply || text == "" {
		t.Fatalf("protect-url must run the resource core, not the unsupported/invalid path; got %q", text)
	}
	if !strings.Contains(text, "URL resource is ready as `$docs`") {
		t.Fatalf("protect-url result should mention the channel alias; got %q", text)
	}
	if strings.Contains(text, "exact target URL") {
		t.Fatalf("agent protect-url must not surface the exact-target dashboard lookup failure: %q", text)
	}
	bound, found, err := hc.h.cfg.AdminStore.LookupChannelAlias(context.Background(), "T1", "C1", "docs")
	if err != nil {
		t.Fatalf("LookupChannelAlias: %v", err)
	}
	if !found || bound != testAgentCreatedURLResourceID {
		t.Fatalf("channel alias = (%q, %v), want newly-created resource %q", bound, found, testAgentCreatedURLResourceID)
	}
}

func TestConfirm_RecordsStructuredAuditResults(t *testing.T) {
	cases := []struct {
		name        string
		pa          *pendingAction
		before      func(*testing.T, *confirmHarness)
		wantSuccess bool
		wantResult  string
	}{
		{
			name:        "revoke resolve failure",
			pa:          &pendingAction{Action: agent.ActionRevoke, Token: "missing", ChannelID: "C1"},
			wantSuccess: false,
			wantResult:  "Resource could not be resolved for revoke.",
		},
		{
			name:        "set-alias success",
			pa:          &pendingAction{Action: agent.ActionSetAlias, Alias: "oncall", Target: "staging", ChannelID: "C1"},
			wantSuccess: true,
			wantResult:  "Alias now points to the qURL Connector in this channel.",
		},
		{
			name:        "set-alias target not found",
			pa:          &pendingAction{Action: agent.ActionSetAlias, Alias: "oncall", Target: "missing", ChannelID: "C1"},
			wantSuccess: false,
			wantResult:  "qURL Connector was not found.",
		},
		{
			name: "protect-url alias already bound",
			pa:   &pendingAction{Action: agent.ActionProtectURL, URL: "https://docs.example.com/handbook", Alias: "docs", ChannelID: "C1"},
			before: func(t *testing.T, hc *confirmHarness) {
				t.Helper()
				if err := hc.h.cfg.AdminStore.BindChannelAlias(context.Background(), "T1", "C1", "docs", "r_existing"); err != nil {
					t.Fatalf("seed bound alias: %v", err)
				}
			},
			wantSuccess: false,
			wantResult:  "URL resource is ready, but the alias is already bound in this channel.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hc := newConfirmHarness(t, "Uadmin")
			if c.before != nil {
				c.before(t, hc)
			}
			id := hc.seedPending(t, c.pa)
			hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, time.Now())
			ro, _ := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
			if !ro || !hc.claimed(id) {
				t.Fatalf("approve should claim and replace the card; replace=%v claimed=%v", ro, hc.claimed(id))
			}

			entry := requireSingleAuditEntry(t, hc, "Uadmin")
			if entry.Result != c.wantResult {
				t.Fatalf("Result = %q, want %q (entry=%+v)", entry.Result, c.wantResult, entry)
			}
			if entry.ResultSuccess == nil || *entry.ResultSuccess != c.wantSuccess {
				t.Fatalf("ResultSuccess = %v, want %v (entry=%+v)", entry.ResultSuccess, c.wantSuccess, entry)
			}
		})
	}
}

func TestConfirm_ProtectURLRejectsInvalidInput(t *testing.T) {
	// Public card → an LLM-distilled URL/alias out of grammar (non-URL target,
	// backtick/bidi alias, missing alias, or non-HTTPS create target) must
	// surface the GENERIC reply, never bind, and never echo the value. Exact
	// equality proves no part of the input leaks.
	cases := []struct {
		name string
		pa   *pendingAction
	}{
		{"non-url target", &pendingAction{Action: agent.ActionProtectURL, URL: "file:///etc/passwd", Alias: "docs", ChannelID: "C1"}},
		{"http url", &pendingAction{Action: agent.ActionProtectURL, URL: "http://docs.example.com/handbook", Alias: "docs", ChannelID: "C1"}},
		{"backtick alias", &pendingAction{Action: agent.ActionProtectURL, URL: "https://docs.example.com/handbook", Alias: "ev`il", ChannelID: "C1"}},
		{"missing alias", &pendingAction{Action: agent.ActionProtectURL, URL: "https://docs.example.com/handbook", Alias: "", ChannelID: "C1"}},
		{"bidi-control alias", &pendingAction{Action: agent.ActionProtectURL, URL: "https://docs.example.com/handbook", Alias: "do\u202ecs", ChannelID: "C1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hc := newConfirmHarness(t, "Uadmin")
			id := hc.seedPending(t, c.pa)
			hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, time.Now())
			ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
			if !ro || text != agentConfirmInvalidProtectURLReply {
				t.Fatalf("invalid protect-url input must replace the card with the generic reply (no echo); replace=%v text=%q", ro, text)
			}
		})
	}
}

func TestConfirm_ProtectURLIsAdminGated(t *testing.T) {
	// protect-url is admin-gated (adminGatedFor): a non-admin click is denied
	// ephemerally and claims nothing.
	hc := newConfirmHarness(t, "Uadmin") // admin = Uadmin; the clicker is Uother
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionProtectURL, URL: "https://docs.example.com/handbook", Alias: "docs", ChannelID: "C1"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uother", hc.respURL, id), id, true, time.Now())

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ro || !strings.Contains(strings.ToLower(text), "admin-only") {
		t.Fatalf("non-admin protect-url must be denied ephemerally; replace=%v text=%q", ro, text)
	}
	if hc.claimed(id) {
		t.Fatalf("a denied non-admin protect-url must not claim")
	}
}

func TestPostAgentConfirm_SnapshotsProtectURLFields(t *testing.T) {
	// The snapshot must carry URL + Alias so the click can execute protect-url (the
	// click never reads them off the wire).
	hc := newConfirmHarness(t, "")
	prop := &agent.Proposal{Action: agent.ActionProtectURL, URL: "https://docs.example.com/handbook", Alias: "docs", Summary: "Protect the handbook URL as $docs."}
	env := &slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{Channel: "C1", User: "U2", TS: "100.1"}}
	hc.h.postAgentConfirm(slog.Default(), env, "100.1", prop)

	blob, found, err := hc.store.LoadPendingAction(context.Background(), "T1", hc.pendingID(t, "T1"))
	if err != nil || !found {
		t.Fatalf("pending action not stored: found=%v err=%v", found, err)
	}
	var pa pendingAction
	if err := json.Unmarshal(blob, &pa); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pa.Action != agent.ActionProtectURL || pa.URL != "https://docs.example.com/handbook" || pa.Alias != "docs" {
		t.Fatalf("snapshot mismatch: %+v", pa)
	}
}

// openViewCapture records the single views.open the connector confirm path makes.
type openViewCapture struct {
	calls     int
	view      []byte
	trigger   string
	team      string
	returnErr error
}

func (c *openViewCapture) fn() OpenViewFunc {
	return func(_ context.Context, teamID, triggerID string, viewJSON []byte) error {
		c.calls++
		c.view = append([]byte(nil), viewJSON...)
		c.trigger, c.team = triggerID, teamID
		return c.returnErr
	}
}

func modalMeta(t *testing.T, viewJSON []byte) TunnelInstallModalMetadata {
	t.Helper()
	var view struct {
		PrivateMetadata string `json:"private_metadata"`
	}
	if err := json.Unmarshal(viewJSON, &view); err != nil {
		t.Fatalf("view JSON: %v", err)
	}
	var meta TunnelInstallModalMetadata
	if err := json.Unmarshal([]byte(view.PrivateMetadata), &meta); err != nil {
		t.Fatalf("private_metadata: %v", err)
	}
	return meta
}

func TestConfirm_ProtectConnectorOpensModalOnApprove(t *testing.T) {
	// protect-connector is admin-gated and modal-based: an admin approve claims
	// (consume-once), then opens the guided install modal with the click's trigger_id
	// and replaces the card with a terminal "opening setup" line. The modal SUBMIT
	// (covered in handler_tunnel/interaction tests) is the real enforcement + key
	// delivery; here we pin the confirm orchestration.
	hc := newConfirmHarness(t, "Uadmin")
	hc.h.now = func() time.Time { return fixedNow }
	ov := &openViewCapture{}
	hc.h.cfg.OpenView = ov.fn()
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionProtectConnector, ChannelID: "C1"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, fixedNow)

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ov.calls != 1 {
		t.Fatalf("protect-connector approve should open exactly one modal; opens=%d", ov.calls)
	}
	if ov.trigger != "trig" || ov.team != "T1" {
		t.Fatalf("modal must open with the click's trigger_id + team; trigger=%q team=%q", ov.trigger, ov.team)
	}
	if !ro || text != agentConfirmConnectorOpenedReply {
		t.Fatalf("card should go terminal with the opened reply; replace=%v text=%q", ro, text)
	}
	if !hc.claimed(id) {
		t.Fatal("protect-connector approve must claim (consume-once before open)")
	}
	// Key-delivery privacy: meta.UserID must be the approving admin so the modal's
	// same-user-submit gate aligns the ephemeral key target to the approver; the
	// channel must be the (mismatch-guarded) proposing channel.
	meta := modalMeta(t, ov.view)
	if meta.UserID != "Uadmin" || meta.ChannelID != "C1" || meta.ResponseURL != hc.respURL {
		t.Fatalf("modal metadata = %+v, want UserID=Uadmin ChannelID=C1 ResponseURL=card", meta)
	}
}

func TestConfirm_ProtectConnectorIsAdminGated(t *testing.T) {
	// A non-admin click is denied ephemerally, opens no modal, and claims nothing.
	hc := newConfirmHarness(t, "Uadmin") // admin = Uadmin; the clicker is Uother
	ov := &openViewCapture{}
	hc.h.cfg.OpenView = ov.fn()
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionProtectConnector, ChannelID: "C1"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uother", hc.respURL, id), id, true, fixedNow)

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ro || !strings.Contains(strings.ToLower(text), "admin-only") {
		t.Fatalf("non-admin protect-connector must be denied ephemerally; replace=%v text=%q", ro, text)
	}
	if ov.calls != 0 {
		t.Fatalf("a denied non-admin must not open the modal; opens=%d", ov.calls)
	}
	if hc.claimed(id) {
		t.Fatal("a denied non-admin protect-connector must not claim")
	}
}

func TestConfirm_ProtectConnectorTriggerExpired(t *testing.T) {
	// The trigger window has already passed by the time the async body runs: no
	// views.open is attempted (no wasted Slack RPC), and the card goes terminal with
	// the "ask again" prompt. The action is still claimed (claim precedes the open).
	hc := newConfirmHarness(t, "Uadmin")
	hc.h.now = func() time.Time { return fixedNow }
	ov := &openViewCapture{}
	hc.h.cfg.OpenView = ov.fn()
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionProtectConnector, ChannelID: "C1"})
	expired := fixedNow.Add(-slackTriggerMaxAge - time.Second) // trigger long gone
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, expired)

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if !ro || text != agentConfirmConnectorWindowExpiredReply {
		t.Fatalf("expired trigger should replace the card with the window-expired reply; replace=%v text=%q", ro, text)
	}
	if ov.calls != 0 {
		t.Fatalf("expired trigger must not attempt views.open; opens=%d", ov.calls)
	}
	if !hc.claimed(id) {
		t.Fatal("protect-connector claims before the trigger-budget check")
	}
}

func TestConfirm_ProtectConnectorOpenFails(t *testing.T) {
	// views.open failure maps to cause-specific terminal copy: a non-expiry cause must
	// NOT say "ask me again" (which won't fix it). The card always goes terminal (never
	// a silent miss) and the action stays claimed.
	cases := []struct {
		name      string
		openErr   error
		wantReply string
	}{
		{"trigger expired → ask again", ErrSlackTriggerExpired, agentConfirmConnectorWindowExpiredReply},
		{"deadline → ask again", context.DeadlineExceeded, agentConfirmConnectorWindowExpiredReply},
		{"rate limited → wait", NewSlackRateLimitError("3"), agentConfirmConnectorRateLimitedReply},
		{"no bot token → unavailable", auth.ErrSlackBotTokenNotConfigured, agentConfirmConnectorUnavailableReply},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hc := newConfirmHarness(t, "Uadmin")
			hc.h.now = func() time.Time { return fixedNow }
			ov := &openViewCapture{returnErr: c.openErr}
			hc.h.cfg.OpenView = ov.fn()
			id := hc.seedPending(t, &pendingAction{Action: agent.ActionProtectConnector, ChannelID: "C1"})
			hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, fixedNow)

			ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
			if ov.calls != 1 {
				t.Fatalf("open-fails case should still attempt one views.open; opens=%d", ov.calls)
			}
			if !ro || text != c.wantReply {
				t.Fatalf("open error %v should replace the card with %q; replace=%v text=%q", c.openErr, c.wantReply, ro, text)
			}
		})
	}
}

func TestConfirm_ProtectConnectorGridFallback(t *testing.T) {
	// The confirm path threads payload.Enterprise.ID into openViewWithGridFallback:
	// when the workspace bot token is missing, the open retries with the Enterprise
	// Grid org-install token. Pins that the connector branch passes enterpriseID
	// through (the fallback mechanism itself is covered via the slash path).
	hc := newConfirmHarness(t, "Uadmin")
	hc.h.now = func() time.Time { return fixedNow }
	var owners []string
	hc.h.cfg.OpenView = func(_ context.Context, teamID, _ string, _ []byte) error {
		owners = append(owners, teamID)
		if teamID == "T1" {
			return auth.ErrSlackBotTokenNotConfigured // workspace token missing → retry org
		}
		return nil
	}
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionProtectConnector, ChannelID: "C1"})
	p := confirmPayload("T1", "C1", "Uadmin", hc.respURL, id)
	p.Enterprise.ID = "E1"
	hc.h.processAgentConfirm(context.Background(), slog.Default(), p, id, true, fixedNow)

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if len(owners) != 2 || owners[0] != "T1" || owners[1] != "E1" {
		t.Fatalf("Grid fallback should retry views.open with the enterprise token; owners=%v", owners)
	}
	if !ro || text != agentConfirmConnectorOpenedReply {
		t.Fatalf("the fallback open should succeed and post the opened reply; replace=%v text=%q", ro, text)
	}
}

func TestConfirm_ProtectConnectorOpenViewUnconfigured(t *testing.T) {
	// OpenView not wired: the card goes terminal with a graceful "use the slash
	// command" reply rather than hanging.
	hc := newConfirmHarness(t, "Uadmin")
	hc.h.now = func() time.Time { return fixedNow }
	hc.h.cfg.OpenView = nil
	id := hc.seedPending(t, &pendingAction{Action: agent.ActionProtectConnector, ChannelID: "C1"})
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, fixedNow)

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if !ro || text != agentConfirmConnectorUnavailableReply {
		t.Fatalf("unconfigured OpenView should replace the card with the unavailable reply; replace=%v text=%q", ro, text)
	}
}

func TestConfirm_AliasKindsAreAdminGated(t *testing.T) {
	// set-alias and unset-alias are admin-gated (adminGatedFor): a non-admin click is
	// denied ephemerally and claims nothing. The gate is shared with revoke's, but
	// assert it directly for the new kinds rather than relying on inheritance.
	for _, pa := range []*pendingAction{
		{Action: agent.ActionSetAlias, Alias: "oncall", Target: "staging", ChannelID: "C1"},
		{Action: agent.ActionUnsetAlias, Alias: "oncall", ChannelID: "C1"},
	} {
		t.Run(string(pa.Action), func(t *testing.T) {
			hc := newConfirmHarness(t, "Uadmin") // admin = Uadmin; the clicker is not
			id := hc.seedPending(t, pa)
			hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uother", hc.respURL, id), id, true, time.Now())
			ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
			if ro || !strings.Contains(strings.ToLower(text), "admin-only") {
				t.Fatalf("non-admin %s must be denied ephemerally; replace=%v text=%q", pa.Action, ro, text)
			}
			if hc.claimed(id) {
				t.Fatalf("a denied non-admin %s must not claim", pa.Action)
			}
		})
	}
}

func TestPostAgentConfirm_SnapshotsAliasFields(t *testing.T) {
	// The pending snapshot must carry Alias+Target so the click can execute a
	// set-alias (the click never reads them off the wire).
	hc := newConfirmHarness(t, "")
	prop := &agent.Proposal{Action: agent.ActionSetAlias, Alias: "oncall", Target: "staging", Summary: "Bind $oncall → $staging."}
	env := &slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{Channel: "C1", User: "U2", TS: "100.1"}}
	hc.h.postAgentConfirm(slog.Default(), env, "100.1", prop)

	blob, found, err := hc.store.LoadPendingAction(context.Background(), "T1", hc.pendingID(t, "T1"))
	if err != nil || !found {
		t.Fatalf("pending action not stored: found=%v err=%v", found, err)
	}
	var pa pendingAction
	if err := json.Unmarshal(blob, &pa); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pa.Alias != "oncall" || pa.Target != "staging" {
		t.Fatalf("set-alias snapshot must carry Alias+Target, got %+v", pa)
	}
}

func TestConfirm_FlagOffClickDoesNotExecute(t *testing.T) {
	// A card clicked after the deploy-time kill switch flips off (cards live ~10m)
	// must not execute or claim — the click-time re-check makes "flag off ⇒ nothing
	// executes" unconditional.
	hc := newConfirmHarness(t, "Uadmin")
	hc.h.cfg.AgentConfirmEnabled = false
	id := hc.seedPending(t, revokePending())
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, time.Now())

	ro, _ := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ro || hc.claimed(id) {
		t.Fatal("a click with the confirm flag off must not execute or claim")
	}
}

func TestConfirm_ConsumeOnceNoDoubleExecute(t *testing.T) {
	hc := newConfirmHarness(t, "Uadmin")
	id := hc.seedPending(t, revokePending())
	payload := confirmPayload("T1", "C1", "Uadmin", hc.respURL, id)

	hc.h.processAgentConfirm(context.Background(), slog.Default(), payload, id, true, time.Now())
	_ = hc.bodies.waitForBody(t, 2*time.Second)
	// Second click on the same id: already claimed → ephemeral "already handled".
	hc.h.processAgentConfirm(context.Background(), slog.Default(), payload, id, true, time.Now())

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
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uother", hc.respURL, id), id, false, time.Now())
	ro, _ := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ro || hc.claimed(id) {
		t.Fatal("a non-admin reject of a gated action must be denied ephemerally and not claim")
	}

	// An admin reject cancels (claims + replaces with Canceled).
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, false, time.Now())
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
	hc.h.processAgentConfirm(context.Background(), slog.Default(), confirmPayload("T1", "Cother", "Uadmin", hc.respURL, id), id, true, time.Now())
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
	// Pin the invariant: every kind confirmExecutable green-lights actually DOES
	// something on Approve, never the no-op "unsupported" default. confirmModalRouted
	// kinds (protect-connector) are handled by openAgentConnectorModal BEFORE
	// executeAgentAction — their handled-ness is pinned by the dedicated modal tests
	// (TestConfirm_ProtectConnector*) — so they're excluded here via the SAME predicate
	// the click router uses, so the two can't drift when a modal kind is added.
	hc := newConfirmHarness(t, "Uadmin")
	payload := confirmPayload("T1", "C1", "Uadmin", hc.respURL, "x")
	allKinds := []agent.ActionKind{
		agent.ActionGet, agent.ActionRevoke, agent.ActionSetAlias,
		agent.ActionUnsetAlias, agent.ActionProtectConnector, agent.ActionProtectURL,
	}
	handled := 0
	for _, kind := range allKinds {
		if !confirmExecutable(kind) || confirmModalRouted(kind) {
			continue // modal-routed kinds don't flow through executeAgentAction
		}
		handled++
		got := hc.h.executeAgentAction(context.Background(), slog.Default(),
			&pendingAction{Action: kind, Token: "staging", Alias: "oncall", Target: "staging", ChannelID: "C1"}, payload)
		if got.cardText == agentConfirmUnsupportedReply {
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
		openView  bool // OpenView wired? (only matters for modal-routed kinds)
		wantCard  bool
	}{
		{"executable + flag on → card", agent.Result{Proposal: &agent.Proposal{Action: agent.ActionRevoke, Summary: "Revoke $x."}}, true, false, true},
		// Every real kind is executable as of PR4c; a synthetic unknown kind exercises
		// the non-executable → preview gate (fail-closed: no live button for a kind the
		// click can't act on).
		{"non-executable + flag on → preview", agent.Result{Proposal: &agent.Proposal{Action: agent.ActionKind("bogus"), Summary: "Mystery."}}, true, false, false},
		{"executable + flag off → preview", agent.Result{Proposal: &agent.Proposal{Action: agent.ActionRevoke, Summary: "Revoke $x."}}, false, false, false},
		{"plain reply → preview", agent.Result{Reply: "hello"}, true, false, false},
		// Modal-routed connector renders a card ONLY when OpenView is wired, else the
		// Approve could only dead-end into "unavailable" (and the claim would burn it).
		{"connector + OpenView wired → card", agent.Result{Proposal: &agent.Proposal{Action: agent.ActionProtectConnector, Summary: "Protect a connector."}}, true, true, true},
		{"connector + OpenView unwired → preview", agent.Result{Proposal: &agent.Proposal{Action: agent.ActionProtectConnector, Summary: "Protect a connector."}}, true, false, false},
		// protect-url renders a card only when URL+alias pass the SAME grammar the
		// execute path uses; a grammar-invalid proposal (whitespace in the URL splits
		// the token stream; an out-of-charset alias; a non-HTTPS create target) would
		// dead-end on Approve, so it falls back to preview — closing the
		// propose→approve gap the seed-pendingAction invalid-input tests don't
		// exercise.
		{"protect-url valid → card", agent.Result{Proposal: &agent.Proposal{Action: agent.ActionProtectURL, URL: "https://docs.example.com/h", Alias: "docs", Summary: "Protect docs."}}, true, false, true},
		{"protect-url http url → preview", agent.Result{Proposal: &agent.Proposal{Action: agent.ActionProtectURL, URL: "http://docs.example.com/h", Alias: "docs", Summary: "Protect docs."}}, true, false, false},
		{"protect-url whitespace url → preview", agent.Result{Proposal: &agent.Proposal{Action: agent.ActionProtectURL, URL: "https://docs.example.com/a b", Alias: "docs", Summary: "Protect docs."}}, true, false, false},
		{"protect-url out-of-charset alias → preview", agent.Result{Proposal: &agent.Proposal{Action: agent.ActionProtectURL, URL: "https://docs.example.com/h", Alias: "MyDocs", Summary: "Protect docs."}}, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hc := newConfirmHarness(t, "")
			hc.h.cfg.AgentConfirmEnabled = c.confirmOn
			if c.openView {
				hc.h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }
			}
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

func TestPostAgentConfirm_DoesNotExecuteAtProposeTime(t *testing.T) {
	// The core security invariant — the LLM only proposes; execution is a human
	// click. Proposing must invoke NO mutation core. Every core dials the qURL API
	// through NewClient, so a NewClient that fails the test proves nothing executed
	// at propose time (the card + pending action are written, but no mutation runs).
	hc := newConfirmHarness(t, "")
	hc.h.cfg.NewClient = func(string) *client.Client {
		t.Fatal("propose must not execute a mutation core (no qURL client call at propose time)")
		return nil
	}
	prop := &agent.Proposal{Action: agent.ActionRevoke, Token: "staging", Summary: "Revoke $staging."}
	env := &slackEventEnvelope{TeamID: "T1", Event: slackInnerEvent{Channel: "C1", User: "U2", TS: "100.1"}}
	hc.h.postAgentConfirm(slog.Default(), env, "100.1", prop)

	if len(hc.blocks.calls) != 1 {
		t.Fatalf("propose should post exactly one card, got %d", len(hc.blocks.calls))
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
