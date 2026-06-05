package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
// Most `/qurl list` fixtures below are tunnel resources carrying a slug; URL
// resource tests add explicit URL rows where that behavior matters.
const (
	testListAliasProdDB   = "prod-db"
	testListAliasSecret   = "secret"
	testListAliasAlpha    = "alpha"
	testListAliasGrafana  = "grafana"
	testListAliasDocs     = "docs"
	testListSlugOpsTunnel = "ops-tunnel"
	testListResIDProdDB   = "r_prod_db_aa"
	testListResIDURLDocs  = "r_url_docs01"
	testListURLDocs       = "https://docs.example.com"
	testListURLFirst      = "https://first.example.com"
	testListURLSecond     = "https://second.example.com"
	testListGetCommand    = "/qurl get"
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
// renders every tunnel exposed to the invoker's channel, each row showing
// the slug as the copy-paste-ready `$<slug>` token. The listing is
// channel-scoped (see TestHandleList_ScopedToChannel), so both tunnels are
// exposed to C_test here; the admin-vs-non-admin distinction does not affect
// it (both see the same scoped set).
func TestHandleList_RendersAllTunnels(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testListResIDProdDB, testKeyType: client.ResourceTypeTunnel, testKeySlug: testListAliasProdDB},
			{testKeyResourceID: "r_stage_db_bb", testKeyType: client.ResourceTypeTunnel, testKeySlug: "stage-db"},
		}, "", false)
	})
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", testListResIDProdDB, "r_stage_db_bb")
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Protected Resources") {
		t.Errorf("async reply missing header: %q", async)
	}
	if !strings.Contains(async, "`$prod-db`") {
		t.Errorf("async reply missing prod-db row: %q", async)
	}
	if !strings.Contains(async, "`$stage-db`") {
		t.Errorf("async reply missing stage-db row: %q", async)
	}
	if !strings.Contains(async, testListGetCommand) {
		t.Errorf("async reply missing copy-paste hint: %q", async)
	}
}

// TestHandleList_ExcludesRevokedTunnels fences that /qurl list drops revoked
// tunnels. The list endpoint is status-visible for active AND revoked rows (a
// revoke is a soft delete; the row is hard-deleted only after a retention
// window), so without the filter a revoked tunnel keeps showing — with a
// "Create qURL" button that can't mint against it.
func TestHandleList_ExcludesRevokedTunnels(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testListResIDProdDB, testKeyType: client.ResourceTypeTunnel, testKeySlug: testListAliasProdDB, testKeyStatus: client.StatusActive},
			{testKeyResourceID: "r_revoked_01", testKeyType: client.ResourceTypeTunnel, testKeySlug: "dead-db", testKeyStatus: client.StatusRevoked},
		}, "", false)
	})
	// Expose BOTH to C_test so the revoked-row exclusion is what hides dead-db,
	// not the channel scope.
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", testListResIDProdDB, "r_revoked_01")
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$prod-db`") {
		t.Errorf("active tunnel missing from list: %q", async)
	}
	if strings.Contains(async, "dead-db") {
		t.Errorf("revoked tunnel should be filtered from /qurl list: %q", async)
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
	// Description doubles as the Display Name and is always set; here it
	// carries the install default ("Slack tunnel install for <slug>"). The
	// row shows the bound aliases AND the Display Name after the em-dash.
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: resID, testKeyType: client.ResourceTypeTunnel, testKeySlug: "kktest", testKeyDescription: "Slack tunnel install for kktest"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$kktest` (aliases: `$kevin-dashboard`, `$ops`) — Slack tunnel install for kktest") {
		t.Errorf("async reply missing slug + bound-aliases + Display Name row: %q", async)
	}
}

