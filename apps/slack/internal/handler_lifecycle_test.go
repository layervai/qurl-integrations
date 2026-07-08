package internal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
)

// recordingStateDeleter is a recordingAuthProvider that also satisfies
// workspaceStateDeleter, so purgeWorkspace's capability type-assertion succeeds
// and the workspace_state delete is exercised. The bare recordingAuthProvider
// (used by the uninstall tests) deliberately does NOT implement it, which keeps
// those tests asserting the "provider can't delete a row" skip path.
type recordingStateDeleter struct {
	recordingAuthProvider
	deleteStateCalls        int
	deleteStateWorkspaceID  string
	deleteStateWorkspaceIDs []string
	deleteStateErr          error
}

func (p *recordingStateDeleter) DeleteWorkspaceState(_ context.Context, workspaceID string) error {
	p.deleteStateCalls++
	p.deleteStateWorkspaceID = workspaceID
	p.deleteStateWorkspaceIDs = append(p.deleteStateWorkspaceIDs, workspaceID)
	return p.deleteStateErr
}

// appUninstalledBody is a signature-valid Events API app_uninstalled envelope for
// the given team. tokens_revoked carries an inner tokens object; app_uninstalled
// carries just the inner type.
func appUninstalledBody(teamID string) string {
	return `{"type":"event_callback","team_id":"` + teamID + `","api_app_id":"A1","event_id":"EvUninstall","event":{"type":"app_uninstalled"}}`
}

func appUninstalledEnterpriseBody(enterpriseID string) string {
	return `{"type":"event_callback","enterprise_id":"` + enterpriseID + `","api_app_id":"A1","event_id":"EvEnterpriseUninstall","event":{"type":"app_uninstalled"}}`
}

func appUninstalledGridBody(teamID, enterpriseID string) string {
	return `{"type":"event_callback","team_id":"` + teamID + `","enterprise_id":"` + enterpriseID + `","api_app_id":"A1","event_id":"EvGridUninstall",` +
		`"authorizations":[{"enterprise_id":"` + enterpriseID + `","team_id":"` + teamID + `","is_enterprise_install":true}],` +
		`"event":{"type":"app_uninstalled"}}`
}

func appUninstalledGridTeamBody(teamID, enterpriseID string) string {
	return `{"type":"event_callback","team_id":"` + teamID + `","enterprise_id":"` + enterpriseID + `","api_app_id":"A1","event_id":"EvGridTeamUninstall",` +
		`"authorizations":[{"enterprise_id":"` + enterpriseID + `","team_id":"` + teamID + `","is_enterprise_install":false}],` +
		`"event":{"type":"app_uninstalled"}}`
}

func tokensRevokedBody(teamID string) string {
	return `{"type":"event_callback","team_id":"` + teamID + `","api_app_id":"A1","event_id":"EvRevoke",` +
		`"event":{"type":"tokens_revoked","tokens":{"bot":["B123"]}}}`
}

func userTokensRevokedBody(teamID string) string {
	return `{"type":"event_callback","team_id":"` + teamID + `","api_app_id":"A1","event_id":"EvUserRevoke",` +
		`"event":{"type":"tokens_revoked","tokens":{"oauth":["U123"]}}}`
}

// newLifecycleTestHandler wires an admin handler (real *slackdata.Store over
// fakeDDB) with a state-deleting provider, and seeds a fully-populated workspace:
// a workspace_mappings admin row plus two channel_policies rows. Returns the
// handler, the provider (for delete-call assertions), and the test servers (for
// store-read assertions).
func newLifecycleTestHandler(t *testing.T) (*Handler, *recordingStateDeleter, *adminTestServers) {
	t.Helper()
	return newLifecycleTestHandlerForWorkspace(t, testAdminTeamID)
}

