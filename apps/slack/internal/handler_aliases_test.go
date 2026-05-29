package internal

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

// TestHandleAliases_HappyPath fences the canonical /qurl aliases
// flow: GetChannelPolicy → per-group resource fetch (by resource_id) →
// rendered list. Single alias binding on the channel.
func TestHandleAliases_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("GET", "/v1/resources/"+testResourceIDFix, func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixtureWithTarget(t, w, testResourceIDFix, "prod-db", "https://prod.example.com")
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
// rendering for tunnel-backed aliases: a tunnel resource has no
// target_url, so the row shows the tunnel's `$<slug>` (the same token
// /qurl list renders and /qurl get accepts) instead of the opaque
// resource_id.
func TestHandleAliases_TunnelAliasShowsSlug(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{
		"bastion": "r_bastion01",
	})
	ts.addCustomer("GET", "/v1/resources/r_bastion01", func(w http.ResponseWriter, _ *http.Request) {
		writeTunnelResourceFixture(t, w, "r_bastion01", "bastion", "ops-bastion")
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

// TestHandleAliases_ShowsDisplayName fences the Display Name annotation on
// the aliases view: when the resolved tunnel carries a description (which
// doubles as the Display Name; install seeds it and admins set it via
// `/qurl-admin set-display-name`), an em-dash joins it to the id ahead of
// the alias mapping: • `$<slug>` — <Display Name> → `$<alias>`.
func TestHandleAliases_ShowsDisplayName(t *testing.T) {
	const resID = "r_dn_jumphost"
	ts := newAdminTestServers(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{
		"bastion": resID,
	})
	ts.addCustomer("GET", "/v1/resources/"+resID, func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID:  resID,
			testKeyType:        client.ResourceTypeTunnel,
			testKeySlug:        "ops-bastion",
			testKeyStatus:      client.StatusActive,
			testKeyDescription: "Ops jump host",
		})
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$ops-bastion` — Ops jump host → `$bastion`") {
		t.Errorf("aliases reply missing id + Display Name + alias mapping: %q", async)
	}
}

// TestHandleAliases_MultipleAliasesOneTunnelCollapse fences change #2's
// headline behavior: several aliases pointing at the SAME tunnel
// collapse onto one line — `$<slug> → $<a1>, $<a2>` — with the aliases
// sorted and a single resource fetch for the group (one by-id lookup
// covers the whole group, not one per alias).
func TestHandleAliases_MultipleAliasesOneTunnelCollapse(t *testing.T) {
	ts := newAdminTestServers(t)
	// Two aliases bound to the same tunnel resource_id.
	ts.seedPolicyAliasBindings(t, testAdminTeamID, "C_test", map[string]string{
		"dashboard":       "r_kktest01",
		"kevin-dashboard": "r_kktest01",
	})
	// The group resolves once by its shared resource_id. One fetch covers
	// the whole group regardless of how many aliases point at it; assert
	// the by-id endpoint is hit exactly once.
	var fetches atomic.Int32
	ts.addCustomer("GET", "/v1/resources/r_kktest01", func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		writeTunnelResourceFixture(t, w, "r_kktest01", "dashboard", "kktest")
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "`$kktest` → `$dashboard`, `$kevin-dashboard`") {
		t.Errorf("aliases reply did not collapse both aliases onto one slug line: %q", async)
	}
	if got := fetches.Load(); got != 1 {
		t.Errorf("resource fetches = %d, want 1 (one fetch per tunnel group, not per alias)", got)
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
	// Per-group resource fetch (by resource_id) returns a unique target
	// URL so we can assert all three lines independently.
	ts.addCustomer("GET", "/v1/resources/r_alpha", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixtureWithTarget(t, w, "r_alpha", "alpha", "https://alpha.example.com")
	})
	ts.addCustomer("GET", "/v1/resources/r_mu", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixtureWithTarget(t, w, "r_mu", "mu", "https://mu.example.com")
	})
	ts.addCustomer("GET", "/v1/resources/r_zeta", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixtureWithTarget(t, w, "r_zeta", "zeta", "https://zeta.example.com")
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

// TestFanoutAliasGroups_RespectsCtxCancellation fences the dispatcher
// loop's ctx-aware semaphore acquire: a canceled ctx during dispatch
// fills un-dispatched groups with alias-only fallbacks (no goroutine
// leaks, no deadlock).
func TestFanoutAliasGroups_RespectsCtxCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the dispatcher bails on first iteration.

	// Distinct resource_ids → one group per entry.
	groups := groupAliasEntriesByResource([]slackdata.PolicyEntry{
		{Alias: "a", ResourceID: "r_a", ChannelID: "C"},
		{Alias: "b", ResourceID: "r_b", ChannelID: "C"},
		{Alias: "c", ResourceID: "r_c", ChannelID: "C"},
	})

	var hits atomic.Int32
	ts := newAdminTestServers(t)
	ts.addCustomerPrefix("GET", "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := newAdminTestHandler(t, ts)
	c, err := h.authenticatedClient(context.Background(), testAdminTeamID)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	log := slogTestLogger(t)

	lines := fanoutAliasGroups(ctx, log, c, groups, 1)
	if len(lines) != 3 {
		t.Fatalf("lines len = %d, want 3", len(lines))
	}
	// All three should be alias-only fallbacks because ctx is canceled
	// before any worker dispatched — never the opaque resource_id.
	for i, l := range lines {
		// Match the backtick-prefixed `r_ shape a leaked id would take, not
		// a bare "r_" that a future alias could legitimately contain.
		if !strings.Contains(l, "`$") || strings.Contains(l, "`r_") {
			t.Errorf("line[%d] = %q, want alias-only fallback (no resource_id)", i, l)
		}
	}
	// At most one hit may have leaked through before the cancel was
	// observed — but the dispatcher MUST not have queued all three.
	if hits.Load() >= 3 {
		t.Errorf("dispatcher dispatched %d/3 rows despite pre-canceled ctx", hits.Load())
	}
}

// TestFanoutAliasRows_DeadlineDuringFanoutDoesNotLeak fences the
// worst-case budget posture on /qurl aliases: a channel with many
// alias_bindings and a slow upstream resource API. The combined
// wallclock of sequential fetches (N × per-call latency) can blow
// past asyncWorkTimeout; the dispatcher must:
//
//   - return one line per entry (no entry silently dropped),
//   - return within ctx.Deadline() + a small grace (no goroutine
//     leak that holds onto the worker pool past timeout),
//   - degrade un-dispatched and slow-fetched rows to alias-only
//     fallbacks (the user sees a complete list, just with some
//     bare `$alias` lines instead of `$slug` → `$alias`), never
//     leaking the opaque resource_id.
//
// Without this fence, a refactor that swallows ctx in
// [fanoutAliasGroups]'s per-group goroutine (e.g., dropping the
// ctx-canceled branch in the error switch) would leave the
// dispatcher waiting on wg.Wait() past the response_url deadline,
// silently failing the entire `/qurl aliases` reply.
func TestFanoutAliasGroups_DeadlineDuringFanoutDoesNotLeak(t *testing.T) {
	const numEntries = 80
	entries := make([]slackdata.PolicyEntry, numEntries)
	for i := range entries {
		entries[i] = slackdata.PolicyEntry{
			Alias:      fmt.Sprintf("alias-%02d", i),
			ResourceID: fmt.Sprintf("r_%02d", i),
			ChannelID:  "C",
		}
	}
	// Distinct resource_ids → one group per entry.
	groups := groupAliasEntriesByResource(entries)

	ts := newAdminTestServers(t)
	// Every resource fetch blocks until ctx is canceled — simulates a
	// slow customer API where per-call latency is on the order of the
	// remaining ctx budget. The handler's ctx is propagated through
	// the SDK; the handler.go semaphore-and-ctx logic must observe it.
	ts.addCustomerPrefix("GET", "/v1/resources/", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		// Reply doesn't matter — ctx-canceled errors surface to the
		// caller via the SDK's request layer, not from the response
		// body.
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h := newAdminTestHandler(t, ts)
	c, err := h.authenticatedClient(context.Background(), testAdminTeamID)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	log := slogTestLogger(t)

	// 150ms deadline keeps the test cheap while still letting the
	// dispatcher fan out the first batch of 8 workers (limit) before
	// ctx fires. The remaining 72 entries hit the ctx-aware semaphore
	// branch and fall back to alias-only fallbacks.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	lines := fanoutAliasGroups(ctx, log, c, groups, aliasesResourceFanoutLimit)
	elapsed := time.Since(start)

	if len(lines) != numEntries {
		t.Fatalf("lines len = %d, want %d (every entry must produce one line, even fallback)", len(lines), numEntries)
	}
	// Grace window: dispatched workers wait on ctx-canceled, then
	// rendering takes a few ms. 1s is generously above the deadline
	// and well below asyncWorkTimeout (25s) — a regression that
	// blocks on wg.Wait() would blow past this.
	if elapsed > time.Second {
		t.Errorf("fanout exceeded grace window: elapsed=%s, want ≤1s (deadline=150ms)", elapsed)
	}
	// Every line must be present and non-empty — the contract is
	// "one line per entry, fallback rather than drop".
	for i, l := range lines {
		if l == "" {
			t.Errorf("line[%d] empty — entry was dropped, not fallback-rendered", i)
		}
	}
	// The upstream never resolved a slug (it only ever 503'd or was
	// ctx-canceled), so every line — dispatched and un-dispatched alike —
	// must be an alias-only fallback, and the opaque resource_id must
	// never leak. Match the backtick-prefixed `r_ shape formatAliasGroupLine
	// would emit for a leaked id, so an alias that merely contains "r_"
	// can't false-positive.
	fallbacks := 0
	for _, l := range lines {
		if strings.Contains(l, "`r_") {
			t.Errorf("line leaked opaque resource_id: %q", l)
		}
		if strings.Contains(l, "`$alias-") {
			fallbacks++
		}
	}
	if fallbacks != numEntries {
		t.Errorf("alias-only fallback count = %d, want %d (every row falls back under a failing upstream)", fallbacks, numEntries)
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
	ts.addCustomer("GET", "/v1/resources/r_primary", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixtureWithTarget(t, w, "r_primary", "primary", "https://prod.example.com")
	})
	ts.addCustomerPrefix("GET", "/v1/resources/", func(w http.ResponseWriter, _ *http.Request) {
		// Force a 404 on any non-primary fetch — if the handler leaks
		// a sibling channel's alias, the resource fetch lands here
		// and renders as an alias-only fallback (not the prod-example.com
		// URL).
		w.WriteHeader(http.StatusNotFound)
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