// TestFormatTunnelListLine fences the per-row rendering contract
// directly so the slug-only and slug + Display Name (no-alias) shapes are
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
		{name: "slug only, no aliases, no Display Name", resource: tunnel(testListAliasProdDB, ""), boundAliases: nil, want: "• `$prod-db`"},
		{name: "slug + Display Name, no aliases", resource: tunnel(testListAliasProdDB, "Prod database"), boundAliases: nil, want: "• `$prod-db` — Prod database"},
		{name: "slug + one non-slug alias", resource: tunnel(testListAliasProdDB, ""), boundAliases: []string{testListAliasGrafana}, want: "• `$prod-db` (alias: `$grafana`)"},
		{name: "self-binding slug excluded from extras", resource: tunnel(testListAliasProdDB, "Prod database"), boundAliases: []string{testListAliasProdDB, testListAliasGrafana}, want: "• `$prod-db` (alias: `$grafana`) — Prod database"},
		{name: "install-default description renders as Display Name", resource: tunnel(testListAliasProdDB, "Slack tunnel install for "+testListAliasProdDB), boundAliases: nil, want: "• `$prod-db` — Slack tunnel install for " + testListAliasProdDB},
		{name: "only the self-binding slug bound — no extras rendered", resource: tunnel(testListAliasProdDB, ""), boundAliases: []string{testListAliasProdDB}, want: "• `$prod-db`"},
		// Slug-less, resource-alias-less tunnel: no `$<token>` of its own.
		{name: "slug-less tunnel with no bound alias renders bare resource_id", resource: &client.Resource{ResourceID: "r_noslug0001", Type: client.ResourceTypeTunnel, Status: client.StatusActive}, boundAliases: nil, want: "• `r_noslug0001` (no ID — ask your Slack admin to set one)"},
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

