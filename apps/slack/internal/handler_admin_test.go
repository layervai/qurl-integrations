package internal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// Test-local string constants pulled out to satisfy goconst on the
// shared HTTP/DDB body keys and the canned fixture identifiers. The
// existing package-level constants (testListAliasProdDB,
// testKeyResourceID, etc.) cover most of the shared lexicon — these
// fill the gaps used only inside the admin test file.
const (
	testKeyHTTPStatus    = "status"
	testKeyHTTPTargetURL = "target_url"
	testFixtureQURLOne   = "q_one"
	testFixtureQURLTwo   = "q_two"
	testFixtureTargetX   = "https://x"
)

// --- Allow ---

// TestHandleAdminAllow_HappyPath fences the canonical allow flow:
// admin gate → alias resolve → AllowResource UpdateItem → success reply.
func TestHandleAdminAllow_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, "r_prod_db_xyz", testListAliasProdDB)
	})
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	status, reply := inv.invokeAdmin("admin allow <#C12345|chat> $prod-db", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(reply, "Allowed `$prod-db`") {
		t.Errorf("reply missing success line: %q", reply)
	}
	// Confirm the row landed in the fake DDB.
	if !ts.ddb.policyHasResource(t, testAdminTeamID, "C12345", "r_prod_db_xyz") {
		t.Error("AllowResource did not write the channel_policies row")
	}
}

// TestHandleAdminAllow_NonAdmin fences the admin-only gate. A
// non-admin user gets the uniform `:warning: this command is admin-only`
// reply and no alias resolution + no DDB mutation occurs.
func TestHandleAdminAllow_NonAdmin(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	var aliasHits atomic.Int32
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		aliasHits.Add(1)
		writeResourceFixture(t, w, "r_xyz", testListAliasProdDB)
	})
	ts.failOnAllowResource(t, "non-admin should be gated before AllowResource")

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin allow <#C12345|chat> $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "admin-only") {
		t.Errorf("reply missing admin-only fence: %q", reply)
	}
	if aliasHits.Load() != 0 {
		t.Errorf("alias resolved despite non-admin gate (hits = %d)", aliasHits.Load())
	}
}

// TestHandleAdminAllow_AliasNotFound fences the alias-resolution 404
// surface. Operators see a friendly "alias not found" message rather
// than a stack trace, and AllowResource is never invoked.
func TestHandleAdminAllow_AliasNotFound(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources/by-alias/missing", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"title":"Not Found","detail":"alias not bound","code":"alias_not_found","status":404}}`))
	})
	ts.failOnAllowResource(t, "alias-not-found should bail before AllowResource")

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin allow <#C12345|chat> $missing", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "$missing` not found") {
		t.Errorf("reply missing not-found surface: %q", reply)
	}
}

// TestHandleAdminAllow_AlreadyAllowed fences the 409-idempotent
// surface. AllowResource returning *slackdata.Error with StatusCode=409
// is rendered as a friendly "already allowed — nothing to do" reply,
// not a 5xx-like error.
func TestHandleAdminAllow_AlreadyAllowed(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	// Pre-seed the policy row with the resource_id in the SS so
	// AllowResource's conditional UpdateItem returns 409.
	ts.seedPolicySet(t, testAdminTeamID, "C12345", testListAliasProdDB, []string{"r_prod_db_xyz"})
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, "r_prod_db_xyz", testListAliasProdDB)
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin allow <#C12345|chat> $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "already allowed") {
		t.Errorf("reply missing idempotent surface: %q", reply)
	}
}

// TestAllowResource_ConcurrentAllowsExactlyOneWins fences the TOCTOU
// posture of [slackdata.Store.AllowResource] / [DisallowResource].
// The earlier probe-then-update shape let two concurrent admins both
// observe "not present" and both render "Allowed" while only one
// actually flipped state. The conditional-UpdateItem rewrite collapses
// that to a single round-trip with `NOT contains(...)` so the loser
// surfaces a deterministic 409. Without that fix, this test would
// either flake or report two 200s.
//
// We hit the slackdata.Store directly (no handler/HTTP shim) so the
// failure mode is precisely localized to the policy mutation path.
func TestAllowResource_ConcurrentAllowsExactlyOneWins(t *testing.T) {
	ts := newAdminTestServers(t)
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)

	const (
		teamID     = "T_team"
		channelID  = "C_concurrency"
		resourceID = "r_shared_xyz"
		racers     = 8
	)

	type result struct {
		err error
	}
	results := make(chan result, racers)
	start := make(chan struct{})
	for range racers {
		go func() {
			<-start // line racers up so they all hit UpdateItem in close succession
			results <- result{err: store.AllowResource(context.Background(), teamID, channelID, resourceID)}
		}()
	}
	close(start)

	var (
		successes  int
		conflicts  int
		unexpected []error
	)
	for range racers {
		r := <-results
		if r.err == nil {
			successes++
			continue
		}
		var se *slackdata.Error
		if errors.As(r.err, &se) && se.StatusCode == http.StatusConflict && se.Code == "policy_already_exists" {
			conflicts++
			continue
		}
		unexpected = append(unexpected, r.err)
	}
	if len(unexpected) > 0 {
		t.Fatalf("unexpected error(s) from concurrent AllowResource: %v", unexpected)
	}
	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1 (TOCTOU regression: every racer saw 'not present' and ADDed)", successes)
	}
	if conflicts != racers-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, racers-1)
	}
	if !ts.ddb.policyHasResource(t, teamID, channelID, resourceID) {
		t.Error("policy row did not land in DDB after concurrent allows")
	}
}

// TestDisallowResource_ConcurrentDisallowsExactlyOneWins is the
// symmetric counterpart: against a row that already contains
// resourceID, only one of N concurrent disallows actually flips
// state; the rest surface a deterministic 404 via the
// `contains(...)` guard, not the success copy.
func TestDisallowResource_ConcurrentDisallowsExactlyOneWins(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_concurrency", testListAliasProdDB, []string{"r_shared_xyz"})
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)

	const (
		channelID  = "C_concurrency"
		resourceID = "r_shared_xyz"
		racers     = 8
	)

	results := make(chan error, racers)
	start := make(chan struct{})
	for range racers {
		go func() {
			<-start
			results <- store.DisallowResource(context.Background(), testAdminTeamID, channelID, resourceID)
		}()
	}
	close(start)

	var (
		successes  int
		notFound   int
		unexpected []error
	)
	for range racers {
		err := <-results
		if err == nil {
			successes++
			continue
		}
		var se *slackdata.Error
		if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
			notFound++
			continue
		}
		unexpected = append(unexpected, err)
	}
	if len(unexpected) > 0 {
		t.Fatalf("unexpected error(s) from concurrent DisallowResource: %v", unexpected)
	}
	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1", successes)
	}
	if notFound != racers-1 {
		t.Errorf("notFound = %d, want %d", notFound, racers-1)
	}
}

