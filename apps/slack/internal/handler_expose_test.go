package internal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

const testExposeChannel = "C_test"

// --- chooser (slash) ------------------------------------------------------

// TestExposeChooserBlocks fences the picker's shape: a section, a target-channel
// context line, and an actions row with exactly the two buttons wired to the
// connector/URL action_ids.
func TestExposeChooserBlocks(t *testing.T) {
	blocks := exposeChooserBlocks(testExposeChannel)
	js, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal chooser blocks: %v", err)
	}
	s := string(js)
	for _, want := range []string{exposeConnectorActionID, exposeURLActionID, "Expose qURL Connector", "Expose URL", testExposeChannel} {
		if !strings.Contains(s, want) {
			t.Errorf("chooser blocks missing %q: %s", want, s)
		}
	}
	// Exactly one actions block carrying both buttons.
	var actions int
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && m[blockKitFieldType] == blockKitTypeActions {
			actions++
			if els, _ := m[blockKitFieldElements].([]any); len(els) != 2 {
				t.Errorf("actions row has %d buttons, want 2", len(els))
			}
		}
	}
	if actions != 1 {
		t.Errorf("actions blocks = %d, want 1", actions)
	}
}

// TestHandleExpose_AdminRendersChooser fences that an admin gets the picker
// (and that `expose` dispatches at all — a missing verb would render the
// unknown-subcommand reply instead).
func TestHandleExpose_AdminRendersChooser(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }
	inv := newAdminSlashInvoker(t, h)

	status, reply := inv.invokeAdmin("expose", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(reply, "expose") {
		t.Fatalf("reply = %q, want the chooser fallback text", reply)
	}
}

// TestHandleExpose_NonAdminDenied fences the in-code admin gate (OpenView is
// wired so the request reaches the gate rather than the not-configured branch).
func TestHandleExpose_NonAdminDenied(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("expose", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "admin-only") {
		t.Fatalf("reply = %q, want admin denial", reply)
	}
}

// TestHandleExpose_NoOpenViewConfigured fences that without guided setup wired
// the chooser declines and points at the typed forms (its buttons would be dead
// otherwise).
func TestHandleExpose_NoOpenViewConfigured(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	// OpenView intentionally left nil.
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("expose", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "Guided setup is not configured") {
		t.Fatalf("reply = %q, want not-configured notice", reply)
	}
}

// TestAdminHelpReflectsExposeVerb fences that `/qurl-admin help` advertises the
// expose chooser as the guided entry when OpenView is wired.
func TestAdminHelpReflectsExposeVerb(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	help := h.adminHelpMessage(commandAdmin)
	if !strings.Contains(help, "/qurl-admin expose") {
		t.Fatalf("admin help missing the expose verb:\n%s", help)
	}
}

// --- shortcut verbs: expose-connector / expose-url ------------------------

// countActionButtons returns the number of button elements across every
// actions block in a slash-command JSON response body, asserting the named
// action_id appears. It lets the shortcut-verb handler tests prove they emit
// the single button their block builder carries (not just matching fallback
// text), guarding against a handler → wrong-builder miswiring.
func countActionButtons(t *testing.T, body []byte, wantActionID string) (buttons int, hasWant bool) {
	t.Helper()
	var resp struct {
		Blocks []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("response JSON: %v\n%s", err, body)
	}
	for _, b := range resp.Blocks {
		if b[blockKitFieldType] != blockKitTypeActions {
			continue
		}
		els, _ := b[blockKitFieldElements].([]any)
		for _, e := range els {
			buttons++
			if m, ok := e.(map[string]any); ok && m[blockKitFieldActionID] == wantActionID {
				hasWant = true
			}
		}
	}
	return buttons, hasWant
}

// countSliceActionButtons counts button elements across the actions blocks of a
// raw block slice (as returned by the *Blocks builders, before any envelope
// wraps them). The handler-body counterpart is countActionButtons.
func countSliceActionButtons(t *testing.T, blocks []any) (buttons int) {
	t.Helper()
	for _, b := range blocks {
		m, ok := b.(map[string]any)
		if !ok || m[blockKitFieldType] != blockKitTypeActions {
			continue
		}
		els, _ := m[blockKitFieldElements].([]any)
		buttons += len(els)
	}
	return buttons
}

