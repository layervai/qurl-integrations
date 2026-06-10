package internal

// Tests for the per-resource default link expiry: the slackdata store
// contract (resource_default_ttls on workspace_mappings), the `/qurl list`
// Edit modal's dropdown (pre-fill, set, reset, untouched-no-write), and the
// `/qurl get` mint path consuming the stored override.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// --- store contract --------------------------------------------------------

// TestResourceDefaultTTLStore_RoundTrip fences the set → get → overwrite →
// clear lifecycle, plus clear-idempotence and the missing-row/entry "" reads.
func TestResourceDefaultTTLStore_RoundTrip(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	ctx := context.Background()

	if got, err := store.GetResourceDefaultTTL(ctx, testAdminTeamID, testEditResourceID); err != nil || got != "" {
		t.Fatalf("get before set = (%q, %v), want (\"\", nil)", got, err)
	}
	if err := store.SetResourceDefaultTTL(ctx, testAdminTeamID, testEditResourceID, "6h"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, _ := store.GetResourceDefaultTTL(ctx, testAdminTeamID, testEditResourceID); got != "6h" {
		t.Errorf("get after set = %q, want 6h", got)
	}
	// A second resource on the same workspace row must not collide.
	if err := store.SetResourceDefaultTTL(ctx, testAdminTeamID, "r_other", "7d"); err != nil {
		t.Fatalf("set second resource: %v", err)
	}
	if got, _ := store.GetResourceDefaultTTL(ctx, testAdminTeamID, testEditResourceID); got != "6h" {
		t.Errorf("first resource clobbered by second set: %q", got)
	}
	if err := store.SetResourceDefaultTTL(ctx, testAdminTeamID, testEditResourceID, "1h"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got, _ := store.GetResourceDefaultTTL(ctx, testAdminTeamID, testEditResourceID); got != "1h" {
		t.Errorf("get after overwrite = %q, want 1h", got)
	}
	if err := store.SetResourceDefaultTTL(ctx, testAdminTeamID, testEditResourceID, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := store.GetResourceDefaultTTL(ctx, testAdminTeamID, testEditResourceID); got != "" {
		t.Errorf("get after clear = %q, want \"\"", got)
	}
	// Clearing an already-absent entry is the desired end state, not an error.
	if err := store.SetResourceDefaultTTL(ctx, testAdminTeamID, testEditResourceID, ""); err != nil {
		t.Errorf("clear again: %v, want nil (idempotent)", err)
	}
}

// TestResourceDefaultTTLStore_UnboundWorkspace fences that a set against a
// workspace with no mappings row refuses with workspace_not_bound and — the
// load-bearing part — does NOT materialize a phantom row that would break
// BindWorkspace's attribute_not_exists first-claim condition.
func TestResourceDefaultTTLStore_UnboundWorkspace(t *testing.T) {
	ts := newAdminTestServers(t) // no seedAdmin: workspace table empty
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)

	err := store.SetResourceDefaultTTL(context.Background(), testAdminTeamID, testEditResourceID, "6h")
	var se *slackdata.Error
	if !errors.As(err, &se) || se.Code != slackdata.ErrCodeWorkspaceNotBound {
		t.Fatalf("set on unbound workspace = %v, want *Error{Code: workspace_not_bound}", err)
	}
	if n := len(ts.ddb.tables[ts.tableNames.workspace]); n != 0 {
		t.Errorf("workspace table has %d rows after a refused set, want 0 (phantom row would break first-claim binds)", n)
	}
}

// --- parse + modal render --------------------------------------------------

func TestParseEditLinkExpiry(t *testing.T) {
	expiryValues := func(selected string) map[string]map[string]interactionStateValue {
		return map[string]map[string]interactionStateValue{
			tunnelEditBlockLinkExpiry: {tunnelEditActionLinkExpiry: {SelectedOption: &interactionSelectedOption{Value: selected}}},
		}
	}
	meta := &TunnelEditModalMetadata{DefaultTTL: "6h"}

	t.Run("absent block reads as unchanged", func(t *testing.T) {
		// A view in flight from before the dropdown deployed has no such block.
		got, msg := parseEditLinkExpiry(map[string]map[string]interactionStateValue{}, meta)
		if got != "6h" || msg != "" {
			t.Errorf("got (%q, %q), want (\"6h\", \"\")", got, msg)
		}
	})
	t.Run("built-in default selection clears the override", func(t *testing.T) {
		got, msg := parseEditLinkExpiry(expiryValues(resourceLinkExpiry), meta)
		if got != "" || msg != "" {
			t.Errorf("got (%q, %q), want (\"\", \"\")", got, msg)
		}
	})
	t.Run("listed option selects verbatim", func(t *testing.T) {
		got, msg := parseEditLinkExpiry(expiryValues("1h"), meta)
		if got != "1h" || msg != "" {
			t.Errorf("got (%q, %q), want (\"1h\", \"\")", got, msg)
		}
	})
	t.Run("unlisted value is a field error", func(t *testing.T) {
		// static_select can't produce this — a hand-crafted payload can, and the
		// value would land on the mint wire, so it's refused rather than stored.
		if _, msg := parseEditLinkExpiry(expiryValues("999d"), meta); msg == "" {
			t.Error("expected a field error for an unlisted expiry value")
		}
	})
}

func TestLinkExpiryInitialOption(t *testing.T) {
	if got := linkExpiryInitialOption("6h"); got[blockKitFieldValue] != "6h" {
		t.Errorf("stored 6h pre-selects %v, want 6h", got[blockKitFieldValue])
	}
	for name, stored := range map[string]string{"no override": "", "unrecognized override": "999d"} {
		t.Run(name, func(t *testing.T) {
			got := linkExpiryInitialOption(stored)
			if got[blockKitFieldValue] != resourceLinkExpiry {
				t.Errorf("pre-selected %v, want the built-in default %q", got[blockKitFieldValue], resourceLinkExpiry)
			}
		})
	}
	// The default option's label carries the "(default)" marker so admins can
	// tell reset from override.
	label, _ := linkExpiryInitialOption("")["text"].(map[string]any)
	if text, _ := label["text"].(string); !strings.Contains(text, "(default)") {
		t.Errorf("default option label = %q, want it to contain \"(default)\"", text)
	}
}

// TestHandleListEditClick_PrefillsStoredLinkExpiry fences the open path: a
// stored override is read fresh on click, carried in private_metadata (the
// submit diff baseline), and pre-selects the dropdown.
func TestHandleListEditClick_PrefillsStoredLinkExpiry(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	if err := h.cfg.AdminStore.SetResourceDefaultTTL(context.Background(), testAdminTeamID, testEditResourceID, "6h"); err != nil {
		t.Fatalf("seed TTL: %v", err)
	}
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
		t.Fatalf("edit click status = %d, want 200", w.Code)
	}

	var view []byte
	select {
	case view = <-views:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenView was not called")
	}
	var modal struct {
		PrivateMetadata string `json:"private_metadata"`
		Blocks          []struct {
			BlockID string `json:"block_id"`
			Element struct {
				InitialOption struct {
					Value string `json:"value"`
				} `json:"initial_option"`
			} `json:"element"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(view, &modal); err != nil {
		t.Fatalf("modal JSON: %v", err)
	}
	var meta TunnelEditModalMetadata
	if err := json.Unmarshal([]byte(modal.PrivateMetadata), &meta); err != nil {
		t.Fatalf("private_metadata JSON: %v", err)
	}
	if meta.DefaultTTL != "6h" {
		t.Errorf("private_metadata default_ttl = %q, want 6h", meta.DefaultTTL)
	}
	found := false
	for _, b := range modal.Blocks {
		if b.BlockID == tunnelEditBlockLinkExpiry {
			found = true
			if b.Element.InitialOption.Value != "6h" {
				t.Errorf("dropdown initial option = %q, want 6h", b.Element.InitialOption.Value)
			}
		}
	}
	if !found {
		t.Errorf("modal has no %s block: %s", tunnelEditBlockLinkExpiry, view)
	}
}

// --- submission ------------------------------------------------------------

// tunnelEditViewSubmissionBodyWithExpiry mirrors tunnelEditViewSubmissionBody
// with the default-link-expiry dropdown's selection included.
func tunnelEditViewSubmissionBodyWithExpiry(t *testing.T, meta *TunnelEditModalMetadata, displayName, aliasesText, expiry string) string {
	t.Helper()
	pm, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal private_metadata: %v", err)
	}
	return viewSubmissionBody(t, "V_test_edit", callbackIDTunnelEdit, string(pm), meta.TeamID, meta.UserID,
		map[string]map[string]interactionStateValue{
			tunnelEditBlockDisplayName: {tunnelEditActionDisplayName: {Value: displayName}},
			tunnelEditBlockAliases:     {tunnelEditActionAliases: {Value: aliasesText}},
			tunnelEditBlockLinkExpiry:  {tunnelEditActionLinkExpiry: {SelectedOption: &interactionSelectedOption{Value: expiry}}},
		})
}

// submitTunnelEditExpiry drives one Edit-modal submission whose only change is
// the expiry dropdown and returns the posted summary. currentTTL seeds both
// the store and the modal's private_metadata baseline ("" for neither).
func submitTunnelEditExpiry(t *testing.T, currentTTL, selected string) (summary string, store *slackdata.Store) {
	t.Helper()
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer(http.MethodPatch, "/v1/resources/"+testEditResourceID, func(http.ResponseWriter, *http.Request) {
		t.Errorf("PATCH reached for an expiry-only edit (name unchanged)")
	})
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	if currentTTL != "" {
		if err := h.cfg.AdminStore.SetResourceDefaultTTL(context.Background(), testAdminTeamID, testEditResourceID, currentTTL); err != nil {
			t.Fatalf("seed TTL: %v", err)
		}
	}

	var got []byte
	srv, done := editResultServer(t, &got)
	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken,
		DisplayName: testEditDisplay, DefaultTTL: currentTTL,
	}
	body := tunnelEditViewSubmissionBodyWithExpiry(t, &meta, testEditDisplay, "", selected)
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
	return parseSlackText(t, got), h.cfg.AdminStore
}

func TestHandleTunnelEdit_SetsDefaultLinkExpiry(t *testing.T) {
	summary, store := submitTunnelEditExpiry(t, "", "1h")
	if !strings.Contains(summary, "Default link expiry set to 1 hour") {
		t.Errorf("summary missing the expiry change: %s", summary)
	}
	if got, _ := store.GetResourceDefaultTTL(context.Background(), testAdminTeamID, testEditResourceID); got != "1h" {
		t.Errorf("stored override = %q, want 1h", got)
	}
}

func TestHandleTunnelEdit_ResetsDefaultLinkExpiry(t *testing.T) {
	summary, store := submitTunnelEditExpiry(t, "6h", resourceLinkExpiry)
	if !strings.Contains(summary, "Default link expiry reset to "+resourceLinkExpiryHuman) {
		t.Errorf("summary missing the reset: %s", summary)
	}
	if got, _ := store.GetResourceDefaultTTL(context.Background(), testAdminTeamID, testEditResourceID); got != "" {
		t.Errorf("override should be cleared, still %q", got)
	}
}

// TestHandleTunnelEdit_UntouchedExpiryNoWrite fences the changed-only diff: an
// untouched dropdown (submitted value == pre-fill baseline) must not write the
// workspace row at all — that's also what makes a degraded pre-fill safe.
func TestHandleTunnelEdit_UntouchedExpiryNoWrite(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	if err := h.cfg.AdminStore.SetResourceDefaultTTL(context.Background(), testAdminTeamID, testEditResourceID, "6h"); err != nil {
		t.Fatalf("seed TTL: %v", err)
	}
	ts.ddb.SetUpdateItemHook(func(in interface{}) {
		if u, ok := in.(*dynamodb.UpdateItemInput); ok && aws.ToString(u.TableName) == ts.tableNames.workspace {
			t.Errorf("workspace UpdateItem reached for an untouched expiry dropdown")
		}
	})

	var got []byte
	srv, done := editResultServer(t, &got)
	meta := TunnelEditModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testEditChannel, UserID: testAdminUserID,
		ResponseURL: srv.URL, ResourceID: testEditResourceID, Token: testEditToken,
		DisplayName: testEditDisplay, DefaultTTL: "6h",
	}
	body := tunnelEditViewSubmissionBodyWithExpiry(t, &meta, testEditDisplay, "", "6h")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("submission ack = %d, want 200", w.Code)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no result posted to response_url")
	}
	if summary := parseSlackText(t, got); !strings.Contains(summary, "No changes.") {
		t.Errorf("summary = %q, want \"No changes.\"", summary)
	}
}

// --- mint path -------------------------------------------------------------

// mintCapture registers the resource-scoped mint route and captures the
// request's expires_in.
func mintCapture(t *testing.T, ts *adminTestServers) *atomic.Value {
	t.Helper()
	var expiresIn atomic.Value
	ts.addCustomer("POST", mintByTestResourcePath, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ExpiresIn string `json:"expires_in"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		expiresIn.Store(in.ExpiresIn)
		writeCreateFixture(t, w, "https://qurl.link/abc", testResourceIDFix)
	})
	return &expiresIn
}

// TestHandleGet_MintsWithStoredDefaultLinkExpiry fences the consumer end: a
// stored per-resource override rides the mint's expires_in and the reply's
// "link expires in …" suffix.
func TestHandleGet_MintsWithStoredDefaultLinkExpiry(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	expiresIn := mintCapture(t, ts)
	h := newAdminTestHandler(t, ts)
	if err := h.cfg.AdminStore.SetResourceDefaultTTL(context.Background(), testAdminTeamID, testResourceIDFix, "6h"); err != nil {
		t.Fatalf("seed TTL: %v", err)
	}
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if got := expiresIn.Load(); got != "6h" {
		t.Errorf("minted expires_in = %v, want 6h", got)
	}
	if !strings.Contains(async, "link expires in 6 hours") {
		t.Errorf("reply missing the override's expiry suffix: %q", async)
	}
}

// TestHandleGet_MintsBuiltInDefaultWithoutOverride pins the unchanged default:
// no stored override mints with the built-in 1 minute expiry.
func TestHandleGet_MintsBuiltInDefaultWithoutOverride(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	expiresIn := mintCapture(t, ts)
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if got := expiresIn.Load(); got != resourceLinkExpiry {
		t.Errorf("minted expires_in = %v, want the built-in %q", got, resourceLinkExpiry)
	}
	if !strings.Contains(async, "link expires in "+resourceLinkExpiryHuman) {
		t.Errorf("reply missing the default expiry suffix: %q", async)
	}
}

// TestHandleGet_UnrecognizedStoredExpiryMintsDefault fences the read-side
// fence: a stored value outside linkExpiryOptions (hand-edited row, or an
// option later removed) never reaches the wire — the mint falls back to the
// built-in default.
func TestHandleGet_UnrecognizedStoredExpiryMintsDefault(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	expiresIn := mintCapture(t, ts)
	h := newAdminTestHandler(t, ts)
	// The store stores verbatim (validation is the modal parser's job), which
	// is exactly how a junk value could exist — seed one directly.
	if err := h.cfg.AdminStore.SetResourceDefaultTTL(context.Background(), testAdminTeamID, testResourceIDFix, "999d"); err != nil {
		t.Fatalf("seed junk TTL: %v", err)
	}
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if got := expiresIn.Load(); got != resourceLinkExpiry {
		t.Errorf("minted expires_in = %v, want the built-in %q (junk must not reach the wire)", got, resourceLinkExpiry)
	}
	if !strings.Contains(async, "link expires in "+resourceLinkExpiryHuman) {
		t.Errorf("reply should render the default expiry: %q", async)
	}
}
