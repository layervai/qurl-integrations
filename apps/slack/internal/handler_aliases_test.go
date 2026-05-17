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
// rendered list. Single alias binding on the channel.
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
	// Per-alias resource fetch returns the alias label + a unique
	// target URL so we can assert all three lines independently.
	ts.addCustomer("GET", "/v1/resources/by-alias/alpha", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixtureWithTarget(t, w, "r_alpha", "alpha", "https://alpha.example.com")
	})
	ts.addCustomer("GET", "/v1/resources/by-alias/mu", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixtureWithTarget(t, w, "r_mu", "mu", "https://mu.example.com")
	})
	ts.addCustomer("GET", "/v1/resources/by-alias/zeta", func(w http.ResponseWriter, _ *http.Request) {
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
	if !strings.Contains(async, "No aliases are allowed") {
		t.Errorf("async reply missing empty-state hint: %q", async)
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
