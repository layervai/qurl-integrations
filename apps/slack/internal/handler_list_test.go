package internal

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/shared/client"
)

// Shared fixtures used across the /qurl list test cases. Lifted to
// constants to satisfy goconst (min-occurrences=3) and to keep the
// resource-row builder lines visually aligned. Assertion sites read
// these names too, so a rename surfaces every site at once.
//
// `/qurl list` is tunnel-only and renders the slug as the `$<token>`,
// so the fixtures below are tunnel resources (testKeyType:
// client.ResourceTypeTunnel) carrying a slug.
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

// TestHandleList_RendersAllTunnels fences the happy path: /qurl list
// renders every tunnel the master listing returns, with each row
// showing the slug as the copy-paste-ready `$<slug>` token. Post-revert
// of #234 (#459) the listing is unscoped — no channel-policy filter —
// so this is what every workspace member sees.
func TestHandleList_RendersAllTunnels(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testListResIDProdDB, testKeyType: client.ResourceTypeTunnel, testKeySlug: testListAliasProdDB},
			{testKeyResourceID: "r_stage_db_bb", testKeyType: client.ResourceTypeTunnel, testKeySlug: "stage-db"},
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

// TestHandleList_UnscopedAcrossChannels pins the post-revert (#459)
// disclosure surface: /qurl list is workspace-wide for everyone, so the
// SAME complete tunnel listing renders regardless of the caller's
// channel. It exercises the three channel shapes that diverged
// pre-revert (#234) for a non-admin:
//
//   - a channel carrying a restrictive channel_policies row (would have
//     filtered the listing down to prod-db, hiding secret);
//   - a channel with no policy row (would have fail-closed to empty);
//   - a DM (`D…`) channel (would have fail-closed to empty) — the most
//     user-surprising case, "I ran /qurl list in a 1:1 and saw URLs
//     from #ops".
//
// Post-revert all three must show every tunnel. The seedNonAdmin +
// restrictive seedPolicySet below are load-bearing, not inert: the list
// handler no longer reads them, but if any channel-policy filter were
// re-introduced on /qurl list this non-admin caller would see the old
// filtered/empty output and the test would fail.
func TestHandleList_UnscopedAcrossChannels(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	// A channel_policies row that, under the reverted gate, would have
	// filtered the listing down to prod-db only (secret excluded).
	ts.seedPolicySet(t, testAdminTeamID, "C_with_policy", testListAliasProdDB, []string{testListResIDProdDB})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testListResIDProdDB, testKeyType: client.ResourceTypeTunnel, testKeySlug: testListAliasProdDB},
			{testKeyResourceID: "r_secret_xx", testKeyType: client.ResourceTypeTunnel, testKeySlug: testListAliasSecret},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)

	// "D…" is a Slack DM channel ID; the others are a policy-bearing and
	// a policy-free regular channel. All must render the full listing.
	for _, channelID := range []string{"C_with_policy", "C_no_policy_here", "D_direct_msg_1to1"} {
		inv := newAdminSlashInvokerOnChannel(t, h, channelID)
		_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
		if !strings.Contains(async, "`$prod-db`") {
			t.Errorf("channel %q: non-admin should see prod-db tunnel (listing is unscoped post-revert): %q", channelID, async)
		}
		if !strings.Contains(async, "`$secret`") {
			t.Errorf("channel %q: non-admin should see the secret tunnel (no per-channel filter post-revert): %q", channelID, async)
		}
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
			{testKeyResourceID: "r_tun_aaaaaa", testKeyType: client.ResourceTypeTunnel, testKeySlug: "alpha-tunnel"},
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
			{testKeyResourceID: "r_tunnel_slg", fAttrAlias: "dash-alias", testKeyType: client.ResourceTypeTunnel, testKeySlug: "prod-dashboard"},
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
			{testKeyResourceID: "r_tunnel_aaa", fAttrAlias: "tun", testKeyType: client.ResourceTypeTunnel},
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
			{testKeyResourceID: "r_tunnel_noa", testKeyType: client.ResourceTypeTunnel},
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
			{testKeyResourceID: "r_tun_desc1", testKeyType: client.ResourceTypeTunnel, testKeySlug: "ops-bastion", testKeyDescription: "ops jump host"},
			{testKeyResourceID: "r_tun_nodes", testKeyType: client.ResourceTypeTunnel, testKeySlug: "no-desc-tun"},
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
			{testKeyResourceID: "r_one_aa", testKeyType: client.ResourceTypeTunnel, testKeySlug: "one-tun"},
		}, "cursor_xyz", true)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "more resources past") {
		t.Errorf("async reply missing has_more footer: %q", async)
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
			{testKeyResourceID: "r_zzz_aaaaa", testKeyType: client.ResourceTypeTunnel},
			{testKeyResourceID: "r_aaa_xxxxx", testKeyType: client.ResourceTypeTunnel, testKeySlug: testListAliasAlpha},
			{testKeyResourceID: "r_mmm_yyyyy", testKeyType: client.ResourceTypeTunnel, testKeySlug: "middle"},
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

// TestHandleList_SortTiebreakerOnTokenCollision exercises the
// resource_id tiebreaker directly: two tunnels yield the SAME
// tunnelToken (`$dup`) — one via its slug, one via a resource-level
// alias with no slug — so the comparator falls through to comparing
// resource_id. The row with the lexicographically-smaller resource_id
// must sort first, deterministically, rather than inheriting unstable
// upstream order. Descriptions disambiguate the two identical tokens.
func TestHandleList_SortTiebreakerOnTokenCollision(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		// Returned in reverse resource_id order; both render `$dup`.
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_bbb_dup", fAttrAlias: "dup", testKeyType: client.ResourceTypeTunnel, testKeyDescription: "beta-desc"},
			{testKeyResourceID: "r_aaa_dup", testKeyType: client.ResourceTypeTunnel, testKeySlug: "dup", testKeyDescription: "alpha-desc"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	// Tiebreaker: r_aaa_dup < r_bbb_dup, so alpha-desc renders first.
	alphaPos := strings.Index(async, "alpha-desc")
	betaPos := strings.Index(async, "beta-desc")
	if alphaPos < 0 || betaPos < 0 {
		t.Fatalf("missing colliding-token rows: %q", async)
	}
	if alphaPos >= betaPos {
		t.Errorf("tiebreaker not applied: expected alpha-desc (r_aaa) before beta-desc (r_bbb): %q", async)
	}
}

// TestTunnelToken pins the slug → alias → resource_id precedence of the
// `$<token>` shown for a tunnel row, directly (not just via the rendered
// list output), so a future refactor that reorders the fallback chain
// fails loudly. Intended posture: the slug wins even when a
// resource-level alias is also set, because the slug is the stable
// handle `/qurl tunnel install <slug>` binds for `/qurl get $<slug>`.
func TestTunnelToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		r    client.Resource
		want string
	}{
		{name: "slug wins over alias and resource_id", r: client.Resource{ResourceID: "r_one", Alias: "alias-one", Slug: "slug-one"}, want: "slug-one"},
		{name: "alias used when no slug", r: client.Resource{ResourceID: "r_two", Alias: "alias-two"}, want: "alias-two"},
		{name: "resource_id last resort when no slug or alias", r: client.Resource{ResourceID: "r_three"}, want: "r_three"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tunnelToken(&tc.r); got != tc.want {
				t.Errorf("tunnelToken() = %q, want %q", got, tc.want)
			}
		})
	}
}