func newLifecycleTestHandlerForWorkspace(t *testing.T, workspaceID string) (*Handler, *recordingStateDeleter, *adminTestServers) {
	t.Helper()
	ts := newAdminTestServers(t)
	ts.seedWorkspace(t, workspaceID, testAdminOwnerID, testAdminUserID, testWorkspaceConfiguredAt)
	ts.seedPolicyAliasBindings(t, workspaceID, "C_one", map[string]string{"grafana": "r_aaa"})
	ts.seedPolicySet(t, workspaceID, "C_two", "", []string{"r_bbb"})
	provider := &recordingStateDeleter{recordingAuthProvider: recordingAuthProvider{apiKey: "test-key"}}
	h := newAdminTestHandler(t, ts)
	h.cfg.AuthProvider = provider
	h.cfg.AgentStore = &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "agent_state"}
	seedLifecycleAgentState(t, h.cfg.AgentStore, workspaceID)
	return h, provider, ts
}

func seedLifecycleAgentState(t *testing.T, store *slackdata.AgentStore, partition string) {
	t.Helper()
	if err := store.SaveConversation(context.Background(), partition, "C_agent:1", []byte(`[{"role":"user","content":"hi"}]`), 0); err != nil {
		t.Fatalf("seed agent conversation: %v", err)
	}
	if err := store.PutPendingAction(context.Background(), partition, "pending_agent", []byte(`{"action":"get"}`)); err != nil {
		t.Fatalf("seed agent pending action: %v", err)
	}
}

func assertLifecycleAgentStatePurged(t *testing.T, store *slackdata.AgentStore, partition string) {
	t.Helper()
	blob, _, err := store.LoadConversation(context.Background(), partition, "C_agent:1")
	if err != nil {
		t.Fatalf("LoadConversation after purge: %v", err)
	}
	if blob != nil {
		t.Fatalf("LoadConversation after purge = %q, want nil", blob)
	}
	if payload, found, err := store.LoadPendingAction(context.Background(), partition, "pending_agent"); err != nil || found || payload != nil {
		t.Fatalf("LoadPendingAction after purge: payload=%q found=%v err=%v, want absent", payload, found, err)
	}
}

func assertLifecycleAgentStatePresent(t *testing.T, store *slackdata.AgentStore, partition string) {
	t.Helper()
	blob, _, err := store.LoadConversation(context.Background(), partition, "C_agent:1")
	if err != nil {
		t.Fatalf("LoadConversation after other-partition purge: %v", err)
	}
	if blob == nil {
		t.Fatalf("agent conversation for partition %q was unexpectedly purged", partition)
	}
}

func TestHandleLifecycleEvent_AppUninstalledPurgesWorkspace(t *testing.T) {
	h, provider, ts := newLifecycleTestHandler(t)
	_ = ts // seeded rows are read back through h.cfg.AdminStore below

	w := httptest.NewRecorder()
	body := appUninstalledBody(testAdminTeamID)
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))

	// Slack must get a prompt 200 regardless of the purge outcome.
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	// The purge runs on a tracked async goroutine; drain it before asserting.
	h.Wait()

	// workspace_state delete attempted with the right workspace id.
	if provider.deleteStateCalls != 1 {
		t.Fatalf("DeleteWorkspaceState calls = %d, want 1", provider.deleteStateCalls)
	}
	if provider.deleteStateWorkspaceID != testAdminTeamID {
		t.Fatalf("DeleteWorkspaceState workspaceID = %q, want %q", provider.deleteStateWorkspaceID, testAdminTeamID)
	}
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testAdminTeamID)

	// workspace_mappings row gone: ListAdmins now 404s.
	_, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), testAdminTeamID)
	var ae *slackdata.Error
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
		t.Fatalf("ListAdmins after purge: err = %v, want 404 *Error (mapping row should be gone)", err)
	}

	// channel_policies rows gone: both seeded channels read empty.
	for _, ch := range []string{"C_one", "C_two"} {
		entries, err := h.cfg.AdminStore.GetChannelPolicy(context.Background(), testAdminTeamID, ch)
		if err != nil {
			t.Fatalf("GetChannelPolicy(%q) after purge: %v", ch, err)
		}
		if len(entries) != 0 {
			t.Fatalf("GetChannelPolicy(%q) after purge = %v, want empty (policy row should be gone)", ch, entries)
		}
	}
}

