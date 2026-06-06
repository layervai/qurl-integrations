package internal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// Test-local string constants — keeps the test file free of magic
// literals that would otherwise repeat across cases. Slack user IDs
// are uppercase alphanumeric (no underscore), so the fixtures here
// use that shape — the parser's userMentionPattern rejects the
// `U_admin`-style IDs the older test fixtures used.
const (
	testTargetUserID  = "UTARGET01"
	testTargetMention = "<@UTARGET01>"
	testOtherAdminID  = "UOTHER001"
	testAdminListCmd  = "admins"
)

// --- Add ---

// TestHandleAdminAdd_HappyPath fences the canonical add flow: admin
// gate → AddAdmin UpdateItem → success reply. The post-state
// assertion confirms the target lands on the admin set.
func TestHandleAdminAdd_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("add "+testTargetMention, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "Added <@"+testTargetUserID+">") {
		t.Errorf("reply missing success line: %q", reply)
	}
	if !ts.ddb.workspaceMappingHasAdmin(t, testAdminTeamID, testTargetUserID) {
		t.Error("AddAdmin did not write the workspace_mappings row")
	}
}

// TestHandleAdminAdd_AlreadyAdmin fences the 409-idempotent surface:
// adding a user who is already on the admin set renders a friendly
// "nothing to do" reply.
func TestHandleAdminAdd_AlreadyAdmin(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	// Seed the target onto the existing admin set so AddAdmin's
	// conditional `NOT contains(...)` fails immediately. AddAdmin's
	// ADD-on-SS is set-union, so the caller stays admin.
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	if err := store.AddAdmin(context.Background(), testAdminTeamID, testTargetUserID); err != nil {
		t.Fatalf("seed target as admin: %v", err)
	}

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("add "+testTargetMention, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "already an admin") {
		t.Errorf("reply missing idempotent surface: %q", reply)
	}
}

// TestHandleAdminAdd_WorkspaceNotBound fences the 404 surface at the
// slackdata layer: a pre-claim workspace returns ErrCodeWorkspaceNotBound.
//
// Coverage gap (known, not fixed): the handler-layer mapping of that
// store error to the user-visible "Workspace isn't bound — run
// `/qurl setup <email>` first" copy isn't fenced end-to-end here, because
// requireAdminSync short-circuits on the same missing row (CheckAdmin
// returns isAdmin=false → "admin-only" reply). The handler arm IS
// marked "unreachable in practice; kept for safety against gate
// refactors" — if a future refactor flips the gate to allow non-admins
// through, this test would still pin the store contract but the user
// copy could drift. Acceptable trade for not introducing an AdminStore
// interface just for this fence.
func TestHandleAdminAdd_WorkspaceNotBound(t *testing.T) {
	ts := newAdminTestServers(t)
	// No seedAdmin — the workspace_mappings row doesn't exist.
	// Direct-store dispatch bypasses the admin gate (which would
	// also fail) so we can assert the 404 mapping in isolation.
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	err := store.AddAdmin(context.Background(), testAdminTeamID, testTargetUserID)
	if err == nil {
		t.Fatal("AddAdmin against missing workspace returned nil")
	}
	var se *slackdata.Error
	if !errors.As(err, &se) {
		t.Fatalf("expected *slackdata.Error, got %T", err)
	}
	if se.StatusCode != http.StatusNotFound || se.Code != slackdata.ErrCodeWorkspaceNotBound {
		t.Errorf("status=%d code=%q, want 404 / %q", se.StatusCode, se.Code, slackdata.ErrCodeWorkspaceNotBound)
	}
}

// TestAddAdmin_DisambiguationCantConfirmMembership fences the
// ErrCodeAdminAddUnverified surface: when the conditional UpdateItem
// fires (CCFE) but the post-CCFE disambiguation read sees a workspace
// row without the target on admin_slack_user_ids, AddAdmin must NOT
// misreport "already an admin" — that copy would be misleading. The
// store layer returns a distinct unverified code; the handler renders
// a retry hint.
//
// Hits the seam by injecting a CCFE on UpdateItem and seeding a row
// without the target on the SS, so the disambig read returns the row
// with the target absent.
func TestAddAdmin_DisambiguationCantConfirmMembership(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t) // target NOT on admin_slack_user_ids
	ts.ddb.SetUpdateItemErr(ts.tableNames.workspace, &ddbtypes.ConditionalCheckFailedException{Message: stringPtr("injected CCFE")})

	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	err := store.AddAdmin(context.Background(), testAdminTeamID, testTargetUserID)
	if err == nil {
		t.Fatal("AddAdmin returned nil error; expected unverified surface")
	}
	var se *slackdata.Error
	if !errors.As(err, &se) {
		t.Fatalf("expected *slackdata.Error, got %T", err)
	}
	if se.StatusCode != http.StatusConflict || se.Code != slackdata.ErrCodeAdminAddUnverified {
		t.Errorf("status=%d code=%q, want 409 / %q", se.StatusCode, se.Code, slackdata.ErrCodeAdminAddUnverified)
	}
}

// stringPtr is a tiny test helper for taking the address of a string
// literal (DDB SDK exception types use pointer-string fields).
func stringPtr(s string) *string { return &s }

