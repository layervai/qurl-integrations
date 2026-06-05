package internal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// --- bare verb → guided picker (handleExposeURLWizard) --------------------

// TestHandleExposeURLBareOpensPickerModal fences that a bare `/qurl-admin
// expose-url` (no arguments) opens the same URL-resource picker the chooser's
// "Expose URL" button does — fetching the workspace's URL resources and opening
// ExposeURLModal via the slash command's own trigger. This is the no-arguments
// guided path; `expose-url <target>` is the typed path covered elsewhere.
func TestHandleExposeURLBareOpensPickerModal(t *testing.T) {
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
	inv := newAdminSlashInvoker(t, h)

	status, _ := inv.invokeAdmin("expose-url", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("ack status = %d, want 200", status)
	}
	var view []byte
	select {
	case view = <-views:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenView was not called for bare expose-url")
	}
	js := string(view)
	if !strings.Contains(js, callbackIDExposeURL) {
		t.Errorf("modal callback_id missing %q: %s", callbackIDExposeURL, js)
	}
	if !strings.Contains(js, testResourceExposeID) {
		t.Errorf("URL resource option (value=%s) missing: %s", testResourceExposeID, js)
	}
	if strings.Contains(js, "r_tunnel_x") {
		t.Errorf("tunnel resource leaked into the bare expose-url picker: %s", js)
	}
}

// TestHandleExposeURLBareNonAdminDenied fences the admin re-check on the bare
// path: a non-admin gets the denial via response_url, not the picker.
func TestHandleExposeURLBareNonAdminDenied(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	var opened atomic.Int32
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { opened.Add(1); return nil }
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("expose-url", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "admin-only") {
		t.Fatalf("async reply = %q, want admin denial", async)
	}
	if opened.Load() != 0 {
		t.Errorf("OpenView called %d times for a non-admin, want 0", opened.Load())
	}
}

// TestHandleExposeURLBareNoResources fences that with no URL resources the bare
// path posts the guidance ephemeral and never opens an empty picker.
func TestHandleExposeURLBareNoResources(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, nil, "", false)
	})
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	var opened atomic.Int32
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { opened.Add(1); return nil }
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("expose-url", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "No URL resources found") {
		t.Fatalf("async = %q, want the no-resources guidance", async)
	}
	if opened.Load() != 0 {
		t.Errorf("OpenView called %d times with no resources, want 0", opened.Load())
	}
}

// TestHandleExposeURLBareNoOpenViewDeclines fences that without guided setup
// wired the bare verb declines synchronously and points at the typed form.
func TestHandleExposeURLBareNoOpenViewDeclines(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	// OpenView intentionally left nil.
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("expose-url", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "Guided setup is not configured") || !strings.Contains(reply, "expose-url $<alias>") {
		t.Fatalf("reply = %q, want not-configured notice with the typed fallback", reply)
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