func TestHandleLifecycleEvent_EnterpriseFallbackPurgesWorkspace(t *testing.T) {
	h, provider, ts := newLifecycleTestHandlerForWorkspace(t, testEnterpriseID)
	_ = ts

	w := httptest.NewRecorder()
	body := appUninstalledEnterpriseBody(testEnterpriseID)
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	if provider.deleteStateCalls != 1 {
		t.Fatalf("DeleteWorkspaceState calls = %d, want 1", provider.deleteStateCalls)
	}
	if provider.deleteStateWorkspaceID != testEnterpriseID {
		t.Fatalf("DeleteWorkspaceState workspaceID = %q, want %q", provider.deleteStateWorkspaceID, testEnterpriseID)
	}
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testEnterpriseID)

	_, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), testEnterpriseID)
	var ae *slackdata.Error
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
		t.Fatalf("ListAdmins after enterprise purge: err = %v, want 404 *Error", err)
	}
	for _, ch := range []string{"C_one", "C_two"} {
		entries, err := h.cfg.AdminStore.GetChannelPolicy(context.Background(), testEnterpriseID, ch)
		if err != nil {
			t.Fatalf("GetChannelPolicy(%q) after enterprise purge: %v", ch, err)
		}
		if len(entries) != 0 {
			t.Fatalf("GetChannelPolicy(%q) after enterprise purge = %v, want empty", ch, entries)
		}
	}
}

func TestHandleLifecycleEvent_EnterpriseGridOrgInstallPurgesEnterpriseAndTeamKeys(t *testing.T) {
	h, provider, ts := newLifecycleTestHandlerForWorkspace(t, testEnterpriseID)
	ts.seedWorkspace(t, testAdminTeamID, testAdminOwnerID, testAdminUserID, testWorkspaceConfiguredAt)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_team", map[string]string{"grafana": "r_team"})
	seedLifecycleAgentState(t, h.cfg.AgentStore, testAdminTeamID)

	w := httptest.NewRecorder()
	body := appUninstalledGridBody(testAdminTeamID, testEnterpriseID)
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	wantIDs := testEnterpriseID + "," + testAdminTeamID
	if got := strings.Join(provider.deleteStateWorkspaceIDs, ","); got != wantIDs {
		t.Fatalf("DeleteWorkspaceState ids = %q, want %q", got, wantIDs)
	}
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testEnterpriseID)
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testAdminTeamID)

	for _, workspaceID := range []string{testEnterpriseID, testAdminTeamID} {
		_, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), workspaceID)
		var ae *slackdata.Error
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
			t.Fatalf("ListAdmins(%q) after Grid purge: err = %v, want 404 *Error", workspaceID, err)
		}
	}
	for _, ch := range []string{"C_one", "C_two"} {
		entries, err := h.cfg.AdminStore.GetChannelPolicy(context.Background(), testEnterpriseID, ch)
		if err != nil {
			t.Fatalf("GetChannelPolicy(%q) after Grid purge: %v", ch, err)
		}
		if len(entries) != 0 {
			t.Fatalf("GetChannelPolicy(%q) after Grid purge = %v, want empty", ch, entries)
		}
	}
	teamEntries, err := h.cfg.AdminStore.GetChannelPolicy(context.Background(), testAdminTeamID, "C_team")
	if err != nil {
		t.Fatalf("GetChannelPolicy(team) after Grid purge: %v", err)
	}
	if len(teamEntries) != 0 {
		t.Fatalf("GetChannelPolicy(team) after Grid purge = %v, want empty", teamEntries)
	}
}

func TestHandleLifecycleEvent_EnterpriseGridTeamInstallPurgesTeamKeyOnly(t *testing.T) {
	h, provider, ts := newLifecycleTestHandler(t)
	ts.seedWorkspace(t, testEnterpriseID, testAdminOwnerID, testAdminUserID, testWorkspaceConfiguredAt)
	seedLifecycleAgentState(t, h.cfg.AgentStore, testEnterpriseID)

	w := httptest.NewRecorder()
	body := appUninstalledGridTeamBody(testAdminTeamID, testEnterpriseID)
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	wantIDs := testAdminTeamID
	if got := strings.Join(provider.deleteStateWorkspaceIDs, ","); got != wantIDs {
		t.Fatalf("DeleteWorkspaceState ids = %q, want %q", got, wantIDs)
	}
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testAdminTeamID)
	assertLifecycleAgentStatePresent(t, h.cfg.AgentStore, testEnterpriseID)

	_, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), testAdminTeamID)
	var ae *slackdata.Error
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
		t.Fatalf("ListAdmins after team-key purge: err = %v, want 404 *Error", err)
	}
	if _, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), testEnterpriseID); err != nil {
		t.Fatalf("ListAdmins for enterprise key after team-key purge: %v", err)
	}
}

