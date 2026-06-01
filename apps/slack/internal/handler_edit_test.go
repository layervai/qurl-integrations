package internal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

// Edit test data, hoisted to satisfy goconst.
const (
	testEditChannel    = "C_test"
	testEditResourceID = "r_edit_prod"
	testEditToken      = "prod-db"
	testEditDisplay    = "Prod database"
	testEditAlias      = "primary"

	// Slack interaction-payload top-level keys, shared by the block_actions
	// and view_submission test builders.
	payloadKeyTeam = "team"
	payloadKeyUser = "user"
)

// --- pure unit tests -----------------------------------------------------

func TestBuildTunnelEditButtonValue(t *testing.T) {
	// boundAliases includes the token itself (install binds $slug as a channel
	// alias); the snapshot must EXCLUDE it so the modal never offers to unbind
	// the tunnel's canonical name.
	val, ok := buildTunnelEditButtonValue(testEditResourceID, testEditToken, testEditDisplay, []string{testEditAlias, testEditToken, "staging"})
	if !ok {
		t.Fatalf("buildTunnelEditButtonValue ok=false for a normal row")
	}
	var snap tunnelEditButtonValue
	if err := json.Unmarshal([]byte(val), &snap); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v", err)
	}
	if snap.ResourceID != testEditResourceID || snap.Token != testEditToken || snap.DisplayName != testEditDisplay {
		t.Errorf("snapshot identity = %+v, want r=%s t=%s d=%s", snap, testEditResourceID, testEditToken, testEditDisplay)
	}
	if got, want := strings.Join(snap.Aliases, ","), "primary,staging"; got != want {
		t.Errorf("snapshot aliases = %q, want %q (token excluded)", got, want)
	}

	// A pathologically large alias set blows the button-value cap → (_, false).
	big := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		big = append(big, "alias-with-a-fairly-long-name-"+strings.Repeat("x", 20))
	}
	if _, ok := buildTunnelEditButtonValue(testEditResourceID, testEditToken, testEditDisplay, big); ok {
		t.Errorf("buildTunnelEditButtonValue ok=true for an over-cap snapshot")
	}
}

func TestParseEditAliasLines(t *testing.T) {
	t.Run("happy with optional sigil, dedup, token excluded", func(t *testing.T) {
		got, msg := parseEditAliasLines("$primary\nstaging\n\n$primary\n"+testEditToken, testEditToken)
		if msg != "" {
			t.Fatalf("unexpected rejection: %s", msg)
		}
		if want := "primary,staging"; strings.Join(got, ",") != want {
			t.Errorf("aliases = %v, want %q (sigil optional, deduped, token dropped)", got, want)
		}
	})
	t.Run("invalid alias rejected", func(t *testing.T) {
		if _, msg := parseEditAliasLines("$Bad_Alias", testEditToken); msg == "" {
			t.Error("expected rejection for an out-of-charset alias")
		}
	})
	t.Run("too many aliases rejected", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < listEditMaxAliases+1; i++ {
			b.WriteString("$alias-")
			b.WriteByte(byte('a' + i%26))
			b.WriteString(strings.Repeat("z", i)) // keep each distinct
			b.WriteByte('\n')
		}
		if _, msg := parseEditAliasLines(b.String(), testEditToken); !strings.Contains(msg, "Too many") {
			t.Errorf("expected too-many rejection, got %q", msg)
		}
	})
}