// TestHandleAdminAdd_Unverified fences the end-to-end handler
// mapping for ErrCodeAdminAddUnverified. The store-layer fence
// (TestAddAdmin_DisambiguationCantConfirmMembership) pins the
// slackdata contract; this test pins the user-visible "couldn't
// confirm — please retry" reply that handleAdminAdd renders.
func TestHandleAdminAdd_Unverified(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t) // target NOT on admin_slack_user_ids
	// Inject a CCFE on the workspace UpdateItem so the
	// disambiguation read sees the row WITHOUT the target on the
	// SS — surfaces as ErrCodeAdminAddUnverified.
	ts.ddb.SetUpdateItemErr(ts.tableNames.workspace, &ddbtypes.ConditionalCheckFailedException{Message: stringPtr("injected CCFE")})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("add "+testTargetMention, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "couldn't confirm admin add") {
		t.Errorf("reply missing unverified-retry surface: %q", reply)
	}
}

// TestHandleAdminAdd_NonAdminCaller fences the admin-only gate on
// add. A non-admin caller is denied before any mutation.
func TestHandleAdminAdd_NonAdminCaller(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	ts.failOnAdminMutation(t, "non-admin should be gated before AddAdmin")

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("add "+testTargetMention, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "admin-only") {
		t.Errorf("reply missing admin-only fence: %q", reply)
	}
}

// TestHandleAdminAdd_SelfAdd fences the explicit self-add reply: an
// admin who runs `/qurl admin add @themselves` sees "you're already
// an admin" rather than the indirect "<@self> is already an admin"
// surface that the 409 idempotent path would render.
func TestHandleAdminAdd_SelfAdd(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.failOnAdminMutation(t, "self-add should bail before AddAdmin")

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("add <@"+testAdminUserID+">", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "You're already an admin") {
		t.Errorf("reply missing self-add surface: %q", reply)
	}
}

// TestHandleAdminAdd_InvalidMention fences the parser path: a
// missing or malformed `<@U…>` mention surfaces as a parser error
// without reaching AddAdmin.
func TestHandleAdminAdd_InvalidMention(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.failOnAdminMutation(t, "invalid mention should bail before AddAdmin")

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	for _, text := range []string{"add", "add someone", "add @someone"} {
		_, reply := inv.invokeAdmin(text, testAdminTeamID, testAdminUserID)
		if !strings.Contains(reply, ":warning:") {
			t.Errorf("%q: reply missing parser-error surface: %q", text, reply)
		}
	}
}

// --- Remove ---

// TestHandleAdminRemove_HappyPath fences the canonical remove flow:
// admin gate → RemoveAdmin UpdateItem → success reply. The
// post-state assertion confirms the target is no longer on the admin
// set.
func TestHandleAdminRemove_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	// Add a second admin so removing them leaves the caller as the
	// only admin (avoids tangling with the owner-check fence).
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	if err := store.AddAdmin(context.Background(), testAdminTeamID, testOtherAdminID); err != nil {
		t.Fatalf("seed other admin: %v", err)
	}

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("remove <@"+testOtherAdminID+">", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "Removed <@"+testOtherAdminID+">") {
		t.Errorf("reply missing success line: %q", reply)
	}
	if ts.ddb.workspaceMappingHasAdmin(t, testAdminTeamID, testOtherAdminID) {
		t.Error("RemoveAdmin did not strip the target from admin_slack_user_ids")
	}
}

// TestHandleAdminRemove_NotAdmin fences the 404-idempotent surface:
// removing a user who isn't on the admin set renders a friendly
// "nothing to do" reply.
func TestHandleAdminRemove_NotAdmin(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	// testTargetUserID is NOT on the admin set.

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("remove "+testTargetMention, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "isn't an admin") {
		t.Errorf("reply missing idempotent surface: %q", reply)
	}
}

// TestHandleAdminRemove_SelfRemoveRefused fences the self-remove
// guard: a fat-fingered admin who tries to demote themselves sees a
// clear "ask another admin" copy.
func TestHandleAdminRemove_SelfRemoveRefused(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.failOnAdminMutation(t, "self-remove should bail before RemoveAdmin")

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("remove <@"+testAdminUserID+">", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "can't remove yourself") {
		t.Errorf("reply missing self-remove guard: %q", reply)
	}
}

// TestHandleAdminRemove_OwnerRemoveRefused fences the owner-check
// guard: an admin trying to demote the workspace owner sees a clear
// "transfer via OAuth re-install" copy.
//
// Default seedAdmin makes the owner `testAdminOwnerID` (UOWNER001)
// while the calling admin is `testAdminUserID` (UADMIN001) —
// distinct users — so the caller can target the owner without
// tripping the self-remove guard.
func TestHandleAdminRemove_OwnerRemoveRefused(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.failOnAdminMutation(t, "owner-remove should bail before the UpdateItem (only GetItem fires)")

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("remove <@"+testAdminOwnerID+">", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "connected qURL to this workspace") {
		t.Errorf("reply missing owner-remove guard: %q", reply)
	}
}