func TestHandleLifecycleEvent_MultipleWorkspaceIDsPurgeEachID(t *testing.T) {
	const secondTeamID = "T_second"

	h, provider, ts := newLifecycleTestHandler(t)
	ts.seedWorkspace(t, secondTeamID, testAdminOwnerID, testAdminUserID, testWorkspaceConfiguredAt)
	ts.seedPolicyAliasBindings(t, secondTeamID, "C_three", map[string]string{"jenkins": "r_ccc"})
	seedLifecycleAgentState(t, h.cfg.AgentStore, secondTeamID)

	h.handleLifecycleEvent(&slackEventEnvelope{
		Type:         slackEnvelopeTypeEventCallback,
		TeamID:       testAdminTeamID,
		EnterpriseID: testEnterpriseID,
		Authorizations: []slackEventAuthorization{
			{TeamID: testAdminTeamID, EnterpriseID: testEnterpriseID},
			{TeamID: secondTeamID, EnterpriseID: testEnterpriseID},
		},
		EventID: "EvMultiWorkspace",
		Event:   slackInnerEvent{Type: slackEventTypeAppUninstalled},
	})
	h.Wait()

	wantIDs := testAdminTeamID + "," + secondTeamID
	if got := strings.Join(provider.deleteStateWorkspaceIDs, ","); got != wantIDs {
		t.Fatalf("DeleteWorkspaceState ids = %q, want %q", got, wantIDs)
	}
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testAdminTeamID)
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, secondTeamID)

	for _, workspaceID := range []string{testAdminTeamID, secondTeamID} {
		_, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), workspaceID)
		var ae *slackdata.Error
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
			t.Fatalf("ListAdmins(%q) after multi-id purge: err = %v, want 404 *Error", workspaceID, err)
		}
	}
}

func TestLifecycleWorkspaceIDs_NoAuthorizationPayloadUsesTeamBeforeEnterprise(t *testing.T) {
	env := &slackEventEnvelope{TeamID: " " + testAdminTeamID + " ", EnterpriseID: testEnterpriseID}

	wantIDs := testAdminTeamID
	if got := strings.Join(lifecycleWorkspaceIDs(env), ","); got != wantIDs {
		t.Fatalf("lifecycleWorkspaceIDs = %q, want %q", got, wantIDs)
	}

	enterpriseOnly := &slackEventEnvelope{EnterpriseID: testEnterpriseID}
	if got := strings.Join(lifecycleWorkspaceIDs(enterpriseOnly), ","); got != testEnterpriseID {
		t.Fatalf("lifecycleWorkspaceIDs(enterprise-only) = %q, want %q", got, testEnterpriseID)
	}
}

func TestLifecycleWorkspaceIDs_PartialEnterpriseAuthorizationFallsBackToEnvelope(t *testing.T) {
	env := &slackEventEnvelope{
		TeamID:       testAdminTeamID,
		EnterpriseID: testEnterpriseID,
		Authorizations: []slackEventAuthorization{{
			IsEnterpriseInstall: true,
		}},
	}

	wantIDs := testEnterpriseID + "," + testAdminTeamID
	if got := strings.Join(lifecycleWorkspaceIDs(env), ","); got != wantIDs {
		t.Fatalf("lifecycleWorkspaceIDs(partial enterprise auth) = %q, want %q", got, wantIDs)
	}
}