// invokeAdminRawBody issues a signed `/qurl-admin` slash request and returns the
// status + full (block-carrying) sync response body. invokeAdmin unwraps to the
// text field, which can't see the blocks; the shortcut-verb render tests need
// the blocks to assert the exact button. The response is synchronous
// (respondSlackBlocks), so the response_url is never hit — a placeholder is fine.
func invokeAdminRawBody(t *testing.T, h *Handler, text, teamID, userID string) (status int, body []byte) {
	t.Helper()
	form := url.Values{
		"command":      {commandAdmin},
		"text":         {text},
		"team_id":      {teamID},
		"user_id":      {userID},
		"channel_id":   {testExposeChannel},
		"response_url": {"https://hooks.slack.com/expose-shortcut"},
		"trigger_id":   {"trigger_test"},
	}
	encoded := form.Encode()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackCommands, encoded, encoded))
	return w.Code, w.Body.Bytes()
}

// TestExposeConnectorButtonBlocks fences the expose-connector shortcut's single
// button: one actions row with exactly the connector button wired to
// exposeConnectorActionID (so its click reuses handleExposeConnectorClick).
func TestExposeConnectorButtonBlocks(t *testing.T) {
	blocks := exposeConnectorButtonBlocks(testExposeChannel)
	js, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal connector button blocks: %v", err)
	}
	s := string(js)
	for _, want := range []string{exposeConnectorActionID, "Expose qURL Connector", testExposeChannel} {
		if !strings.Contains(s, want) {
			t.Errorf("connector button blocks missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, exposeURLActionID) {
		t.Errorf("connector button blocks should not carry the URL button: %s", s)
	}
	if buttons := countSliceActionButtons(t, blocks); buttons != 1 {
		t.Errorf("connector blocks have %d buttons (want exactly 1, the connector button)", buttons)
	}
}

// TestExposeURLButtonBlocks is the URL counterpart: one actions row with exactly
// the URL button wired to exposeURLActionID.
func TestExposeURLButtonBlocks(t *testing.T) {
	blocks := exposeURLButtonBlocks(testExposeChannel)
	js, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal url button blocks: %v", err)
	}
	s := string(js)
	for _, want := range []string{exposeURLActionID, "Expose URL", testExposeChannel} {
		if !strings.Contains(s, want) {
			t.Errorf("url button blocks missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, exposeConnectorActionID) {
		t.Errorf("url button blocks should not carry the connector button: %s", s)
	}
	if buttons := countSliceActionButtons(t, blocks); buttons != 1 {
		t.Errorf("url blocks have %d buttons (want exactly 1, the URL button)", buttons)
	}
}

// TestHandleExposeConnectorCmd_AdminRendersButton fences that an admin gets the
// single connector button (and that the verb dispatches — a missing case would
// render the unknown-subcommand reply). Inspects the full body so a swap to the
// URL builder is caught, not just the fallback text.
func TestHandleExposeConnectorCmd_AdminRendersButton(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	status, body := invokeAdminRawBody(t, h, "expose-connector", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	buttons, hasConnector := countActionButtons(t, body, exposeConnectorActionID)
	if buttons != 1 || !hasConnector {
		t.Fatalf("expose-connector rendered %d buttons / connector=%v, want exactly the connector button:\n%s", buttons, hasConnector, body)
	}
	if strings.Contains(string(body), exposeURLActionID) {
		t.Fatalf("expose-connector must not render the URL button:\n%s", body)
	}
}

// TestHandleExposeConnectorCmd_NonAdminDenied fences the in-code admin gate
// (OpenView wired so the request reaches the gate, not the not-configured arm).
func TestHandleExposeConnectorCmd_NonAdminDenied(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("expose-connector", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "admin-only") {
		t.Fatalf("reply = %q, want admin denial", reply)
	}
}

// TestHandleExposeConnectorCmd_NoOpenViewConfigured fences that without guided
// setup wired the shortcut declines and points at the typed installer (its
// button would be dead otherwise).
func TestHandleExposeConnectorCmd_NoOpenViewConfigured(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	// OpenView intentionally left nil.
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("expose-connector", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "Guided setup is not configured") {
		t.Fatalf("reply = %q, want not-configured notice", reply)
	}
}

// TestHandleExposeURLCmd_AdminRendersButton mirrors the connector render test
// for the URL shortcut.
func TestHandleExposeURLCmd_AdminRendersButton(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	status, body := invokeAdminRawBody(t, h, "expose-url", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	buttons, hasURL := countActionButtons(t, body, exposeURLActionID)
	if buttons != 1 || !hasURL {
		t.Fatalf("expose-url rendered %d buttons / url=%v, want exactly the URL button:\n%s", buttons, hasURL, body)
	}
	if strings.Contains(string(body), exposeConnectorActionID) {
		t.Fatalf("expose-url must not render the connector button:\n%s", body)
	}
}

// TestHandleExposeURLCmd_NonAdminDenied fences the in-code admin gate.
func TestHandleExposeURLCmd_NonAdminDenied(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("expose-url", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "admin-only") {
		t.Fatalf("reply = %q, want admin denial", reply)
	}
}

// TestHandleExposeURLCmd_NoOpenViewConfigured fences the not-configured arm,
// which points at the typed `resource expose` form.
func TestHandleExposeURLCmd_NoOpenViewConfigured(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	// OpenView intentionally left nil.
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("expose-url", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "Guided setup is not configured") {
		t.Fatalf("reply = %q, want not-configured notice", reply)
	}
}

// TestAdminHelpReflectsExposeShortcutVerbs fences that `/qurl-admin help`
// advertises both single-button shortcuts when OpenView is wired.
func TestAdminHelpReflectsExposeShortcutVerbs(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	help := h.adminHelpMessage(commandAdmin)
	for _, want := range []string{"/qurl-admin expose-connector", "/qurl-admin expose-url"} {
		if !strings.Contains(help, want) {
			t.Fatalf("admin help missing %q:\n%s", want, help)
		}
	}
}

// --- button clicks (block_actions) ----------------------------------------

// TestHandleExposeConnectorClick_OpensInstallModal fences that the "Expose qURL
// Connector" button opens the existing connector installer modal (reused
// wholesale — same callback_id its bare-command path uses).
func TestHandleExposeConnectorClick_OpensInstallModal(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	views := make(chan []byte, 1)
	h.cfg.OpenView = func(_ context.Context, _, _ string, view []byte) error {
		views <- view
		return nil
	}

	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, testExposeChannel, "https://hooks.slack.com/expose", exposeConnectorActionID, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("connector click ack = %d %q, want 200 {}", w.Code, w.Body.String())
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
	if modal[blockKitFieldCallbackID] != callbackIDTunnelInstall {
		t.Errorf("callback_id = %v, want %s", modal[blockKitFieldCallbackID], callbackIDTunnelInstall)
	}
}

// TestHandleExposeURLClick_OpensModalWithResourceOptions fences that the "Expose
// URL" button fetches the workspace's URL resources and opens the picker
// pre-populated with them (resource_id as the option value).
func TestHandleExposeURLClick_OpensModalWithResourceOptions(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testResourceExposeID, testKeyType: client.ResourceTypeURL, fAttrAlias: testResourceExposeAlias, testKeyTargetURL: testResourceExposeURL, testKeyStatus: client.StatusActive},
			// A tunnel resource must be filtered out of the URL picker.
			{testKeyResourceID: "r_tunnel_x", testKeyType: client.ResourceTypeTunnel, testKeySlug: "tun", testKeyStatus: client.StatusActive},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	views := make(chan []byte, 1)
	h.cfg.OpenView = func(_ context.Context, _, _ string, view []byte) error {
		views <- view
		return nil
	}

	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, testExposeChannel, "https://hooks.slack.com/expose", exposeURLActionID, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("url click ack = %d, want 200", w.Code)
	}

	var view []byte
	select {
	case view = <-views:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenView was not called")
	}
	js := string(view)
	if !strings.Contains(js, callbackIDExposeURL) {
		t.Errorf("modal callback_id missing %q: %s", callbackIDExposeURL, js)
	}
	if !strings.Contains(js, testResourceExposeID) {
		t.Errorf("URL resource option (value=%s) missing: %s", testResourceExposeID, js)
	}
	if strings.Contains(js, "r_tunnel_x") {
		t.Errorf("tunnel resource leaked into the URL picker: %s", js)
	}
}

// TestHandleExposeURLClick_NoResourcesPostsEphemeral fences that with no URL
// resources the button posts a guidance ephemeral and never opens an empty
// picker.
func TestHandleExposeURLClick_NoResourcesPostsEphemeral(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, nil, "", false)
	})
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	var opened atomic.Int32
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { opened.Add(1); return nil }

	captured := &capturedResponseURL{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured.record(b)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, testExposeChannel, srv.URL, exposeURLActionID, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("url click ack = %d, want 200", w.Code)
	}

	got := parseSlackText(t, captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(got, "No URL resources found") {
		t.Errorf("ephemeral = %q, want the no-resources guidance", got)
	}
	if opened.Load() != 0 {
		t.Errorf("OpenView called %d times with no resources, want 0", opened.Load())
	}
}

