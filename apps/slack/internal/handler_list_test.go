package internal

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

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
	testListAliasProdDB  = "prod-db"
	testListAliasSecret  = "secret"
	testListAliasAlpha   = "alpha"
	testListAliasGrafana = "grafana"
	testListResIDProdDB  = "r_prod_db_aa"
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
	// seedAdmin supplies the workspace_mappings / API-key fixture that
	// authenticatedClient needs; the admin-vs-non-admin distinction no
	// longer affects /qurl list, so this happy path renders identically
	// for a non-admin (see TestHandleList_UnscopedAcrossChannels).
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
	if !strings.Contains(async, "Protected Tunnel Resources") {
		t.Errorf("async reply missing header: %q", async)
	}
	if !strings.Contains(async, "`$prod-db`") {
		t.Errorf("async reply missing prod-db row: %q", async)
	}
	if !strings.Contains(async, "`$stage-db`") {
		t.Errorf("async reply missing stage-db row: %q", async)
	}
	if !strings.Contains(async, "/qurl get") {
		t.Errorf("async reply missing copy-paste hint: %q", async)
	}
}

// TestHandleList_ShowsBoundAliases fences the slug + bound-alias
// rendering: a tunnel with several channel `$alias` shortcuts shows the
// slug as the token and the OTHER aliases as "(aliases: …)", sorted, with
// the slug-binding itself excluded (the install flow binds `$<slug>` as
// a channel alias, so it must not be repeated).
func TestHandleList_ShowsBoundAliases(t *testing.T) {
	const resID = "r_kktest01"
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{
		"kktest":          resID, // the install-bound slug alias
		"kevin-dashboard": resID,
		"ops":             resID,
	})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: resID, testKeyType: client.ResourceTypeTunnel, testKeySlug: "kktest", testKeyDescription: "Slack tunnel install for kktest"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$kktest` (aliases: `$kevin-dashboard`, `$ops`) → Slack tunnel install for kktest") {
		t.Errorf("async reply missing slug + bound-aliases row: %q", async)
	}
}

// TestFormatTunnelListLine fences the per-row rendering contract
// directly so the slug-only and slug+description (no-alias) shapes are
// pinned independently of the combined end-to-end TestHandleList_*
// tests. In particular it locks the self-binding exclusion: the
// install-flow binds `$<slug>` as a channel alias, and that name must
// NOT re-appear in the "(aliases: …)" extras.
func TestFormatTunnelListLine(t *testing.T) {
	tunnel := func(slug, desc string) *client.Resource {
		return &client.Resource{
			ResourceID:  "r_" + slug,
			Type:        client.ResourceTypeTunnel,
			Slug:        slug,
			Status:      client.StatusActive,
			Description: desc,
		}
	}
	cases := []struct {
		name         string
		resource     *client.Resource
		boundAliases []string
		want         string
	}{
		{name: "slug only, no aliases, no description", resource: tunnel(testListAliasProdDB, ""), boundAliases: nil, want: "• `$prod-db`"},
		{name: "slug + description, no aliases", resource: tunnel(testListAliasProdDB, "Prod database"), boundAliases: nil, want: "• `$prod-db` → Prod database"},
		{name: "slug + one non-slug alias", resource: tunnel(testListAliasProdDB, ""), boundAliases: []string{testListAliasGrafana}, want: "• `$prod-db` (alias: `$grafana`)"},
		{name: "self-binding slug excluded from extras", resource: tunnel(testListAliasProdDB, "Prod database"), boundAliases: []string{testListAliasProdDB, testListAliasGrafana}, want: "• `$prod-db` (alias: `$grafana`) → Prod database"},
		{name: "only the self-binding slug bound — no extras rendered", resource: tunnel(testListAliasProdDB, ""), boundAliases: []string{testListAliasProdDB}, want: "• `$prod-db`"},
		// Slug-less, resource-alias-less tunnel: no `$<token>` of its own.
		{name: "slug-less tunnel with no bound alias renders bare resource_id", resource: &client.Resource{ResourceID: "r_noslug0001", Type: client.ResourceTypeTunnel, Status: client.StatusActive}, boundAliases: nil, want: "• `r_noslug0001` (no slug — ask your Slack admin to set one)"},
		{name: "slug-less tunnel with bound aliases promotes first to primary", resource: &client.Resource{ResourceID: "r_noslug0001", Type: client.ResourceTypeTunnel, Status: client.StatusActive}, boundAliases: []string{testListAliasGrafana, "metrics"}, want: "• `$grafana` (alias: `$metrics`)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTunnelListLine(tc.resource, tc.boundAliases); got != tc.want {
				t.Errorf("formatTunnelListLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestChannelAliasesByResourceID fences the best-effort posture of the
// alias-display helper DIRECTLY (TestHandleList_ShowsBoundAliases only
// reaches it through the full happy path): the two short-circuits and the
// fetch-error arm must each degrade to nil rather than panic, and the
// happy path must group multiple aliases per resource and sort them.
func TestChannelAliasesByResourceID(t *testing.T) {
	t.Parallel()
	log := slogTestLogger(t)
	ctx := context.Background()

	newH := func(t *testing.T, seed map[string]ddbtypes.AttributeValue) *Handler {
		t.Helper()
		names := defaultTestTableNames()
		ddb := newFakeDDB(t, names, nil)
		if seed != nil {
			ddb.seedItem(t, names.channelPolicy, seed)
		}
		return &Handler{cfg: Config{AdminStore: newStoreFromFake(t, ddb, names, nil)}}
	}

	t.Run("nil AdminStore yields nil", func(t *testing.T) {
		t.Parallel()
		h := &Handler{}
		if got := h.channelAliasesByResourceID(ctx, log, "T1", "C1"); got != nil {
			t.Errorf("nil AdminStore: got %v, want nil", got)
		}
	})

	t.Run("empty channelID short-circuits to nil before any fetch", func(t *testing.T) {
		t.Parallel()
		h := newH(t, nil)
		if got := h.channelAliasesByResourceID(ctx, log, "T1", ""); got != nil {
			t.Errorf("empty channelID: got %v, want nil", got)
		}
	})

	t.Run("policy fetch error degrades to nil", func(t *testing.T) {
		t.Parallel()
		// GetChannelPolicy rejects an empty teamID (BadRequest);
		// channelAliasesByResourceID only guards channelID, so this drives
		// the err != nil arm (the closest reachable stand-in for a fetch
		// failure with the current fake, which has no GetItem error hook).
		h := newH(t, nil)
		if got := h.channelAliasesByResourceID(ctx, log, "", "C1"); got != nil {
			t.Errorf("fetch error: got %v, want nil", got)
		}
	})

	t.Run("groups + sorts multiple aliases per resource", func(t *testing.T) {
		t.Parallel()
		const sharedRID = "r_shared0001" // two aliases point here
		seed := seedChannelPolicyAliasBindings("T1", "C1", map[string]string{
			"zed": sharedRID, "abe": sharedRID, "solo": "r_solo000001",
		})
		h := newH(t, seed)
		got := h.channelAliasesByResourceID(ctx, log, "T1", "C1")
		want := map[string][]string{
			sharedRID:      {"abe", "zed"}, // grouped, lexically sorted
			"r_solo000001": {"solo"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
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
	// Subtests so -run can target a single branch and PASS/FAIL is
	// reported per channel.
	for _, channelID := range []string{"C_with_policy", "C_no_policy_here", "D_direct_msg_1to1"} {
		t.Run(channelID, func(t *testing.T) {
			inv := newAdminSlashInvokerOnChannel(t, h, channelID)
			_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
			if !strings.Contains(async, "`$prod-db`") {
				t.Errorf("non-admin should see prod-db tunnel (listing is unscoped post-revert): %q", async)
			}
			if !strings.Contains(async, "`$secret`") {
				t.Errorf("non-admin should see the secret tunnel (no per-channel filter post-revert): %q", async)
			}
			// Negative fence: a filter reintroduced on only one branch
			// would surface the empty-state or the (removed) non-admin
			// pagination-gap copy for this non-admin caller, even if
			// another branch still rendered rows.
			if strings.Contains(async, "No tunnels found") {
				t.Errorf("empty-state copy fired — a channel filter may have dropped the listing: %q", async)
			}
			if strings.Contains(async, "past the first page") {
				t.Errorf("removed non-admin pagination-gap copy reappeared: %q", async)
			}
		})
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
	if !strings.Contains(async, "/qurl-admin tunnel install") {
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
// rendering: a legacy tunnel with neither a slug nor an alias has no
// usable `$<token>` (get is slug/alias-only), so it renders the bare
// resource_id with a "(no slug — ask your Slack admin to set one)" marker —
// NOT a `$r_<id>` get token a user would paste and have rejected — and no
// `(tunnel)` label.
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
	if !strings.Contains(async, "`r_tunnel_noa` (no slug — ask your Slack admin to set one)") {
		t.Errorf("async reply missing bare resource_id fallback: %q", async)
	}
	if strings.Contains(async, "`$r_tunnel_noa`") {
		t.Errorf("slug-less tunnel rendered a `$r_<id>` get token (no longer mintable): %q", async)
	}
	if strings.Contains(async, "(tunnel)") {
		t.Errorf("redundant (tunnel) label leaked: %q", async)
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
// are sorted by the displayed token (slug, else alias) so two consecutive
// `/qurl list` calls render identically. A slug-less row has an empty
// token (get is slug/alias-only), so it sorts ahead of the slugged rows.
func TestHandleList_StableSortByToken(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		// Server returns in non-alphabetical order; the handler must sort.
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_zzz_aaaaa", testKeyType: client.ResourceTypeTunnel},
			{testKeyResourceID: "r_aaa_xxxxx", testKeyType: client.ResourceTypeTunnel, testKeySlug: testListAliasAlpha},
			{testKeyResourceID: "r_mmm_yyyyy", testKeyType: client.ResourceTypeTunnel, testKeySlug: "middle"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	// Legible tokens lead in order ("alpha" < "middle"); the tokenless
	// slug-less row sorts to the END.
	zzzPos := strings.Index(async, "`r_zzz_aaaaa` (no slug — ask your Slack admin to set one)")
	alphaPos := strings.Index(async, "`$alpha`")
	middlePos := strings.Index(async, "`$middle`")
	if alphaPos < 0 || middlePos < 0 || zzzPos < 0 {
		t.Fatalf("missing rows in async reply: %q", async)
	}
	if alphaPos >= middlePos || middlePos >= zzzPos {
		t.Errorf("rows not sorted (legible tokens first, tokenless last): alpha=%d middle=%d zzz(no-slug)=%d in %q", alphaPos, middlePos, zzzPos, async)
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

// TestHandleList_SlugLessTunnelsSortByPromotedAlias fences the sort/render
// agreement for slug-less tunnels: each has no intrinsic token but a bound
// channel alias that formatTunnelListLine promotes to the primary token. The
// sort must key on that SAME promoted alias (via tunnelDisplayToken), not
// fall back to resource_id. Here the row whose alias sorts first ("apple")
// carries the lexicographically LARGER resource_id, so a resource_id-keyed
// sort would order it last — the assertion passes only if the sort follows
// the displayed alias.
func TestHandleList_SlugLessTunnelsSortByPromotedAlias(t *testing.T) {
	const (
		appleRID = "r_zzz_apple0" // larger resource_id, smaller alias
		zebraRID = "r_aaa_zebra0" // smaller resource_id, larger alias
	)
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{
		"apple": appleRID,
		"zebra": zebraRID,
	})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		// Both slug-less (no slug, no resource-level alias); returned
		// zebra-first to defeat any incidental upstream order.
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: zebraRID, testKeyType: client.ResourceTypeTunnel},
			{testKeyResourceID: appleRID, testKeyType: client.ResourceTypeTunnel},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	applePos := strings.Index(async, "`$apple`")
	zebraPos := strings.Index(async, "`$zebra`")
	if applePos < 0 || zebraPos < 0 {
		t.Fatalf("missing promoted-alias rows: %q", async)
	}
	if applePos >= zebraPos {
		t.Errorf("slug-less rows not sorted by promoted alias (a resource_id-keyed sort would invert this): apple=%d zebra=%d in %q", applePos, zebraPos, async)
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
		{name: "empty when neither slug nor alias (slug-less tunnel)", r: client.Resource{ResourceID: "r_three"}, want: ""},
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