// TestHandleAdminRemove_NonAdminCaller fences the admin-only gate
// on remove. A non-admin caller is denied before any mutation.
func TestHandleAdminRemove_NonAdminCaller(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	ts.failOnAdminMutation(t, "non-admin should be gated before RemoveAdmin")

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("remove "+testTargetMention, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "admin-only") {
		t.Errorf("reply missing admin-only fence: %q", reply)
	}
}

// --- List ---

// TestHandleAdminList_HappyPath fences the canonical list flow: the
// reply renders the owner on its own line and any extra admins on a
// joined `Admins:` line.
func TestHandleAdminList_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	if err := store.AddAdmin(context.Background(), testAdminTeamID, testOtherAdminID); err != nil {
		t.Fatalf("seed other admin: %v", err)
	}

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin(testAdminListCmd, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "Owner (connected qURL): <@"+testAdminOwnerID+">") {
		t.Errorf("reply missing owner line: %q", reply)
	}
	if !strings.Contains(reply, "Admins:") {
		t.Errorf("reply missing admins line: %q", reply)
	}
	// Both seeded admins should appear on the line (sorted by user ID).
	for _, want := range []string{"<@" + testAdminUserID + ">", "<@" + testOtherAdminID + ">"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing admin %q: %q", want, reply)
		}
	}
	// Sort-order fence: ListAdmins documents sort.Strings(adminIDs).
	// UADMIN001 < UOTHER001 ascending, so the admin mention must
	// appear before the other one in the rendered reply. Pins the
	// audit-paste-determinism contract.
	adminIdx := strings.Index(reply, "<@"+testAdminUserID+">")
	otherIdx := strings.Index(reply, "<@"+testOtherAdminID+">")
	if adminIdx > otherIdx {
		t.Errorf("admins not in sorted order: %q (adminIdx=%d, otherIdx=%d)", reply, adminIdx, otherIdx)
	}
}

// TestHandleAdminList_OwnerOnAdminSet fences the owner-filter path
// when the owner is on the admin set alongside other admins. The
// rendered "Admins:" line must NOT include the owner (filtered out
// to avoid `Owner: <@X>\nAdmins: <@X>, <@Y>, <@Z>` duplication).
func TestHandleAdminList_OwnerOnAdminSet(t *testing.T) {
	ts := newAdminTestServers(t)
	// Seed with the owner as one of the admins (the canonical post-
	// BindWorkspace shape), plus two additional admins.
	ts.seedWorkspace(t, testAdminTeamID, testAdminOwnerID, testAdminOwnerID, testWorkspaceConfiguredAt)
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	if err := store.AddAdmin(context.Background(), testAdminTeamID, testAdminUserID); err != nil {
		t.Fatalf("seed admin 1: %v", err)
	}
	if err := store.AddAdmin(context.Background(), testAdminTeamID, testOtherAdminID); err != nil {
		t.Fatalf("seed admin 2: %v", err)
	}

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin(testAdminListCmd, testAdminTeamID, testAdminOwnerID)
	if !strings.Contains(reply, "Owner (connected qURL): <@"+testAdminOwnerID+">") {
		t.Errorf("reply missing owner line: %q", reply)
	}
	// Both non-owner admins must appear on the Admins line.
	for _, a := range []string{testAdminUserID, testOtherAdminID} {
		if !strings.Contains(reply, "<@"+a+">") {
			t.Errorf("reply missing admin <@%s>: %q", a, reply)
		}
	}
	// Owner mention must NOT appear on the "Admins:" line — locate
	// the line and assert.
	for _, line := range strings.Split(reply, "\n") {
		if strings.HasPrefix(line, "Admins:") && strings.Contains(line, "<@"+testAdminOwnerID+">") {
			t.Errorf("owner duplicated on Admins line: %q", line)
		}
	}
}

// TestHandleAdminList_OwnerOnly fences the single-admin variant: a
// workspace with no admins beyond the owner renders the owner line
// only (no redundant "Admins: <@owner>" follow-up).
func TestHandleAdminList_OwnerOnly(t *testing.T) {
	ts := newAdminTestServers(t)
	// Seed a workspace where the owner IS the only admin — a
	// fresh-claim workspace looks like this.
	ts.seedWorkspace(t, testAdminTeamID, testAdminOwnerID, testAdminOwnerID, testWorkspaceConfiguredAt)
	// The slash-command caller needs to be on the admin set too;
	// seedWorkspace put the owner there, so use the owner as caller.
	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin(testAdminListCmd, testAdminTeamID, testAdminOwnerID)
	if !strings.Contains(reply, "Owner (connected qURL): <@"+testAdminOwnerID+">") {
		t.Errorf("reply missing owner line: %q", reply)
	}
	if strings.Contains(reply, "Admins:") {
		t.Errorf("owner-only workspace rendered redundant Admins line: %q", reply)
	}
}