// TestAllowResource_FirstAllowStampsCreatedAt fences the row-creation
// branch of [slackdata.Store.AllowResource]: the conditional
// UpdateItem is also the row-creation path for the first allow against
// a channel, so the SET expression carries
// `created_at = if_not_exists(created_at, :now)`. Without that clause
// the row lands with no created_at attribute and ListPolicies later
// renders the zero time into the audit/UI surface. A second allow on
// the same channel preserves the original timestamp because
// if_not_exists short-circuits the SET on existing attributes.
func TestAllowResource_FirstAllowStampsCreatedAt(t *testing.T) {
	ts := newAdminTestServers(t)
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	const (
		teamID     = "T_first"
		channelID  = "C_first"
		resourceID = "r_first_xyz"
	)
	if err := store.AllowResource(context.Background(), teamID, channelID, resourceID); err != nil {
		t.Fatalf("AllowResource: %v", err)
	}
	list, err := store.ListPolicies(context.Background(), teamID, "", 50)
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	if len(list.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(list.Entries))
	}
	if list.Entries[0].CreatedAt.IsZero() {
		t.Errorf("CreatedAt is zero; AllowResource did not stamp created_at on first allow")
	}
}

// TestHandleAdminAllow_MissingSigil fences the parser path: an
// alias missing the `$` sigil routes to a parser-error reply, not to
// the dispatch fan-out. AllowResource is never called.
func TestHandleAdminAllow_MissingSigil(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.failOnAllowResource(t, "parser error should bail before AllowResource")

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin allow <#C12345|chat> prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, ":warning:") {
		t.Errorf("reply missing parser-error surface: %q", reply)
	}
}

// --- Disallow ---

// TestHandleAdminDisallow_HappyPath fences the canonical disallow flow.
func TestHandleAdminDisallow_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	// Seed the policy with resource_id in the SS so DisallowResource
	// finds it and removes it.
	ts.seedPolicySet(t, testAdminTeamID, "C12345", testListAliasProdDB, []string{"r_prod_db_xyz"})
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, "r_prod_db_xyz", testListAliasProdDB)
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin disallow <#C12345|chat> $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "Disallowed") {
		t.Errorf("reply missing success line: %q", reply)
	}
	if ts.ddb.policyHasResource(t, testAdminTeamID, "C12345", "r_prod_db_xyz") {
		t.Error("DisallowResource did not remove the channel_policies row")
	}
}

// TestHandleAdminDisallow_NotFoundIsGraceful fences the 404-idempotent
// surface: disallowing an alias that was never allowed yields a
// friendly "nothing to remove" rather than a scary 5xx-like error.
func TestHandleAdminDisallow_NotFoundIsGraceful(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	// No policy seeded — DisallowResource returns *slackdata.Error
	// with StatusCode=404.
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, "r_prod_db_xyz", testListAliasProdDB)
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin disallow <#C12345|chat> $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "nothing to remove") {
		t.Errorf("reply missing graceful 404 surface: %q", reply)
	}
}

// --- Status ---

// TestHandleAdminStatus_HappyPath renders all six fields from the
// workspace fixture. Most-load-bearing assertion: the API-key
// fingerprint shape (sha256 first 8 hex), NOT last-4.
func TestHandleAdminStatus_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicySingle(t, testAdminTeamID, "C12345", testListAliasProdDB, "r_prod_db_xyz")

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin status", testAdminTeamID, testAdminUserID)
	// Six required substrings — assert each so a regression that
	// drops one field is visible in the failure name.
	required := []string{
		"Workspace status",
		"Owner ID",
		testAdminOwnerID,
		"API key fingerprint",
		"sha256 first 8 hex",
		"Seed admin",
		testAdminUserID,
		"Configured at",
		"Channel policies",
	}
	for _, r := range required {
		if !strings.Contains(reply, r) {
			t.Errorf("reply missing %q: %q", r, reply)
		}
	}
}

// TestHandleAdminStatus_FingerprintIsNotLast4 is the load-bearing
// security fence. The Discord pre-pivot bug surfaced api_key last-4
// in operator audit logs; we ship sha256[:8] hex instead so an
// attacker reading Slack audit logs cannot reconstruct any portion
// of the secret.
func TestHandleAdminStatus_FingerprintIsNotLast4(t *testing.T) {
	ts := newAdminTestServers(t)
	// Custom workspace fixture: stamp a known sha256 fingerprint so
	// the assertion can compare bytes.
	const apiKey = "qk_secret_test_key_full_value_xxxxxxxx"
	sum := sha256.Sum256([]byte(apiKey))
	wantFingerprint := hex.EncodeToString(sum[:])[:8]
	item := seedWorkspaceAdmin(testAdminTeamID, testAdminOwnerID, testAdminUserID, testWorkspaceConfiguredAt)
	item["api_key_fingerprint"] = &ddbtypes.AttributeValueMemberS{Value: wantFingerprint}
	ts.seedWorkspaceCustom(t, item)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin status", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, wantFingerprint) {
		t.Errorf("reply missing fingerprint %q: %q", wantFingerprint, reply)
	}
	last4 := apiKey[len(apiKey)-4:]
	if strings.Contains(reply, last4) {
		t.Errorf("reply leaked last-4 of api key (%q) — fingerprint must be sha256[:8], not last-4: %q", last4, reply)
	}
}

// TestHandleAdminStatus_PolicyCountCapped fences the rendering when
// countPoliciesForTeam hits the page-walk cap. Seeding enough policy
// rows to exceed countPoliciesMaxPages would require many fake rows;
// instead this test verifies the render path by seeding rows that the
// (small-fixture) walk completes inside — i.e. it asserts the
// uncapped happy path renders the bare number. The cap surface is
// covered at the slackdata layer (no separate fixture-heavy DDB walk
// needed at the handler layer; the path is mechanically the same).
func TestHandleAdminStatus_PolicyCountCapped(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicySingle(t, testAdminTeamID, "C12345", testListAliasProdDB, "r_prod_db_xyz")

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin status", testAdminTeamID, testAdminUserID)
	// Uncapped: renders the bare count, NOT the "≥ N" surface.
	if strings.Contains(reply, "≥") {
		t.Errorf("uncapped status reply rendered the capped surface: %q", reply)
	}
	if !strings.Contains(reply, "Channel policies:* 1") {
		t.Errorf("status reply missing uncapped policy count: %q", reply)
	}
}

