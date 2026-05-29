package internal

import (
	"net/http"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

// TestHandleAliases_HappyPath fences the canonical /qurl aliases
// flow: GetChannelPolicy → resolve slugs from a single ListResources
// page (joined by resource_id) → rendered list. Single alias binding.
func TestHandleAliases_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: testResourceIDFix, testKeyTargetURL: "https://prod.example.com"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Aliases configured for this channel") {
		t.Errorf("async reply missing header: %q", async)
	}
	// Legacy URL binding: the resource has a target_url and no slug, so the
	// line reads "<url> (legacy URL) → `$<alias>`".
	if !strings.Contains(async, "https://prod.example.com (legacy URL) → `$prod-db`") {
		t.Errorf("async reply missing prod-db line: %q", async)
	}
}

// TestHandleAliases_TunnelAliasShowsSlug fences the resource_id→slug
// rendering for tunnel-backed aliases: the workspace list resolves the
// tunnel's `$<slug>` (the same token /qurl list renders and /qurl get
// accepts) so the row shows the slug, never the opaque resource_id.
func TestHandleAliases_TunnelAliasShowsSlug(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{
		"bastion": "r_bastion01",
	})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_bastion01", testKeyType: client.ResourceTypeTunnel, testKeySlug: "ops-bastion"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	// Tunnel-backed: the slug is the canonical name (left of the arrow)
	// and the alias is its alternate name.
	if !strings.Contains(async, "`$ops-bastion` → `$bastion`") {
		t.Errorf("aliases reply missing slug→alias mapping: %q", async)
	}
	if strings.Contains(async, "r_bastion01") {
		t.Errorf("aliases reply leaked opaque resource_id instead of slug: %q", async)
	}
}

// TestHandleAliases_MultipleAliasesOneTunnelCollapse fences the headline
// behavior: several aliases pointing at the SAME tunnel collapse onto one
// line — `$<slug> → $<a1>, $<a2>` — with the aliases sorted. The single
// ListResources page resolves every group's slug in one call.
func TestHandleAliases_MultipleAliasesOneTunnelCollapse(t *testing.T) {
	ts := newAdminTestServers(t)
	// Two aliases bound to the same tunnel resource_id.
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{
		"dashboard":       "r_kktest01",
		"kevin-dashboard": "r_kktest01",
	})
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_kktest01", testKeyType: client.ResourceTypeTunnel, testKeySlug: "team-dash"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$team-dash` → `$dashboard`, `$kevin-dashboard`") {
		t.Errorf("aliases reply did not collapse both aliases onto one slug line: %q", async)
	}
}

// TestHandleAliases_MultiAliasChannelDisplaysAllBindings fences the
// post-pivot multi-binding behavior: a channel with three
// alias_bindings emits a PolicyEntry per binding, and `/qurl
// aliases` renders all three in deterministic alias-name ascending
// order (the handler sorts lines lexicographically; each line
// starts with `• \`$<alias>\“ so byte-order = alias-name order).
func TestHandleAliases_MultiAliasChannelDisplaysAllBindings(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{
		"zeta":  "r_zeta",
		"alpha": "r_alpha",
		"mu":    "r_mu",
	})
	// One ListResources page returns all three resources with unique target
	// URLs so we can assert all three lines independently.
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_alpha", testKeyTargetURL: "https://alpha.example.com"},
			{testKeyResourceID: "r_mu", testKeyTargetURL: "https://mu.example.com"},
			{testKeyResourceID: "r_zeta", testKeyTargetURL: "https://zeta.example.com"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	for _, alias := range []string{"alpha", "mu", "zeta"} {
		if !strings.Contains(async, "`$"+alias+"`") {
			t.Errorf("async reply missing alias %q line: %q", alias, async)
		}
	}
	// Deterministic order — alpha < mu < zeta when sorted lex.
	idxAlpha := strings.Index(async, "`$alpha`")
	idxMu := strings.Index(async, "`$mu`")
	idxZeta := strings.Index(async, "`$zeta`")
	if idxAlpha >= idxMu || idxMu >= idxZeta {
		t.Errorf("alias lines not in ascending order: alpha=%d mu=%d zeta=%d body=%q", idxAlpha, idxMu, idxZeta, async)
	}
}

// TestHandleAliases_ChannelWithNoAliasBindingsShowsNone fences the
// orthogonality of `alias_bindings` and `allowed_resource_ids`: a
// channel with only the allowed-set populated (no alias_bindings)
// renders the empty-state hint, not silent success. This is the
// `/qurl get`-only channel shape.
func TestHandleAliases_ChannelWithNoAliasBindingsShowsNone(t *testing.T) {
	ts := newAdminTestServers(t)
	// allowed_resource_ids populated, alias_bindings absent — pass
	// alias="" to seedPolicySet so it does NOT auto-seed a binding.
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "", []string{testResourceIDFix})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "No aliases are configured for this channel") {
		t.Errorf("async reply missing empty-state hint: %q", async)
	}
}

