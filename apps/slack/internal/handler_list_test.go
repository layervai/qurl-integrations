package internal

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Shared fixtures used across the /qurl list test cases. Lifted to
// constants to satisfy goconst (min-occurrences=3) and to keep the
// resource-row builder lines visually aligned. Assertion sites read
// these names too, so a rename surfaces every site at once.
//
// `/qurl list` is tunnel-only and renders the slug as the `$<token>`,
// so the fixtures below are tunnel resources (testKeyType:
// resourceTypeTunnel) carrying a slug.
const (
	testListAliasProdDB = "prod-db"
	testListAliasSecret = "secret"
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

// TestHandleList_AdminSeesAllTunnels fences the admin happy path: a
// workspace admin sees every tunnel the master listing returns, without
// channel-policy filtering. Each row renders the slug as the
// copy-paste-ready `$<slug>` token.
func TestHandleList_AdminSeesAllTunnels(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testListResIDProdDB, testKeyType: resourceTypeTunnel, testKeySlug: testListAliasProdDB},
			{testKeyResourceID: "r_stage_db_bb", testKeyType: resourceTypeTunnel, testKeySlug: "stage-db"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "qURL Tunnels") {
		t.Errorf("async reply missing header: %q", async)
	}
	if !strings.Contains(async, "`$prod-db`") {
		t.Errorf("async reply missing prod-db row: %q", async)
	}
	if !strings.Contains(async, "`$stage-db`") {
		t.Errorf("async reply missing stage-db row: %q", async)
	}
	if !strings.Contains(async, "/qurl get $slug") {
		t.Errorf("async reply missing copy-paste hint: %q", async)
	}
}

// TestHandleList_NonAdminFiltersToChannelPolicy fences the non-admin
// path: only tunnels allowed in the current channel are visible.
func TestHandleList_NonAdminFiltersToChannelPolicy(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testListResIDProdDB})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testListResIDProdDB, testKeyType: resourceTypeTunnel, testKeySlug: testListAliasProdDB},
			{testKeyResourceID: "r_secret_xx", testKeyType: resourceTypeTunnel, testKeySlug: testListAliasSecret},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$prod-db`") {
		t.Errorf("async reply missing allowed prod-db: %q", async)
	}
	if strings.Contains(async, "secret") {
		t.Errorf("async reply leaked non-allowed tunnel: %q", async)
	}
}

// TestHandleList_NonAdminSeesAllowedResourceIDsWithoutAliasBinding fences
// the `allowed_resource_ids`-only branch of the union: a tunnel whose
// channel_policies has only `allowed_resource_ids` populated (no
// `alias_bindings`) MUST surface in non-admin `/qurl list`. Pre-fix the
// list handler walked alias-bindings only, so a pure-allowed-set
// resource was `/qurl get`-mintable but invisible in `/qurl list` —
// the two surfaces diverged. The row still renders its slug token
// (the slug is a resource attribute, independent of the channel
// alias_binding this test deliberately omits).
func TestHandleList_NonAdminSeesAllowedResourceIDsWithoutAliasBinding(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	// alias="" → seedPolicySet skips the auto-attached alias_bindings
	// Map. Row carries ONLY `allowed_resource_ids`.
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "", []string{"r_allow_only1"})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_allow_only1", testKeyType: resourceTypeTunnel, testKeySlug: "allow-only-tun"},
			{testKeyResourceID: "r_secret_xx", testKeyType: resourceTypeTunnel, testKeySlug: testListAliasSecret},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$allow-only-tun`") {
		t.Errorf("non-admin list dropped a tunnel in allowed_resource_ids but with no alias_binding: %q", async)
	}
	if strings.Contains(async, "secret") {
		t.Errorf("async reply leaked non-allowed tunnel: %q", async)
	}
}