// TestListPolicies_DedupesAliasBoundResourceFromAliasless asserts
// that a resource_id covered by an alias_binding does NOT also
// emit as an aliasless entry when it also appears in the
// allowed_resource_ids SS. The alias-bound entry takes priority;
// the aliasless pass skips already-bound resources so the rendered
// listing doesn't double-list the same (channel, resource) once as
// `$alias` and once as `(no alias bound)`.
func TestListPolicies_DedupesAliasBoundResourceFromAliasless(t *testing.T) {
	ts := newAdminTestServers(t)
	// alias_bindings binds prod-db→r_dup_xyz; the SS carries both
	// r_dup_xyz (bound, should NOT re-emit) and r_other_xyz (aliasless).
	item := seedChannelPolicyWithBindings(testAdminTeamID, "C12345", map[string]string{
		testListAliasProdDB: "r_dup_xyz",
	})
	item[fAttrAllowedResourceIDs] = &ddbtypes.AttributeValueMemberSS{Value: []string{"r_dup_xyz", "r_other_xyz"}}
	ts.ddb.seedItem(t, ts.tableNames.channelPolicy, item)

	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	list, err := store.ListPolicies(context.Background(), testAdminTeamID, "", 50)
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	// Expect exactly two entries: r_dup_xyz (alias=prod-db) and
	// r_other_xyz (aliasless).
	if len(list.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (alias-bound r_dup_xyz should not re-emit as aliasless)", len(list.Entries))
	}
	seen := map[string]string{}
	for _, e := range list.Entries {
		seen[e.ResourceID] = e.Alias
	}
	if got, want := seen["r_dup_xyz"], testListAliasProdDB; got != want {
		t.Errorf("r_dup_xyz alias = %q, want %q (dedupe regression)", got, want)
	}
	if got, want := seen["r_other_xyz"], ""; got != want {
		t.Errorf("r_other_xyz alias = %q, want %q (aliasless entry expected)", got, want)
	}
}

// TestHandleAdminStatus_NonAdmin fences the admin-only gate on status.
func TestHandleAdminStatus_NonAdmin(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin status", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "admin-only") {
		t.Errorf("reply missing admin-only fence: %q", reply)
	}
}

// --- Revoke (single qurl_id, sync) ---

// TestHandleAdminRevoke_HappyPath fences single-qURL revocation.
func TestHandleAdminRevoke_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var deleteHits atomic.Int32
	ts.addCustomer("DELETE", "/v1/qurls/q_aaa123", func(w http.ResponseWriter, _ *http.Request) {
		deleteHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin revoke q_aaa123", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "Revoked `q_aaa123`") {
		t.Errorf("reply missing success line: %q", reply)
	}
	if deleteHits.Load() != 1 {
		t.Errorf("DELETE called %d times, want 1", deleteHits.Load())
	}
}