// TestHandleAliases_NoEntries fences the empty-list path: the user
// gets a helpful "ask an admin" hint rather than an empty reply.
func TestHandleAliases_NoEntries(t *testing.T) {
	ts := newAdminTestServers(t)
	// No seeded policies — LookupChannelAlias returns empty.
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "No aliases are configured for this channel") {
		t.Errorf("async reply missing empty-state hint: %q", async)
	}
}

// TestHandleAliases_AdminStoreNil fences the no-DDB sandbox case.
func TestHandleAliases_AdminStoreNil(t *testing.T) {
	ts := newAdminTestServers(t)
	h := newAdminTestHandler(t, ts)
	h.cfg.AdminStore = nil
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Admin features are not configured") {
		t.Errorf("async reply missing not-configured message: %q", async)
	}
}

// TestGroupAliasEntriesByResource fences the grouping helper behind
// /qurl aliases: aliases sharing a resource_id collapse into one group
// (sorted), distinct resources stay separate, and resource-less rows
// (the defensive empty-resource_id case) are keyed per-alias so they
// don't merge into each other.
func TestGroupAliasEntriesByResource(t *testing.T) {
	groups := groupAliasEntriesByResource([]slackdata.PolicyEntry{
		{Alias: "zeta", ResourceID: "r_one"},
		{Alias: "alpha", ResourceID: "r_one"},
		{Alias: "solo", ResourceID: "r_two"},
		{Alias: "ghostA", ResourceID: ""},
		{Alias: "ghostB", ResourceID: ""},
	})
	if len(groups) != 4 {
		t.Fatalf("group count = %d, want 4 (r_one collapses 2 aliases, r_two, + 2 distinct resource-less)", len(groups))
	}
	byKey := map[string][]string{}
	for i := range groups {
		// Resource-less rows share resourceID "" — key the assertion map
		// by the first alias so they stay distinguishable.
		key := groups[i].resourceID
		if key == "" {
			key = groups[i].aliases[0]
		}
		byKey[key] = groups[i].aliases
	}
	if got := byKey["r_one"]; len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Errorf("r_one aliases = %v, want sorted [alpha zeta]", got)
	}
	if got := byKey["r_two"]; len(got) != 1 || got[0] != "solo" {
		t.Errorf("r_two aliases = %v, want [solo]", got)
	}
	if len(byKey["ghostA"]) != 1 || len(byKey["ghostB"]) != 1 {
		t.Errorf("resource-less rows merged: ghostA=%v ghostB=%v", byKey["ghostA"], byKey["ghostB"])
	}
}

// TestHandleAliases_OtherChannelsDoNotLeak fences the channel-scoped
// read: seeding alias bindings in OTHER channels under the same team
// does NOT contaminate the calling channel's listing. The post-#233
// shape uses a single GetItem against (team, channel) — there's no
// "filter team-wide after pagination" window in which a sibling
// channel's row could leak through.
func TestHandleAliases_OtherChannelsDoNotLeak(t *testing.T) {
	ts := newAdminTestServers(t)
	const callerChannel = "C_caller"
	ts.seedPolicySet(t, testAdminTeamID, callerChannel, "primary", []string{"r_primary"})
	// Seed many sibling-channel rows to confirm the GetItem scope
	// stays tight even when the team has dozens of channels.
	for i := 0; i < 20; i++ {
		ts.seedPolicySet(t, testAdminTeamID, "C_other_"+string(rune('a'+i)), "leak-canary", []string{"r_leak"})
	}
	// The workspace list carries both tunnels, but the caller channel only
	// binds `primary` — leak-canary is bound in OTHER channels, whose
	// policies GetChannelPolicy(callerChannel) never reads. So the listing
	// must show `$primary` and never `leak-canary`, regardless of what the
	// workspace-wide list returns.
	ts.addCustomer("GET", "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceListFixture(t, w, []map[string]any{
			{testKeyResourceID: "r_primary", testKeyTargetURL: "https://prod.example.com"},
			{testKeyResourceID: "r_leak", testKeyType: client.ResourceTypeTunnel, testKeySlug: "leak-canary"},
		}, "", false)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvokerOnChannel(t, h, callerChannel)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$primary`") {
		t.Errorf("async reply missing primary alias: %q", async)
	}
	if strings.Contains(async, "leak-canary") {
		t.Errorf("async reply leaked sibling-channel alias: %q", async)
	}
}

// TestHandleAliases_EmptyChannelIDRejected fences the no-channel
// guard: a slash command with an empty channel_id (synthetic test
// payload, or some future channel-less invocation) MUST NOT fan
// out a team-wide listing. The v1 surface is channel-scoped.
func TestHandleAliases_EmptyChannelIDRejected(t *testing.T) {
	ts := newAdminTestServers(t)
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvokerOnChannel(t, h, "")

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "must be invoked from a channel") {
		t.Errorf("response missing channel-required guard: %q", async)
	}
}
