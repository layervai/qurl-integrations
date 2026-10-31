package internal

// BindWorkspace seeds the owner into admin-list display as well as retaining
// the owner_id gate. This uses the real store across the fake DDB seam.

import (
	"context"
	"slices"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

func TestBindWorkspaceSeedsAdminThatCheckAdminAccepts(t *testing.T) {
	names := defaultTestTableNames()
	ddb := newFakeDDB(t, names, nil)
	store := newStoreFromFake(t, ddb, names, nil)

	const (
		teamID  = "T_e2eBindCheck"
		ownerID = "USEEDADMIN1"
	)

	if err := store.BindWorkspace(context.Background(),
		&slackdata.WorkspaceMapping{TeamID: teamID, OwnerID: ownerID}); err != nil {
		t.Fatalf("BindWorkspace: %v", err)
	}

	isAdmin, gotOwner, err := store.CheckAdmin(context.Background(), teamID, ownerID)
	if err != nil {
		t.Fatalf("CheckAdmin: %v", err)
	}
	if !isAdmin {
		t.Errorf("CheckAdmin isAdmin = false, want true (seed admin set by BindWorkspace must be recognized)")
	}
	if gotOwner != ownerID {
		t.Errorf("CheckAdmin ownerID = %q, want %q", gotOwner, ownerID)
	}

	listedOwner, admins, err := store.ListAdmins(context.Background(), teamID)
	if err != nil {
		t.Fatalf("ListAdmins: %v", err)
	}
	if listedOwner != ownerID || !slices.Equal(admins, []string{ownerID}) {
		t.Errorf("fresh admin list = owner %q members %v, want owner only", listedOwner, admins)
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