// TestHandleAdminList_EmptyOwnerCorruption fences the
// storage-corruption render path: when workspace_mappings is
// missing owner_id (impossible today via readStringSet but
// defensive against a future contract change), the reply renders
// the explicit "(unknown — the qURL setup record is missing;
// contact support)" copy instead of a malformed `Owner: <@>`
// mrkdwn link. User-visible copy omits the internal table name;
// the slog.Error carries it for triage.
func TestHandleAdminList_EmptyOwnerCorruption(t *testing.T) {
	ts := newAdminTestServers(t)
	// Seed a workspace_mappings row with admin_slack_user_ids set
	// but owner_id ABSENT. Bypasses seedWorkspace (which stamps
	// owner_id) by writing directly with ddbtypes.
	row := map[string]ddbtypes.AttributeValue{
		fAttrSlackTeamID:       stringMember(testAdminTeamID),
		fAttrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{testAdminUserID}},
		fAttrCreatedAt:         stringMember(testWorkspaceConfiguredAt.UTC().Format(time.RFC3339)),
	}
	ts.ddb.seedItem(t, ts.tableNames.workspace, row)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin(testAdminListCmd, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "qURL setup record is missing") {
		t.Errorf("reply missing storage-corruption surface: %q", reply)
	}
	if strings.Contains(reply, "Owner: <@>") {
		t.Errorf("reply rendered malformed `Owner: <@>` mrkdwn: %q", reply)
	}
}

// TestHandleAdminList_WorkspaceNotBound fences the 404 surface at
// the slackdata layer. The handler path can't easily hit this
// because the admin gate would deny first (no row → CheckAdmin
// returns isAdmin=false), so the direct-store call is the cleanest
// way to fence the contract.
func TestHandleAdminList_WorkspaceNotBound(t *testing.T) {
	ts := newAdminTestServers(t)
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	_, _, err := store.ListAdmins(context.Background(), testAdminTeamID)
	if err == nil {
		t.Fatal("ListAdmins against missing workspace returned nil")
	}
	var se *slackdata.Error
	if !errors.As(err, &se) {
		t.Fatalf("expected *slackdata.Error, got %T", err)
	}
	if se.StatusCode != http.StatusNotFound || se.Code != slackdata.ErrCodeWorkspaceNotBound {
		t.Errorf("status=%d code=%q, want 404 / %q", se.StatusCode, se.Code, slackdata.ErrCodeWorkspaceNotBound)
	}
}

// TestHandleAdminList_NonAdminCaller fences the admin-only gate on
// list. The reply must say "admin-only", not surface the admin
// listing to a non-admin caller.
func TestHandleAdminList_NonAdminCaller(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin(testAdminListCmd, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "admin-only") {
		t.Errorf("reply missing admin-only fence: %q", reply)
	}
}

// --- Concurrent AddAdmin race fence ---

// TestAddAdmin_Concurrent fences the TOCTOU posture of
// [slackdata.Store.AddAdmin]. The conditional UpdateItem
// `NOT contains(admin_slack_user_ids, :uid)` folds the membership
// check into the same DDB item-lock as the ADD mutation, so N racers
// adding the same target produce one success and N-1 deterministic
// 409s — never a flake or double-add. Without the conditional, two
// racers could both observe "not present" on a probe and both render
// "added" while only one actually flipped state.
//
// We hit the slackdata.Store directly (no handler/HTTP shim) so the
// failure mode is precisely localized to the admin-set mutation path.
//
// Note on test fidelity: the fakeDDB serializes all ops under a
// single mutex, so the actual race window is collapsed at the
// fake-storage layer. The test's load-bearing assertion is the
// (1 success + N-1 deterministic 409s) contract, which the
// fake-mutex implementation honors by sequencing the conditional
// UpdateItem semantics in DDB-spec order. A future fine-grained
// fake-DDB lock would exercise a different code path — adjust the
// expectations if it ever lands.
func TestAddAdmin_Concurrent(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)

	const racers = 8

	results := make(chan error, racers)
	start := make(chan struct{})
	for range racers {
		go func() {
			<-start
			results <- store.AddAdmin(context.Background(), testAdminTeamID, testTargetUserID)
		}()
	}
	close(start)

	var (
		successes  int
		conflicts  int
		unexpected []error
	)
	for range racers {
		err := <-results
		if err == nil {
			successes++
			continue
		}
		var se *slackdata.Error
		if errors.As(err, &se) && se.StatusCode == http.StatusConflict && se.Code == slackdata.ErrCodeAdminAlreadyExists {
			conflicts++
			continue
		}
		unexpected = append(unexpected, err)
	}
	if len(unexpected) > 0 {
		t.Fatalf("unexpected error(s) from concurrent AddAdmin: %v", unexpected)
	}
	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1 (TOCTOU regression)", successes)
	}
	if conflicts != racers-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, racers-1)
	}
	if !ts.ddb.workspaceMappingHasAdmin(t, testAdminTeamID, testTargetUserID) {
		t.Error("admin set didn't carry the target after concurrent AddAdmin")
	}
}