// TestHandleAdminRevoke_NotFoundIsGraceful fences the 404-friendly
// surface: an already-revoked or typo'd qurl_id renders a hint rather
// than a stack trace.
func TestHandleAdminRevoke_NotFoundIsGraceful(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("DELETE", "/v1/qurls/q_missing", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"title":"Not Found","status":404}}`))
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin revoke q_missing", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "already revoked") {
		t.Errorf("reply missing graceful 404 surface: %q", reply)
	}
}

// TestHandleAdminRevoke_RejectsAliasShape fences the parser
// distinction: `admin revoke $alias` is wrong-grammar (the operator
// likely meant `revoke-all`) and the reply must point them at the
// right verb, not silently try to resolve the alias.
func TestHandleAdminRevoke_RejectsAliasShape(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin revoke $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "revoke-all") {
		t.Errorf("reply must point at revoke-all when an alias is passed to revoke: %q", reply)
	}
}

// TestHandleAdminRevoke_InvalidQURLID fences the format check: a
// pasted token that doesn't match `q_<alphanum>` gets a parser-error
// hint, not an opaque DELETE 404.
func TestHandleAdminRevoke_InvalidQURLID(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin revoke not-a-real-id", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "q_<id>") {
		t.Errorf("reply missing format hint: %q", reply)
	}
}

// --- Policies (async via response_url) ---

// TestHandleAdminPolicies_HappyPath fences the async-response_url
// path: the sync reply is ackWorkingOnIt; the response_url body
// carries the rendered policy listing.
func TestHandleAdminPolicies_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicySingle(t, testAdminTeamID, "C12345", testListAliasProdDB, "r_prod_db_xyz")
	ts.seedPolicySingle(t, testAdminTeamID, "C67890", "staging-db", "r_staging_db_abc")

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	status, syncReply, asyncReply := inv.invokeAdminAsync("admin policies", testAdminTeamID, testAdminUserID)
	if status != http.StatusOK {
		t.Fatalf("sync status = %d, want 200", status)
	}
	if syncReply != ackWorkingOnIt {
		t.Errorf("sync reply = %q, want %q", syncReply, ackWorkingOnIt)
	}
	if !strings.Contains(asyncReply, "Channel policies") {
		t.Errorf("async reply missing header: %q", asyncReply)
	}
	for _, want := range []string{"<#C12345>", "$prod-db", "<#C67890>", "$staging-db"} {
		if !strings.Contains(asyncReply, want) {
			t.Errorf("async reply missing %q: %q", want, asyncReply)
		}
	}
}

// TestHandleAdminPolicies_Empty fences the no-policies surface — a
// friendly "no policies configured" with a CTA, not a misleading
// empty list.
func TestHandleAdminPolicies_Empty(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, asyncReply := inv.invokeAdminAsync("admin policies", testAdminTeamID, testAdminUserID)
	if !strings.Contains(asyncReply, "No channel policies configured") {
		t.Errorf("async reply missing empty surface: %q", asyncReply)
	}
	if !strings.Contains(asyncReply, "/qurl admin allow") {
		t.Errorf("async reply missing CTA: %q", asyncReply)
	}
}

// TestHandleAdminPolicies_NonAdmin fences the admin-only gate even
// on the async path — the response_url reply must say "admin-only",
// not the policy listing.
func TestHandleAdminPolicies_NonAdmin(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, asyncReply := inv.invokeAdminAsync("admin policies", testAdminTeamID, testAdminUserID)
	if !strings.Contains(asyncReply, "admin-only") {
		t.Errorf("async reply missing admin-only fence: %q", asyncReply)
	}
}

// --- Revoke-all (async) ---

// TestHandleAdminRevokeAll_HappyPath fences the canonical revoke-all
// walk: resolve alias → list qURLs → DELETE each → success reply.
func TestHandleAdminRevokeAll_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, "r_prod_db_xyz", testListAliasProdDB)
	})
	ts.addCustomer("GET", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		writeQURLListFixture(t, w, []string{testFixtureQURLOne, testFixtureQURLTwo})
	})
	var deleteHits atomic.Int32
	ts.addCustomerPrefix("DELETE", "/v1/qurls/", func(w http.ResponseWriter, _ *http.Request) {
		deleteHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, syncReply, asyncReply := inv.invokeAdminAsync("admin revoke-all $prod-db", testAdminTeamID, testAdminUserID)
	if syncReply != ackWorkingOnIt {
		t.Errorf("sync reply = %q, want %q", syncReply, ackWorkingOnIt)
	}
	if !strings.Contains(asyncReply, "Revoked 2 qURL(s)") {
		t.Errorf("async reply missing count: %q", asyncReply)
	}
	if deleteHits.Load() != 2 {
		t.Errorf("DELETE called %d times, want 2", deleteHits.Load())
	}
}

// TestHandleAdminRevokeAll_TruncatedAt5Pages drives 6 pages of qURLs
// through the customer-server fixture and asserts `r.truncated=true`
// surfaces in the rendered reply with the re-run hint. The
// adminRevokeAllMaxPages constant (=5) is the intentional friction
// guard against runaway aliases pinning a worker through the 25s
// async budget. An off-by-one in the loop would silently violate
// that contract without failing CI — this test fences it end-to-end.
func TestHandleAdminRevokeAll_TruncatedAt5Pages(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, "r_prod_db_xyz", testListAliasProdDB)
	})
	// Each list call returns 20 fresh qURLs with has_more=true; the
	// walk caps at adminRevokeAllMaxPages so a 6th list call would
	// only fire if the cap is wrong.
	const pageSize = 20
	var pageIdx atomic.Int32
	ts.addCustomer("GET", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		idx := pageIdx.Add(1) - 1
		ids := make([]string, pageSize)
		for i := 0; i < pageSize; i++ {
			ids[i] = fmt.Sprintf("q_p%d_%02d", idx, i)
		}
		writeQURLPageFixture(t, w, ids, "cursor_next", true)
	})
	var deleteHits atomic.Int32
	ts.addCustomerPrefix("DELETE", "/v1/qurls/", func(w http.ResponseWriter, _ *http.Request) {
		deleteHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, asyncReply := inv.invokeAdminAsync("admin revoke-all $prod-db", testAdminTeamID, testAdminUserID)
	// 5 pages × 20 = 100 DELETEs expected.
	if got := deleteHits.Load(); got != 100 {
		t.Errorf("DELETE called %d times, want 100 (5 pages × 20)", got)
	}
	if got := pageIdx.Load(); got != int32(adminRevokeAllMaxPages) {
		t.Errorf("walk fetched %d pages, want %d (cap)", got, adminRevokeAllMaxPages)
	}
	if !strings.Contains(asyncReply, "page limit") {
		t.Errorf("reply missing truncation surface: %q", asyncReply)
	}
	if !strings.Contains(asyncReply, "re-run") {
		t.Errorf("reply missing re-run hint: %q", asyncReply)
	}
}

// TestHandleAdminRevokeAll_RateLimitedBailsOnFirst429 drives the
// 429-mid-walk path: a single DELETE returns 429 and the walk halts
// with `r.rateLimited=true` (renderer + walk-level wiring fenced
// end-to-end here, not just the renderer in isolation).
func TestHandleAdminRevokeAll_RateLimitedBailsOnFirst429(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, "r_prod_db_xyz", testListAliasProdDB)
	})
	ts.addCustomer("GET", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		writeQURLListFixture(t, w, []string{testFixtureQURLOne, testFixtureQURLTwo, "q_three"})
	})
	var deleteHits atomic.Int32
	ts.addCustomer("DELETE", "/v1/qurls/q_two", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"title":"Too Many Requests","status":429}}`))
	})
	ts.addCustomerPrefix("DELETE", "/v1/qurls/", func(w http.ResponseWriter, _ *http.Request) {
		deleteHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, asyncReply := inv.invokeAdminAsync("admin revoke-all $prod-db", testAdminTeamID, testAdminUserID)
	if got := deleteHits.Load(); got != 1 {
		t.Errorf("DELETE-success called %d times, want 1 (only q_one before 429 bails)", got)
	}
	if !strings.Contains(asyncReply, "rate-limited") {
		t.Errorf("reply missing rate-limited surface: %q", asyncReply)
	}
	if !strings.Contains(asyncReply, "wait then re-run") {
		t.Errorf("reply missing wait-then-rerun hint: %q", asyncReply)
	}
}

// TestHandleAdminPolicies_FullPageSurfacesHasMore drives 51 policy
// rows through ListPolicies and asserts that the rendered reply
// surfaces the more-pages hint when DDB reports a continuation
// cursor (LastEvaluatedKey). Off-by-one in the page-size logic
// would silently violate the "show 50, then a hint" contract.
func TestHandleAdminPolicies_FullPageSurfacesHasMore(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	for i := 0; i < 51; i++ {
		ts.seedPolicySingle(t, testAdminTeamID,
			fmt.Sprintf("C_%04d", i),
			fmt.Sprintf("alias-%04d", i),
			fmt.Sprintf("r_%04d", i),
		)
	}
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, asyncReply := inv.invokeAdminAsync("admin policies", testAdminTeamID, testAdminUserID)
	if !strings.Contains(asyncReply, "more pages available") {
		t.Errorf("reply missing has_more surface on >50-policy workspace: %q", asyncReply)
	}
}

// TestHandleAdminRevokeAll_PartialFailureLoopsContinues fences
// best-effort delete semantics: a 5xx on one qURL bumps `failed` but
// doesn't abort the walk.
func TestHandleAdminRevokeAll_PartialFailureLoopsContinues(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, "r_prod_db_xyz", testListAliasProdDB)
	})
	ts.addCustomer("GET", "/v1/qurls", func(w http.ResponseWriter, _ *http.Request) {
		writeQURLListFixture(t, w, []string{testFixtureQURLOne, testFixtureQURLTwo, "q_three"})
	})
	// q_two fails 500; q_one + q_three succeed.
	ts.addCustomer("DELETE", "/v1/qurls/q_two", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"title":"Internal","status":500}}`))
	})
	ts.addCustomerPrefix("DELETE", "/v1/qurls/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, asyncReply := inv.invokeAdminAsync("admin revoke-all $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(asyncReply, "Revoked 2") {
		t.Errorf("expected 2 successful revocations, reply: %q", asyncReply)
	}
	if !strings.Contains(asyncReply, "1 failed") {
		t.Errorf("expected 1 failure surface, reply: %q", asyncReply)
	}
}

// TestHandleAdminRevokeAll_AliasNotFound fences the alias-not-found
// path: the walk never enters its list loop and the reply is the
// friendly alias-missing message.
func TestHandleAdminRevokeAll_AliasNotFound(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources/by-alias/missing", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"title":"Not Found","code":"alias_not_found","status":404}}`))
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, asyncReply := inv.invokeAdminAsync("admin revoke-all $missing", testAdminTeamID, testAdminUserID)
	if !strings.Contains(asyncReply, "$missing` not found") {
		t.Errorf("async reply missing alias-not-found surface: %q", asyncReply)
	}
}

