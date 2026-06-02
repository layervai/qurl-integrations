package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	testEditOldAlias   = "old-alias"

	// Slack conversation-id-shaped channel IDs for the "expose to channels"
	// tests (must satisfy slackChannelIDPattern). Lifted to constants because
	// they recur across the channel-reconcile tests and views_test.go.
	testEditHomeChannel  = "C0home00000"
	testEditExtraChannel = "C0extra0000"

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
	// aliasLines builds n distinct `$alias-…` lines.
	aliasLines := func(n int) []string {
		out := make([]string, n)
		for i := 0; i < n; i++ {
			out[i] = fmt.Sprintf("$alias-%03d-%s", i, strings.Repeat("z", 10))
		}
		return out
	}
	sigilless := func(lines []string) []string {
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = strings.TrimPrefix(l, "$")
		}
		return out
	}

	t.Run("happy with optional sigil, dedup, token excluded", func(t *testing.T) {
		got, msg := parseEditAliasLines("$primary\nstaging\n\n$primary\n"+testEditToken, testEditToken, nil)
		if msg != "" {
			t.Fatalf("unexpected rejection: %s", msg)
		}
		if want := "primary,staging"; strings.Join(got, ",") != want {
			t.Errorf("aliases = %v, want %q (sigil optional, deduped, token dropped)", got, want)
		}
	})
	t.Run("invalid alias rejected", func(t *testing.T) {
		if _, msg := parseEditAliasLines("$Bad_Alias", testEditToken, nil); msg == "" {
			t.Error("expected rejection for an out-of-charset alias")
		}
	})
	t.Run("too many NEW aliases rejected", func(t *testing.T) {
		lines := aliasLines(listEditMaxAliases + 1)
		if _, msg := parseEditAliasLines(strings.Join(lines, "\n"), testEditToken, nil); !strings.Contains(msg, "Too many") {
			t.Errorf("expected too-many rejection, got %q", msg)
		}
	})
	t.Run("untouched over-cap pre-filled set is allowed", func(t *testing.T) {
		// A tunnel already carrying >listEditMaxAliases aliases: submitting them
		// unchanged (a name-only edit) must NOT trip the cap.
		lines := aliasLines(listEditMaxAliases + 5)
		got, msg := parseEditAliasLines(strings.Join(lines, "\n"), testEditToken, sigilless(lines))
		if msg != "" {
			t.Fatalf("untouched over-cap pre-filled set rejected: %s", msg)
		}
		if len(got) != listEditMaxAliases+5 {
			t.Errorf("alias count = %d, want %d", len(got), listEditMaxAliases+5)
		}
	})
	t.Run("too many aliases ADDED on top of pre-filled rejected", func(t *testing.T) {
		prefilled := []string{"keepa", "keepb", "keepc"} // sigil-free, already bound
		lines := make([]string, 0, len(prefilled)+listEditMaxAliases+1)
		for _, p := range prefilled {
			lines = append(lines, "$"+p)
		}
		lines = append(lines, aliasLines(listEditMaxAliases+1)...) // distinct brand-new
		if _, msg := parseEditAliasLines(strings.Join(lines, "\n"), testEditToken, prefilled); !strings.Contains(msg, "Too many") {
			t.Errorf("expected too-many rejection for >cap newly-added, got %q", msg)
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

	// meta builds modal metadata pre-filled with the given current Display Name.
	meta := func(currentName string) *TunnelEditModalMetadata {
		return &TunnelEditModalMetadata{Token: testEditToken, DisplayName: currentName}
	}

	t.Run("happy", func(t *testing.T) {
		name, changed, aliases, fe := parseTunnelEditModalArgs(values("New Name", "$one\n$two"), meta(testEditDisplay))
		if len(fe) != 0 {
			t.Fatalf("unexpected field errors: %v", fe)
		}
		if !changed {
			t.Errorf("nameChanged = false, want true (New Name != %q)", testEditDisplay)
		}
		if name != "New Name" || strings.Join(aliases, ",") != "one,two" {
			t.Errorf("got name=%q aliases=%v", name, aliases)
		}
	})
	t.Run("empty display name is a field error when changed", func(t *testing.T) {
		_, _, _, fe := parseTunnelEditModalArgs(values("   ", ""), meta(testEditDisplay))
		if _, ok := fe[tunnelEditBlockDisplayName]; !ok {
			t.Errorf("expected a display-name field error, got %v", fe)
		}
	})
	t.Run("mrkdwn-injecting display name rejected when changed", func(t *testing.T) {
		_, _, _, fe := parseTunnelEditModalArgs(values("<!channel> hi", ""), meta(testEditDisplay))
		if _, ok := fe[tunnelEditBlockDisplayName]; !ok {
			t.Errorf("expected display-name char rejection, got %v", fe)
		}
	})
	t.Run("bad alias is a field error", func(t *testing.T) {
		_, _, _, fe := parseTunnelEditModalArgs(values("ok", "$NOPE"), meta(testEditDisplay))
		if _, ok := fe[tunnelEditBlockAliases]; !ok {
			t.Errorf("expected an aliases field error, got %v", fe)
		}
	})
	t.Run("untouched legacy name skips validation (alias-only edit)", func(t *testing.T) {
		// A stored name carrying a now-disallowed backtick, pre-filled and left
		// untouched, must NOT block an alias-only edit.
		const legacy = "Prod `db`"
		_, changed, aliases, fe := parseTunnelEditModalArgs(values(legacy, "$one"), meta(legacy))
		if len(fe) != 0 {
			t.Fatalf("untouched legacy name should not error: %v", fe)
		}
		if changed {
			t.Errorf("nameChanged = true for an untouched pre-filled name")
		}
		if strings.Join(aliases, ",") != "one" {
			t.Errorf("aliases = %v, want [one]", aliases)
		}
	})
	t.Run("whitespace-only difference is not a change", func(t *testing.T) {
		// Stored name with surrounding whitespace, pre-filled and untouched: the
		// trim must not register as a change (which would fire a spurious PATCH).
		_, changed, _, fe := parseTunnelEditModalArgs(values("  "+testEditDisplay+"  ", ""), meta(testEditDisplay))
		if len(fe) != 0 {
			t.Fatalf("unexpected field errors: %v", fe)
		}
		if changed {
			t.Errorf("nameChanged = true for a whitespace-only difference (would fire a spurious PATCH)")
		}
	})
}

func TestFormatTunnelEditSummary_EscapesToken(t *testing.T) {
	// A token can be an upstream slug that never passed the alias charset fence,
	// so a backtick must be escaped or it breaks the code span (defense-in-depth).
	out := formatTunnelEditSummary("ev`il", nil, &aliasReconcileResult{}, &channelExposureResult{})
	if strings.Contains(out, "ev`il") {
		t.Errorf("raw backtick token leaked into the summary: %q", out)
	}
	if !strings.Contains(out, "evˊil") {
		t.Errorf("expected the token's backtick escaped to ˊ, got: %q", out)
	}
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

// TestHandleList_RendersEditButtonForOwnerNotOnAdminSet fences the owner→admin
// self-heal at the /qurl list surface: the bot-assigned owner (recorded by
// `/qurl setup`) sees the Edit button even when they are NOT on
// admin_slack_user_ids. editableListHandler seeds owner=testAdminOwnerID with
// the admin set holding only testAdminUserID, so the owner is deliberately off
// the set — pre-fix this rendered Create-only and the owner was locked out of
// Edit (the reported regression).
func TestHandleList_RendersEditButtonForOwnerNotOnAdminSet(t *testing.T) {
	h, _ := editableListHandler(t)
	inv := newAdminSlashInvoker(t, h)

	if status, _ := inv.invokeAdmin("list", testAdminTeamID, testAdminOwnerID); status != http.StatusOK {
		t.Fatalf("status != 200")
	}
	blocks := parseSlackBlocks(t, inv.captured.waitForBody(t, 2*time.Second))
	if vals := editButtonValues(t, blocks); len(vals) != 1 {
		t.Fatalf("Edit button count = %d for owner caller not on admin set, want 1; blocks=%v", len(vals), blocks)
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

// TestHandleList_OverCapRowFallsBackToCreateOnly fences the snapshot-size guard
// at the render level: a row whose Edit snapshot would exceed Slack's
// button-value cap (here a tunnel with a large bound-alias set) degrades to the
// Create-only accessory button, while a normal sibling row keeps Edit.
func TestHandleList_OverCapRowFallsBackToCreateOnly(t *testing.T) {
	const (
		normalRID = "r_normal"
		normalTok = "normal-tun"
		bigRID    = "r_big"
		bigTok    = "big-tun"
	)
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: normalRID, testKeyType: client.ResourceTypeTunnel, testKeySlug: normalTok, testKeyDescription: "Normal"},
			{testKeyResourceID: bigRID, testKeyType: client.ResourceTypeTunnel, testKeySlug: bigTok, testKeyDescription: "Big"},
		}, "", false)
	})
	// Bind both rows' primary slugs, plus a large extra-alias set on bigRID that
	// pushes its snapshot past slackButtonValueMaxBytes.
	bindings := map[string]string{normalTok: normalRID, bigTok: bigRID}
	for i := 0; i < 80; i++ {
		bindings[fmt.Sprintf("big-alias-%02d-%s", i, strings.Repeat("z", 20))] = bigRID
	}
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, bindings)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	inv := newAdminSlashInvoker(t, h) // default channel_id "C_test" == testEditChannel
	if status, _ := inv.invokeAdmin("list", testAdminTeamID, testAdminUserID); status != http.StatusOK {
		t.Fatalf("status != 200")
	}
	blocks := parseSlackBlocks(t, inv.captured.waitForBody(t, 2*time.Second))

	// Only the normal row carries an Edit button.
	editVals := editButtonValues(t, blocks)
	if len(editVals) != 1 {
		t.Fatalf("Edit button count = %d, want 1 (the over-cap row must drop Edit)", len(editVals))
	}
	var snap tunnelEditButtonValue
	if err := json.Unmarshal([]byte(editVals[0]), &snap); err != nil {
		t.Fatalf("Edit value not JSON: %v", err)
	}
	if snap.ResourceID != normalRID {
		t.Errorf("Edit button is for %q, want the normal row %q", snap.ResourceID, normalRID)
	}
	// The over-cap row degrades to a Create-only accessory button (sectionWithButton,
	// not an actions block), so it's still mintable — just without Edit.
	createVals := createQurlButtonValues(t, blocks)
	if len(createVals) != 1 || createVals[0] != bigTok {
		t.Errorf("over-cap row Create-only accessory = %v, want [%s]", createVals, bigTok)
	}
}