// TestRemoveAdmin_Concurrent is the symmetric fence to
// [TestAddAdmin_Concurrent] for the `contains(...)` half of the
// conditional UpdateItem shape. N racers each attempt to remove the
// same target; exactly one observes the member-present condition
// (success), the rest see a CCFE classified as admin_not_found (404,
// idempotent). Failure mode is benign — both racers can race-to-DELETE
// without a double-delete because DDB's conditional UpdateItem is
// item-locked — but pinning the assertion guards against a future
// refactor of the condition shape.
func TestRemoveAdmin_Concurrent(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	// Seed the target onto the admin set so RemoveAdmin's
	// `contains(admin_slack_user_ids, :uid)` finds them on first read.
	if err := store.AddAdmin(context.Background(), testAdminTeamID, testTargetUserID); err != nil {
		t.Fatalf("seed target as admin: %v", err)
	}

	const racers = 8

	results := make(chan error, racers)
	start := make(chan struct{})
	for range racers {
		go func() {
			<-start
			results <- store.RemoveAdmin(context.Background(), testAdminTeamID, testTargetUserID)
		}()
	}
	close(start)

	var (
		successes  int
		notFounds  int
		unexpected []error
	)
	for range racers {
		err := <-results
		if err == nil {
			successes++
			continue
		}
		var se *slackdata.Error
		if errors.As(err, &se) && se.StatusCode == http.StatusNotFound && se.Code == slackdata.ErrCodeAdminNotFound {
			notFounds++
			continue
		}
		unexpected = append(unexpected, err)
	}
	if len(unexpected) > 0 {
		t.Fatalf("unexpected error(s) from concurrent RemoveAdmin: %v", unexpected)
	}
	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1 (TOCTOU regression)", successes)
	}
	if notFounds != racers-1 {
		t.Errorf("not-founds = %d, want %d", notFounds, racers-1)
	}
	if ts.ddb.workspaceMappingHasAdmin(t, testAdminTeamID, testTargetUserID) {
		t.Error("admin set still carries the target after concurrent RemoveAdmin")
	}
}

// --- Dispatch-shell tests ---

// TestHandleAdmin_LegacyAdminPrefixRedirects fences the deprecated `admin
// <verb>` prefix: bare `admin` and `admin <verb> ...` both get a one-line
// redirect pointing at the flat verbs (the `admin` word is redundant on an
// already-admin command), not a panic in the verb-dispatch switch.
func TestHandleAdmin_LegacyAdminPrefixRedirects(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	for _, text := range []string{"admin", "admin add <@U12345678>"} {
		_, reply := inv.invokeAdmin(text, testAdminTeamID, testAdminUserID)
		if !strings.Contains(reply, "isn't needed") {
			t.Errorf("%q: reply missing the prefix-deprecation redirect: %q", text, reply)
		}
		// The redirect names the flat verbs so the user learns the new grammar.
		for _, want := range []string{"add @user", "remove @user", "admins", "revoke $<id>"} {
			if !strings.Contains(reply, want) {
				t.Errorf("%q: redirect missing flat verb %q: %q", text, want, reply)
			}
		}
	}
}

// TestHandleAdmin_AdminStoreUnconfigured fences the optional-AdminStore
// path: a deployment without slackdata wiring replies "Admin features
// are not configured" rather than panicking.
func TestHandleAdmin_AdminStoreUnconfigured(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, noopQURLServer(t))
	// newTestHandler builds without an AdminStore by default; the
	// admin dispatch hits the nil-guard.

	inv := newAdminSlashInvoker(t, h)
	_, reply := inv.invokeAdmin(testAdminListCmd, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "not configured") {
		t.Errorf("reply missing not-configured surface: %q", reply)
	}
}

// TestAdminHelpReflectsFlatVerbs pins the command-cleanup contract on the
// `/qurl-admin help` text: no redundant "(admin only)" labels; the membership
// + revoke verbs are flat (no `admin` sub-word); listing admins is `admins`;
// revoke is resource-scoped via `$<id>`; and id references carry the `$`
// sigil. newAliasTestHandler wires both aliasStore and AdminStore, so every
// gated help line renders.
// TestAdminHelpGroupsVerbsUnderSections fences the categorized layout: the
// admin help renders its verbs under the four bold section headers instead of
// one flat bullet list. newAliasTestHandler wires aliasStore + AdminStore, so
// every section's gate passes and all four headers render. A regression that
// dropped a header (or flattened the grouping) fails here.
func TestAdminHelpGroupsVerbsUnderSections(t *testing.T) {
	h, _ := newAliasTestHandler(t)
	help := h.adminHelpMessage(commandAdmin)

	for _, want := range []string{
		"*Protect resources*",
		"*Aliases*",
		"*Manage resources*",
		"*Bot admins*",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("admin help missing section header %q:\n%s", want, help)
		}
	}
}

// TestAdminHelpOmitsSectionHeadersWhenUnwired fences the "never render an empty
// header" invariant the adminHelpMessage section comments lean on: with neither
// aliasStore nor AdminStore wired, none of the four section headers appear — a
// no-store deploy renders only the title and the always-present help anchor.
// newTestHandler wires neither store, so it exercises that path directly.
func TestAdminHelpOmitsSectionHeadersWhenUnwired(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	help := h.adminHelpMessage(commandAdmin)

	for _, absent := range []string{
		"*Protect resources*",
		"*Aliases*",
		"*Manage resources*",
		"*Bot admins*",
	} {
		if strings.Contains(help, absent) {
			t.Errorf("unwired admin help leaked section header %q:\n%s", absent, help)
		}
	}
}