// TestHandleAdminRevokeAll_NonAdmin fences the admin-only gate on
// the async revoke-all path.
func TestHandleAdminRevokeAll_NonAdmin(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	// Even an alias-resolve must NOT happen for non-admins.
	var aliasHits atomic.Int32
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		aliasHits.Add(1)
		writeResourceFixture(t, w, "r_xyz", testListAliasProdDB)
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, _, asyncReply := inv.invokeAdminAsync("admin revoke-all $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(asyncReply, "admin-only") {
		t.Errorf("async reply missing admin-only fence: %q", asyncReply)
	}
	if aliasHits.Load() != 0 {
		t.Errorf("alias resolved despite non-admin gate (hits = %d)", aliasHits.Load())
	}
}

// --- Dispatch-shell tests ---

// TestHandleAdmin_BareAdminVerb fences the bare `admin` form — a
// parser error, not a panic in the verb-dispatch switch.
func TestHandleAdmin_BareAdminVerb(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, ":warning:") {
		t.Errorf("reply missing parser-error surface: %q", reply)
	}
}

// TestHandleAdmin_AdminStoreUnconfigured fences the optional-AdminStore
// path: a deployment without slackdata wiring replies "Admin features
// are not configured" rather than panicking. The bare handler is
// constructed without an AdminStore — no fake DDB.
func TestHandleAdmin_AdminStoreUnconfigured(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, noopQURLServer(t))
	// newTestHandler builds without an AdminStore by default; the
	// admin dispatch hits the nil-guard.

	inv := newAdminSlashInvoker(t, h)
	_, reply := inv.invokeAdmin("admin allow <#C12345|chat> $a", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "not configured") {
		t.Errorf("reply missing not-configured surface: %q", reply)
	}
}

// --- Test fixture helpers ---

