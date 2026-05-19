package internal

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Shared alias-name fixtures used across the /qurl list test cases.
// Lifted to constants to satisfy goconst (min-occurrences=3) and to
// keep the resource-row builder lines visually aligned. Assertion
// sites read these names too, so a rename surfaces every site at
// once.
const (
	testListAliasProdDB = "prod-db"
	testListAliasAlpha  = "alpha"
	testListResIDProdDB = "r_prod_db_aa"
)

// writeResourceListFixture writes a /v1/resources success envelope
// with a paginated meta block.
func writeResourceListFixture(t *testing.T, w http.ResponseWriter, resources []map[string]any, nextCursor string, hasMore bool) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		testKeyData: resources,
	}
	if nextCursor != "" || hasMore {
		body["meta"] = map[string]any{
			"next_cursor": nextCursor,
			"has_more":    hasMore,
		}
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

// TestHandleList_RendersResources fences the happy path: every
// resource the master listing returns is rendered, with the
// copy-paste `/qurl get $token` hint in the footer. There is no
// per-channel filter — `/qurl list` is unscoped within a workspace.
//
// No `seedAdmin(t)` here (and none in the other rendering tests in
// this file) — admin status is no longer load-bearing for /qurl list
// output post-revert of #234. The default test setup gives a
// workspace context that's sufficient.
func TestHandleList_RendersResources(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testListResIDProdDB, fAttrAlias: testListAliasProdDB, testKeyTargetURL: "https://prod.example.com"},
			{testKeyResourceID: "r_stage_db_bb", fAttrAlias: "stage-db", testKeyTargetURL: "https://stage.example.com"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "qURL Resources") {
		t.Errorf("async reply missing header: %q", async)
	}
	if !strings.Contains(async, "`$prod-db` → https://prod.example.com") {
		t.Errorf("async reply missing prod-db row: %q", async)
	}
	if !strings.Contains(async, "`$stage-db` → https://stage.example.com") {
		t.Errorf("async reply missing stage-db row: %q", async)
	}
	if !strings.Contains(async, "/qurl get $token") {
		t.Errorf("async reply missing copy-paste hint: %q", async)
	}
}

// TestHandleList_UnscopedAcrossChannels fences the post-revert
// disclosure semantics: every workspace member sees the same master
// alias list regardless of admin status or channel-policy state. A
// non-admin invoking /qurl list from a channel with no alias_bindings
// still sees the upstream master listing as-is.
//
// Load-bearing assertions are the two below: (a) the prod-db row
// renders straight off the upstream payload, and (b) none of the
// removed pagination-gap copy strings ("past the first page",
// "ask an admin to allow", "allow specific resources") reappear.
// Together they catch both a full filter reintroduction (row would
// be filtered out) and a partial reintroduction that gates only the
// empty-state copy (gap copy would reappear without the row
// disappearing).
//
// Capability gating still happens at mint time — /qurl get $alias
// from a channel without that binding returns the alias-not-found
// surface (see handler_get_test.go), so widening list disclosure
// does not widen the capability boundary. Re-introducing a
// channel-policy filter on /qurl list would have to delete or update
// this test.
func TestHandleList_UnscopedAcrossChannels(t *testing.T) {
	ts := newAdminTestServers(t)
	// Non-admin seed is intentional: admin status no longer affects
	// /qurl list output, but seeding a non-admin caller pins the
	// disclosure surface under any future re-introduction of a
	// list-side gate. If a channel-policy filter is re-added, this
	// test fails — surfacing the disclosure-narrowing explicitly.
	ts.seedNonAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testListResIDProdDB, fAttrAlias: testListAliasProdDB, testKeyTargetURL: "https://prod.example.com"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	// Invoke from a channel with no channel_policies row — the
	// list renderer reads `alias` straight off the upstream payload,
	// so the row surfaces regardless of any per-channel binding
	// state. A reintroduced list-side filter would change that.
	inv := newAdminSlashInvokerOnChannel(t, h, "C_no_bindings")

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$prod-db` → https://prod.example.com") {
		t.Errorf("list should surface aliases bound in any channel — got: %q", async)
	}
	// Negative assertions: defend against a partial filter
	// reintroduction that gates only the empty-state copy. If any of
	// the removed pagination-gap phrasing reappears, the test fails
	// even if the row is still rendered.
	for _, leak := range []string{"past the first page", "ask an admin to allow", "allow specific resources"} {
		if strings.Contains(async, leak) {
			t.Errorf("response leaks removed pagination-gap copy %q — possible partial filter reintroduction: %q", leak, async)
		}
	}
}

// TestHandleList_EmptyWorkspace fences the friendly empty-state copy
// for a brand-new workspace with zero resources. The hint nudges the
// user toward `/qurl create`.
func TestHandleList_EmptyWorkspace(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Create one with") {
		t.Errorf("async reply missing empty-state hint: %q", async)
	}
}

// TestHandleList_UnaliasedResource fences the `$r_<id>` rendering for
// resources without a bound alias. The row should be copy-paste-ready
// into `/qurl get $r_<id>`.
func TestHandleList_UnaliasedResource(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_unaliased1", testKeyTargetURL: "https://noalias.example.com"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$r_unaliased1`") {
		t.Errorf("async reply missing unaliased token shape: %q", async)
	}
	if !strings.Contains(async, "(no alias set)") {
		t.Errorf("async reply missing 'no alias set' suffix: %q", async)
	}
}