func TestHandleLifecycleEvent_TokensRevokedPurgesWorkspace(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)

	w := httptest.NewRecorder()
	body := tokensRevokedBody(testAdminTeamID)
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	// A bot-token revoke is treated as an uninstall — the full purge runs.
	if provider.deleteStateCalls != 1 {
		t.Fatalf("DeleteWorkspaceState calls = %d, want 1 (bot tokens_revoked must purge)", provider.deleteStateCalls)
	}
}

func TestHandleLifecycleEvent_BotTokensRevokedWithRotationEnabledDoesNotPurgeWorkspace(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)
	h.cfg.SlackBotTokenRotationEnabled = true

	w := httptest.NewRecorder()
	body := tokensRevokedBody(testAdminTeamID)
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	if provider.deleteStateCalls != 0 {
		t.Fatalf("DeleteWorkspaceState calls = %d, want 0 when bot-token rotation is enabled", provider.deleteStateCalls)
	}
	assertLifecycleAgentStatePresent(t, h.cfg.AgentStore, testAdminTeamID)
	if _, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), testAdminTeamID); err != nil {
		t.Fatalf("ListAdmins after rotation tokens_revoked: %v", err)
	}
}

func TestHandleLifecycleEvent_UserTokensRevokedDoesNotPurgeWorkspace(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)

	w := httptest.NewRecorder()
	body := userTokensRevokedBody(testAdminTeamID)
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	if provider.deleteStateCalls != 0 {
		t.Fatalf("DeleteWorkspaceState calls = %d, want 0 for oauth-only tokens_revoked", provider.deleteStateCalls)
	}
}

func TestHandleLifecycleEvent_NoWorkspaceIDDoesNotPurge(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)

	w := httptest.NewRecorder()
	body := `{"type":"event_callback","api_app_id":"A1","event_id":"EvNoWorkspace","event":{"type":"app_uninstalled"}}`
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	if provider.deleteStateCalls != 0 {
		t.Fatalf("DeleteWorkspaceState calls = %d, want 0 without team_id/enterprise_id", provider.deleteStateCalls)
	}
}

// TestHandleLifecycleEvent_AckEvenWhenPurgeFails fences the "always 200" contract:
// a failing workspace_state delete must not change the ack Slack receives, or
// Slack would retry the delivery forever.
func TestHandleLifecycleEvent_AckEvenWhenPurgeFails(t *testing.T) {
	h, provider, ts := newLifecycleTestHandler(t)
	provider.deleteStateErr = errors.New("kms unavailable")
	// Also fail the channel_policies sweep (its Query) so the ack is proven to
	// survive failures across more than one table in the best-effort purge.
	ts.ddb.SetQueryErr(ts.tableNames.channelPolicy, errors.New("ddb query down"))

	w := httptest.NewRecorder()
	body := appUninstalledBody(testAdminTeamID)
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200 even when purge fails", w.Code)
	}
	h.Wait()

	// The async purge retries boundedly after failures; each attempt still reaches
	// the state delete, proving the ack-first path has a transient-failure retry
	// story without changing Slack's 200 response.
	if provider.deleteStateCalls != lifecyclePurgeRetryAttempts {
		t.Fatalf("DeleteWorkspaceState calls = %d, want %d retry attempts", provider.deleteStateCalls, lifecyclePurgeRetryAttempts)
	}
}