func TestParseTunnelEditModalArgs(t *testing.T) {
	values := func(name, aliases string) map[string]map[string]interactionStateValue {
		return map[string]map[string]interactionStateValue{
			tunnelEditBlockDisplayName: {tunnelEditActionDisplayName: {Value: name}},
			tunnelEditBlockAliases:     {tunnelEditActionAliases: {Value: aliases}},
		}
	}

	t.Run("happy", func(t *testing.T) {
		name, aliases, fe := parseTunnelEditModalArgs(values("New Name", "$one\n$two"), testEditToken)
		if len(fe) != 0 {
			t.Fatalf("unexpected field errors: %v", fe)
		}
		if name != "New Name" || strings.Join(aliases, ",") != "one,two" {
			t.Errorf("got name=%q aliases=%v", name, aliases)
		}
	})
	t.Run("empty display name is a field error", func(t *testing.T) {
		_, _, fe := parseTunnelEditModalArgs(values("   ", ""), testEditToken)
		if _, ok := fe[tunnelEditBlockDisplayName]; !ok {
			t.Errorf("expected a display-name field error, got %v", fe)
		}
	})
	t.Run("mrkdwn-injecting display name rejected", func(t *testing.T) {
		_, _, fe := parseTunnelEditModalArgs(values("<!channel> hi", ""), testEditToken)
		if _, ok := fe[tunnelEditBlockDisplayName]; !ok {
			t.Errorf("expected display-name char rejection, got %v", fe)
		}
	})
	t.Run("bad alias is a field error", func(t *testing.T) {
		_, _, fe := parseTunnelEditModalArgs(values("ok", "$NOPE"), testEditToken)
		if _, ok := fe[tunnelEditBlockAliases]; !ok {
			t.Errorf("expected an aliases field error, got %v", fe)
		}
	})
}

// --- list render ---------------------------------------------------------

// editButtonValues collects the value of every Edit button across the list's
// actions blocks, in order.
func editButtonValues(t *testing.T, blocks []any) []string {
	t.Helper()
	var vals []string
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		if block[blockKitFieldType] != blockKitTypeActions {
			continue
		}
		els, _ := block[blockKitFieldElements].([]any)
		for _, e := range els {
			el, _ := e.(map[string]any)
			if el[blockKitFieldActionID] == listEditTunnelActionID {
				v, _ := el[blockKitFieldValue].(string)
				vals = append(vals, v)
			}
		}
	}
	return vals
}

// editableListHandler wires AdminStore + aliasStore + OpenView and seeds a
// single tunnel (testEditToken → testEditResourceID, description testEditDisplay)
// with the token plus a "primary" alias bound in the channel.
func editableListHandler(t *testing.T) (*Handler, *adminTestServers) {
	t.Helper()
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testEditResourceID, testKeyType: client.ResourceTypeTunnel, testKeySlug: testEditToken, testKeyDescription: testEditDisplay},
		}, "", false)
	})
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, map[string]string{
		testEditToken: testEditResourceID,
		testEditAlias: testEditResourceID,
	})
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }
	return h, ts
}

// TestHandleList_RendersEditButtonForAdmin fences that an admin caller (with
// the modal/alias/admin wiring) sees an Edit button whose value snapshots the
// row's resource_id, token, display name, and EXTRA aliases (token excluded).
func TestHandleList_RendersEditButtonForAdmin(t *testing.T) {
	h, _ := editableListHandler(t)
	inv := newAdminSlashInvoker(t, h)

	if status, _ := inv.invokeAdmin("list", testAdminTeamID, testAdminUserID); status != http.StatusOK {
		t.Fatalf("status != 200")
	}
	blocks := parseSlackBlocks(t, inv.captured.waitForBody(t, 2*time.Second))
	vals := editButtonValues(t, blocks)
	if len(vals) != 1 {
		t.Fatalf("Edit button count = %d, want 1; blocks=%v", len(vals), blocks)
	}
	var snap tunnelEditButtonValue
	if err := json.Unmarshal([]byte(vals[0]), &snap); err != nil {
		t.Fatalf("Edit value not JSON: %v (%q)", err, vals[0])
	}
	if snap.ResourceID != testEditResourceID || snap.Token != testEditToken || snap.DisplayName != testEditDisplay {
		t.Errorf("snapshot = %+v", snap)
	}
	if strings.Join(snap.Aliases, ",") != testEditAlias {
		t.Errorf("snapshot aliases = %v, want [%s] (token excluded)", snap.Aliases, testEditAlias)
	}
	// Create qURL is still present alongside Edit.
	if !strings.Contains(string(mustMarshal(t, blocks)), listCreateQurlActionID) {
		t.Errorf("Create qURL button missing from admin list")
	}
}