// TestHandleList_AdminPastEditCapKeepsCreateButtons fences the no-asymmetry
// fix: an admin whose tunnel count is past listEditButtonMaxRows but within
// listCreateButtonMaxRows still gets Create-only buttons (no per-row Edit),
// rather than the whole list collapsing to text as it would if the edit cap
// gated all buttons.
func TestHandleList_AdminPastEditCapKeepsCreateButtons(t *testing.T) {
	n := listEditButtonMaxRows + 1 // past the edit cap, within the create cap
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	resources := make([]map[string]any, n)
	rids := make([]string, n)
	for i := 0; i < n; i++ {
		slug := fmt.Sprintf("tun-%03d", i)
		rids[i] = "r_" + slug
		resources[i] = map[string]any{testKeyResourceID: "r_" + slug, testKeyType: client.ResourceTypeTunnel, testKeySlug: slug}
	}
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, resources, "", false)
	})
	// All exposed to C_test (channel-scoped list).
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", rids...)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	inv := newAdminSlashInvoker(t, h)
	if status, _ := inv.invokeAdmin("list", testAdminTeamID, testAdminUserID); status != http.StatusOK {
		t.Fatalf("status != 200")
	}
	blocks := parseSlackBlocks(t, inv.captured.waitForBody(t, 2*time.Second))
	if len(blocks) == 0 {
		t.Fatalf("expected interactive Create buttons, got a plain-text (block-less) list")
	}
	if edits := editButtonValues(t, blocks); len(edits) != 0 {
		t.Errorf("Edit buttons should be absent past the edit cap, got %d", len(edits))
	}
	if creates := createQurlButtonValues(t, blocks); len(creates) != n {
		t.Errorf("Create qURL buttons = %d, want %d (admin keeps Create-only past the edit cap)", len(creates), n)
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
		testEditToken:    testEditResourceID,
		testEditOldAlias: testEditResourceID,
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
	srv, done := editResultServer(t, &got)

	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken, DisplayName: testEditDisplay,
		Aliases: []string{testEditOldAlias}, // snapshot the modal pre-filled with
	}
	// New display name + new alias; "old-alias" dropped (it was in the snapshot).
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
	for _, want := range []string{"Updated tunnel `$" + testEditToken + "`", "new-alias", testEditOldAlias} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q: %s", want, summary)
		}
	}
	// Alias store reflects the reconcile.
	if rid, found, _ := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testEditChannel, "new-alias"); !found || rid != testEditResourceID {
		t.Errorf("new-alias not bound: rid=%q found=%v", rid, found)
	}
	if _, found, _ := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testEditChannel, testEditOldAlias); found {
		t.Errorf("old-alias should have been unbound")
	}
}