// --- submission (view_submission) -----------------------------------------

func exposeURLViewSubmissionBody(t *testing.T, meta ExposeURLModalMetadata, payloadTeamID, payloadUserID, resourceID, aliasText string) string {
	t.Helper()
	pm, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal private_metadata: %v", err)
	}
	return viewSubmissionBody(t, "V_test_expose", callbackIDExposeURL, string(pm), payloadTeamID, payloadUserID,
		map[string]map[string]interactionStateValue{
			exposeURLBlockResource: {exposeURLActionResource: {SelectedOption: &interactionSelectedOption{Value: resourceID}}},
			exposeURLBlockAlias:    {exposeURLActionAlias: {Value: aliasText}},
		})
}

// TestHandleExposeURLSubmission_BindsResource fences the happy path: a submitted
// resource + channel alias binds the alias to the resource_id and the admin sees
// the success reply.
func TestHandleExposeURLSubmission_BindsResource(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	captured := &capturedResponseURL{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured.record(b)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	meta := ExposeURLModalMetadata{TeamID: testAdminTeamID, ChannelID: testExposeChannel, UserID: testAdminUserID, ResponseURL: srv.URL}
	body := exposeURLViewSubmissionBody(t, meta, testAdminTeamID, testAdminUserID, testResourceExposeID, "$"+testResourceExposeChannelAlias)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("submission ack = %d %q, want 200 {}", w.Code, w.Body.String())
	}

	got := parseSlackText(t, captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(got, "now available as `$"+testResourceExposeChannelAlias+"`") {
		t.Fatalf("async reply = %q", got)
	}
	bound, found, err := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testExposeChannel, testResourceExposeChannelAlias)
	if err != nil {
		t.Fatalf("LookupChannelAlias: %v", err)
	}
	if !found || bound != testResourceExposeID {
		t.Fatalf("channel alias = (%q, %v), want (%q, true)", bound, found, testResourceExposeID)
	}
}

