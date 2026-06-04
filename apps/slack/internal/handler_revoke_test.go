package internal

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
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