// TestHandleTunnelEdit_NoOpSubmission fences the no-op path: submitting the
// pre-filled values verbatim (same name, same aliases) skips the PATCH (name
// unchanged) and the alias reconcile is a no-op, so the admin sees "No changes."
// and the bindings are untouched.
func TestHandleTunnelEdit_NoOpSubmission(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	// token + one extra alias bound; the submission keeps both unchanged.
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, map[string]string{
		testEditToken: testEditResourceID,
		testEditAlias: testEditResourceID,
	})
	ts.addCustomer(http.MethodPatch, "/v1/resources/"+testEditResourceID, func(http.ResponseWriter, *http.Request) {
		t.Errorf("PATCH reached on a no-op (name-unchanged) submission")
	})
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	var got []byte
	srv, done := editResultServer(t, &got)

	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken,
		DisplayName: testEditDisplay, Aliases: []string{testEditAlias},
	}
	// Submit the pre-filled values verbatim: same name, same single extra alias.
	body := tunnelEditViewSubmissionBody(t, &meta, testAdminTeamID, testAdminUserID, testEditDisplay, "$"+testEditAlias)
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
	if summary := parseSlackText(t, got); !strings.Contains(summary, "No changes.") {
		t.Errorf("summary = %q, want it to contain \"No changes.\"", summary)
	}
	if rid, found, _ := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testEditChannel, testEditAlias); !found || rid != testEditResourceID {
		t.Errorf("extra alias should remain bound: rid=%q found=%v", rid, found)
	}
}