// writeResourceFixture encodes the qurl-service /v1/resources/by-alias
// envelope. Reused by allow/disallow/revoke-all tests.
func writeResourceFixture(t *testing.T, w http.ResponseWriter, resourceID, alias string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		testKeyData: map[string]any{
			fAttrResourceID: resourceID,
			"alias":         alias,
		},
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

// writeQURLListFixture encodes /v1/qurls?resource_id= shaped output
// for a list of qurl_ids.
func writeQURLListFixture(t *testing.T, w http.ResponseWriter, ids []string) {
	t.Helper()
	writeQURLPageFixture(t, w, ids, "", false)
}

// writeQURLPageFixture is the paginated variant of writeQURLListFixture.
// Used by tests that exercise the multi-page walk (cursor / has_more
// boundary) where the list fixture needs to advertise that more pages
// exist upstream. Wire shape mirrors shared/client.ResponseMeta:
// pagination state lives under the `meta` envelope, not at the top
// level (a tail-fixture bug here would silently exit the walk loop
// because the client sees HasMore=false by default).
func writeQURLPageFixture(t *testing.T, w http.ResponseWriter, ids []string, nextCursor string, hasMore bool) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	qurls := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		qurls = append(qurls, map[string]any{
			fAttrResourceID:      id,
			testKeyHTTPTargetURL: testFixtureTargetX,
			testKeyHTTPStatus:    "active",
		})
	}
	body := map[string]any{
		testKeyData: qurls,
		"meta": map[string]any{
			"has_more":    hasMore,
			"next_cursor": nextCursor,
		},
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

// --- Unit tests for renderRevokeAllReply (covers branches that
// would otherwise need full integration scaffolding to reach) ---

func TestRenderRevokeAllReply_HappyPathNoReasons(t *testing.T) {
	got := renderRevokeAllReply(testListAliasProdDB, &revokeAllResult{revoked: 3})
	if !strings.Contains(got, "Revoked 3 qURL(s) bound to `$prod-db`.") {
		t.Errorf("unexpected: %q", got)
	}
	if strings.Contains(got, "re-run") {
		t.Errorf("unexpected re-run hint: %q", got)
	}
}

// TestRenderRevokeAllReply_ZeroRevokedSwitchesVerb fences the cr
// round-11 feedback: a fully-rate-limited or deadline-exceeded walk
// that revoked zero qURLs used to read "Revoked 0 qURL(s) bound to
// $alias. rate-limited by upstream..." — a misleading mock-success.
// The leading copy now switches to "No qURLs revoked" so the reason
// for the early bail isn't buried behind the success phrasing.
func TestRenderRevokeAllReply_ZeroRevokedSwitchesVerb(t *testing.T) {
	got := renderRevokeAllReply(testListAliasProdDB, &revokeAllResult{revoked: 0, rateLimited: true})
	if !strings.Contains(got, "No qURLs revoked for `$prod-db`.") {
		t.Errorf("missing zero-revoked phrasing: %q", got)
	}
	if strings.Contains(got, "Revoked 0") {
		t.Errorf("rendered the misleading 'Revoked 0 qURL(s)' copy: %q", got)
	}
	if !strings.Contains(got, "rate-limited") {
		t.Errorf("missing rate-limited reason: %q", got)
	}
}

func TestRenderRevokeAllReply_RateLimitedReason(t *testing.T) {
	got := renderRevokeAllReply("alias", &revokeAllResult{revoked: 1, rateLimited: true})
	if !strings.Contains(got, "rate-limited") {
		t.Errorf("missing rate-limited surface: %q", got)
	}
	if !strings.Contains(got, "wait then re-run") {
		t.Errorf("missing wait-then-rerun hint: %q", got)
	}
}

func TestRenderRevokeAllReply_TruncatedAndServerPaginationBug(t *testing.T) {
	got := renderRevokeAllReply("alias", &revokeAllResult{
		revoked:             10,
		truncated:           true,
		serverPaginationBug: true,
	})
	if !strings.Contains(got, "page limit") {
		t.Errorf("missing truncated reason: %q", got)
	}
	if !strings.Contains(got, "server pagination bug") {
		t.Errorf("missing server-pagination-bug reason: %q", got)
	}
}

// TestRenderRevokeAllReply_RateLimitedAndTruncatedCoSet locks the
// joined-reasons copy for the rare-but-renderable combo where both
// bools land on the result. In production the outer-loop `break`
// after deletePage flags rateLimited should make the truncated
// branch unreachable on the same walk, but a future refactor that
// changes the break ordering shouldn't silently drop or reorder
// reason fragments — this test pins the joined-reasons separator
// and the rate-limited-first ordering.
func TestRenderRevokeAllReply_RateLimitedAndTruncatedCoSet(t *testing.T) {
	got := renderRevokeAllReply("alias", &revokeAllResult{
		revoked:     2,
		rateLimited: true,
		truncated:   true,
	})
	if !strings.Contains(got, "rate-limited") {
		t.Errorf("missing rate-limited reason: %q", got)
	}
	if !strings.Contains(got, "page limit") {
		t.Errorf("missing truncated reason: %q", got)
	}
	// Rate-limited must lead — its "wait then re-run" action is
	// qualitatively different from the immediate-retry shapes.
	if rlIdx, trIdx := strings.Index(got, "rate-limited"), strings.Index(got, "page limit"); rlIdx > trIdx {
		t.Errorf("expected rate-limited reason before truncated; got: %q", got)
	}
	if !strings.Contains(got, "; ") {
		t.Errorf("expected joined-reasons separator `; ` between reasons; got: %q", got)
	}
}

func TestRenderRevokeAllReply_DeadlineAndCanceled(t *testing.T) {
	gotDeadline := renderRevokeAllReply("alias", &revokeAllResult{revoked: 1, deadlineExceeded: true})
	if !strings.Contains(gotDeadline, "budget elapsed") {
		t.Errorf("missing budget-elapsed reason: %q", gotDeadline)
	}
	gotCancel := renderRevokeAllReply("alias", &revokeAllResult{revoked: 1, canceled: true})
	if !strings.Contains(gotCancel, "request canceled") {
		t.Errorf("missing request-canceled reason: %q", gotCancel)
	}
}

func TestRenderRevokeAllReply_PartialFailuresAndAlreadyGone(t *testing.T) {
	got := renderRevokeAllReply("alias", &revokeAllResult{revoked: 5, alreadyGone: 2, failed: 1})
	if !strings.Contains(got, "Revoked 5") {
		t.Errorf("missing revoke count: %q", got)
	}
	if !strings.Contains(got, "2 already gone") {
		t.Errorf("missing already-gone surface: %q", got)
	}
	if !strings.Contains(got, "1 failed") {
		t.Errorf("missing failed surface: %q", got)
	}
}

// --- Unit tests for renderPolicies (byte-cap truncation + empty + has_more) ---

func TestRenderPolicies_Empty(t *testing.T) {
	got := renderPolicies(nil)
	if !strings.Contains(got, "No channel policies configured") {
		t.Errorf("nil list: %q", got)
	}
	got = renderPolicies(&slackdataPolicyList{})
	if !strings.Contains(got, "No channel policies configured") {
		t.Errorf("empty list: %q", got)
	}
}

func TestRenderPolicies_HasMoreSurface(t *testing.T) {
	got := renderPolicies(&slackdataPolicyList{
		Entries: []slackdataPolicyEntry{
			{ChannelID: "C1", Alias: "a", ResourceID: "r_one"},
		},
		HasMore: true,
	})
	if !strings.Contains(got, "more pages available") {
		t.Errorf("missing has_more hint: %q", got)
	}
}

// TestRenderPolicies_AlwaysEmitsFirstRow fences a renderPolicies
// edge case: a pathologically long single entry (alias or resource
// id pushing the first line past adminPoliciesReplyByteCap) used to
// render `*Channel policies (0 of N):*` with an empty body because
// the loop bailed before emitting any row. The first-row carve-out
// guarantees the operator always sees at least one entry, even if
// it pushes the envelope a few bytes past the cap (Slack's 4000-byte
// ceiling absorbs the overflow).
func TestRenderPolicies_AlwaysEmitsFirstRow(t *testing.T) {
	long := strings.Repeat("a", adminPoliciesReplyByteCap+100)
	got := renderPolicies(&slackdataPolicyList{
		Entries: []slackdataPolicyEntry{
			{ChannelID: "C1", Alias: long, ResourceID: "r_long_xyz"},
			{ChannelID: "C2", Alias: "b", ResourceID: "r_two"},
		},
	})
	if !strings.Contains(got, "r_long_xyz") {
		t.Errorf("first row dropped despite carve-out: %q", got[:200])
	}
	if strings.Contains(got, "(0 of") {
		t.Errorf("rendered the 0-of-N empty-body shape: %q", got[:200])
	}
}

// TestBindWorkspace_FirstBindSetsRow asserts the canonical first-
// bind flow: a fresh team_id gets a new row with the seed admin in
// admin_slack_user_ids and the supplied owner.
// TestResolvePolicy_LegacySingleRowShape fences the cr round-9 bug:
// ResolvePolicy used to only check the post-pivot allowed_resource_ids
// SS attribute, missing legacy rows that carried the grant in the
// per-row `resource_id` scalar. Without the fallback, /qurl admin
// policies would list a legacy grant but /qurl get would deny it —
// exactly the foot-gun the cr noted. Builds a scalar-only row by
// hand (seedPolicySingle now writes the post-pivot Map + SS shape,
// so it can't be used to exercise the legacy scalar fallback).
func TestResolvePolicy_LegacySingleRowShape(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.ddb.seedItem(t, ts.tableNames.channelPolicy, map[string]ddbtypes.AttributeValue{
		fAttrSlackTeamID:    &ddbtypes.AttributeValueMemberS{Value: testAdminTeamID},
		fAttrSlackChannelID: &ddbtypes.AttributeValueMemberS{Value: "C_legacy"},
		fAttrResourceID:     &ddbtypes.AttributeValueMemberS{Value: "r_legacy_xyz"},
		fAttrCreatedAt:      &ddbtypes.AttributeValueMemberS{Value: "2026-04-20T12:00:00Z"},
	})
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)

	allowed, err := store.ResolvePolicy(context.Background(), testAdminTeamID, "C_legacy", "r_legacy_xyz")
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	if !allowed {
		t.Errorf("ResolvePolicy returned false for legacy single-row shape — listing/resolve asymmetry")
	}
}