func TestFormatURLListLine(t *testing.T) {
	urlResource := func(alias, desc string) *client.Resource {
		return &client.Resource{
			ResourceID:  "r_url_" + alias,
			Type:        client.ResourceTypeURL,
			Alias:       alias,
			TargetURL:   "https://" + alias + ".example.com",
			Status:      client.StatusActive,
			Description: desc,
		}
	}
	cases := []struct {
		name         string
		resource     *client.Resource
		boundAliases []string
		token        string
		blockedAlias string
		want         string
	}{
		{name: "resource alias token", resource: urlResource(testListAliasDocs, ""), boundAliases: nil, want: "• `$docs` → https://docs.example.com"},
		{name: "resource alias plus description", resource: urlResource("billing", "Billing portal"), boundAliases: nil, want: "• `$billing` → https://billing.example.com — Billing portal"},
		{name: "description is mrkdwn escaped", resource: urlResource("alerts", "Use <!channel> *now*"), boundAliases: nil, want: "• `$alerts` → https://alerts.example.com — Use &lt;!channel&gt; ∗now∗"},
		{name: "channel alias fallback when resource alias missing", resource: &client.Resource{ResourceID: testListResIDURLDocs, Type: client.ResourceTypeURL, TargetURL: testListURLDocs, Status: client.StatusActive}, boundAliases: []string{testListAliasDocs}, want: "• `$docs` → https://docs.example.com"},
		{name: "resource alias excludes matching channel alias", resource: urlResource(testListAliasDocs, ""), boundAliases: []string{testListAliasDocs, "kb"}, want: "• `$docs` (alias: `$kb`) → https://docs.example.com"},
		{name: "resource alias token is mrkdwn escaped", resource: &client.Resource{ResourceID: testListResIDURLDocs, Type: client.ResourceTypeURL, Alias: "do`cs", TargetURL: testListURLDocs, Status: client.StatusActive}, boundAliases: nil, want: "• `$doˊcs` → https://docs.example.com"},
		{name: "substituted channel alias names shadowed resource alias", resource: urlResource(testListAliasDocs, ""), boundAliases: []string{"kb"}, token: "kb", blockedAlias: testListAliasDocs, want: "• `$kb` (resource alias `$docs` is shadowed here) → https://docs.example.com"},
		{name: "no alias renders visible but unmintable row", resource: &client.Resource{ResourceID: "r_url_noalias", Type: client.ResourceTypeURL, TargetURL: "https://plain.example.com", Status: client.StatusActive}, boundAliases: nil, want: "• `r_url_noalias` (no alias — ask your Slack admin to set one) → https://plain.example.com"},
		{name: "target URL is mrkdwn escaped", resource: &client.Resource{ResourceID: testListResIDURLDocs, Type: client.ResourceTypeURL, Alias: testListAliasDocs, TargetURL: "https://docs.example.com/a?x=<bad>&q=`tick`\n<!channel>", Status: client.StatusActive}, boundAliases: nil, want: "• `$docs` → https://docs.example.com/a?x=&lt;bad&gt;&amp;q=ˊtickˊ &lt;!channel&gt;"},
		// Cosmetic-lookalike chars (`_`/`*`/`~`) stay literal in the target so the
		// URL remains pasteable; only the structural escapes apply (see escapeMrkdwnURL).
		{name: "target URL keeps underscores/tildes literal (pasteable)", resource: &client.Resource{ResourceID: testListResIDURLDocs, Type: client.ResourceTypeURL, Alias: testListAliasDocs, TargetURL: "https://docs.example.com/~team/my_page", Status: client.StatusActive}, boundAliases: nil, want: "• `$docs` → https://docs.example.com/~team/my_page"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := urlDisplayToken(tc.resource, tc.boundAliases)
			if tc.token != "" {
				token = tc.token
			}
			if got := formatURLListLineWithToken(tc.resource, tc.boundAliases, token, tc.blockedAlias); got != tc.want {
				t.Errorf("formatURLListLineWithToken = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFormatTunnelListSection pins the richer, multi-line mrkdwn the interactive
// /qurl list puts in each row's section block: the `$id` bold on its own line,
// the Display Name beneath, and a faint "alias(es):" line — distinct from the
// single-line plain-text fallback in [TestFormatTunnelListLine].
func TestFormatTunnelListSection(t *testing.T) {
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
		{name: "slug only, no Display Name", resource: tunnel(testListAliasProdDB, ""), boundAliases: nil, want: "*`$prod-db`*"},
		{name: "slug + Display Name", resource: tunnel(testListAliasProdDB, "Prod database"), boundAliases: nil, want: "*`$prod-db`*\nProd database"},
		{name: "slug + one non-slug alias, no Display Name", resource: tunnel(testListAliasProdDB, ""), boundAliases: []string{testListAliasGrafana}, want: "*`$prod-db`*\n_alias:_ `$grafana`"},
		{name: "slug + Display Name + two aliases (self-binding slug excluded)", resource: tunnel(testListAliasProdDB, "Prod database"), boundAliases: []string{testListAliasProdDB, testListAliasGrafana, "metrics"}, want: "*`$prod-db`*\nProd database\n_aliases:_ `$grafana`, `$metrics`"},
		{name: "only the self-binding slug bound — no aliases line", resource: tunnel(testListAliasProdDB, "Prod database"), boundAliases: []string{testListAliasProdDB}, want: "*`$prod-db`*\nProd database"},
		{name: "slug-less, alias-less tunnel spells out the missing ID", resource: &client.Resource{ResourceID: "r_noslug0001", Type: client.ResourceTypeTunnel, Status: client.StatusActive}, boundAliases: nil, want: "*`r_noslug0001`*\n_No ID set — ask your Slack admin to set one._"},
		{name: "slug-less tunnel keeps its Display Name above the no-ID note", resource: &client.Resource{ResourceID: "r_noslug0002", Type: client.ResourceTypeTunnel, Status: client.StatusActive, Description: "ops jump host"}, boundAliases: nil, want: "*`r_noslug0002`*\nops jump host\n_No ID set — ask your Slack admin to set one._"},
		{name: "slug-less tunnel promotes first bound alias to primary", resource: &client.Resource{ResourceID: "r_noslug0001", Type: client.ResourceTypeTunnel, Status: client.StatusActive}, boundAliases: []string{testListAliasGrafana, "metrics"}, want: "*`$grafana`*\n_alias:_ `$metrics`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := tunnelDisplayToken(tc.resource, tc.boundAliases)
			if got := formatTunnelListSection(tc.resource, tc.boundAliases, token); got != tc.want {
				t.Errorf("formatTunnelListSection = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatURLListSection(t *testing.T) {
	r := &client.Resource{
		ResourceID:  testListResIDURLDocs,
		Type:        client.ResourceTypeURL,
		Alias:       testListAliasDocs,
		TargetURL:   testListURLDocs,
		Description: "Docs portal",
		Status:      client.StatusActive,
	}
	boundAliases := []string{testListAliasDocs, "kb"}
	if got, want := formatURLListSectionWithToken(r, boundAliases, urlDisplayToken(r, boundAliases), ""), "*`$docs`*\nhttps://docs.example.com\nDocs portal\n_alias:_ `$kb`"; got != want {
		t.Errorf("formatURLListSectionWithToken = %q, want %q", got, want)
	}

	unsafeDescription := &client.Resource{ResourceID: testListResIDURLDocs, Type: client.ResourceTypeURL, Alias: testListAliasDocs, TargetURL: testListURLDocs, Description: "Use <!channel> *now*"}
	if got, want := formatURLListSectionWithToken(unsafeDescription, nil, "docs", ""), "*`$docs`*\nhttps://docs.example.com\nUse &lt;!channel&gt; ∗now∗"; got != want {
		t.Errorf("formatURLListSectionWithToken(unsafe description) = %q, want %q", got, want)
	}

	if got, want := formatURLListSectionWithToken(r, []string{"kb"}, "kb", testListAliasDocs), "*`$kb`*\n_Resource alias `$docs` is shadowed here._\nhttps://docs.example.com\nDocs portal"; got != want {
		t.Errorf("formatURLListSectionWithToken(shadowed alias) = %q, want %q", got, want)
	}

	unsafeTarget := &client.Resource{ResourceID: testListResIDURLDocs, Type: client.ResourceTypeURL, Alias: testListAliasDocs, TargetURL: "https://docs.example.com/a?x=<bad>&q=`tick`\n<!channel>"}
	if got, want := formatURLListSectionWithToken(unsafeTarget, nil, "docs", ""), "*`$docs`*\nhttps://docs.example.com/a?x=&lt;bad&gt;&amp;q=ˊtickˊ &lt;!channel&gt;"; got != want {
		t.Errorf("formatURLListSectionWithToken(unsafe target) = %q, want %q", got, want)
	}

	unsafeAlias := &client.Resource{ResourceID: testListResIDURLDocs, Type: client.ResourceTypeURL, Alias: "do`cs", TargetURL: testListURLDocs}
	if got, want := formatURLListSectionWithToken(unsafeAlias, nil, "do`cs", ""), "*`$doˊcs`*\nhttps://docs.example.com"; got != want {
		t.Errorf("formatURLListSectionWithToken(unsafe alias) = %q, want %q", got, want)
	}

	// Underscores/tildes in the target stay literal so the rendered URL is still
	// pasteable (escapeMrkdwnURL drops the cosmetic-lookalike substitutions).
	underscoreTarget := &client.Resource{ResourceID: testListResIDURLDocs, Type: client.ResourceTypeURL, Alias: testListAliasDocs, TargetURL: "https://docs.example.com/~team/my_page"}
	if got, want := formatURLListSectionWithToken(underscoreTarget, nil, "docs", ""), "*`$docs`*\nhttps://docs.example.com/~team/my_page"; got != want {
		t.Errorf("formatURLListSectionWithToken(underscore target) = %q, want %q", got, want)
	}

	noAlias := &client.Resource{ResourceID: "r_url_noalias", Type: client.ResourceTypeURL, TargetURL: "https://plain.example.com"}
	if got, want := formatURLListSectionWithToken(noAlias, nil, "", ""), "*`r_url_noalias`*\n_No alias set — ask your Slack admin to set one._\nhttps://plain.example.com"; got != want {
		t.Errorf("formatURLListSectionWithToken(no alias) = %q, want %q", got, want)
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

// TestHandleList_ScopedToChannel is the keystone regression fence for the
// channel-scoping fix: /qurl list shows only the tunnels exposed to the
// invoker's channel — for ADMINS as well as non-admins (the reported bug was an
// admin running `/qurl list` and seeing every tunnel in every channel,
// including DMs). It exercises the three channel shapes:
//
//   - a channel where prod-db is exposed (C_with_policy): prod-db shows, the
//     secret tunnel — exposed nowhere — does NOT;
//   - a channel with no policy row (C_no_policy_here): the channel empty state;
//   - a DM (`D…`) channel: the channel empty state (the most user-surprising
//     pre-fix case, "I ran /qurl list in a 1:1 and saw tunnels from #ops").
//
// The C_with_policy case runs for both an admin and a non-admin caller to pin
// that the scope is channel-only — admins are NOT exempt. If the scope filter
// regressed (e.g. an admin bypass, or dropping the allow-set read), the secret
// tunnel would leak and these assertions would fail.
func TestHandleList_ScopedToChannel(t *testing.T) {
	const (
		nonAdminUserID    = "UMEMBER01"
		channelWithPolicy = "C_with_policy"
	)
	ts := newAdminTestServers(t)
	ts.seedAdmin(t) // testAdminUserID is admin/owner; nonAdminUserID is not
	// prod-db is exposed to channelWithPolicy; the secret tunnel is exposed nowhere.
	ts.seedChannelExposure(t, testAdminTeamID, channelWithPolicy, testListResIDProdDB)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testListResIDProdDB, testKeyType: client.ResourceTypeTunnel, testKeySlug: testListAliasProdDB},
			{testKeyResourceID: "r_secret_xx", testKeyType: client.ResourceTypeTunnel, testKeySlug: testListAliasSecret},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)

	// In the channel where prod-db is exposed, both an admin and a non-admin see
	// prod-db and NOT the unexposed secret tunnel.
	for _, caller := range []struct{ name, userID string }{
		{"admin-caller", testAdminUserID},
		{"non-admin-caller", nonAdminUserID},
	} {
		t.Run(channelWithPolicy+"/"+caller.name, func(t *testing.T) {
			inv := newAdminSlashInvokerOnChannel(t, h, channelWithPolicy)
			_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, caller.userID)
			if !strings.Contains(async, "`$"+testListAliasProdDB+"`") {
				t.Errorf("%s should see the exposed prod-db tunnel: %q", caller.name, async)
			}
			if strings.Contains(async, "`$"+testListAliasSecret+"`") || strings.Contains(async, "r_secret_xx") {
				t.Errorf("%s saw the secret tunnel, which is exposed to no channel — scope leak: %q", caller.name, async)
			}
		})
	}

	// A channel with no policy row, and a DM, expose nothing → the channel
	// empty state. (These are the cases #459's revert was meant to avoid
	// dead-ending; the empty state now names the Edit recovery path.)
	for _, channelID := range []string{"C_no_policy_here", "D_direct_msg_1to1"} {
		t.Run("empty/"+channelID, func(t *testing.T) {
			inv := newAdminSlashInvokerOnChannel(t, h, channelID)
			_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
			if !strings.Contains(async, "No protected resources are available in this channel") {
				t.Errorf("expected the channel empty state in %s: %q", channelID, async)
			}
			if !strings.Contains(async, "/qurl-admin tunnel install") {
				t.Errorf("admin empty state should include setup command in %s: %q", channelID, async)
			}
			if !strings.Contains(async, "Edit") {
				t.Errorf("admin empty state should include expose-via-Edit hint in %s: %q", channelID, async)
			}
			if strings.Contains(async, "`$"+testListAliasProdDB+"`") || strings.Contains(async, "`$"+testListAliasSecret+"`") {
				t.Errorf("a tunnel leaked into %s, where none is exposed: %q", channelID, async)
			}
		})
	}
}

// TestHandleList_ExposedViaAllowedSetShowsWithoutAlias fences that a tunnel
// exposed to a channel purely via allowed_resource_ids (no alias binding —
// the shape the Edit modal's "expose to channels" writes) is listed there. It
// pins that the scope gate keys on the full AllowedResourceIDsForChannel union,
// not just alias bindings.
func TestHandleList_ExposedViaAllowedSetShowsWithoutAlias(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_via_set01", testKeyType: client.ResourceTypeTunnel, testKeySlug: "set-exposed"},
		}, "", false)
	})
	// allowed_resource_ids only — no alias_bindings entry in this channel.
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_via_set01")
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$set-exposed`") {
		t.Errorf("tunnel exposed via allowed_resource_ids (no alias) should list: %q", async)
	}
}

// TestHandleList_FailsClosedOnScopeReadError fences the fail-closed posture: if
// the channel allow-set read errors, /qurl list surfaces service-unreachable and
// does NOT fall back to an unscoped listing (the upstream is never even called).
func TestHandleList_FailsClosedOnScopeReadError(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	// Make the channel_policies read (AllowedResourceIDsForChannel) fail.
	ts.ddb.SetGetItemErr(ts.tableNames.channelPolicy, errors.New("ddb unavailable"))
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("ListResources called despite a scope-read failure — must fail closed, not list unscoped")
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_secret_x", testKeyType: client.ResourceTypeTunnel, testKeySlug: "secret"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Could not reach qURL") {
		t.Errorf("scope-read failure should fail closed with service-unreachable: %q", async)
	}
	if strings.Contains(async, "secret") {
		t.Errorf("a tunnel leaked when the scope read failed — must fail closed: %q", async)
	}
}

// TestHandleList_EmptyChannelRejected fences the channel-required guard: a
// synthetic payload with no channel_id can't be scoped, so the listing refuses
// rather than fanning out workspace-wide (mirrors /qurl aliases).
func TestHandleList_EmptyChannelRejected(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("ListResources called for a channel-less list — must refuse before any read")
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvokerOnChannel(t, h, "") // truly-empty channel_id

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, channelRequiredMessage) {
		t.Errorf("empty channel_id should be refused with the channel-required copy: %q", async)
	}
}

// TestHandleList_EmptyChannel fences the friendly empty-state copy when no
// protected resource is available in the invoker's channel. Non-admins get a
// plain admin handoff; confirmed admins get the admin setup command and the
// expose-via-Edit recovery hint. A failing /v1/resources stub asserts the
// empty allow-set short-circuits before the upstream call.
func TestHandleList_EmptyChannel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seed    func(*testing.T, *adminTestServers)
		want    []string
		notWant []string
	}{
		{
			name:    "admin sees setup command",
			seed:    func(t *testing.T, ts *adminTestServers) { ts.seedAdmin(t) },
			want:    []string{"/qurl-admin tunnel install", "Edit"},
			notWant: []string{"Ask a Slack admin"},
		},
		{
			name:    "non-admin gets admin handoff",
			seed:    func(t *testing.T, ts *adminTestServers) { ts.seedNonAdmin(t) },
			want:    []string{"Ask a Slack admin"},
			notWant: []string{"/qurl-admin tunnel install", "Edit", "tunnel"},
		},
		{
			name: "admin check error gets admin handoff",
			seed: func(t *testing.T, ts *adminTestServers) {
				ts.seedAdmin(t)
				ts.ddb.SetGetItemErr(ts.tableNames.workspace, errors.New("injected workspace read failure"))
			},
			want:    []string{"Ask a Slack admin"},
			notWant: []string{"/qurl-admin tunnel install", "Edit", "tunnel"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newAdminTestServers(t)
			tc.seed(t, ts)
			ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
				t.Errorf("ListResources called despite an empty channel allow-set (scope short-circuit regressed)")
				writeAPIError(t, w, http.StatusBadGateway, "upstream_error", "should not be called")
			})
			h := newAdminTestHandler(t, ts)
			inv := newAdminSlashInvoker(t, h)

			_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
			if !strings.Contains(async, "No protected resources are available in this channel") {
				t.Errorf("async reply missing empty-state copy: %q", async)
			}
			for _, want := range tc.want {
				if !strings.Contains(async, want) {
					t.Errorf("async reply missing %q: %q", want, async)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(async, notWant) {
					t.Errorf("async reply leaked %q: %q", notWant, async)
				}
			}
		})
	}
}

// TestHandleList_URLResourcesListed fences the restored URL-resource scope:
// URL resources exposed to this channel appear alongside tunnels, using their
// resource alias as the copy-paste token. A stray slug on a URL row must NOT
// become the token — slugs are tunnel-only. URL rows with no alias stay visible
// but render as "no alias" rows, so the list does not advertise an unmintable
// `$r_...` token.
func TestHandleList_URLResourcesListed(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tun_aaaaaa", testKeyType: client.ResourceTypeTunnel, testKeySlug: "alpha-tunnel"},
			{testKeyResourceID: "r_url_btarg1", testKeyType: client.ResourceTypeURL, fAttrAlias: "burl", testKeyTargetURL: "https://b.example.com", testKeyDescription: "Billing portal"},
			{testKeyResourceID: "r_url_stray1", testKeyTargetURL: "https://c.example.com", testKeySlug: "stray-slug"},
		}, "", false)
	})
	// Expose all three to C_test so the type handling (not channel scope) is
	// what decides how the URL rows render.
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_tun_aaaaaa", "r_url_btarg1", "r_url_stray1")
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$alpha-tunnel`") {
		t.Errorf("async reply missing the tunnel row: %q", async)
	}
	if !strings.Contains(async, "`$burl` → https://b.example.com — Billing portal") {
		t.Errorf("async reply missing URL resource alias row: %q", async)
	}
	if !strings.Contains(async, "`r_url_stray1` (no alias — ask your Slack admin to set one) → https://c.example.com") {
		t.Errorf("async reply missing no-alias URL row: %q", async)
	}
	if strings.Contains(async, "`$stray-slug`") {
		t.Errorf("URL resource with stray slug rendered a tunnel slug token: %q", async)
	}
}

// TestHandleList_TunnelSlugIsToken fences the core display contract:
// a tunnel renders its slug as the `$<token>` — NOT the opaque
// resource_id, NOT a resource-level alias, and with no `(tunnel)`
// label or `[slug:...]` fragment (both redundant now that the whole
// list is tunnels and the token IS the slug). The customer's
// onboarding flow reads this slug to match what the sidecar
// provisioned (via QURL_TUNNEL_ID) and pastes it into `/qurl get`.
func TestHandleList_TunnelSlugIsToken(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tunnel_slg", fAttrAlias: "dash-alias", testKeyType: client.ResourceTypeTunnel, testKeySlug: "prod-dashboard"},
		}, "", false)
	})
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_tunnel_slg")
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
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_tunnel_aaa")
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
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_tunnel_noa")
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`r_tunnel_noa` (no ID — ask your Slack admin to set one)") {
		t.Errorf("async reply missing bare resource_id fallback: %q", async)
	}
	if strings.Contains(async, "`$r_tunnel_noa`") {
		t.Errorf("slug-less tunnel rendered a `$r_<id>` get token (no longer mintable): %q", async)
	}
	if strings.Contains(async, "(tunnel)") {
		t.Errorf("redundant (tunnel) label leaked: %q", async)
	}
}

