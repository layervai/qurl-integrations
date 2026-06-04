package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

// Revoke fixtures. The slug/alias is what the user types (`revoke $<alias>`);
// the resource_id is what the bot resolves it to via the seeded channel alias
// binding and then passes to DELETE /v1/resources/{id}.
const (
	testRevokeAlias      = "prod-db"
	testRevokeResourceID = "r_revoketest1"
)

// seedRevokeFixture seeds an admin workspace plus a channel alias binding
// (testRevokeAlias → testRevokeResourceID in C_test) so resolveTokenForGet
// resolves the token on the binding-hit path (no slug fallback). Returns the
// wired invoker (which holds the handler).
func seedRevokeFixture(t *testing.T, ts *adminTestServers) *adminSlashInvoker {
	t.Helper()
	ts.seedAdmin(t)
	ts.seedPolicyDualShape(t, testAdminTeamID, "C_test", testRevokeAlias, testRevokeResourceID)
	return newAdminSlashInvoker(t, newAdminTestHandler(t, ts))
}

// TestHandleRevoke_HappyPath fences the resource-revoke flow: an admin
// revokes `$<alias>`, which resolves to its resource_id and issues
// DELETE /v1/resources/{id}; the async reply confirms the revoke.
func TestHandleRevoke_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	var gotPath string
	var hits atomic.Int32
	ts.addCustomer(http.MethodDelete, "/v1/resources/"+testRevokeResourceID, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	inv := seedRevokeFixture(t, ts)

	_, _, asyncReply := inv.invokeAdminAsync("revoke $"+testRevokeAlias, testAdminTeamID, testAdminUserID)
	if !strings.Contains(asyncReply, "Revoked") || !strings.Contains(asyncReply, testRevokeAlias) {
		t.Errorf("reply missing revoke confirmation: %q", asyncReply)
	}
	if hits.Load() != 1 {
		t.Errorf("DELETE fired %d times, want exactly 1", hits.Load())
	}
	if gotPath != "/v1/resources/"+testRevokeResourceID {
		t.Errorf("DELETE path = %q, want /v1/resources/%s (slug must resolve to resource_id)", gotPath, testRevokeResourceID)
	}
}

// TestHandleRevoke_NonAdmin fences the admin-only gate: a non-admin gets the
// admin-only reply synchronously (before the ack) and no DELETE is issued.
func TestHandleRevoke_NonAdmin(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	var hits atomic.Int32
	ts.addCustomerPrefix(http.MethodDelete, "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	// Sync path: the gate denies before runAsync, so the denial is in the
	// synchronous reply and no async worker spawns.
	_, reply := inv.invokeAdmin("revoke $"+testRevokeAlias, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "admin-only") {
		t.Errorf("reply missing admin-only fence: %q", reply)
	}
	if hits.Load() != 0 {
		t.Errorf("DELETE fired despite non-admin gate (hits = %d)", hits.Load())
	}
}

// TestHandleRevoke_NotFoundIsGraceful fences the 404/410 surface: revoking an
// already-revoked or stale id reads as a friendly "already revoked" hint, not
// a raw upstream error.
func TestHandleRevoke_NotFoundIsGraceful(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.addCustomer(http.MethodDelete, "/v1/resources/"+testRevokeResourceID, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"title":"Not Found","detail":"resource gone","code":"not_found","status":404}}`))
	})
	inv := seedRevokeFixture(t, ts)

	_, _, asyncReply := inv.invokeAdminAsync("revoke $"+testRevokeAlias, testAdminTeamID, testAdminUserID)
	if !strings.Contains(asyncReply, "not found") || !strings.Contains(asyncReply, "already revoked") {
		t.Errorf("reply missing graceful not-found surface: %q", asyncReply)
	}
}

// TestHandleRevoke_AuthRejected fences the 401/403 surface: a rotated API key
// points the admin at /qurl setup rather than a generic error.
func TestHandleRevoke_AuthRejected(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		ts := newAdminTestServers(t)
		ts.addCustomer(http.MethodDelete, "/v1/resources/"+testRevokeResourceID, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"title":"Unauthorized","detail":"bad key","code":"unauthorized","status":401}}`))
		})
		inv := seedRevokeFixture(t, ts)

		_, _, asyncReply := inv.invokeAdminAsync("revoke $"+testRevokeAlias, testAdminTeamID, testAdminUserID)
		if !strings.Contains(asyncReply, "API key was rejected") || !strings.Contains(asyncReply, "setup") {
			t.Errorf("status %d: reply missing key-rotation guidance: %q", status, asyncReply)
		}
	}
}