// TestHandleList_NoEditButtonWithoutWiring fences that the Edit button is
// gated: with OpenView unset (modal can't be opened), the list renders only
// the Create qURL accessory button — no Edit.
func TestHandleList_NoEditButtonWithoutWiring(t *testing.T) {
	h, _ := editableListHandler(t)
	h.cfg.OpenView = nil // drop the modal opener → no Edit affordance
	inv := newAdminSlashInvoker(t, h)

	if status, _ := inv.invokeAdmin("list", testAdminTeamID, testAdminUserID); status != http.StatusOK {
		t.Fatalf("status != 200")
	}
	blocks := parseSlackBlocks(t, inv.captured.waitForBody(t, 2*time.Second))
	if vals := editButtonValues(t, blocks); len(vals) != 0 {
		t.Errorf("Edit buttons rendered without OpenView wiring: %v", vals)
	}
	if vals := createQurlButtonValues(t, blocks); len(vals) != 1 {
		t.Errorf("Create qURL accessory button missing: %v", vals)
	}
}

// --- modal open (block_actions) ------------------------------------------

// TestHandleListEditClick_OpensModal fences that tapping Edit opens a modal
// pre-filled from the button snapshot: callback_id tunnel_edit, the current
// display name as the display-name input's initial_value, and the extra alias
// pre-loaded into the multiline aliases field.
func TestHandleListEditClick_OpensModal(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	views := make(chan []byte, 1)
	h.cfg.OpenView = func(_ context.Context, _ string, _ string, view []byte) error {
		views <- view
		return nil
	}

	snap, _ := buildTunnelEditButtonValue(testEditResourceID, testEditToken, testEditDisplay, []string{testEditAlias})
	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, testEditChannel, "https://hooks.slack.com/edit", listEditTunnelActionID, snap)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("edit click ack = %d %q, want 200 {}", w.Code, w.Body.String())
	}

	var view []byte
	select {
	case view = <-views:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenView was not called")
	}
	var modal map[string]any
	if err := json.Unmarshal(view, &modal); err != nil {
		t.Fatalf("modal JSON: %v", err)
	}
	if modal[blockKitFieldCallbackID] != callbackIDTunnelEdit {
		t.Errorf("callback_id = %v, want %s", modal[blockKitFieldCallbackID], callbackIDTunnelEdit)
	}
	js := string(view)
	for _, want := range []string{testEditDisplay, testEditAlias, testEditResourceID, testEditToken} {
		if !strings.Contains(js, want) {
			t.Errorf("modal missing %q: %s", want, js)
		}
	}
}

// TestHandleListEditClick_UnparseableValue fences that a malformed button value
// posts the open-failed notice and never calls OpenView.
func TestHandleListEditClick_UnparseableValue(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	var opened int
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { opened++; return nil }

	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, testEditChannel, srv.URL, listEditTunnelActionID, "not json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	h.Wait()
	if opened != 0 {
		t.Errorf("OpenView called %d times for an unparseable value, want 0", opened)
	}
	if !strings.Contains(string(got), "edit dialog") {
		t.Errorf("open-failed notice not posted: %s", got)
	}
}

// --- submission (view_submission) ----------------------------------------

func tunnelEditViewSubmissionBody(t *testing.T, meta *TunnelEditModalMetadata, payloadTeamID, payloadUserID, displayName, aliasesText string) string {
	t.Helper()
	pm, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal private_metadata: %v", err)
	}
	return viewSubmissionBody(t, "V_test_edit", callbackIDTunnelEdit, string(pm), payloadTeamID, payloadUserID,
		map[string]map[string]interactionStateValue{
			tunnelEditBlockDisplayName: {tunnelEditActionDisplayName: {Value: displayName}},
			tunnelEditBlockAliases:     {tunnelEditActionAliases: {Value: aliasesText}},
		})
}