func TestAdminHelpReflectsFlatVerbs(t *testing.T) {
	h, _ := newAliasTestHandler(t)
	help := h.adminHelpMessage(commandAdmin)

	if strings.Contains(help, "(admin only)") {
		t.Errorf("admin help still carries the redundant (admin only) label:\n%s", help)
	}
	for _, want := range []string{
		"/qurl-admin add @user",
		"/qurl-admin remove @user",
		"/qurl-admin admins",
		"/qurl-admin revoke $<id>",
		"/qurl-admin set-display-name $<id>",
		"/qurl-admin unset-display-name $<id>",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("admin help missing %q:\n%s", want, help)
		}
	}
	// The old `<cmd> admin <verb>` membership grammar and the per-link
	// `qurl_id` revoke must be gone. Match the precise old forms — a bare
	// "admin add" substring would false-positive on "/qurl-admin add" (and
	// "admin admin" on "/qurl-admin admins").
	for _, gone := range []string{
		"/qurl-admin admin add",
		"/qurl-admin admin remove",
		"/qurl-admin admin list",
		"/qurl-admin admin revoke",
		"qurl_id",
	} {
		if strings.Contains(help, gone) {
			t.Errorf("admin help still references removed grammar %q:\n%s", gone, help)
		}
	}
}

// TestResolvePolicy_LegacySingleRowShape fences the legacy single-row
// shape compatibility: ResolvePolicy must read the `resource_id`
// scalar (pre-pivot row shape) in addition to the post-pivot SS.
// Without the fallback, a hand-seeded legacy row would not resolve at
// /qurl get even though the legacy data is still on the table.
func TestResolvePolicy_LegacySingleRowShape(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.ddb.seedItem(t, ts.tableNames.channelPolicy, map[string]ddbtypes.AttributeValue{
		fAttrSlackTeamID:    stringMember(testAdminTeamID),
		fAttrSlackChannelID: stringMember("C_legacy"),
		fAttrResourceID:     stringMember("r_legacyxyz1"),
		fAttrCreatedAt:      stringMember("2026-04-20T12:00:00Z"),
	})
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)

	allowed, err := store.ResolvePolicy(context.Background(), testAdminTeamID, "C_legacy", "r_legacyxyz1")
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	if !allowed {
		t.Errorf("ResolvePolicy returned false for legacy single-row shape")
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
	ts.seedPolicyDualShape(t, testAdminTeamID, "C12345", testListAliasProdDB, "r_prod_db_xyz")
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)

	allowed, err := store.ResolvePolicy(context.Background(), testAdminTeamID, "C12345", "r_different_resource")
	if err != nil {
		t.Fatalf("ResolvePolicy: %v", err)
	}
	if allowed {
		t.Errorf("ResolvePolicy returned true for unrelated resource_id")
	}
}

// TestHandleSlashCommand_AdminOvermatchRejected fences the
// admin-dispatch's exact-token boundary. Inputs like `administrator`
// or `adminfoo` (on `/qurl-admin`) must NOT route through handleAdmin;
// they should fall through to the unknown-admin-subcommand branch with a
// help nudge.
func TestHandleSlashCommand_AdminOvermatchRejected(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, noopQURLServer(t))
	inv := newAdminSlashInvoker(t, h)

	for _, text := range []string{"administrator", "adminfoo", "admin-policy"} {
		_, reply := inv.invokeAdmin(text, testAdminTeamID, testAdminUserID)
		if !strings.Contains(reply, "Unknown admin subcommand") {
			t.Errorf("%q routed unexpectedly: %q", text, reply)
		}
	}
}

// TestLooksLikeSlackUserID_MatchesUserMentionPattern pins the bounds
// contract between the handler-side `looksLikeSlackUserID` defensive
// guard and the parser-side `userMentionPattern`. Both gates have to
// agree: a value rejected by the parser (write path) must also be
// rejected by the handler (read path), otherwise an admin write
// stops at parse time but the corresponding render breaks out of
// the mention surface. Drift in either direction is silent without
// this fence.
func TestLooksLikeSlackUserID_MatchesUserMentionPattern(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		id   string
	}{
		{"floor-accept (U + 8)", "U" + strings.Repeat("A", 8)},
		{"ceiling-accept (U + 63)", "U" + strings.Repeat("A", 63)},
		{"floor-accept (W + 8)", "W" + strings.Repeat("A", 8)},
		{"too-short (U + 7)", "U" + strings.Repeat("A", 7)},
		{"too-long (U + 64)", "U" + strings.Repeat("A", 64)},
		{"non-UW prefix", "A" + strings.Repeat("A", 8)},
		{"lowercase suffix", "Uabcdefgh"},
		{"empty", ""},
		{"one char", "U"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handlerOK := looksLikeSlackUserID(tc.id)
			parserOK := userMentionPattern.MatchString("<@" + tc.id + ">")
			if handlerOK != parserOK {
				t.Errorf("drift on %q: looksLikeSlackUserID=%v, userMentionPattern=%v", tc.id, handlerOK, parserOK)
			}
		})
	}
}