// editResultServer wires a response_url capture server and returns it plus a
// done channel closed on the first POST, shared by the submission tests below.
func editResultServer(t *testing.T, got *[]byte) (srv *httptest.Server, done chan struct{}) {
	t.Helper()
	done = make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		close(done)
	}))
	t.Cleanup(srv.Close)
	return srv, done
}

// TestHandleTunnelEdit_EmptyNameAliasOnly fences bug-class #1: a tunnel with NO
// Display Name stays editable for an alias-only change. The empty (optional)
// name field submits empty, which the changed-only diff treats as a no-op (no
// PATCH), while the new alias still binds.
func TestHandleTunnelEdit_EmptyNameAliasOnly(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, map[string]string{
		testEditToken: testEditResourceID, // only the token bound; no display name
	})
	ts.addCustomer(http.MethodPatch, "/v1/resources/"+testEditResourceID, func(http.ResponseWriter, *http.Request) {
		t.Errorf("PATCH reached for an empty-name (no-op) edit")
	})
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	var got []byte
	srv, done := editResultServer(t, &got)
	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken,
		DisplayName: "", // tunnel has no display name
	}
	body := tunnelEditViewSubmissionBody(t, &meta, testAdminTeamID, testAdminUserID, "", "$fresh")
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
	summary := parseSlackText(t, got)
	if !strings.Contains(summary, "fresh") {
		t.Errorf("summary should report the added alias: %s", summary)
	}
	if strings.Contains(summary, "Display Name") {
		t.Errorf("no Display Name change expected for an empty-name edit: %s", summary)
	}
	if rid, found, _ := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testEditChannel, "fresh"); !found || rid != testEditResourceID {
		t.Errorf("'fresh' alias should be bound: rid=%q found=%v", rid, found)
	}
}

// TestHandleTunnelEdit_EmptySnapshotRenamePreservesAliases fences the
// stale/empty-snapshot data-loss window: if the modal opened with an empty
// alias snapshot (e.g. the list-render policy fetch transiently failed) while
// the tunnel actually has aliases bound, a rename-only edit must NOT unbind
// those aliases — removals are scoped to what the admin saw, which was empty.
func TestHandleTunnelEdit_EmptySnapshotRenamePreservesAliases(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	// The tunnel really has an extra alias bound, but meta.Aliases (the snapshot)
	// is empty — the admin never saw "keepme".
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, map[string]string{
		testEditToken: testEditResourceID,
		"keepme":      testEditResourceID,
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
	srv, done := editResultServer(t, &got)
	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken,
		DisplayName: testEditDisplay, Aliases: nil, // empty/stale snapshot
	}
	// Rename only; aliases box left empty (matching the empty snapshot).
	body := tunnelEditViewSubmissionBody(t, &meta, testAdminTeamID, testAdminUserID, "Renamed", "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no result posted to response_url")
	}
	if patched != "Renamed" {
		t.Errorf("PATCHed description = %q, want %q", patched, "Renamed")
	}
	// The alias the admin never saw must survive a rename-only edit.
	if rid, found, _ := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testEditChannel, "keepme"); !found || rid != testEditResourceID {
		t.Errorf("'keepme' must NOT be unbound by an empty-snapshot rename: rid=%q found=%v", rid, found)
	}
	if summary := parseSlackText(t, got); strings.Contains(summary, "Removed") {
		t.Errorf("no removals expected on an empty-snapshot rename: %s", summary)
	}
}