// viewSubmissionBody encodes a Slack view_submission interaction body, shared
// by the tunnel-install and tunnel-edit modal-submission tests so the
// wire-shape JSON keys live in one place.
func viewSubmissionBody(t *testing.T, viewID, callbackID, privateMetadata, payloadTeamID, payloadUserID string, values map[string]map[string]interactionStateValue) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		testKeyType:    "view_submission",
		payloadKeyTeam: map[string]any{"id": payloadTeamID},
		payloadKeyUser: map[string]any{"id": payloadUserID},
		"view": map[string]any{
			"id":                         viewID,
			testFieldCallbackID:          callbackID,
			blockKitFieldPrivateMetadata: privateMetadata,
			"state":                      map[string]any{"values": values},
		},
	})
	if err != nil {
		t.Fatalf("marshal interaction payload: %v", err)
	}
	return url.Values{"payload": {string(payload)}}.Encode()
}

// TestHandleTunnelEdit_HappyPath fences the full submission: the Display Name
// PATCHes (it changed), the alias set reconciles (a new alias binds, the
// dropped one unbinds), and the admin sees a summary.
func TestHandleTunnelEdit_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	// Current channel state: token + "old-alias" bound to this resource.
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, map[string]string{
		testEditToken: testEditResourceID,
		"old-alias":   testEditResourceID,
	})
	var patched string
	ts.addCustomer(http.MethodPatch, "/v1/resources/"+testEditResourceID, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Description *string `json:"description"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Description != nil {
			patched = *in.Description
		}
		respondQURLEnvelope(t, w, map[string]any{testKeyResourceID: testEditResourceID, testKeyType: client.ResourceTypeTunnel, testKeyStatus: client.StatusActive})
	})

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	var got []byte
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		close(done)
	}))
	t.Cleanup(srv.Close)

	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken, DisplayName: testEditDisplay,
	}
	// New display name + new alias; "old-alias" dropped.
	body := tunnelEditViewSubmissionBody(t, &meta, testAdminTeamID, testAdminUserID, "Renamed Prod", "$new-alias")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("submission ack = %d %q, want 200 {}", w.Code, w.Body.String())
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no result posted to response_url")
	}
	if patched != "Renamed Prod" {
		t.Errorf("PATCHed description = %q, want %q", patched, "Renamed Prod")
	}
	summary := parseSlackText(t, got)
	for _, want := range []string{"Updated tunnel `$" + testEditToken + "`", "new-alias", "old-alias"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q: %s", want, summary)
		}
	}
	// Alias store reflects the reconcile.
	if rid, found, _ := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testEditChannel, "new-alias"); !found || rid != testEditResourceID {
		t.Errorf("new-alias not bound: rid=%q found=%v", rid, found)
	}
	if _, found, _ := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testEditChannel, "old-alias"); found {
		t.Errorf("old-alias should have been unbound")
	}
}

// TestHandleTunnelEdit_NonAdminDenied fences the mutation gate: a non-admin
// submitter is refused with an error view and nothing is PATCHed or bound.
func TestHandleTunnelEdit_NonAdminDenied(t *testing.T) {
	const nonAdminUserID = "U_non_admin"
	ts := newAdminTestServers(t)
	ts.seedAdmin(t) // seeds testAdminUserID, NOT nonAdminUserID
	ts.addCustomer(http.MethodPatch, "/v1/resources/"+testEditResourceID, func(http.ResponseWriter, *http.Request) {
		t.Errorf("PATCH reached despite non-admin submitter")
	})
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: nonAdminUserID,
		ResponseURL: "https://hooks.slack.com/x", ResourceID: testEditResourceID, Token: testEditToken, DisplayName: testEditDisplay,
	}
	body := tunnelEditViewSubmissionBody(t, &meta, testAdminTeamID, nonAdminUserID, "Renamed", "$x")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	h.Wait()

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "admin-only") {
		t.Errorf("expected admin-only denial, got %s", w.Body.String())
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
