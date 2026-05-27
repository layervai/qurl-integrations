package internal

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Shared alias-name fixtures used across the /qurl list test cases.
// Lifted to constants to satisfy goconst (min-occurrences=3) and to
// keep the resource-row builder lines visually aligned. Assertion
// sites read these names too, so a rename surfaces every site at
// once.
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

// TestHandleList_AdminSeesAllResources fences the admin happy path:
// a workspace admin sees every resource the master listing returns,
// without channel-policy filtering.
func TestHandleList_AdminSeesAllResources(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
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

// TestHandleList_NonAdminFiltersToChannelPolicy fences the non-admin
// path: only resources allowed in the current channel are visible.
func TestHandleList_NonAdminFiltersToChannelPolicy(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{"r_prod_db_aa"})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testListResIDProdDB, fAttrAlias: testListAliasProdDB, testKeyTargetURL: "https://prod.example.com"},
			{testKeyResourceID: "r_secret_xx", fAttrAlias: testListAliasSecret, testKeyTargetURL: "https://secret.example.com"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$prod-db`") {
		t.Errorf("async reply missing allowed prod-db: %q", async)
	}
	if strings.Contains(async, "secret") {
		t.Errorf("async reply leaked non-allowed resource: %q", async)
	}
}

// TestHandleList_NonAdminSeesAllowedResourceIDsWithoutAlias fences the
// `allowed_resource_ids`-only branch of the union: a row whose
// channel_policies has only `allowed_resource_ids` populated (no
// `alias_bindings`) — a hand-seeded legacy shape from before the
// alias-bindings pivot — MUST surface the resource in non-admin
// `/qurl list`. Pre-fix the list handler walked alias-bindings
// only, so a pure-allowed-set resource was `/qurl get`-mintable
// but invisible in `/qurl list` — the two surfaces diverged.
func TestHandleList_NonAdminSeesAllowedResourceIDsWithoutAlias(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	// alias="" → seedPolicySet skips the auto-attached alias_bindings
	// Map. Row carries ONLY `allowed_resource_ids`.
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "", []string{"r_allow_only1"})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_allow_only1", testKeyTargetURL: "https://allowed.example.com"},
			{testKeyResourceID: "r_secret_xx", fAttrAlias: testListAliasSecret, testKeyTargetURL: "https://secret.example.com"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$r_allow_only1`") {
		t.Errorf("non-admin list dropped a resource that's in allowed_resource_ids but has no alias_binding: %q", async)
	}
	if strings.Contains(async, "secret") {
		t.Errorf("async reply leaked non-allowed resource: %q", async)
	}
}

// TestHandleList_NonAdminUnionsAllowedSetAndAliasBindings fences the
// union behavior across both surfaces on the same row: an
// alias-bindings-only resource AND an allowed-set-only resource must
// both surface (an alias-only resource that lives outside the
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
			{testKeyResourceID: "r_allow_set_a", testKeyTargetURL: "https://allowset.example.com"},
			{testKeyResourceID: "r_alias_only_b", fAttrAlias: "alias-b", testKeyTargetURL: "https://aliasonly.example.com"},
			{testKeyResourceID: "r_neither_xx", fAttrAlias: "neither", testKeyTargetURL: "https://neither.example.com"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$r_allow_set_a`") {
		t.Errorf("union missed the allowed-set entry: %q", async)
	}
	if !strings.Contains(async, "`$alias-b`") {
		t.Errorf("union missed the alias-binding entry: %q", async)
	}
	if strings.Contains(async, "neither") {
		t.Errorf("union leaked a resource present in neither surface: %q", async)
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
			{testKeyResourceID: "r_leaked_xx", fAttrAlias: "leaked", testKeyTargetURL: "https://leaked.example.com"},
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
// more pages. Default empty-state ("Create one with /qurl create")
// would mislead the user — the issue is pagination, not absence.
func TestHandleList_NonAdminPaginationGap(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	// No allowed policies → filter drops everything. Master list
	// reports has_more=true so the non-admin pagination-gap copy
	// fires.
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_unallowed_x", fAttrAlias: "unallowed", testKeyTargetURL: "https://x"},
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
// Regression-pin on the channelID != "" guard added in the round-7
// CR pass.
func TestHandleList_NonAdminEmptyChannelWithHasMoreShowsDefault(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_leaked_xx", fAttrAlias: "leaked", testKeyTargetURL: "https://leaked.example.com"},
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
// for a brand-new workspace with zero resources. The hint nudges the
// user toward `/qurl create`.
func TestHandleList_EmptyWorkspace(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
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
	ts.seedAdmin(t)
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
	ts.seedAdmin(t)
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
	ts.seedAdmin(t)
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

// TestHandleList_TunnelResourceWithSlug fences the customer-onboarding
// Phase 3 rendering: a tunnel resource with a non-empty `slug` (set
// by the sidecar's QURL_TUNNEL_SLUG bootstrap) surfaces the slug as
// a "[slug:<slug>]" fragment between the "(tunnel)" placeholder and
// the optional description suffix. The customer's bot user reads this
// to match what the sidecar provisioned against a resource_id before
// pairing it with an alias via `/qurl set-alias`.
func TestHandleList_TunnelResourceWithSlug(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tunnel_slg", fAttrAlias: "prod-dash", testKeyType: resourceTypeTunnel, testKeySlug: "prod-dashboard"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$prod-dash` → (tunnel) [slug:prod-dashboard]") {
		t.Errorf("async reply missing tunnel-with-slug row: %q", async)
	}
}