// TestHandleTunnelEdit_AliasConflictReported fences the conflict path: adding an
// alias already bound to a DIFFERENT tunnel in the channel is skipped and
// reported, leaving the other tunnel's binding intact.
func TestHandleTunnelEdit_AliasConflictReported(t *testing.T) {
	const otherRID = "r_other_tunnel"
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, map[string]string{
		testEditToken: testEditResourceID,
		"taken":       otherRID, // already owned by a different tunnel
	})
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	var got []byte
	srv, done := editResultServer(t, &got)
	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken,
		DisplayName: testEditDisplay, // unchanged → no PATCH
	}
	body := tunnelEditViewSubmissionBody(t, &meta, testAdminTeamID, testAdminUserID, testEditDisplay, "$taken")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no result posted to response_url")
	}
	if summary := parseSlackText(t, got); !strings.Contains(summary, "Skipped (already used by another tunnel") || !strings.Contains(summary, "taken") {
		t.Errorf("summary missing conflict line: %s", summary)
	}
	if rid, found, _ := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testEditChannel, "taken"); !found || rid != otherRID {
		t.Errorf("'taken' should still belong to %q: rid=%q found=%v", otherRID, rid, found)
	}
}

// TestHandleTunnelEdit_BindErrorSurfacesWarning fences the hadError path: a
// non-conflict bind failure surfaces the ":warning: Some changes may not have
// applied" line rather than reporting a clean success.
func TestHandleTunnelEdit_BindErrorSurfacesWarning(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, map[string]string{
		testEditToken: testEditResourceID,
	})
	// Fail alias writes (UpdateItem) on the channel-policy table; the policy
	// READ still succeeds, so the failure is a bind error, not a read error.
	ts.ddb.SetUpdateItemErr(ts.tableNames.channelPolicy, errors.New("injected bind failure"))
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	var got []byte
	srv, done := editResultServer(t, &got)
	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken,
		DisplayName: testEditDisplay, // unchanged → no PATCH; isolate the alias-bind failure
	}
	body := tunnelEditViewSubmissionBody(t, &meta, testAdminTeamID, testAdminUserID, testEditDisplay, "$newone")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no result posted to response_url")
	}
	if summary := parseSlackText(t, got); !strings.Contains(summary, "Some changes may not have applied") {
		t.Errorf("summary missing hadError warning: %s", summary)
	}
}

// TestHandleTunnelEdit_NamePatchFailureLeavesAliasesUntouched fences the
// fail-fast ordering: when the Display Name PATCH fails, processTunnelEdit
// returns before the alias reconcile, so a combined name+alias edit leaves the
// aliases unchanged and surfaces the name-update failure.
func TestHandleTunnelEdit_NamePatchFailureLeavesAliasesUntouched(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, map[string]string{
		testEditToken:    testEditResourceID,
		testEditOldAlias: testEditResourceID,
	})
	ts.addCustomer(http.MethodPatch, "/v1/resources/"+testEditResourceID, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	var got []byte
	srv, done := editResultServer(t, &got)
	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken,
		DisplayName: testEditDisplay, Aliases: []string{testEditOldAlias},
	}
	// Change the name (PATCH will 500) AND drop the old alias: the alias change
	// must not apply because the name PATCH fails first.
	body := tunnelEditViewSubmissionBody(t, &meta, testAdminTeamID, testAdminUserID, "Renamed", "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no result posted to response_url")
	}
	if summary := parseSlackText(t, got); !strings.Contains(summary, "Failed to update the Display Name") {
		t.Errorf("expected display-name failure message, got: %s", summary)
	}
	// The old alias must still be bound — the reconcile never ran.
	if rid, found, _ := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testEditChannel, testEditOldAlias); !found || rid != testEditResourceID {
		t.Errorf("alias must be untouched after a failed name PATCH: rid=%q found=%v", rid, found)
	}
}

