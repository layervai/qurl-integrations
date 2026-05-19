package internal

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// Test-local string constants — keeps the test file free of magic
// literals that would otherwise repeat across cases. Slack user IDs
// are uppercase alphanumeric (no underscore), so the fixtures here
// use that shape — the parser's userMentionPattern rejects the
// `U_admin`-style IDs the older test fixtures used. qurl_id fixtures
// are ULID-style 26-char suffixes so the parser's {16,64} length
// gate accepts them.
const (
	testTargetUserID  = "UTARGET01"
	testTargetMention = "<@UTARGET01>"
	testOtherAdminID  = "UOTHER001"
	testAdminListCmd  = "admin list"
	testRevokeQURLID  = "q_01HXYZ8ABCDEF0123456789AB"
	testMissingQURLID = "q_01HXYZ8MISS123456789ABCDE"
)

// --- Revoke (single qurl_id, sync) ---

// TestHandleAdminRevoke_HappyPath fences single-qURL revocation.
func TestHandleAdminRevoke_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var deleteHits atomic.Int32
	ts.addCustomer("DELETE", "/v1/qurls/"+testRevokeQURLID, func(w http.ResponseWriter, _ *http.Request) {
		deleteHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin revoke "+testRevokeQURLID, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "Revoked `"+testRevokeQURLID+"`") {
		t.Errorf("reply missing success line: %q", reply)
	}
	if deleteHits.Load() != 1 {
		t.Errorf("DELETE called %d times, want 1", deleteHits.Load())
	}
}

// TestHandleAdminRevoke_404IsGraceful fences the 404-friendly
// surface: an already-revoked or typo'd qurl_id renders a hint rather
// than a stack trace.
func TestHandleAdminRevoke_404IsGraceful(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer("DELETE", "/v1/qurls/"+testMissingQURLID, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"title":"Not Found","status":404}}`))
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin revoke "+testMissingQURLID, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "already revoked") {
		t.Errorf("reply missing graceful 404 surface: %q", reply)
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

// TestHandleAdminRevoke_AuthRejected fences the 401/403 surface: a
// rotated workspace API key surfaces a "re-run /qurl setup" hint
// instead of the generic upstream-error copy, so the admin has a
// concrete next step.
func TestHandleAdminRevoke_AuthRejected(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		ts.addCustomer("DELETE", "/v1/qurls/"+testRevokeQURLID, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"title":"auth rejected","status":` + strconv.Itoa(status) + `}}`))
		})

		h := newAdminTestHandler(t, ts)
		inv := newAdminSlashInvoker(t, h)

		_, reply := inv.invokeAdmin("admin revoke "+testRevokeQURLID, testAdminTeamID, testAdminUserID)
		if !strings.Contains(reply, "re-run `/qurl setup`") {
			t.Errorf("status %d: reply missing rotate-hint: %q", status, reply)
		}
	}
}

// TestHandleAdminRevoke_NonAdmin fences the admin-only gate on revoke.
func TestHandleAdminRevoke_NonAdmin(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedNonAdmin(t)
	var deleteHits atomic.Int32
	ts.addCustomerPrefix("DELETE", "/v1/qurls/", func(w http.ResponseWriter, _ *http.Request) {
		deleteHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin revoke "+testRevokeQURLID, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "admin-only") {
		t.Errorf("reply missing admin-only fence: %q", reply)
	}
	if deleteHits.Load() != 0 {
		t.Errorf("DELETE fired despite non-admin gate (hits = %d)", deleteHits.Load())
	}
}

// --- Add ---

// TestHandleAdminAdd_HappyPath fences the canonical add flow: admin
// gate → AddAdmin UpdateItem → success reply. The post-state
// assertion confirms the target lands on the admin set.
func TestHandleAdminAdd_HappyPath(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin add "+testTargetMention, testAdminTeamID, testAdminUserID)
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

	_, reply := inv.invokeAdmin("admin add "+testTargetMention, testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "already an admin") {
		t.Errorf("reply missing idempotent surface: %q", reply)
	}
}

// TestHandleAdminAdd_WorkspaceNotBound fences the 404 surface at the
// slackdata layer: a pre-claim workspace returns ErrCodeWorkspaceNotBound.
//
// Coverage gap (known, not fixed): the handler-layer mapping of that
// store error to the user-visible "Workspace isn't bound — run
// `/qurl setup` first" copy isn't fenced end-to-end here, because
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

	_, reply := inv.invokeAdmin("admin add "+testTargetMention, testAdminTeamID, testAdminUserID)
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

	_, reply := inv.invokeAdmin("admin add "+testTargetMention, testAdminTeamID, testAdminUserID)
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

	_, reply := inv.invokeAdmin("admin add <@"+testAdminUserID+">", testAdminTeamID, testAdminUserID)
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

	for _, text := range []string{"admin add", "admin add someone", "admin add @someone"} {
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

	_, reply := inv.invokeAdmin("admin remove <@"+testOtherAdminID+">", testAdminTeamID, testAdminUserID)
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

	_, reply := inv.invokeAdmin("admin remove "+testTargetMention, testAdminTeamID, testAdminUserID)
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

	_, reply := inv.invokeAdmin("admin remove <@"+testAdminUserID+">", testAdminTeamID, testAdminUserID)
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

	_, reply := inv.invokeAdmin("admin remove <@"+testAdminOwnerID+">", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "workspace owner") {
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

	_, reply := inv.invokeAdmin("admin remove "+testTargetMention, testAdminTeamID, testAdminUserID)
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
	if !strings.Contains(reply, "Owner: <@"+testAdminOwnerID+">") {
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
	if !strings.Contains(reply, "Owner: <@"+testAdminOwnerID+">") {
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
	if !strings.Contains(reply, "Owner: <@"+testAdminOwnerID+">") {
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
// the explicit "(unknown — workspace_mappings missing owner_id)"
// copy instead of a malformed `Owner: <@>` mrkdwn link.
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
	if !strings.Contains(reply, "workspace_mappings missing owner_id") {
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

// TestHandleAdminRevoke_RejectsAliasShape fences the parser
// distinction: `admin revoke $alias` is wrong-grammar and the reply
// must surface a parser hint rather than silently trying to resolve
// the alias.
func TestHandleAdminRevoke_RejectsAliasShape(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	inv := newAdminSlashInvoker(t, h)

	_, reply := inv.invokeAdmin("admin revoke $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "q_<id>") && !strings.Contains(reply, "qurl_id") {
		t.Errorf("reply must guide user toward the qurl_id form: %q", reply)
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