// TestHandleList_TunnelResourceWithSlugAndDescription fences the
// composition of the slug fragment with the trailing-em-dash
// description suffix. Slug fragment renders BEFORE the description,
// so the line reads "(tunnel) [slug:<slug>] — <description>" — slug
// is structural metadata about the resource identity, description is
// operator-authored prose, and the visual axis preserves that order.
func TestHandleList_TunnelResourceWithSlugAndDescription(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tunnel_sd", fAttrAlias: "ops", testKeyType: resourceTypeTunnel, testKeySlug: "ops-bastion", testKeyDescription: "ops jump host"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$ops` → (tunnel) [slug:ops-bastion] — ops jump host") {
		t.Errorf("async reply missing tunnel-with-slug + description row: %q", async)
	}
}

// TestHandleList_TunnelResourceWithoutSlugOmitsFragment fences the
// legacy / pre-Phase-1A path: a tunnel resource with an empty Slug
// (qurl-service didn't surface slug until PR #743; pre-existing tunnels
// may carry no slug for the lifetime of the row) renders the
// fragment-free "(tunnel)" shape. Choosing "omit cleanly" over
// "slug:none" parallels how URL/transit rows already work and keeps
// the row identical to the pre-Phase-3 shape for legacy tunnels.
func TestHandleList_TunnelResourceWithoutSlugOmitsFragment(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tunnel_nos", fAttrAlias: "legacy-tun", testKeyType: resourceTypeTunnel},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$legacy-tun` → (tunnel)") {
		t.Errorf("async reply missing tunnel placeholder for legacy slug-less tunnel: %q", async)
	}
	if strings.Contains(async, "[slug:") {
		t.Errorf("slug fragment leaked on tunnel row without a slug: %q", async)
	}
	if strings.Contains(async, "slug:none") {
		t.Errorf("slug:none placeholder leaked — implementation chose to omit cleanly: %q", async)
	}
}

