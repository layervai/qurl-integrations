package internal

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// TestHandleAliases_HappyPath fences the canonical /qurl aliases
// flow: ListPolicies → filter by channel → per-row resource fetch →
// rendered list.
//
// SCHEMA NOTE (design fence): channel_policies is keyed by
// (team_id, channel_id), so a single channel can only carry one
// alias scalar today. Multiple aliases per channel would need
// either the `aliases` SS attribute (option A) or per-alias SK
// reshape (option B) — see project_channel_policies_schema_gap.md.
// This test exercises one alias per channel; multi-alias rendering
// is unblocked once Justin picks A vs B.
func TestHandleAliases_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixtureWithTarget(t, w, testResourceIDFix, "prod-db", "https://prod.example.com")
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "Aliases allowed in this channel") {
		t.Errorf("async reply missing header: %q", async)
	}
	if !strings.Contains(async, "`$prod-db` → https://prod.example.com") {
		t.Errorf("async reply missing prod-db line: %q", async)
	}
}

// TestHandleAliases_NoEntries fences the empty-list path: the user
// gets a helpful "ask an admin" hint rather than an empty reply.
func TestHandleAliases_NoEntries(t *testing.T) {
	ts := newAdminTestServers(t)
	// No seeded policies — ListPolicies returns empty.
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "No aliases are allowed") {
		t.Errorf("async reply missing empty-state hint: %q", async)
	}
}

// TestHandleAliases_PerRowFailureDegradesToIDOnly fences the
// parallel-fetch fault tolerance: a 404 on one resource doesn't
// drop the row — it renders as the resource_id-only fallback.
//
// DESIGN FENCE: channel_policies is (team_id, channel_id)-keyed
// (only one alias scalar per row). Multiple alias entries in the
// same channel require either the `aliases` SS attribute (option A)
// or per-alias SK reshape (option B). See
// project_channel_policies_schema_gap.md and SLACK_QURL_ROLLOUT.md
// §"Wave 4". Justin to decide A vs B; this skip stays until the
// decision lands.
func TestHandleAliases_PerRowFailureDegradesToIDOnly(t *testing.T) {
	t.Skip("design fence (channel_policies schema gap): asserting per-row degrade across multiple aliases in one channel requires the multi-alias schema. Awaiting Justin's call on option A (aliases SS attr) vs option B (per-alias SK reshape). See project_channel_policies_schema_gap.md.")
}

// TestHandleAliases_MultipleAliasesInOneChannel fences the user
// promise from the pre-pivot UX: a channel can have two distinct
// aliases visible in `/qurl aliases`.
//
// DESIGN FENCE: blocked on the channel_policies schema gap (see
// the sibling skip above). Today's (team_id, channel_id) key with
// a single `alias` scalar collapses multi-alias-per-channel into
// a single row whose alias field can hold only one value. This
// skip preserves the test as documentation of the expected
// behavior once the schema lands.
func TestHandleAliases_MultipleAliasesInOneChannel(t *testing.T) {
	t.Skip("design fence (channel_policies schema gap): pre-pivot UX promised two distinct aliases per channel. Today's schema can't carry it. Justin's call on option A (aliases SS attr) vs option B (per-alias SK reshape). See project_channel_policies_schema_gap.md.")
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

// TestFanoutAliasRows_RespectsCtxCancellation fences the dispatcher
// loop's ctx-aware semaphore acquire: a canceled ctx during dispatch
// fills un-dispatched rows with id-only fallbacks (no goroutine
// leaks, no deadlock).
func TestFanoutAliasRows_RespectsCtxCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the dispatcher bails on first iteration.

	entries := []slackdata.PolicyEntry{
		{Alias: "a", ResourceID: "r_a", ChannelID: "C"},
		{Alias: "b", ResourceID: "r_b", ChannelID: "C"},
		{Alias: "c", ResourceID: "r_c", ChannelID: "C"},
	}

	var hits atomic.Int32
	ts := newAdminTestServers(t)
	ts.addCustomerPrefix("GET", "/v1/resources/by-alias/", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := newAdminTestHandler(t, ts)
	c, err := h.authenticatedClient(context.Background(), testAdminTeamID)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	log := slogTestLogger(t)

	lines := fanoutAliasRows(ctx, log, c, entries, 1)
	if len(lines) != 3 {
		t.Fatalf("lines len = %d, want 3", len(lines))
	}
	// All three should be id-only fallbacks because ctx is canceled
	// before any worker dispatched.
	for i, l := range lines {
		if !strings.Contains(l, "`r_") {
			t.Errorf("line[%d] = %q, want id-only fallback", i, l)
		}
	}
	// At most one hit may have leaked through before the cancel was
	// observed — but the dispatcher MUST not have queued all three.
	if hits.Load() >= 3 {
		t.Errorf("dispatcher dispatched %d/3 rows despite pre-canceled ctx", hits.Load())
	}
}

// TestHandleAliases_HasMoreFooter fences the truncation footer: when
// the slackdata store reports has_more=true, the user-visible reply
// surfaces a "more results truncated" hint so they know to expect
// pagination in a future release.
//
// Setup constraint: channel_policies is (team_id, channel_id)-keyed
// (one row per channel). To trigger has_more=true under the Query
// Limit, we seed 1 row in C_test (the channel the test slash-cmd
// is invoked in) and aliasesPageLimit more in distinct channels —
// total = aliasesPageLimit + 1 rows for the team, so the page
// returns LastEvaluatedKey and the handler's filter retains the
// C_test row.
func TestHandleAliases_HasMoreFooter(t *testing.T) {
	ts := newAdminTestServers(t)
	// "C_aaa" sorts before any "C_other_*" so the C_test row (renamed
	// to C_aaa for this test) is in the first page returned by Query.
	const filterChannelID = "C_aaa_first"
	ts.seedPolicySet(t, testAdminTeamID, filterChannelID, "primary", []string{"r_primary"})
	for i := 0; i < aliasesPageLimit; i++ {
		channel := "C_zzz_other_"
		if i < 26 {
			channel += string(rune('a' + i))
		} else {
			channel += string(rune('a'+(i/26)-1)) + string(rune('a'+(i%26)))
		}
		ts.seedPolicySet(t, testAdminTeamID, channel, "alias", []string{"r_other"})
	}
	ts.addCustomerPrefix("GET", "/v1/resources/by-alias/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvokerOnChannel(t, h, filterChannelID)

	_, _, async := inv.invokeAdminAsync("aliases", testAdminTeamID, testAdminUserID)
	if !strings.Contains(async, "more results truncated") {
		t.Errorf("async reply missing has_more footer: %q", async)
	}
}
