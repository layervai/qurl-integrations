package internal

// Tests for the per-workspace conversation-mode toggle (PR6a): the
// workspaceAgentEnabled decision matrix, the gate wired at both the event and the
// confirm-click paths, and the `/qurl-admin agent on|off` command.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// --- workspaceAgentEnabled decision matrix ---

func TestWorkspaceAgentEnabled_DecisionMatrix(t *testing.T) {
	ctx := context.Background()
	log := slog.Default()

	t.Run("no AdminStore falls back to the org default", func(t *testing.T) {
		for _, def := range []bool{false, true} {
			h := NewHandler(Config{AgentDefaultEnabled: def})
			if got := h.workspaceAgentEnabled(ctx, log, "T1"); got != def {
				t.Errorf("default=%v: got %v, want the org default", def, got)
			}
		}
	})

	// newWS returns a Handler whose AdminStore is a real Store over a seeded
	// workspace row for T1, plus the store so the test can set the toggle.
	newWS := func(t *testing.T, def bool) (*Handler, *slackdata.Store) {
		t.Helper()
		ts := newAdminTestServers(t)
		ts.seedWorkspace(t, "T1", "Uowner", "Uadmin", testWorkspaceConfiguredAt)
		store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
		return NewHandler(Config{AdminStore: store, AgentDefaultEnabled: def}), store
	}

	t.Run("explicit on wins over a default-off deployment", func(t *testing.T) {
		h, store := newWS(t, false)
		if err := store.SetAgentEnabled(ctx, "T1", true); err != nil {
			t.Fatalf("SetAgentEnabled: %v", err)
		}
		if !h.workspaceAgentEnabled(ctx, log, "T1") {
			t.Error("an explicit opt-in must enable even while the org default is off")
		}
	})

	t.Run("explicit off survives a default-on flip", func(t *testing.T) {
		h, store := newWS(t, true)
		if err := store.SetAgentEnabled(ctx, "T1", false); err != nil {
			t.Fatalf("SetAgentEnabled: %v", err)
		}
		if h.workspaceAgentEnabled(ctx, log, "T1") {
			t.Error("an explicit opt-out must stay off even after the GA default flips on")
		}
	})

	t.Run("unset follows the org default", func(t *testing.T) {
		for _, def := range []bool{false, true} {
			h, _ := newWS(t, def)
			if got := h.workspaceAgentEnabled(ctx, log, "T1"); got != def {
				t.Errorf("unset, default=%v: got %v, want the org default", def, got)
			}
		}
	})
}

func TestWorkspaceAgentEnabled_FailsClosedOnReadError(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedWorkspace(t, "T1", "Uowner", "Uadmin", testWorkspaceConfiguredAt)
	ts.ddb.SetGetItemErr(ts.tableNames.workspace, errors.New("ddb down"))
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)

	// Default ON makes the fail-closed observable: a non-failing read would enable,
	// so a false result can only come from the read error path.
	h := NewHandler(Config{AdminStore: store, AgentDefaultEnabled: true})
	if h.workspaceAgentEnabled(context.Background(), slog.Default(), "T1") {
		t.Error("a toggle read error must fail closed (disabled), even with the org default on")
	}
}

// --- the gate is wired at the event path, before dedupe ---

func TestProcessAgentEvent_OptOutGatedBeforeDedupe(t *testing.T) {
	ctx := context.Background()
	ts := newAdminTestServers(t)
	ts.seedWorkspace(t, "T1", "Uowner", "Uadmin", testWorkspaceConfiguredAt)
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	if err := store.SetAgentEnabled(ctx, "T1", false); err != nil {
		t.Fatalf("opt out: %v", err)
	}

	agentStore := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	post, posts, mu := capturingPostMessage()
	h := NewHandler(Config{
		AgentLLM:    fakeAgentLLM{reply: "You can reach staging."},
		AgentStore:  agentStore,
		PostMessage: post,
		AdminStore:  store,
		// Org default ON, so only the per-workspace opt-out can suppress this.
		AgentDefaultEnabled: true,
	})
	t.Cleanup(h.Wait)

	// Opted out: the @mention is ignored — no reply.
	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvGate")))
	h.Wait()
	mu.Lock()
	n := len(*posts)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("an opted-out workspace must not reply, got %d", n)
	}

	// Re-enable and replay the SAME event_id. It must reply — proving the toggle
	// gate runs BEFORE MarkEventSeen: a mention dropped while disabled was never
	// dedupe-committed, so opting in later doesn't swallow the replay as a retry.
	if err := store.SetAgentEnabled(ctx, "T1", true); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	h.handleEvent(httptest.NewRecorder(), []byte(appMentionBody("EvGate")))
	h.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(*posts) != 1 {
		t.Fatalf("after opt-in the replayed mention must reply once (gate precedes dedupe), got %d", len(*posts))
	}
}

// --- the gate is wired at the confirm-click path, before the claim ---

func TestConfirm_DisabledWorkspaceGatedBeforeClaim(t *testing.T) {
	hc := newConfirmHarness(t, "Uadmin")
	if err := hc.h.cfg.AdminStore.SetAgentEnabled(context.Background(), "T1", false); err != nil {
		t.Fatalf("opt out: %v", err)
	}
	id := hc.seedPending(t, revokePending())

	hc.h.processAgentConfirm(context.Background(), slog.Default(),
		confirmPayload("T1", "C1", "Uadmin", hc.respURL, id), id, true, time.Now())

	ro, text := parseResponse(t, hc.bodies.waitForBody(t, 2*time.Second))
	if ro || !strings.Contains(text, "expired") {
		t.Fatalf("a click in an opted-out workspace must get the ephemeral expired reply, got replace=%v text=%q", ro, text)
	}
	if hc.claimed(id) {
		t.Fatal("a gated click must NOT claim the pending action")
	}
}