// TestHandleTunnelEdit_ReplayCrossChecks fences the modal-replay access
// boundary: a view_submission whose Slack-signed envelope (team/user) doesn't
// match the private_metadata — or whose metadata is unparseable/incomplete — is
// rejected with an error view before any mutation. Mirrors the install modal's
// posture.
func TestHandleTunnelEdit_ReplayCrossChecks(t *testing.T) {
	baseMeta := func() TunnelEditModalMetadata {
		return TunnelEditModalMetadata{
			TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
			ResponseURL: "https://hooks.slack.com/x", ResourceID: testEditResourceID, Token: testEditToken, DisplayName: testEditDisplay,
		}
	}
	cases := []struct {
		name            string
		rawMetadata     string // used verbatim when non-empty (e.g. unparseable)
		mutate          func(*TunnelEditModalMetadata)
		payloadTeamID   string
		payloadUserID   string
		wantErrContains string
	}{
		{name: "unparseable metadata", rawMetadata: "not json", payloadTeamID: testAdminTeamID, payloadUserID: testAdminUserID, wantErrContains: "Could not verify this dialog"},
		{name: "incomplete metadata", mutate: func(m *TunnelEditModalMetadata) { m.ResourceID = "" }, payloadTeamID: testAdminTeamID, payloadUserID: testAdminUserID, wantErrContains: "Could not verify this dialog"},
		{name: "team mismatch", payloadTeamID: "T_other", payloadUserID: testAdminUserID, wantErrContains: "different workspace"},
		{name: "user mismatch", payloadTeamID: testAdminTeamID, payloadUserID: "U_other", wantErrContains: "Only the admin who opened"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newAdminTestServers(t)
			ts.seedAdmin(t)
			ts.addCustomer(http.MethodPatch, "/v1/resources/"+testEditResourceID, func(http.ResponseWriter, *http.Request) {
				t.Errorf("PATCH reached despite a failed replay cross-check")
			})
			h := newAdminTestHandler(t, ts)
			h.SetAliasStore(h.cfg.AdminStore)
			h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

			pm := tc.rawMetadata
			if pm == "" {
				m := baseMeta()
				if tc.mutate != nil {
					tc.mutate(&m)
				}
				b, err := json.Marshal(m)
				if err != nil {
					t.Fatalf("marshal meta: %v", err)
				}
				pm = string(b)
			}
			body := viewSubmissionBody(t, "V_replay", callbackIDTunnelEdit, pm, tc.payloadTeamID, tc.payloadUserID,
				map[string]map[string]interactionStateValue{
					tunnelEditBlockDisplayName: {tunnelEditActionDisplayName: {Value: "Renamed"}},
					tunnelEditBlockAliases:     {tunnelEditActionAliases: {Value: ""}},
				})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
			h.Wait()
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if !strings.Contains(w.Body.String(), tc.wantErrContains) {
				t.Errorf("response missing %q: %s", tc.wantErrContains, w.Body.String())
			}
		})
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

// --- channel exposure (the "expose to channels" field) -------------------

// tunnelEditViewSubmissionBodyWithChannels is tunnelEditViewSubmissionBody plus
// a channels multi-select selection, for the channel-exposure reconcile tests.
func tunnelEditViewSubmissionBodyWithChannels(t *testing.T, meta *TunnelEditModalMetadata, payloadTeamID, payloadUserID, displayName, aliasesText string, channels []string) string {
	t.Helper()
	pm, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal private_metadata: %v", err)
	}
	return viewSubmissionBody(t, "V_test_edit", callbackIDTunnelEdit, string(pm), payloadTeamID, payloadUserID,
		map[string]map[string]interactionStateValue{
			tunnelEditBlockDisplayName: {tunnelEditActionDisplayName: {Value: displayName}},
			tunnelEditBlockAliases:     {tunnelEditActionAliases: {Value: aliasesText}},
			tunnelEditBlockChannels:    {tunnelEditActionChannels: {SelectedConversations: channels}},
		})
}