// TestHandleList_TunnelResource fences the tunnel-resource rendering:
// type=tunnel renders "(tunnel)" target placeholder regardless of
// target_url. Keys on r.Type, NOT on empty target_url, so a non-tunnel
// row with a data glitch (transient empty target) doesn't get
// mislabeled.
func TestHandleList_TunnelResource(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tunnel_aaa", fAttrAlias: "tun", testKeyType: resourceTypeTunnel},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$tun` → (tunnel)") {
		t.Errorf("async reply missing tunnel placeholder: %q", async)
	}
}

// TestHandleList_TunnelResourceWithoutAlias fences the "(no alias set)"
// suffix being suppressed on the tunnel branch: rendering
// "$r_<id> → (tunnel) (no alias set)" doubles up two distinct
// visual signals. Tunnel without an alias renders as just
// "$r_<id> → (tunnel)" — the placeholder is signal enough.
func TestHandleList_TunnelResourceWithoutAlias(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tunnel_noa", testKeyType: resourceTypeTunnel},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$r_tunnel_noa` → (tunnel)") {
		t.Errorf("async reply missing tunnel placeholder for un-aliased tunnel: %q", async)
	}
	if strings.Contains(async, "(no alias set)") {
		t.Errorf("(no alias set) leaked on tunnel branch — doubled-up signal: %q", async)
	}
}

// TestHandleList_ResourceWithDescription fences the trailing
// "— <description>" annotation. Legacy /qurl list rendered description
// on a separate line; the resource-pivoted version preserves the
// operator-authored context as a one-line suffix so each row stays
// copy-paste-greppable.
func TestHandleList_ResourceWithDescription(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_desc_aaaaa", fAttrAlias: "prod", testKeyTargetURL: "https://prod", "description": "production gateway"},
			{testKeyResourceID: "r_nodesc_bbbb", fAttrAlias: "stage", testKeyTargetURL: "https://stage"},
			{testKeyResourceID: "r_tun_descrip", fAttrAlias: "tun", testKeyType: resourceTypeTunnel, "description": "ops bastion"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	// Description-bearing row renders trailing em-dash + description.
	if !strings.Contains(async, "`$prod` → https://prod — production gateway") {
		t.Errorf("async reply missing description suffix on aliased row: %q", async)
	}
	// No-description row stays clean (no trailing em-dash).
	if !strings.Contains(async, "`$stage` → https://stage") {
		t.Errorf("async reply missing un-described row: %q", async)
	}
	if strings.Contains(async, "`$stage` → https://stage —") {
		t.Errorf("un-described row should not render a trailing em-dash: %q", async)
	}
	// Tunnel rows also carry description.
	if !strings.Contains(async, "`$tun` → (tunnel) — ops bastion") {
		t.Errorf("async reply missing description suffix on tunnel row: %q", async)
	}
}

// TestHandleList_HasMoreFooter fences the truncation footer when the
// master list reports has_more=true.
func TestHandleList_HasMoreFooter(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_one_aa", fAttrAlias: "one", testKeyTargetURL: "https://one"},
		}, "cursor_xyz", true)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "more results past") {
		t.Errorf("async reply missing has_more footer: %q", async)
	}
}

// TestHandleList_UpstreamError fences the friendly error surface when
// the customer API returns 5xx. Raw API error text MUST NOT reach the
// user.
func TestHandleList_UpstreamError(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(t, w, http.StatusBadGateway, "upstream_error", "Bad Gateway from internal API")
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if strings.Contains(async, "internal API") {
		t.Errorf("async reply leaked raw API error text: %q", async)
	}
	if !strings.Contains(async, "Could not reach qURL") {
		t.Errorf("async reply missing service-unreachable message: %q", async)
	}
}

// TestHandleList_StableSortBetweenAliasAndResourceID fences the sort
// order: rows are sorted by the underlying token (alias if bound,
// resource_id otherwise) so two consecutive `/qurl list` calls render
// identically.
func TestHandleList_StableSortBetweenAliasAndResourceID(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		// Server returns in non-alphabetical order; the handler must
		// sort.
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_zzz_aaaaa", testKeyTargetURL: "https://zzz"},
			{testKeyResourceID: "r_aaa_xxxxx", fAttrAlias: testListAliasAlpha, testKeyTargetURL: "https://alpha"},
			{testKeyResourceID: "r_mmm_yyyyy", fAttrAlias: "middle", testKeyTargetURL: "https://mid"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	// "alpha" < "middle" < "r_zzz" (alphabetical, alias values
	// sorted naturally; un-aliased rows sort by resource_id).
	alphaPos := strings.Index(async, "`$alpha`")
	middlePos := strings.Index(async, "`$middle`")
	zzzPos := strings.Index(async, "`$r_zzz_aaaaa`")
	if alphaPos < 0 || middlePos < 0 || zzzPos < 0 {
		t.Fatalf("missing rows in async reply: %q", async)
	}
	if alphaPos >= middlePos || middlePos >= zzzPos {
		t.Errorf("rows not sorted by token: alpha=%d middle=%d zzz=%d in %q", alphaPos, middlePos, zzzPos, async)
	}
}