// TestHandleList_TunnelWithDisplayName fences the trailing
// "— <Display Name>" annotation, and that a tunnel with no Display Name
// renders just the bare token (no dangling em-dash).
func TestHandleList_TunnelWithDisplayName(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_tun_desc1", testKeyType: client.ResourceTypeTunnel, testKeySlug: "ops-bastion", testKeyDescription: "ops jump host"},
			{testKeyResourceID: "r_tun_nodes", testKeyType: client.ResourceTypeTunnel, testKeySlug: "no-desc-tun"},
		}, "", false)
	})
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_tun_desc1", "r_tun_nodes")
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$ops-bastion` — ops jump host") {
		t.Errorf("async reply missing slug + Display Name row: %q", async)
	}
	if !strings.Contains(async, "`$no-desc-tun`") {
		t.Errorf("async reply missing no-Display-Name tunnel row: %q", async)
	}
	if strings.Contains(async, "`$no-desc-tun` —") {
		t.Errorf("tunnel with no Display Name should not render a trailing em-dash: %q", async)
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
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_one_aa")
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
	// A non-empty channel allow-set so the scope short-circuit doesn't skip the
	// upstream call this test is exercising (the rid needn't match any fixture
	// row — the upstream errors before filtering).
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_unused01")
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

func TestMapListResourcesErrorIncludesRequestIDWithoutLeakingAPIText(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	err := &client.APIError{
		StatusCode: http.StatusBadGateway,
		Code:       "upstream_error",
		Title:      "Bad Gateway from internal API",
		Detail:     "db: connection to internal-host:5432 refused",
		RequestID:  "req_list123",
	}

	msg := mapListResourcesError(log, testAdminTeamID, err)
	for _, leak := range []string{err.Title, err.Detail, "internal-host"} {
		if strings.Contains(msg, leak) {
			t.Errorf("list error response leaked %q: %q", leak, msg)
		}
	}
	for _, want := range []string{"Could not reach qURL", "Please try again", "req_list123"} {
		if !strings.Contains(msg, want) {
			t.Errorf("list error response missing %q: %q", want, msg)
		}
	}
	if !strings.Contains(logs.String(), "request_id=req_list123") {
		t.Errorf("list error log missing request_id: %q", logs.String())
	}
}

func TestMapListResourcesErrorRateLimitUsesRetryHintWithoutLeakingAPIText(t *testing.T) {
	cases := []struct {
		name       string
		retryAfter int
		requestID  string
		wantRetry  string
	}{
		{name: "retry after seconds", retryAfter: 45, requestID: "req_rate123", wantRetry: "Try again in 45s"},
		{name: "missing retry after", retryAfter: 0, requestID: "req_rate_zero", wantRetry: "Try again in a moment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			err := &client.APIError{
				StatusCode: http.StatusTooManyRequests,
				Code:       testAPIErrorCodeRateLimited,
				Title:      "Too Many Requests from internal API",
				Detail:     "tenant quota shard qurl-internal-7 exceeded",
				RequestID:  tc.requestID,
				RetryAfter: tc.retryAfter,
			}

			msg := mapListResourcesError(log, testAdminTeamID, err)
			for _, leak := range []string{err.Title, err.Detail, "qurl-internal-7", testAPIErrorCodeRateLimited} {
				if strings.Contains(msg, leak) {
					t.Errorf("list rate-limit response leaked %q: %q", leak, msg)
				}
			}
			for _, want := range []string{"Rate limit hit", tc.wantRetry, tc.requestID} {
				if !strings.Contains(msg, want) {
					t.Errorf("list rate-limit response missing %q: %q", want, msg)
				}
			}
			if !strings.Contains(logs.String(), "request_id="+tc.requestID) {
				t.Errorf("list rate-limit log missing request_id: %q", logs.String())
			}
		})
	}
}

func TestMapListResourcesErrorPermanentClassUsesGenericListFailure(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	err := &client.APIError{
		StatusCode: http.StatusUnprocessableEntity,
		Code:       "invalid_cursor",
		Title:      "Invalid Cursor",
		Detail:     "cursor tenant shard mismatch",
	}

	msg := mapListResourcesError(log, testAdminTeamID, err)
	for _, leak := range []string{err.Title, err.Detail, "invalid_cursor"} {
		if strings.Contains(msg, leak) {
			t.Errorf("list permanent-class response leaked %q: %q", leak, msg)
		}
	}
	if !strings.Contains(msg, "Failed to list qURL resources") {
		t.Errorf("list permanent-class response missing generic list failure: %q", msg)
	}
	if strings.Contains(msg, "Could not reach qURL") {
		t.Errorf("list permanent-class response looked like transport failure: %q", msg)
	}
	if strings.Contains(msg, "Please try again") {
		t.Errorf("list permanent-class response included retry hint: %q", msg)
	}
	if strings.Contains(logs.String(), "request_id=") {
		t.Errorf("list error log included empty request_id attr: %q", logs.String())
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
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_zzz_aaaaa", "r_aaa_xxxxx", "r_mmm_yyyyy")
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
	// Legible tokens lead in order ("alpha" < "middle"); the tokenless
	// slug-less row sorts to the END.
	zzzPos := strings.Index(async, "`r_zzz_aaaaa` (no ID — ask your Slack admin to set one)")
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
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_bbb_dup", "r_aaa_dup")
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