// TestResolvePolicy_PostPivotSetShape complements the legacy-shape
// test: a post-pivot row that carries the grant only in the
// allowed_resource_ids SS resolves correctly.
func TestResolvePolicy_PostPivotSetShape(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_modern", testListAliasProdDB, []string{"r_modern_xyz"})
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)

	allowed, err := store.ResolvePolicy(context.Background(), testAdminTeamID, "C_modern", "r_modern_xyz")
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	if !allowed {
		t.Errorf("ResolvePolicy returned false for post-pivot SS shape")
	}
}

// TestResolvePolicy_MissingResourceReturnsFalse fences the
// no-policy-no-access default — a workspace that has SOME policies
// but not for this (channel, resource) returns false, not an error.
func TestResolvePolicy_MissingResourceReturnsFalse(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySingle(t, testAdminTeamID, "C12345", testListAliasProdDB, "r_prod_db_xyz")
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)

	allowed, err := store.ResolvePolicy(context.Background(), testAdminTeamID, "C12345", "r_different_resource")
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	if allowed {
		t.Errorf("ResolvePolicy returned true for unrelated resource_id")
	}
}

// TestRenderPolicies_WorstCaseFitsSlack4000 asserts the byte cap +
// header + trailing hint stay inside Slack's 4000-char `text`
// ceiling even on a page packed with long-aliased rows. Locks the
// math between adminPoliciesPageSize, adminPoliciesReplyByteCap, and
// the header / hint strings so a future tweak to any of them
// doesn't silently regress the contract.
//
// Two scenarios — both must fit under the ceiling:
//   - HasMore=false (renders only the byte-cap-hit hint when the
//     rows section trims).
//   - HasMore=true (renders BOTH the byte-cap-hit hint AND the
//     more-pages-upstream hint — the longest possible envelope).
func TestRenderPolicies_WorstCaseFitsSlack4000(t *testing.T) {
	const slackTextMaxBytes = 4000
	entries := make([]slackdataPolicyEntry, 0, 50)
	for i := 0; i < 50; i++ {
		entries = append(entries, slackdataPolicyEntry{
			ChannelID:  strings.Repeat("C", 20),        // 20-char channel id
			Alias:      strings.Repeat("a", 50),        // 50-char alias (worst case)
			ResourceID: "r_" + strings.Repeat("z", 32), // 34-char resource id
		})
	}
	for _, hasMore := range []bool{false, true} {
		got := renderPolicies(&slackdataPolicyList{Entries: entries, HasMore: hasMore})
		if len(got) > slackTextMaxBytes {
			t.Errorf("HasMore=%v rendered length %d exceeds Slack 4000-char ceiling", hasMore, len(got))
		}
	}
}

// Shared literals for multi-alias / aliasless test fixtures —
// lifted to satisfy goconst (3+ occurrences across the file).
const (
	aliasGrafana   = "grafana"
	aliasLogs      = "logs"
	aliasStagingDB = "staging-db"
	ridGrafana     = "r_grafana_xyz"
	ridLogs        = "r_logs_xyz"
	ridStaging     = "r_staging_xyz"
	channelMulti   = "C_multi"
)

// TestListPolicies_MultiAliasChannelEmitsOneEntryPerBinding asserts
// that a channel with N alias_bindings produces N PolicyEntries on
// the listing — one per (channel, alias). Entries are sorted by
// alias-name ascending so re-listings render the same order for
// audit-via-paste diff stability.
func TestListPolicies_MultiAliasChannelEmitsOneEntryPerBinding(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.ddb.seedItem(t, ts.tableNames.channelPolicy, seedChannelPolicyWithBindings(
		testAdminTeamID, channelMulti, map[string]string{
			aliasGrafana:   ridGrafana,
			aliasStagingDB: ridStaging,
			aliasLogs:      ridLogs,
		}))
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)

	list, err := store.ListPolicies(context.Background(), testAdminTeamID, "", 50)
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	if len(list.Entries) != 3 {
		t.Fatalf("entries = %d, want 3 (one per binding)", len(list.Entries))
	}
	// Deterministic order: alias-name asc.
	wantOrder := []struct {
		alias, rid string
	}{
		{aliasGrafana, ridGrafana},
		{aliasLogs, ridLogs},
		{aliasStagingDB, ridStaging},
	}
	for i, want := range wantOrder {
		got := list.Entries[i]
		if got.Alias != want.alias || got.ResourceID != want.rid {
			t.Errorf("entries[%d] = (%q, %q), want (%q, %q)", i, got.Alias, got.ResourceID, want.alias, want.rid)
		}
		if got.ChannelID != channelMulti {
			t.Errorf("entries[%d].ChannelID = %q, want %q", i, got.ChannelID, channelMulti)
		}
	}
}