// TestHandleList_NonAdminUnionsAllowedSetAndAliasBindings fences the
// union behavior across both surfaces on the same row: an
// alias-bindings-only tunnel AND an allowed-set-only tunnel must both
// surface (an alias-only resource that lives outside the
// allowed_resource_ids gate is still mintable via the alias path's
// channel-scoped binding, so it belongs in the listing).
func TestHandleList_NonAdminUnionsAllowedSetAndAliasBindings(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	// Manually compose a row carrying BOTH surfaces with disjoint
	// resource IDs — `allowed_resource_ids` covers r_allow_set_a,
	// `alias_bindings` covers r_alias_only_b. Pre-fix, the bindings
	// path won and the allowed-set entry was invisible.
	ts.ddb.seedItem(t, ts.tableNames.channelPolicy, map[string]ddbtypes.AttributeValue{
		fAttrSlackTeamID:        stringMember(testAdminTeamID),
		fAttrSlackChannelID:     stringMember("C_test"),
		fAttrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"r_allow_set_a"}},
		fAttrAliasBindings: &ddbtypes.AttributeValueMemberM{
			Value: map[string]ddbtypes.AttributeValue{
				"alias-b": stringMember("r_alias_only_b"),
			},
		},
		fAttrCreatedAt: stringMember("2026-04-20T12:00:00Z"),
	})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_allow_set_a", testKeyType: resourceTypeTunnel, testKeySlug: "allowset-tun"},
			{testKeyResourceID: "r_alias_only_b", testKeyType: resourceTypeTunnel, testKeySlug: "aliasonly-tun"},
			{testKeyResourceID: "r_neither_xx", testKeyType: resourceTypeTunnel, testKeySlug: "neither-tun"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$allowset-tun`") {
		t.Errorf("union missed the allowed-set entry: %q", async)
	}
	if !strings.Contains(async, "`$aliasonly-tun`") {
		t.Errorf("union missed the alias-binding entry: %q", async)
	}
	if strings.Contains(async, "neither") {
		t.Errorf("union leaked a tunnel present in neither surface: %q", async)
	}
}

// TestHandleList_NonAdminEmptyChannelFailsClose fences the fail-closed
// posture: a non-admin slash command with no channel_id (synthetic
// test payload or wire-shape regression) returns the empty state
// rather than leaking the unfiltered master list.
func TestHandleList_NonAdminEmptyChannelFailsClose(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_leaked_xx", testKeyType: resourceTypeTunnel, testKeySlug: "leaked-tun"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvokerOnChannel(t, h, "")

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if strings.Contains(async, "leaked") {
		t.Errorf("non-admin + empty channel_id leaked master list: %q", async)
	}
}

// TestHandleList_NonAdminPaginationGap fences the distinct empty-state
// copy when a non-admin's filter is empty AND the master list has
// more pages. The plain empty-state would mislead the user — the issue
// is pagination, not absence.
func TestHandleList_NonAdminPaginationGap(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	// No allowed policies → filter drops everything. Master list
	// reports has_more=true so the non-admin pagination-gap copy
	// fires.
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_unallowed_x", testKeyType: resourceTypeTunnel, testKeySlug: "unallowed-tun"},
		}, "cursor_xyz", true)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "past the first page") {
		t.Errorf("async reply missing pagination-gap copy: %q", async)
	}
}

// TestHandleList_NonAdminEmptyChannelWithHasMoreShowsDefault fences
// the empty-channel + has_more=true branch: the pagination-gap copy
// must NOT fire when channel_id is empty (the message references
// "this channel" — misleading when by construction there is none).
func TestHandleList_NonAdminEmptyChannelWithHasMoreShowsDefault(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_leaked_xx", testKeyType: resourceTypeTunnel, testKeySlug: "leaked-tun"},
		}, "cursor_xyz", true)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvokerOnChannel(t, h, "")

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if strings.Contains(async, "past the first page") {
		t.Errorf("empty-channel branch leaked pagination-gap copy: %q", async)
	}
	if strings.Contains(async, "leaked") {
		t.Errorf("empty-channel + non-admin leaked master list: %q", async)
	}
}

// TestHandleList_EmptyWorkspace fences the friendly empty-state copy
// for a workspace with zero tunnels. The hint nudges the user toward
// `/qurl tunnel install`.
func TestHandleList_EmptyWorkspace(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "No tunnels found") {
		t.Errorf("async reply missing empty-state copy: %q", async)
	}
	if !strings.Contains(async, "/qurl tunnel install") {
		t.Errorf("async reply missing tunnel-install hint: %q", async)
	}
}

// TestHandleList_URLResourcesFiltered fences the tunnel-only scope:
// URL/transit resources MUST NOT appear in `/qurl list` at all — only
// type=tunnel rows survive. A stray `slug` on a URL row doesn't rescue
// it either; the filter keys on Type, not the slug field.
func TestHandleList_URLResourcesFiltered(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tun_aaaaaa", testKeyType: resourceTypeTunnel, testKeySlug: "alpha-tunnel"},
			{testKeyResourceID: "r_url_btarg1", fAttrAlias: "burl", testKeyTargetURL: "https://b.example.com"},
			{testKeyResourceID: "r_url_stray1", testKeyTargetURL: "https://c.example.com", testKeySlug: "stray-slug"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$alpha-tunnel`") {
		t.Errorf("async reply missing the tunnel row: %q", async)
	}
	if strings.Contains(async, "burl") || strings.Contains(async, "b.example.com") {
		t.Errorf("URL resource leaked into tunnel-only list: %q", async)
	}
	if strings.Contains(async, "c.example.com") || strings.Contains(async, "stray-slug") {
		t.Errorf("URL resource with stray slug leaked into tunnel-only list: %q", async)
	}
}