// --- /qurl-admin agent on|off command ---

func TestHandleAgentToggle_OnPersistsAndReplies(t *testing.T) {
	ctx := context.Background()
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("agent on", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "*on*") {
		t.Errorf("reply missing on-confirmation: %q", reply)
	}
	// The org surface is dark in this handler (no AgentLLM wired), so the success
	// reply warns the toggle won't take effect until the operator enables it.
	if !strings.Contains(reply, "isn't enabled for this deployment yet") {
		t.Errorf("reply missing org-dark heads-up: %q", reply)
	}
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	v, set, err := store.AgentEnabledFor(ctx, testAdminTeamID)
	if err != nil || !set || !v {
		t.Fatalf("AgentEnabledFor = (%v,%v,%v), want (true,true,nil)", v, set, err)
	}
}

func TestHandleAgentToggle_OffPersistsOptOut(t *testing.T) {
	ctx := context.Background()
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("agent off", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "*off*") {
		t.Errorf("reply missing off-confirmation: %q", reply)
	}
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	v, set, err := store.AgentEnabledFor(ctx, testAdminTeamID)
	if err != nil || !set || v {
		t.Fatalf("AgentEnabledFor = (%v,%v,%v), want (false,true,nil) — an explicit opt-out must persist", v, set, err)
	}
}

func TestHandleAgentToggle_StatusReflectsState(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	// Unset → the default, flagged as not explicitly set.
	_, reply := inv.invokeAdmin("agent", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "not explicitly set") {
		t.Errorf("unset status should flag the default: %q", reply)
	}

	// After an explicit set, status reports it as explicit.
	inv.invokeAdmin("agent on", testAdminTeamID, testAdminUserID)
	_, reply = inv.invokeAdmin("agent", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "explicitly *on*") {
		t.Errorf("explicit status should report explicitly on: %q", reply)
	}
}

func TestHandleAgentToggle_BogusArgRendersUsage(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("agent maybe", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "Usage") {
		t.Errorf("a bogus arg should render the usage hint: %q", reply)
	}
}

func TestHandleAgentToggle_NonAdminDenied(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t) // workspace exists but the caller is not on the admin set
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("agent on", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "admin-only") {
		t.Errorf("a non-admin must be denied: %q", reply)
	}
	// The admin gate fires before any write — the toggle stays unset.
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	if _, set, _ := store.AgentEnabledFor(context.Background(), testAdminTeamID); set {
		t.Error("a denied non-admin must NOT persist a toggle value")
	}
}

func TestHandleAgentToggle_AdminStoreUnconfigured(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, noopQURLServer(t)) // builds without an AdminStore
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("agent on", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "not configured") {
		t.Errorf("no AdminStore should reply not-configured: %q", reply)
	}
}

// TestAgentToggleSetError_Maps404ToSetupHint and
// TestAgentToggleStatus_ReadErrorIsGeneric exercise the handler error branches
// directly. They're unreachable through the slash command — requireAdminSync's
// CheckAdmin reads the same workspace row first, so a missing row or a dead store
// denies (or fails the gate) before SetAgentEnabled/AgentEnabledFor runs — but the
// branches exist as defense-in-depth against a future gate refactor, so they're
// fenced here rather than left uncovered.
func TestAgentToggleSetError_Maps404ToSetupHint(t *testing.T) {
	got404 := agentToggleSetError(&slackdata.Error{StatusCode: http.StatusNotFound, Title: "not bound"})
	if !strings.Contains(got404, "/qurl setup") {
		t.Errorf("a 404 should point the admin at setup: %q", got404)
	}
	gotGeneric := agentToggleSetError(errors.New("ddb down"))
	if strings.Contains(gotGeneric, "setup") || !strings.Contains(gotGeneric, "Couldn't update") {
		t.Errorf("a non-404 error should render the generic update-failure copy: %q", gotGeneric)
	}
}

func TestAgentToggleStatus_ReadErrorIsGeneric(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedWorkspace(t, "T1", "Uowner", "Uadmin", testWorkspaceConfiguredAt)
	ts.ddb.SetGetItemErr(ts.tableNames.workspace, errors.New("ddb down"))
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	h := NewHandler(Config{AdminStore: store})

	if got := h.agentToggleStatus(context.Background(), "T1"); !strings.Contains(got, "Couldn't read") {
		t.Errorf("a toggle read error should render the generic read-failure copy: %q", got)
	}
}

// TestAdminHelpIncludesConversationMode fences the help listing: the agent verbs
// appear under their section when AdminStore is wired (same gate the verbs use at
// runtime) and are omitted when it isn't, so a no-store deploy doesn't advertise a
// command that replies "not configured".
func TestAdminHelpIncludesConversationMode(t *testing.T) {
	wired, _ := newAliasTestHandler(t) // wires aliasStore + AdminStore
	help := wired.adminHelpMessage(commandAdmin)
	for _, want := range []string{"*Conversation mode*", "`/qurl-admin agent on`", "`/qurl-admin agent off`"} {
		if !strings.Contains(help, want) {
			t.Errorf("wired admin help missing %q:\n%s", want, help)
		}
	}

	bare := newTestHandler(t, noopQURLServer(t)) // wires neither store
	if h := bare.adminHelpMessage(commandAdmin); strings.Contains(h, "*Conversation mode*") {
		t.Errorf("no-store admin help must omit the conversation-mode section:\n%s", h)
	}
}