// TestSlashCommandUninstallPurgesWorkspace fences the `/qurl uninstall` extension:
// after the existing qURL-key delete, the command must also forget the rest of
// the workspace (bot token via DeleteWorkspaceState, mappings, policies), so an
// uninstall leaves nothing behind. The provider here implements
// workspaceStateDeleter; DeleteAPIKey clears the qURL columns and purgeWorkspace
// then sweeps the row + mappings + policies.
func TestSlashCommandUninstallPurgesWorkspace(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)

	resp := slashUninstallAsAdmin(t, h)

	// Existing behavior preserved: the qURL key delete still happens synchronously
	// (it gates the success reply).
	if provider.deleteCalls != 1 {
		t.Fatalf("DeleteAPIKey calls = %d, want 1", provider.deleteCalls)
	}
	// The full purge runs on a tracked async goroutine (off the slash ack's sync
	// budget); drain it before asserting the rest of the workspace is gone.
	h.Wait()
	// New behavior: the workspace_state row (bot token + all) is removed too.
	if provider.deleteStateCalls != 1 {
		t.Fatalf("DeleteWorkspaceState calls = %d, want 1 (uninstall must forget the bot token)", provider.deleteStateCalls)
	}
	if provider.deleteStateWorkspaceID != testAdminTeamID {
		t.Fatalf("DeleteWorkspaceState workspaceID = %q, want %q", provider.deleteStateWorkspaceID, testAdminTeamID)
	}
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testAdminTeamID)
	// Success copy stays accurate (recordingAuthProvider's DeleteAPIKey returns
	// nil, and the upstream revoke degrades to local-only, so this is the
	// local-only disconnect reply).
	if !strings.Contains(resp[respFieldText], "disconnected from this workspace") {
		t.Fatalf("uninstall reply missing confirmation: %q", resp[respFieldText])
	}
	if !strings.Contains(resp[respFieldText], "Local Slack app data") {
		t.Fatalf("uninstall reply missing local Slack app data teardown: %q", resp[respFieldText])
	}
	if !strings.Contains(resp[respFieldText], "Slack features stay disconnected") {
		t.Fatalf("uninstall reply missing Slack reconnect impact: %q", resp[respFieldText])
	}

	// mappings + policies gone after the async purge.
	_, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), testAdminTeamID)
	var ae *slackdata.Error
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
		t.Fatalf("ListAdmins after uninstall: err = %v, want 404 *Error", err)
	}
	for _, ch := range []string{"C_one", "C_two"} {
		entries, err := h.cfg.AdminStore.GetChannelPolicy(context.Background(), testAdminTeamID, ch)
		if err != nil {
			t.Fatalf("GetChannelPolicy(%q) after uninstall: %v", ch, err)
		}
		if len(entries) != 0 {
			t.Fatalf("GetChannelPolicy(%q) after uninstall = %v, want empty", ch, entries)
		}
	}
}

func TestSlashCommandUninstallGridOrgInstallPurgesTeamAndEnterpriseKeys(t *testing.T) {
	h, provider, ts := newLifecycleTestHandler(t)
	ts.seedWorkspace(t, testEnterpriseID, testAdminOwnerID, testAdminUserID, testWorkspaceConfiguredAt)
	seedLifecycleAgentState(t, h.cfg.AgentStore, testEnterpriseID)

	inv := newAdminSlashInvoker(t, h)
	inv.enterpriseID = testEnterpriseID
	inv.isEnterpriseInstall = slackFormBoolTrue
	status, reply := inv.invokeAdmin(uninstallVerb, testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("uninstall status = %d, want 200; reply=%q", status, reply)
	}

	if provider.deleteCalls != 1 {
		t.Fatalf("DeleteAPIKey calls = %d, want 1", provider.deleteCalls)
	}
	if provider.deleteWorkspaceID != testAdminTeamID {
		t.Fatalf("DeleteAPIKey workspaceID = %q, want %q", provider.deleteWorkspaceID, testAdminTeamID)
	}
	h.Wait()

	wantIDs := testAdminTeamID + "," + testEnterpriseID
	if got := strings.Join(provider.deleteStateWorkspaceIDs, ","); got != wantIDs {
		t.Fatalf("DeleteWorkspaceState ids = %q, want %q", got, wantIDs)
	}
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testAdminTeamID)
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testEnterpriseID)
	for _, workspaceID := range []string{testAdminTeamID, testEnterpriseID} {
		_, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), workspaceID)
		var ae *slackdata.Error
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
			t.Fatalf("ListAdmins(%q) after Grid org uninstall: err = %v, want 404 *Error", workspaceID, err)
		}
	}
}