// TestHandleList_TunnelSlugIsToken fences the core display contract:
// a tunnel renders its slug as the `$<token>` — NOT the opaque
// resource_id, NOT a resource-level alias, and with no `(tunnel)`
// label or `[slug:...]` fragment (both redundant now that the whole
// list is tunnels and the token IS the slug). The customer's
// onboarding flow reads this slug to match what the sidecar
// provisioned (via QURL_TUNNEL_SLUG) and pastes it into `/qurl get`.
func TestHandleList_TunnelSlugIsToken(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tunnel_slg", fAttrAlias: "dash-alias", testKeyType: resourceTypeTunnel, testKeySlug: "prod-dashboard"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$prod-dashboard`") {
		t.Errorf("async reply missing slug token: %q", async)
	}
	// Slug wins over the resource-level alias and never falls back to r_<id>.
	if strings.Contains(async, "dash-alias") {
		t.Errorf("resource-level alias shown instead of slug: %q", async)
	}
	if strings.Contains(async, "r_tunnel_slg") {
		t.Errorf("opaque resource_id leaked when a slug was available: %q", async)
	}
	// No legacy decorations.
	if strings.Contains(async, "(tunnel)") {
		t.Errorf("redundant (tunnel) label leaked: %q", async)
	}
	if strings.Contains(async, "[slug:") {
		t.Errorf("redundant [slug:...] fragment leaked: %q", async)
	}
}

// TestHandleList_TunnelAliasFallbackWhenNoSlug fences the fallback
// when a tunnel carries no slug but does carry a resource-level alias:
// the alias is used as the token (never the r_<id>), still with no
// `(tunnel)` label.
func TestHandleList_TunnelAliasFallbackWhenNoSlug(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tunnel_aaa", fAttrAlias: "tun", testKeyType: resourceTypeTunnel},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$tun`") {
		t.Errorf("async reply missing alias-fallback token: %q", async)
	}
	if strings.Contains(async, "(tunnel)") {
		t.Errorf("redundant (tunnel) label leaked: %q", async)
	}
}

// TestHandleList_TunnelResourceIDFallback fences the last-resort
// fallback: a legacy tunnel with neither a slug nor an alias renders
// the raw resource_id as the token (better than an empty `$`), with no
// `(tunnel)` label and no "(no alias set)" suffix.
func TestHandleList_TunnelResourceIDFallback(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tunnel_noa", testKeyType: resourceTypeTunnel},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$r_tunnel_noa`") {
		t.Errorf("async reply missing resource_id fallback token: %q", async)
	}
	if strings.Contains(async, "(tunnel)") {
		t.Errorf("redundant (tunnel) label leaked: %q", async)
	}
	if strings.Contains(async, "(no alias set)") {
		t.Errorf("legacy (no alias set) suffix leaked: %q", async)
	}
}