// TestHandleList_URLResourceNeverShowsSlugFragment fences the
// tunnel-scope of the slug fragment: a URL/transit resource MUST NOT
// render a "[slug:...]" fragment even if a stray `slug` field shows
// up on the wire (qurl-service rejects slug on non-tunnel creates,
// but defense-in-depth: the renderer keys the fragment on Type=tunnel,
// not on the slug field's presence).
func TestHandleList_URLResourceNeverShowsSlugFragment(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			// Stray slug on a URL row — server shouldn't emit this,
			// but the renderer must defend the type-scoped contract.
			{testKeyResourceID: "r_url_xxxxxx", fAttrAlias: "prod-url", testKeyTargetURL: "https://url-row.example.com", testKeySlug: "should-not-render"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$prod-url` → https://url-row.example.com") {
		t.Errorf("async reply missing URL row: %q", async)
	}
	if strings.Contains(async, "[slug:") {
		t.Errorf("slug fragment leaked on a URL/transit row: %q", async)
	}
	if strings.Contains(async, "should-not-render") {
		t.Errorf("stray wire-level slug bled through on a URL row: %q", async)
	}
}

// TestHandleList_MixedTypesOnlyTunnelRowsShowSlug fences the
// per-row scoping in a heterogeneous list: only tunnel rows surface
// "[slug:...]" fragments; URL rows render plain.
func TestHandleList_MixedTypesOnlyTunnelRowsShowSlug(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tun_slug_a", fAttrAlias: "atun", testKeyType: resourceTypeTunnel, testKeySlug: "alpha-tunnel"},
			{testKeyResourceID: "r_url_btarg1", fAttrAlias: "burl", testKeyTargetURL: "https://b.example.com"},
			{testKeyResourceID: "r_tun_noslug", fAttrAlias: "ctun", testKeyType: resourceTypeTunnel},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	// Tunnel with slug → fragment present.
	if !strings.Contains(async, "`$atun` → (tunnel) [slug:alpha-tunnel]") {
		t.Errorf("async reply missing slug-bearing tunnel row: %q", async)
	}
	// URL row → no fragment, plain target.
	if !strings.Contains(async, "`$burl` → https://b.example.com") {
		t.Errorf("async reply missing URL row: %q", async)
	}
	// Tunnel without slug → bare "(tunnel)" with no fragment.
	if !strings.Contains(async, "`$ctun` → (tunnel)") {
		t.Errorf("async reply missing slug-less tunnel row: %q", async)
	}
	// Exactly one slug fragment in the whole reply — the URL and
	// slug-less tunnel rows MUST NOT carry one.
	if got := strings.Count(async, "[slug:"); got != 1 {
		t.Errorf("expected exactly one [slug:...] fragment across mixed list, got %d: %q", got, async)
	}
}

// TestHandleList_ResourceWithDescription fences the trailing
// "— <description>" annotation. Legacy /qurl list rendered description
// on a separate line; the resource-pivoted version preserves the
// operator-authored context as a one-line suffix so each row stays
// copy-paste-greppable.
func TestHandleList_ResourceWithDescription(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_desc_aaaaa", fAttrAlias: "prod", testKeyTargetURL: "https://prod", testKeyDescription: "production gateway"},
			{testKeyResourceID: "r_nodesc_bbbb", fAttrAlias: "stage", testKeyTargetURL: "https://stage"},
			{testKeyResourceID: "r_tun_descrip", fAttrAlias: "tun", testKeyType: resourceTypeTunnel, testKeyDescription: "ops bastion"},
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
	ts.seedAdmin(t)
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

// TestHandleList_NonAdminPartialPageHasMoreFooter fences the distinct
// non-admin footer when the filtered set is NON-empty and master
// has_more=true. The admin footer ("more results past first N") under-
// states the gap because allow-listed resources may sit past the first
// scan invisibly; the non-admin copy makes that explicit.
func TestHandleList_NonAdminPartialPageHasMoreFooter(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	// One allowed resource in the channel, plus has_more=true so the
	// non-admin pagination-aware footer branch fires.
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "one", []string{"r_one_xxxxxx"})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_one_xxxxxx", fAttrAlias: "one", testKeyTargetURL: "https://one"},
		}, "cursor_xyz", true)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Showing allow-listed resources") {
		t.Errorf("async reply missing non-admin partial-page footer: %q", async)
	}
	// Admin-only "more results past" copy must NOT fire on the non-admin
	// path — these two branches are deliberately disjoint.
	if strings.Contains(async, "more results past") {
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
			{testKeyResourceID: "r_master_xx", fAttrAlias: "master", testKeyTargetURL: "https://x"},
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

// TestHandleList_StableSortBetweenAliasAndResourceID fences the sort
// order: rows are sorted by the underlying token (alias if bound,
// resource_id otherwise) so two consecutive `/qurl list` calls render
// identically.
func TestHandleList_StableSortBetweenAliasAndResourceID(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
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