func TestSlashCommandUninstallGridWorkspaceInstallKeepsEnterpriseKey(t *testing.T) {
	h, provider, ts := newLifecycleTestHandler(t)
	ts.seedWorkspace(t, testEnterpriseID, testAdminOwnerID, testAdminUserID, testWorkspaceConfiguredAt)
	seedLifecycleAgentState(t, h.cfg.AgentStore, testEnterpriseID)

	inv := newAdminSlashInvoker(t, h)
	inv.enterpriseID = testEnterpriseID
	inv.isEnterpriseInstall = "false"
	status, reply := inv.invokeAdmin(uninstallVerb, testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("uninstall status = %d, want 200; reply=%q", status, reply)
	}
	h.Wait()

	wantIDs := testAdminTeamID
	if got := strings.Join(provider.deleteStateWorkspaceIDs, ","); got != wantIDs {
		t.Fatalf("DeleteWorkspaceState ids = %q, want %q", got, wantIDs)
	}
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testAdminTeamID)
	assertLifecycleAgentStatePresent(t, h.cfg.AgentStore, testEnterpriseID)
	if _, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), testEnterpriseID); err != nil {
		t.Fatalf("ListAdmins for enterprise key after workspace-level uninstall: %v", err)
	}
}

func TestSlashCommandUninstallPurgesWorkspaceWhenAPIKeyAlreadyCleared(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)
	provider.deleteErr = auth.ErrWorkspaceNotConfigured

	resp := slashUninstallAsAdmin(t, h)

	if provider.deleteCalls != 1 {
		t.Fatalf("DeleteAPIKey calls = %d, want 1", provider.deleteCalls)
	}
	if !strings.Contains(resp[respFieldText], "isn't currently connected") {
		t.Fatalf("uninstall reply missing not-connected message: %q", resp[respFieldText])
	}
	if !strings.Contains(resp[respFieldText], "Local Slack app data") {
		t.Fatalf("uninstall reply missing local Slack app data teardown: %q", resp[respFieldText])
	}
	if !strings.Contains(resp[respFieldText], "Slack features stay disconnected") {
		t.Fatalf("uninstall reply missing Slack reconnect impact: %q", resp[respFieldText])
	}
	h.Wait()
	if provider.deleteStateCalls != 1 {
		t.Fatalf("DeleteWorkspaceState calls = %d, want 1 (already-cleared uninstall must still forget bot state)", provider.deleteStateCalls)
	}
	if provider.deleteStateWorkspaceID != testAdminTeamID {
		t.Fatalf("DeleteWorkspaceState workspaceID = %q, want %q", provider.deleteStateWorkspaceID, testAdminTeamID)
	}
	assertLifecycleAgentStatePurged(t, h.cfg.AgentStore, testAdminTeamID)

	_, _, err := h.cfg.AdminStore.ListAdmins(context.Background(), testAdminTeamID)
	var ae *slackdata.Error
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound {
		t.Fatalf("ListAdmins after already-cleared uninstall: err = %v, want 404 *Error", err)
	}
	for _, ch := range []string{"C_one", "C_two"} {
		entries, err := h.cfg.AdminStore.GetChannelPolicy(context.Background(), testAdminTeamID, ch)
		if err != nil {
			t.Fatalf("GetChannelPolicy(%q) after already-cleared uninstall: %v", ch, err)
		}
		if len(entries) != 0 {
			t.Fatalf("GetChannelPolicy(%q) after already-cleared uninstall = %v, want empty", ch, entries)
		}
	}
}

// TestHandleEvent_NonLifecycleEventNotPurged fences the router: an ordinary
// event_callback (e.g. an app_mention) must NOT trigger a workspace purge.
func TestHandleEvent_NonLifecycleEventNotPurged(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)

	w := httptest.NewRecorder()
	// app_mention is a non-lifecycle event_callback. The agent is unwired here, so
	// handleAgentEvent no-ops; the key assertion is that no purge fires.
	body := `{"type":"event_callback","team_id":"` + testAdminTeamID + `","event_id":"EvMention","event":{"type":"app_mention","user":"U2","channel":"C1","ts":"1.1","text":"hi"}}`
	h.ServeHTTP(w, newSignedRequest(t, pathSlackEvents, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("ack code = %d, want 200", w.Code)
	}
	h.Wait()

	if provider.deleteStateCalls != 0 {
		t.Fatalf("DeleteWorkspaceState calls = %d, want 0 for a non-lifecycle event", provider.deleteStateCalls)
	}
}