// TestHandleList_TunnelWithDescription fences the trailing
// "→ <description>" annotation, and that an undescribed tunnel renders
// just the bare token (no dangling arrow).
func TestHandleList_TunnelWithDescription(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tun_desc1", testKeyType: resourceTypeTunnel, testKeySlug: "ops-bastion", testKeyDescription: "ops jump host"},
			{testKeyResourceID: "r_tun_nodes", testKeyType: resourceTypeTunnel, testKeySlug: "no-desc-tun"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$ops-bastion` → ops jump host") {
		t.Errorf("async reply missing slug + description row: %q", async)
	}
	if !strings.Contains(async, "`$no-desc-tun`") {
		t.Errorf("async reply missing undescribed tunnel row: %q", async)
	}
	if strings.Contains(async, "`$no-desc-tun` →") {
		t.Errorf("undescribed tunnel should not render a trailing arrow: %q", async)
	}
}

// TestHandleList_HasMoreFooter fences the truncation footer when the
// master list reports has_more=true.
func TestHandleList_HasMoreFooter(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_one_aa", testKeyType: resourceTypeTunnel, testKeySlug: "one-tun"},
		}, "cursor_xyz", true)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "more resources past") {
		t.Errorf("async reply missing has_more footer: %q", async)
	}
}

// TestHandleList_NonAdminPartialPageHasMoreFooter fences the distinct
// non-admin footer when the filtered set is NON-empty and master
// has_more=true. The admin footer understates the gap because
// allow-listed tunnels may sit past the first scan invisibly; the
// non-admin copy makes that explicit.
func TestHandleList_NonAdminPartialPageHasMoreFooter(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	// One allowed tunnel in the channel, plus has_more=true so the
	// non-admin pagination-aware footer branch fires.
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "one", []string{"r_one_xxxxxx"})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_one_xxxxxx", testKeyType: resourceTypeTunnel, testKeySlug: "one-tun"},
		}, "cursor_xyz", true)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Showing allow-listed tunnels") {
		t.Errorf("async reply missing non-admin partial-page footer: %q", async)
	}
	// Admin-only "more resources past" copy must NOT fire on the non-admin
	// path — these two branches are deliberately disjoint.
	if strings.Contains(async, "more resources past") {
		t.Errorf("async reply leaked admin-only footer copy on non-admin path: %q", async)
	}
}

// TestHandleList_AdminStoreNilTreatedAsNonAdmin fences the no-DDB
// sandbox case. Without AdminStore we can't check admin status, so
// we treat the user as non-admin and fail-closed (empty list, no leak).
func TestHandleList_AdminStoreNilTreatedAsNonAdmin(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_master_xx", testKeyType: resourceTypeTunnel, testKeySlug: "master-tun"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	h.cfg.AdminStore = nil
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if strings.Contains(async, "master") {
		t.Errorf("async reply leaked master list when AdminStore nil: %q", async)
	}
}

// TestHandleList_UpstreamError fences the friendly error surface when
// the customer API returns 5xx. Raw API error text MUST NOT reach the
// user.
func TestHandleList_UpstreamError(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
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

// TestHandleList_StableSortByToken fences the sort order: tunnel rows
// are sorted by the displayed token (slug, else alias, else
// resource_id) so two consecutive `/qurl list` calls render
// identically.
func TestHandleList_StableSortByToken(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		// Server returns in non-alphabetical order; the handler must
		// sort. The slug-less row sorts by its resource_id.
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_zzz_aaaaa", testKeyType: resourceTypeTunnel},
			{testKeyResourceID: "r_aaa_xxxxx", testKeyType: resourceTypeTunnel, testKeySlug: testListAliasAlpha},
			{testKeyResourceID: "r_mmm_yyyyy", testKeyType: resourceTypeTunnel, testKeySlug: "middle"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	// "alpha" < "middle" < "r_zzz_aaaaa" (alphabetical; the slug-less
	// row sorts by resource_id).
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
