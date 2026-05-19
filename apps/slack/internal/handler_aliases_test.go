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

// TestFanoutAliasRows_DeadlineDuringFanoutDoesNotLeak fences the
// worst-case budget posture on /qurl aliases: a channel with many
// alias_bindings and a slow upstream resource API. The combined
// wallclock of sequential fetches (N × per-call latency) can blow
// past asyncWorkTimeout; the dispatcher must:
//
//   - return one line per entry (no entry silently dropped),
//   - return within ctx.Deadline() + a small grace (no goroutine
//     leak that holds onto the worker pool past timeout),
//   - degrade un-dispatched and slow-fetched rows to id-only
//     fallbacks (the user sees a complete list, just with some
//     `$alias` → `r_xxx` lines instead of `$alias` → https://...).
//
// Without this fence, a refactor that swallows ctx in
// [fanoutAliasRows]'s per-row goroutine (e.g., dropping the
// ctx-canceled branch in the error switch) would leave the
// dispatcher waiting on wg.Wait() past the response_url deadline,
// silently failing the entire `/qurl aliases` reply.
func TestFanoutAliasRows_DeadlineDuringFanoutDoesNotLeak(t *testing.T) {
	const numEntries = 80
	entries := make([]slackdata.PolicyEntry, numEntries)
	for i := range entries {
		entries[i] = slackdata.PolicyEntry{
			Alias:      fmt.Sprintf("alias-%02d", i),
			ResourceID: fmt.Sprintf("r_%02d", i),
			ChannelID:  "C",
		}
	}

	ts := newAdminTestServers(t)
	// Every resource fetch blocks until ctx is canceled — simulates a
	// slow customer API where per-call latency is on the order of the
	// remaining ctx budget. The handler's ctx is propagated through
	// the SDK; the handler.go semaphore-and-ctx logic must observe it.
	ts.addCustomerPrefix("GET", "/v1/resources/by-alias/", func(w http.ResponseWriter, r *http.Request) {
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
	// branch and fall back to id-only fallbacks.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	lines := fanoutAliasRows(ctx, log, c, entries, aliasesResourceFanoutLimit)
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
	// At least the tail of the list (the un-dispatched majority) MUST
	// have rendered as id-only `r_xx` fallbacks, never as upstream
	// targets (the upstream never returned anything other than 503).
	idOnly := 0
	for _, l := range lines {
		if strings.Contains(l, "`r_") {
			idOnly++
		}
	}
	if idOnly < numEntries-aliasesResourceFanoutLimit {
		t.Errorf("id-only fallback count = %d, want ≥%d (un-dispatched rows must fall back)", idOnly, numEntries-aliasesResourceFanoutLimit)
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
	ts.addCustomer("GET", "/v1/resources/by-alias/primary", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixtureWithTarget(t, w, "r_primary", "primary", "https://prod.example.com")
	})
	ts.addCustomerPrefix("GET", "/v1/resources/by-alias/", func(w http.ResponseWriter, _ *http.Request) {
		// Force a 404 on any non-primary fetch — if the handler leaks
		// a sibling channel's alias, the resource fetch lands here
		// and renders as id-only (not the prod-example.com URL).
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