// TestHandleRevoke_Upstream5xx fences the generic upstream-error surface: a
// 5xx maps to a sanitized failure (with the support Reference handle), not a
// leaked upstream body.
func TestHandleRevoke_Upstream5xx(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.addCustomer(http.MethodDelete, "/v1/resources/"+testRevokeResourceID, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"title":"Internal","detail":"boom","code":"internal","status":500}}`))
	})
	inv := seedRevokeFixture(t, ts)

	_, _, asyncReply := inv.invokeAdminAsync("revoke $"+testRevokeAlias, testAdminTeamID, testAdminUserID)
	if !strings.Contains(asyncReply, "Failed to revoke") {
		t.Errorf("reply missing generic upstream-error surface: %q", asyncReply)
	}
	// The upstream detail ("boom") must NOT leak to the wire.
	if strings.Contains(asyncReply, "boom") {
		t.Errorf("reply leaked upstream detail: %q", asyncReply)
	}
}

// TestHandleRevoke_UsageOnBareRevoke fences the arg hint: bare `revoke` (no
// token) surfaces the usage hint synchronously rather than dispatching an
// unresolvable revoke or panicking.
func TestHandleRevoke_UsageOnBareRevoke(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.failOnAdminMutation(t, "bare revoke must not reach any mutation")
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	for _, text := range []string{"revoke", "revoke prod-db"} { // missing token, then missing sigil
		_, reply := inv.invokeAdmin(text, testAdminTeamID, testAdminUserID)
		if !strings.Contains(reply, "Usage") || !strings.Contains(reply, "revoke") {
			t.Errorf("%q: reply missing usage hint: %q", text, reply)
		}
	}
}

// TestHandleRevoke_AdminStoreUnconfigured fences the optional-AdminStore path
// for revoke: a deployment without slackdata wiring (AdminStore == nil) replies
// "Admin features are not configured" synchronously rather than panicking in
// the requireAdminSync nil-deref. Mirrors TestHandleAdmin_AdminStoreUnconfigured
// — revoke routes straight to handleRevoke, so it needs its own
// requireAdminStoreSync gate that the membership verbs get via handleAdmin.
func TestHandleRevoke_AdminStoreUnconfigured(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, noopQURLServer(t))
	// newTestHandler builds without an AdminStore by default; the revoke
	// dispatch hits the requireAdminStoreSync nil-guard before requireAdminSync
	// can dereference the nil AdminStore.

	inv := newAdminSlashInvoker(t, h)
	// Sync path: the store guard fires before runAsync, so the reply is in the
	// synchronous body (not response_url) — same shape as the non-admin fence.
	_, reply := inv.invokeAdmin("revoke $"+testRevokeAlias, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "not configured") {
		t.Errorf("reply missing not-configured surface: %q", reply)
	}
}

// TestHandleRevoke_EmptyChannelRejected fences processRevoke's channel-required
// guard: resolveTokenForGet is channel-scoped, so a channel-less invocation
// can't authorize. Revoke must refuse with the channel-required copy and issue
// no DELETE (mirrors /qurl get and /qurl list's empty-channel guards).
func TestHandleRevoke_EmptyChannelRejected(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t) // admin gate must PASS so the refusal is the channel guard, not the admin gate
	ts.addCustomerPrefix(http.MethodDelete, "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("DELETE reached for a channel-less revoke — must refuse before any resolve/delete")
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvokerOnChannel(t, h, "") // truly-empty channel_id

	_, _, async := inv.invokeAdminAsync("revoke $"+testRevokeAlias, testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, channelRequiredMessage) {
		t.Errorf("empty channel_id should be refused with the channel-required copy: %q", async)
	}
}

// --- /qurl list Revoke button (block_actions) ---

// findActionButton returns the first button element across all `actions` blocks
// whose action_id matches, or nil. (createQurlButtonValues only inspects
// single-button `accessory` rows; the admin row's buttons live in an `actions`
// block.)
func findActionButton(blocks []any, actionID string) map[string]any {
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		if block["type"] != "actions" {
			continue
		}
		els, _ := block["elements"].([]any)
		for _, e := range els {
			if el, _ := e.(map[string]any); el["action_id"] == actionID {
				return el
			}
		}
	}
	return nil
}

// withListEditWiring enables the full edit/revoke affordance wiring on h
// (OpenView + alias store), which listCallerCanEdit requires before the Edit
// and Revoke buttons render. Mirrors TestHandleListEditClick_OpensModal's setup.
func withListEditWiring(h *Handler) {
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }
}

// TestHandleList_RendersRevokeButton fences that an admin `/qurl list` row
// carries a red Revoke button (style danger) guarded by a confirm dialog whose
// copy warns the action is irreversible and takes all the resource's qURLs.
func TestHandleList_RendersRevokeButton(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testRevokeResourceID, testKeyType: client.ResourceTypeTunnel, testKeySlug: testRevokeAlias},
		}, "", false)
	})
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", testRevokeResourceID)
	h := newAdminTestHandler(t, ts)
	withListEditWiring(h)
	inv := newAdminSlashInvoker(t, h)

	if status, _ := inv.invokeAdmin("list", testAdminTeamID, testAdminUserID); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	blocks := parseSlackBlocks(t, inv.captured.waitForBody(t, 2*time.Second))

	btn := findActionButton(blocks, listRevokeTunnelActionID)
	if btn == nil {
		t.Fatalf("admin /qurl list row missing a Revoke button: %v", blocks)
	}
	if btn["style"] != "danger" {
		t.Errorf("Revoke button style = %v, want danger", btn["style"])
	}
	confirm, ok := btn["confirm"].(map[string]any)
	if !ok {
		t.Fatalf("Revoke button missing confirm dialog: %v", btn)
	}
	confirmText, _ := confirm["text"].(map[string]any)
	if txt, _ := confirmText["text"].(string); !strings.Contains(txt, "every qURL") || !strings.Contains(txt, "can't be undone") {
		t.Errorf("confirm dialog copy missing irreversible/all-qURLs warning: %v", confirm)
	}
	// The button value must carry the resolved resource_id so the click
	// handler revokes without a slug re-resolve.
	if v, _ := btn["value"].(string); !strings.Contains(v, testRevokeResourceID) {
		t.Errorf("Revoke button value missing resource_id: %q", v)
	}
}

// TestHandleListRevokeClick_Revokes fences the happy path: an admin clicking
// Revoke (after the confirm) acks 200 and revokes the row's resource via
// DELETE /v1/resources/{id}, posting the confirmation to response_url.
func TestHandleListRevokeClick_Revokes(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var hits atomic.Int32
	ts.addCustomer(http.MethodDelete, "/v1/resources/"+testRevokeResourceID, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	val, ok := buildTunnelRevokeButtonValue(testRevokeResourceID, testRevokeAlias)
	if !ok {
		t.Fatal("buildTunnelRevokeButtonValue ok=false for a normal row")
	}
	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, "C_test", inv.responseU.URL, listRevokeTunnelActionID, val)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("revoke click ack = %d %q, want 200 {}", w.Code, w.Body.String())
	}

	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "Revoked") || !strings.Contains(async, testRevokeAlias) {
		t.Errorf("async reply missing revoke confirmation: %q", async)
	}
	if hits.Load() != 1 {
		t.Errorf("DELETE fired %d times, want 1", hits.Load())
	}
}

// TestHandleListRevokeClick_NonAdminDenied fences the mutation re-gate: a
// non-admin click (the button shouldn't render for them, but the handler
// re-checks anyway) is denied and issues no DELETE.
func TestHandleListRevokeClick_NonAdminDenied(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	var hits atomic.Int32
	ts.addCustomerPrefix(http.MethodDelete, "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	val, _ := buildTunnelRevokeButtonValue(testRevokeResourceID, testRevokeAlias)
	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, "C_test", inv.responseU.URL, listRevokeTunnelActionID, val)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "admin-only") {
		t.Errorf("non-admin revoke click should be denied: %q", async)
	}
	if hits.Load() != 0 {
		t.Errorf("DELETE fired despite non-admin re-gate (hits = %d)", hits.Load())
	}
}

// TestHandleListRevokeClick_UnparseableValue fences that a malformed button
// value posts the failure notice and never issues a DELETE.
func TestHandleListRevokeClick_UnparseableValue(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var hits atomic.Int32
	ts.addCustomerPrefix(http.MethodDelete, "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	body := listCreateQurlBlockActionsBody(t, testAdminTeamID, testAdminUserID, "C_test", inv.responseU.URL, listRevokeTunnelActionID, "not json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, commonRevokeFailedMessage) {
		t.Errorf("unparseable revoke value should post the failure notice: %q", async)
	}
	if hits.Load() != 0 {
		t.Errorf("DELETE fired for an unparseable value (hits = %d)", hits.Load())
	}
}