// TestHandleSetup_OwnerGate exercises the slash-command-level owner
// gate on `/qurl setup <email>`. Three branches: fresh install (no workspace
// row → anyone allowed), owner reruns setup (idempotent → URL minted),
// non-owner attempts setup (refuse with friendly copy mentioning the
// owner).
//
// AdminStore-backed: the gate calls AdminStore.CheckAdmin to read the
// stored owner_id. Uses newAdminTestServers + newAdminTestHandler so
// the underlying DDB row reflects what the handler reads.
func TestHandleSetup_OwnerGate(t *testing.T) {
	const (
		// Use the test-suite constants from admin_test_helpers_test.go.
		owner    = testAdminOwnerID // UOWNER001 (matches looksLikeSlackUserID).
		stranger = "USTRANGER000"   // Different Slack user — non-owner caller.
		team     = testAdminTeamID  // T_team
	)
	const slackBaseURL = "https://slack-bot.example"
	stateSecret := []byte("0123456789abcdef0123456789abcdef") // 32 bytes.

	wireSetup := func(t *testing.T, h *Handler) {
		t.Helper()
		h.SetOAuthSetup(oauth.SetupConfig{
			StateSecret:  stateSecret,
			SlackBaseURL: slackBaseURL,
		})
	}

	invokeSetup := func(t *testing.T, h *Handler, userID string) string {
		t.Helper()
		body := url.Values{
			fieldCommand: {testSlashCmd},
			fieldText:    {setupAdminExampleText},
			fieldTeamID:  {team},
			fieldUserID:  {userID},
		}.Encode()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
		if w.Code != http.StatusOK {
			t.Fatalf("/qurl setup <email> status: %d body=%s", w.Code, w.Body.String())
		}
		return parseSlackText(t, w.Body.Bytes())
	}

	t.Run("fresh install: no workspace row → anyone allowed", func(t *testing.T) {
		ts := newAdminTestServers(t)
		// No seedAdmin / seedWorkspace — workspace_mappings table is
		// empty. CheckAdmin returns ("", "", nil); the gate falls
		// through to mint.
		h := newAdminTestHandler(t, ts)
		wireSetup(t, h)

		got := invokeSetup(t, h, stranger)
		if !strings.Contains(got, "/oauth/qurl/start?state=") {
			t.Errorf("fresh install: expected setup URL, got: %q", got)
		}
	})

	t.Run("AdminStore nil (sandbox/no-DDB): owner gate skipped, setup URL minted", func(t *testing.T) {
		ts := newAdminTestServers(t)
		h := newAdminTestHandler(t, ts)
		wireSetup(t, h)
		// Sandbox / no-DDB posture: with AdminStore unset the owner gate
		// is skipped entirely and /setup mints unconditionally, same as a
		// fresh install. Null the store after construction to exercise that
		// short-circuit (mirrors the sandbox cmd/main.go wiring). Even a
		// non-owner (stranger) must get a URL since the gate never runs.
		h.cfg.AdminStore = nil

		got := invokeSetup(t, h, stranger)
		if !strings.Contains(got, "/oauth/qurl/start?state=") {
			t.Errorf("AdminStore nil: expected setup URL (owner gate must be skipped), got: %q", got)
		}
	})

	t.Run("owner reruns setup (owner on the admin set): setup URL minted", func(t *testing.T) {
		ts := newAdminTestServers(t)
		// Realistic post-bind state: owner_id IS the sole admin —
		// BindWorkspace seeds the owner onto admin_slack_user_ids at first
		// bind. Seed it explicitly (not via seedAdmin, which puts a
		// *different* user on the admin set) so this case genuinely diverges
		// from the "owner not on admin set" subtest below: together they
		// prove the gate keys off owner_id regardless of admin-set membership.
		ts.seedWorkspace(t, team, owner, owner, testWorkspaceConfiguredAt)
		h := newAdminTestHandler(t, ts)
		wireSetup(t, h)

		got := invokeSetup(t, h, owner)
		if !strings.Contains(got, "/oauth/qurl/start?state=") {
			t.Errorf("owner rerun: expected setup URL, got: %q", got)
		}
	})

	t.Run("non-owner reruns setup: refused with owner mention", func(t *testing.T) {
		ts := newAdminTestServers(t)
		// Workspace is bound to UOWNER001. The caller is USTRANGER000
		// — not on the admin set, definitely not the owner. Gate
		// rejects upfront with a copy that mentions the existing owner.
		ts.seedAdmin(t)
		h := newAdminTestHandler(t, ts)
		wireSetup(t, h)

		got := invokeSetup(t, h, stranger)
		if strings.Contains(got, "/oauth/qurl/start?state=") {
			t.Fatalf("non-owner: setup URL was minted (should be refused): %q", got)
		}
		// The reply must mention the person who connected qURL so the
		// requester knows whom to ask. The mention syntax `<@U…>` is what
		// Slack renders into a clickable user reference.
		if !strings.Contains(got, "<@"+owner+">") {
			t.Errorf("non-owner: reply missing owner mention <@%s>, got: %q", owner, got)
		}
		// Copy names who can re-run setup ("the person who first connected
		// qURL") rather than the ambiguous "workspace owner" — guard the
		// new framing stays.
		if !strings.Contains(got, "connected qURL") {
			t.Errorf("non-owner: reply missing 'connected qURL' framing for clarity, got: %q", got)
		}
	})

	t.Run("shape-bad owner_id (pre-pivot Auth0 sub): setup allowed so the legacy row can be reclaimed", func(t *testing.T) {
		ts := newAdminTestServers(t)
		// Migration-day state: a pre-pivot row whose owner_id holds the
		// Auth0 id_token sub, not a Slack ID. No Slack user can ever match
		// it, so the gate must NOT dead-end — it falls through to mint the
		// setup URL, and BindWorkspace self-heals on the callback by
		// reclaiming the orphaned row for the caller (first-come-claims).
		// The reply must not leak the raw Auth0 sub. Seed the row directly
		// since BindWorkspace now only ever writes Slack IDs.
		const auth0Sub = "auth0|653fpre-pivot-subxyz"
		ts.ddb.seedItem(t, ts.tableNames.workspace, map[string]ddbtypes.AttributeValue{
			fAttrSlackTeamID:       stringMember(team),
			fAttrOwnerID:           stringMember(auth0Sub),
			fAttrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{testAdminUserID}},
			fAttrCreatedAt:         stringMember(testWorkspaceConfiguredAt.UTC().Format(time.RFC3339)),
		})
		h := newAdminTestHandler(t, ts)
		wireSetup(t, h)

		got := invokeSetup(t, h, stranger)
		if !strings.Contains(got, "/oauth/qurl/start?state=") {
			t.Fatalf("shape-bad owner: expected setup URL so the legacy row can be reclaimed, got: %q", got)
		}
		if strings.Contains(got, auth0Sub) {
			t.Errorf("shape-bad owner: reply leaked the raw Auth0 sub: %q", got)
		}
		if strings.Contains(got, "<@") {
			t.Errorf("shape-bad owner: reply rendered a mention surface (should be the plain setup-URL copy): %q", got)
		}
	})

	t.Run("owner not on admin set: setup URL still minted (gate keys off owner_id, not admin membership)", func(t *testing.T) {
		ts := newAdminTestServers(t)
		// Regression guard: the owner gate must consult owner_id ALONE,
		// never admin_slack_user_ids membership. Seed a row whose owner_id
		// is the caller (UOWNER001) but whose admin set does NOT include
		// them (only UADMIN001) — the state after an owner is dropped from
		// the admin set via /qurl admin remove. /setup must still mint for
		// the owner. If a future change re-coupled the gate to admin-set
		// membership (the pre-PR BindWorkspace behavior), this fails.
		ts.ddb.seedItem(t, ts.tableNames.workspace, map[string]ddbtypes.AttributeValue{
			fAttrSlackTeamID:       stringMember(team),
			fAttrOwnerID:           stringMember(owner),
			fAttrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{testAdminUserID}},
			fAttrCreatedAt:         stringMember(testWorkspaceConfiguredAt.UTC().Format(time.RFC3339)),
		})
		h := newAdminTestHandler(t, ts)
		wireSetup(t, h)

		got := invokeSetup(t, h, owner)
		if !strings.Contains(got, "/oauth/qurl/start?state=") {
			t.Errorf("owner-not-on-admin-set: expected setup URL (gate must key off owner_id alone), got: %q", got)
		}
	})

	t.Run("CheckAdmin error: fail-closed, no setup URL minted", func(t *testing.T) {
		ts := newAdminTestServers(t)
		// Workspace is bound, but the owner-gate's CheckAdmin read
		// fails (transient DDB). The gate is security-relevant, so it
		// must fail CLOSED: surface the upstream-error reply and do NOT
		// fall through to mint a setup URL. Mirrors the requireAdminSync
		// fail-closed posture the membership verbs share.
		ts.seedAdmin(t)
		ts.ddb.SetGetItemErr(ts.tableNames.workspace, errors.New("injected DDB transient"))
		h := newAdminTestHandler(t, ts)
		wireSetup(t, h)

		got := invokeSetup(t, h, owner)
		if strings.Contains(got, "/oauth/qurl/start?state=") {
			t.Fatalf("CheckAdmin error: setup URL was minted (must fail closed): %q", got)
		}
		if !strings.Contains(got, "could not verify who connected qURL") {
			t.Errorf("CheckAdmin error: reply missing the upstream-error surface, got: %q", got)
		}
	})

	t.Run("added admin (not owner) reruns setup: refused", func(t *testing.T) {
		ts := newAdminTestServers(t)
		// seedAdmin binds owner=UOWNER001 and seeds UADMIN001 on the
		// admin set. UADMIN001 is an "admin" (can run /qurl admin
		// list/add/remove/revoke + tunnel etc.) but is NOT the owner.
		// /setup must refuse them — this is the load-bearing
		// safeguard against admins rotating the workspace credential.
		ts.seedAdmin(t)
		h := newAdminTestHandler(t, ts)
		wireSetup(t, h)

		got := invokeSetup(t, h, testAdminUserID)
		if strings.Contains(got, "/oauth/qurl/start?state=") {
			t.Fatalf("added admin: setup URL was minted (should be refused — owner-only): %q", got)
		}
		if !strings.Contains(got, "<@"+owner+">") {
			t.Errorf("added admin: reply missing owner mention <@%s>, got: %q", owner, got)
		}
	})
}