// TestHandleExposeURLSubmission_NonAdminDenied fences the mutation gate: a
// non-admin submission is refused and binds nothing (the picker is only shown to
// admins, but the submit re-checks rather than trusting the render-time gate).
func TestHandleExposeURLSubmission_NonAdminDenied(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	meta := ExposeURLModalMetadata{TeamID: testAdminTeamID, ChannelID: testExposeChannel, UserID: testAdminUserID, ResponseURL: "https://hooks.slack.com/x"}
	body := exposeURLViewSubmissionBody(t, meta, testAdminTeamID, testAdminUserID, testResourceExposeID, "$"+testResourceExposeChannelAlias)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	h.Wait()
	if !strings.Contains(w.Body.String(), "admin-only") {
		t.Fatalf("submission body = %q, want admin denial", w.Body.String())
	}
	if _, found, err := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testExposeChannel, testResourceExposeChannelAlias); err != nil {
		t.Fatalf("LookupChannelAlias: %v", err)
	} else if found {
		t.Fatalf("non-admin submission should not have bound an alias")
	}
}

// TestParseExposeURLModalArgs fences the modal field validation: a valid pick,
// a missing/garbage resource value, and a missing/invalid channel alias.
func TestParseExposeURLModalArgs(t *testing.T) {
	tests := []struct {
		name          string
		resourceValue string
		aliasValue    string
		wantResource  string
		wantAlias     string
		wantErrBlock  string
	}{
		{name: "valid", resourceValue: testResourceExposeID, aliasValue: "$docs", wantResource: testResourceExposeID, wantAlias: "docs"},
		{name: "alias without sigil", resourceValue: testResourceExposeID, aliasValue: "docs", wantResource: testResourceExposeID, wantAlias: "docs"},
		{name: "missing resource", resourceValue: "", aliasValue: "$docs", wantErrBlock: exposeURLBlockResource},
		{name: "non-resource-id value", resourceValue: "not-an-id", aliasValue: "$docs", wantErrBlock: exposeURLBlockResource},
		{name: "missing alias", resourceValue: testResourceExposeID, aliasValue: "", wantErrBlock: exposeURLBlockAlias},
		{name: "invalid alias", resourceValue: testResourceExposeID, aliasValue: "$Bad Alias", wantErrBlock: exposeURLBlockAlias},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]map[string]interactionStateValue{
				exposeURLBlockResource: {exposeURLActionResource: {SelectedOption: &interactionSelectedOption{Value: tc.resourceValue}}},
				exposeURLBlockAlias:    {exposeURLActionAlias: {Value: tc.aliasValue}},
			}
			resID, alias, fieldErrors := parseExposeURLModalArgs(values)
			if tc.wantErrBlock != "" {
				if _, ok := fieldErrors[tc.wantErrBlock]; !ok {
					t.Fatalf("field errors = %v, want an error on %q", fieldErrors, tc.wantErrBlock)
				}
				return
			}
			if len(fieldErrors) != 0 {
				t.Fatalf("unexpected field errors: %v", fieldErrors)
			}
			if resID != tc.wantResource || alias != tc.wantAlias {
				t.Fatalf("got (resource=%q alias=%q), want (resource=%q alias=%q)", resID, alias, tc.wantResource, tc.wantAlias)
			}
		})
	}
}