func TestParseEditChannelSelection(t *testing.T) {
	meta := &TunnelEditModalMetadata{ChannelID: testEditHomeChannel}
	withChannels := func(channels []string) map[string]map[string]interactionStateValue {
		return map[string]map[string]interactionStateValue{
			tunnelEditBlockChannels: {tunnelEditActionChannels: {SelectedConversations: channels}},
		}
	}
	t.Run("force-includes current channel first and dedups", func(t *testing.T) {
		got := parseEditChannelSelection(withChannels([]string{testEditExtraChannel, testEditExtraChannel}), meta)
		if len(got) != 2 || got[0] != testEditHomeChannel || got[1] != testEditExtraChannel {
			t.Errorf("got %v, want [%s %s]", got, testEditHomeChannel, testEditExtraChannel)
		}
	})
	t.Run("drops malformed conversation ids", func(t *testing.T) {
		got := parseEditChannelSelection(withChannels([]string{testEditHomeChannel, "bad id!", ""}), meta)
		if len(got) != 1 || got[0] != testEditHomeChannel {
			t.Errorf("got %v, want just the current channel (malformed dropped)", got)
		}
	})
	t.Run("empty selection still keeps the current channel", func(t *testing.T) {
		got := parseEditChannelSelection(map[string]map[string]interactionStateValue{}, meta)
		if len(got) != 1 || got[0] != testEditHomeChannel {
			t.Errorf("got %v, want [%s]", got, testEditHomeChannel)
		}
	})
}

// TestHandleTunnelEdit_ExposesNewChannel fences requirement #4: selecting a new
// channel in the Edit modal grants the tunnel access there
// (allowed_resource_ids), so it becomes visible in that channel's /qurl list
// and mintable via /qurl get.
func TestHandleTunnelEdit_ExposesNewChannel(t *testing.T) {
	const newChannel = "C0newchan01"
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	// The tunnel lives in C_test via its slug alias; the modal opened showing
	// only C_test as exposed.
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, map[string]string{
		testEditToken: testEditResourceID,
	})
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	var got []byte
	srv, done := editResultServer(t, &got)
	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken, DisplayName: testEditDisplay,
		ExposedChannels: []string{testEditChannel}, // modal showed current channel only
	}
	// Keep the current channel, add a new one; no name/alias change.
	body := tunnelEditViewSubmissionBodyWithChannels(t, &meta, testAdminTeamID, testAdminUserID, testEditDisplay, "", []string{testEditChannel, newChannel})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no result posted to response_url")
	}
	allowed, err := h.cfg.AdminStore.AllowedResourceIDsForChannel(context.Background(), testAdminTeamID, newChannel)
	if err != nil {
		t.Fatalf("AllowedResourceIDsForChannel: %v", err)
	}
	if _, ok := allowed[testEditResourceID]; !ok {
		t.Errorf("resource not exposed to the new channel; allow-set = %v", allowed)
	}
	if summary := parseSlackText(t, got); !strings.Contains(summary, "Exposed to:") || !strings.Contains(summary, newChannel) {
		t.Errorf("summary missing expose line for the new channel: %s", summary)
	}
}

// TestHandleTunnelEdit_RevokesDeselectedChannel fences the inverse: de-selecting
// a channel the modal showed removes the tunnel's allow-set grant there.
func TestHandleTunnelEdit_RevokesDeselectedChannel(t *testing.T) {
	const extraChannel = "C0extra0001"
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, map[string]string{
		testEditToken: testEditResourceID,
	})
	// Also exposed to extraChannel via its allow-set.
	ts.seedChannelExposure(t, testAdminTeamID, extraChannel, testEditResourceID)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	var got []byte
	srv, done := editResultServer(t, &got)
	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken, DisplayName: testEditDisplay,
		ExposedChannels: []string{testEditChannel, extraChannel}, // modal showed both
	}
	// Admin de-selects extraChannel (keeps only the current channel).
	body := tunnelEditViewSubmissionBodyWithChannels(t, &meta, testAdminTeamID, testAdminUserID, testEditDisplay, "", []string{testEditChannel})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no result posted to response_url")
	}
	allowed, err := h.cfg.AdminStore.AllowedResourceIDsForChannel(context.Background(), testAdminTeamID, extraChannel)
	if err != nil {
		t.Fatalf("AllowedResourceIDsForChannel: %v", err)
	}
	if _, ok := allowed[testEditResourceID]; ok {
		t.Errorf("resource should have been revoked from extraChannel; allow-set = %v", allowed)
	}
	if summary := parseSlackText(t, got); !strings.Contains(summary, "Revoked from:") || !strings.Contains(summary, extraChannel) {
		t.Errorf("summary missing revoke line for the de-selected channel: %s", summary)
	}
}

