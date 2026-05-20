package internal

// BindWorkspace → CheckAdmin end-to-end across the real
// slackdata.Store + fakeDDB. Other layers fence the producer
// (PutItem inspection) and consumer (pre-seeded fixture rows)
// in isolation; this fences the `admin_slack_user_ids` SS-
// attribute seam between them.

import (
	"context"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

func TestBindWorkspaceSeedsAdminThatCheckAdminAccepts(t *testing.T) {
	names := defaultTestTableNames()
	ddb := newFakeDDB(t, names, nil)
	store := newStoreFromFake(t, ddb, names, nil)

	const (
		teamID    = "T_e2eBindCheck"
		ownerID   = "auth0|seed-admin-owner"
		seedAdmin = "USEEDADMIN1"
	)

	if err := store.BindWorkspace(context.Background(),
		&slackdata.WorkspaceMapping{TeamID: teamID, OwnerID: ownerID}, seedAdmin); err != nil {
		t.Fatalf("BindWorkspace: %v", err)
	}

	isAdmin, gotOwner, err := store.CheckAdmin(context.Background(), teamID, seedAdmin)
	if err != nil {
		t.Fatalf("CheckAdmin: %v", err)
	}
	if !isAdmin {
		t.Errorf("CheckAdmin isAdmin = false, want true (seed admin set by BindWorkspace must be recognized)")
	}
	if gotOwner != ownerID {
		t.Errorf("CheckAdmin ownerID = %q, want %q", gotOwner, ownerID)
	}

	// Non-seed user on the same workspace must NOT be admin —
	// guards against a write that promiscuously promotes any
	// post-bind reader.
	const otherUser = "UOTHERUSER1"
	isOther, _, err := store.CheckAdmin(context.Background(), teamID, otherUser)
	if err != nil {
		t.Fatalf("CheckAdmin(other): %v", err)
	}
	if isOther {
		t.Error("CheckAdmin(non-seed user) isAdmin = true, want false (BindWorkspace must seed only the named seedAdmin)")
	}
}