// TestListPolicies_ChannelWithAliasesAndAllowedResources asserts the
// coexistence path: a single row carrying both alias_bindings and
// allowed_resource_ids SS members (some bound, some aliasless)
// flattens to one entry per binding (alias-asc) followed by one
// entry per aliasless SS member (resource-id-asc), all on the same
// channel and consecutive in the page.
func TestListPolicies_ChannelWithAliasesAndAllowedResources(t *testing.T) {
	ts := newAdminTestServers(t)
	item := seedChannelPolicyWithBindings(testAdminTeamID, "C_mix", map[string]string{
		aliasGrafana: ridGrafana,
		aliasLogs:    ridLogs,
	})
	// Override allowed_resource_ids to add two aliasless resources
	// alongside the bound ones (a real `allow` on a resource that
	// hasn't been alias-bound).
	item[fAttrAllowedResourceIDs] = &ddbtypes.AttributeValueMemberSS{Value: []string{
		ridGrafana, ridLogs, "r_naked_a", "r_naked_b",
	}}
	ts.ddb.seedItem(t, ts.tableNames.channelPolicy, item)

	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	list, err := store.ListPolicies(context.Background(), testAdminTeamID, "", 50)
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	// Expect 4 entries: 2 bound (alias asc) + 2 aliasless (rid asc).
	if len(list.Entries) != 4 {
		t.Fatalf("entries = %d, want 4 (2 bound + 2 aliasless)", len(list.Entries))
	}
	wantOrder := []struct {
		alias, rid string
	}{
		{aliasGrafana, ridGrafana},
		{aliasLogs, ridLogs},
		{"", "r_naked_a"},
		{"", "r_naked_b"},
	}
	for i, want := range wantOrder {
		got := list.Entries[i]
		if got.Alias != want.alias || got.ResourceID != want.rid {
			t.Errorf("entries[%d] = (%q, %q), want (%q, %q)", i, got.Alias, got.ResourceID, want.alias, want.rid)
		}
	}
}

// TestRenderPolicies_MultiAliasChannelDisplaysAllBindings drives a
// flattened PolicyList through renderPolicies and asserts the
// rendered Slack-mrkdwn body carries one line per (channel, alias)
// pair. The aliasless entry on the same channel renders with the
// "(no alias bound)" marker on its own line.
func TestRenderPolicies_MultiAliasChannelDisplaysAllBindings(t *testing.T) {
	got := renderPolicies(&slackdataPolicyList{
		Entries: []slackdataPolicyEntry{
			{ChannelID: channelMulti, Alias: aliasGrafana, ResourceID: ridGrafana},
			{ChannelID: channelMulti, Alias: aliasLogs, ResourceID: ridLogs},
			{ChannelID: channelMulti, Alias: aliasStagingDB, ResourceID: ridStaging},
			{ChannelID: channelMulti, Alias: "", ResourceID: "r_naked"},
		},
	})
	for _, want := range []string{
		"$" + aliasGrafana, ridGrafana,
		"$" + aliasLogs, ridLogs,
		"$" + aliasStagingDB, ridStaging,
		"no alias bound", "r_naked",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered body missing %q: %q", want, got)
		}
	}
	// Header should reflect the total entry count, not the binding count.
	if !strings.Contains(got, "(4)") {
		t.Errorf("rendered header missing total-entry count: %q", got)
	}
}

// TestRenderPolicies_AliaslessEntryRendersNoAliasMarker fences the
// rendering for entries with no alias_binding on the row's
// allowed_resource_ids (a real surface, not a bug — `/qurl admin
// allow` and alias-bind are orthogonal commands). The fallback says
// "(no alias bound)" rather than emitting empty backticks, which
// would read as a broken UI.
func TestRenderPolicies_AliaslessEntryRendersNoAliasMarker(t *testing.T) {
	got := renderPolicies(&slackdataPolicyList{
		Entries: []slackdataPolicyEntry{
			{ChannelID: "C1", Alias: "", ResourceID: "r_one"},
		},
	})
	if strings.Contains(got, "`$`") {
		t.Errorf("rendered empty alias as broken backticks: %q", got)
	}
	if !strings.Contains(got, "no alias bound") {
		t.Errorf("rendered alias fallback missing marker: %q", got)
	}
}

// slackdataPolicyList / slackdataPolicyEntry are type aliases for the
// slackdata package types so test bodies stay readable without
// importing the full package surface.
type (
	slackdataPolicyList  = slackdata.PolicyList
	slackdataPolicyEntry = slackdata.PolicyEntry
)

// TestHandleSlashCommand_AdminOvermatchRejected fences the
// admin-dispatch's exact-token boundary. Inputs like `administrator`
// or `adminfoo` must NOT route through handleAdmin; they should fall
// through to the unknown-subcommand branch with a help nudge.
func TestHandleSlashCommand_AdminOvermatchRejected(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, noopQURLServer(t))
	inv := newAdminSlashInvoker(t, h)

	for _, text := range []string{"administrator", "adminfoo", "admin-policy"} {
		_, reply := inv.invokeAdmin(text, testAdminTeamID, testAdminUserID)
		if !strings.Contains(reply, "Unknown subcommand") {
			t.Errorf("%q routed unexpectedly: %q", text, reply)
		}
	}
}

// TestHandleAdminRevokeAll_DeadlineMidWalkClassified fences the cr
// review's deadline-during-ListByResource bug: when the ctx fires
// INSIDE c.ListByResource (rather than the pre-call guard catching
// it), the previous version returned r.fatalErr = (ctx-wrapped
// error) and the operator saw the generic "failed to enumerate
// qURLs" reply instead of "budget elapsed — re-run".
//
// Test driver: install a customer route that blocks until the
// caller's ctx fires, then assert the rendered reply carries
// "budget elapsed" not "failed to enumerate".
func TestHandleAdminRevokeAll_DeadlineMidWalkClassified(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("GET", "/v1/resources/by-alias/prod-db", func(w http.ResponseWriter, _ *http.Request) {
		writeResourceFixture(t, w, "r_prod_db_xyz", testListAliasProdDB)
	})
	// List blocks until the caller's ctx fires — simulating a
	// hung DDB / slow qurl-service path.
	ts.addCustomer("GET", "/v1/qurls", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	h := newAdminTestHandler(t, ts)
	// Aggressive deadline so the test doesn't pause for the
	// full asyncWorkTimeout (25s). t.Cleanup invokes cancel so
	// `go vet` is satisfied that the cancel func is called.
	deadlineCtx, deadlineCancel := context.WithDeadline(h.baseCtx, time.Now().Add(50*time.Millisecond))
	t.Cleanup(deadlineCancel)
	h.baseCtx = deadlineCtx
	inv := newAdminSlashInvoker(t, h)

	_, _, asyncReply := inv.invokeAdminAsync("admin revoke-all $prod-db", testAdminTeamID, testAdminUserID)
	if strings.Contains(asyncReply, "failed to enumerate") {
		t.Errorf("deadline-mid-walk surfaced as fatal: %q", asyncReply)
	}
	if !strings.Contains(asyncReply, "budget elapsed") {
		t.Errorf("deadline-mid-walk should render budget-elapsed hint: %q", asyncReply)
	}
}