// TestHandleTunnelEdit_CurrentChannelNeverRevoked fences that the channel the
// admin is editing from keeps access even if it's de-selected — the tunnel must
// not vanish from the very list the admin is managing it on. The current channel
// is exposed via its allow-set here so a wrongful revoke WOULD remove it,
// making the assertion bite.
func TestHandleTunnelEdit_CurrentChannelNeverRevoked(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedChannelExposure(t, testAdminTeamID, testEditChannel, testEditResourceID)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	var got []byte
	srv, done := editResultServer(t, &got)
	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken, DisplayName: testEditDisplay,
		ExposedChannels: []string{testEditChannel},
	}
	// Admin clears the channels field entirely.
	body := tunnelEditViewSubmissionBodyWithChannels(t, &meta, testAdminTeamID, testAdminUserID, testEditDisplay, "", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no result posted to response_url")
	}
	allowed, err := h.cfg.AdminStore.AllowedResourceIDsForChannel(context.Background(), testAdminTeamID, testEditChannel)
	if err != nil {
		t.Fatalf("AllowedResourceIDsForChannel: %v", err)
	}
	if _, ok := allowed[testEditResourceID]; !ok {
		t.Errorf("current channel was revoked despite the never-revoke guard; allow-set = %v", allowed)
	}
	if summary := parseSlackText(t, got); strings.Contains(summary, "Revoked from:") {
		t.Errorf("no revocation expected for a current-channel-only edit: %s", summary)
	}
}

// TestHandleListEditClick_PrefillsExposedChannels fences the modal-open
// enumeration: the channels multi-select is pre-filled with every channel the
// tunnel is exposed to (current channel via alias + another via allow-set).
func TestHandleListEditClick_PrefillsExposedChannels(t *testing.T) {
	const otherChannel = "C0other0001"
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, map[string]string{
		testEditToken: testEditResourceID,
	})
	ts.seedChannelExposure(t, testAdminTeamID, otherChannel, testEditResourceID)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	views := make(chan []byte, 1)
	h.cfg.OpenView = func(_ context.Context, _ string, _ string, view []byte) error {
		views <- view
		return nil
	}

	snap, _ := buildTunnelEditButtonValue(testEditResourceID, testEditToken, testEditDisplay, nil)
	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, testEditChannel, "https://hooks.slack.com/edit", listEditTunnelActionID, snap)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var view []byte
	select {
	case view = <-views:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenView not called")
	}
	js := string(view)
	for _, want := range []string{blockKitTypeMultiConvSelect, "initial_conversations", testEditChannel, otherChannel} {
		if !strings.Contains(js, want) {
			t.Errorf("modal pre-fill missing %q: %s", want, js)
		}
	}
}

// TestHandleListEditClick_ChannelEnumerationDegrades fences the best-effort
// degradation: when the enumeration Query fails (e.g. the dynamodb:Query grant
// isn't deployed), the modal still opens with the channels field pre-filled
// with just the current channel — the un-enumerable channel is absent, and
// because it's not in the baseline the submit reconcile can't revoke it.
func TestHandleListEditClick_ChannelEnumerationDegrades(t *testing.T) {
	const otherChannel = "C0other0002"
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testEditChannel, map[string]string{testEditToken: testEditResourceID})
	ts.seedChannelExposure(t, testAdminTeamID, otherChannel, testEditResourceID)
	ts.ddb.SetQueryErr(ts.tableNames.channelPolicy, errors.New("AccessDenied: dynamodb:Query"))
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	views := make(chan []byte, 1)
	h.cfg.OpenView = func(_ context.Context, _ string, _ string, view []byte) error {
		views <- view
		return nil
	}

	snap, _ := buildTunnelEditButtonValue(testEditResourceID, testEditToken, testEditDisplay, nil)
	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, testEditChannel, "https://hooks.slack.com/edit", listEditTunnelActionID, snap)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var view []byte
	select {
	case view = <-views:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenView not called — the modal must still open when enumeration fails")
	}
	js := string(view)
	if !strings.Contains(js, blockKitTypeMultiConvSelect) || !strings.Contains(js, testEditChannel) {
		t.Errorf("degraded modal missing channels field / current channel: %s", js)
	}
	if strings.Contains(js, otherChannel) {
		t.Errorf("enumeration failed yet the other channel appeared in the pre-fill: %s", js)
	}
}
