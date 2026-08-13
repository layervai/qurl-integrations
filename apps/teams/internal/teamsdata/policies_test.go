package teamsdata

import (
	"context"
	"errors"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	testWorkspacesTable = "workspaces"
	testPoliciesTable   = "channel_policies"
)

func newPolicyTestStore() (*Store, *fakeDDB) {
	fake := newFakeDDB(map[string][]string{
		testWorkspacesTable: {attrTenantID},
		testPoliciesTable:   {attrTenantID, attrScopeID},
	})
	return &Store{
		Client:                fake,
		WorkspaceMappingsName: testWorkspacesTable,
		ChannelPoliciesName:   testPoliciesTable,
		Now:                   func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}, fake
}

// TestScopeAliasLifecycleOnFreshScope walks expose -> bind -> lookup -> purge
// against a scope row that does not exist yet.
//
// This is the path that fails on real DynamoDB when alias_bindings is never
// seeded: the very first bind in any channel writes a nested path under a
// parent map that does not exist.
func TestScopeAliasLifecycleOnFreshScope(t *testing.T) {
	store, _ := newPolicyTestStore()
	ctx := context.Background()

	if err := store.ExposeResourceToScope(ctx, "tenant-1", "scope-1", "res-1"); err != nil {
		t.Fatalf("ExposeResourceToScope error = %v", err)
	}
	if err := store.BindScopeAlias(ctx, "tenant-1", "scope-1", "docs", "res-1"); err != nil {
		t.Fatalf("BindScopeAlias on fresh scope error = %v", err)
	}

	got, found, err := store.LookupScopeAlias(ctx, "tenant-1", "scope-1", "docs")
	if err != nil {
		t.Fatalf("LookupScopeAlias error = %v", err)
	}
	if !found || got != "res-1" {
		t.Fatalf("LookupScopeAlias = %q, found = %v, want res-1", got, found)
	}

	allowed, err := store.AllowedResourceIDsForScope(ctx, "tenant-1", "scope-1")
	if err != nil {
		t.Fatalf("AllowedResourceIDsForScope error = %v", err)
	}
	if _, ok := allowed["res-1"]; !ok {
		t.Fatalf("AllowedResourceIDsForScope = %v, want res-1 present", allowed)
	}

	aliases, err := store.PurgeResourceFromScope(ctx, "tenant-1", "scope-1", "res-1")
	if err != nil {
		t.Fatalf("PurgeResourceFromScope error = %v", err)
	}
	if len(aliases) != 1 || aliases[0] != "docs" {
		t.Fatalf("PurgeResourceFromScope = %v, want [docs]", aliases)
	}

	if _, found, err := store.LookupScopeAlias(ctx, "tenant-1", "scope-1", "docs"); err != nil || found {
		t.Fatalf("LookupScopeAlias after purge found = %v, err = %v, want not found", found, err)
	}
	allowed, err = store.AllowedResourceIDsForScope(ctx, "tenant-1", "scope-1")
	if err != nil {
		t.Fatalf("AllowedResourceIDsForScope after purge error = %v", err)
	}
	if len(allowed) != 0 {
		t.Fatalf("AllowedResourceIDsForScope after purge = %v, want empty", allowed)
	}
}

// TestBindScopeAliasWithoutPriorExpose covers a bind on a row that no other
// write has created, which is the barest form of the missing-parent case.
func TestBindScopeAliasWithoutPriorExpose(t *testing.T) {
	store, _ := newPolicyTestStore()

	if err := store.BindScopeAlias(context.Background(), "tenant-1", "scope-1", "docs", "res-1"); err != nil {
		t.Fatalf("BindScopeAlias on absent row error = %v", err)
	}
	got, found, err := store.LookupScopeAlias(context.Background(), "tenant-1", "scope-1", "docs")
	if err != nil {
		t.Fatalf("LookupScopeAlias error = %v", err)
	}
	if !found || got != "res-1" {
		t.Fatalf("LookupScopeAlias = %q, found = %v, want res-1", got, found)
	}
}

func TestBindScopeAliasRejectsDuplicate(t *testing.T) {
	store, _ := newPolicyTestStore()
	ctx := context.Background()

	if err := store.BindScopeAlias(ctx, "tenant-1", "scope-1", "docs", "res-1"); err != nil {
		t.Fatalf("first BindScopeAlias error = %v", err)
	}
	err := store.BindScopeAlias(ctx, "tenant-1", "scope-1", "docs", "res-2")
	if !errors.Is(err, ErrAliasAlreadyBound) {
		t.Fatalf("duplicate BindScopeAlias error = %v, want ErrAliasAlreadyBound", err)
	}
	// The losing bind must not have overwritten the existing binding.
	got, found, err := store.LookupScopeAlias(ctx, "tenant-1", "scope-1", "docs")
	if err != nil {
		t.Fatalf("LookupScopeAlias error = %v", err)
	}
	if !found || got != "res-1" {
		t.Fatalf("LookupScopeAlias = %q, found = %v, want res-1 (binding must survive a duplicate bind)", got, found)
	}
}

// TestBindScopeAliasPreservesSiblingAliases guards the seed step: it must not
// clobber alias_bindings once the map already holds entries.
func TestBindScopeAliasPreservesSiblingAliases(t *testing.T) {
	store, _ := newPolicyTestStore()
	ctx := context.Background()

	if err := store.BindScopeAlias(ctx, "tenant-1", "scope-1", "docs", "res-1"); err != nil {
		t.Fatalf("BindScopeAlias(docs) error = %v", err)
	}
	if err := store.BindScopeAlias(ctx, "tenant-1", "scope-1", "specs", "res-2"); err != nil {
		t.Fatalf("BindScopeAlias(specs) error = %v", err)
	}
	for alias, want := range map[string]string{"docs": "res-1", "specs": "res-2"} {
		got, found, err := store.LookupScopeAlias(ctx, "tenant-1", "scope-1", alias)
		if err != nil {
			t.Fatalf("LookupScopeAlias(%s) error = %v", alias, err)
		}
		if !found || got != want {
			t.Fatalf("LookupScopeAlias(%s) = %q, found = %v, want %q", alias, got, found, want)
		}
	}
}

func TestUnbindScopeAliasMissingAliasIsNotFound(t *testing.T) {
	store, _ := newPolicyTestStore()

	err := store.UnbindScopeAlias(context.Background(), "tenant-1", "scope-1", "docs")
	if !errors.Is(err, ErrAliasNotFound) {
		t.Fatalf("UnbindScopeAlias error = %v, want ErrAliasNotFound", err)
	}
}

// TestPurgeResourceFromScopeWithoutAliases covers the common revoke path: a
// resource exposed to a scope with no alias bound to it. That branch builds no
// #aN placeholders, so ExpressionAttributeNames must be left nil rather than
// sent as an empty map.
func TestPurgeResourceFromScopeWithoutAliases(t *testing.T) {
	store, _ := newPolicyTestStore()
	ctx := context.Background()

	if err := store.ExposeResourceToScope(ctx, "tenant-1", "scope-1", "res-1"); err != nil {
		t.Fatalf("ExposeResourceToScope error = %v", err)
	}
	aliases, err := store.PurgeResourceFromScope(ctx, "tenant-1", "scope-1", "res-1")
	if err != nil {
		t.Fatalf("PurgeResourceFromScope error = %v", err)
	}
	if len(aliases) != 0 {
		t.Fatalf("PurgeResourceFromScope = %v, want no aliases", aliases)
	}
	allowed, err := store.AllowedResourceIDsForScope(ctx, "tenant-1", "scope-1")
	if err != nil {
		t.Fatalf("AllowedResourceIDsForScope error = %v", err)
	}
	if len(allowed) != 0 {
		t.Fatalf("AllowedResourceIDsForScope = %v, want empty after purge", allowed)
	}
}

// TestSavePersonalConversationRefSeedsParentMap covers the workspace-side
// nested map. The pre-fix expression seeded personal_conversation_refs and set
// personal_conversation_refs.#actor in one statement, which DynamoDB rejects as
// overlapping document paths.
func TestSavePersonalConversationRefSeedsParentMap(t *testing.T) {
	store, fake := newPolicyTestStore()
	ctx := context.Background()
	fake.seed(testWorkspacesTable, map[string]ddbtypes.AttributeValue{
		attrTenantID: stringAttr("tenant-1"),
	})

	ref := &PersonalConversationRef{ServiceURL: "https://service.example.test", ConversationID: "conv-1"}
	if err := store.SavePersonalConversationRef(ctx, "tenant-1", "user-1", ref); err != nil {
		t.Fatalf("SavePersonalConversationRef error = %v", err)
	}

	got, ok, err := store.PersonalConversationRef(ctx, "tenant-1", "user-1")
	if err != nil {
		t.Fatalf("PersonalConversationRef error = %v", err)
	}
	if !ok {
		t.Fatal("PersonalConversationRef not found after save")
	}
	if got.ConversationID != "conv-1" || got.ServiceURL != "https://service.example.test" {
		t.Fatalf("PersonalConversationRef = %+v, want conv-1 / service.example.test", got)
	}

	// A second actor must land beside the first, not replace the map.
	second := &PersonalConversationRef{ServiceURL: "https://service.example.test", ConversationID: "conv-2"}
	if err := store.SavePersonalConversationRef(ctx, "tenant-1", "user-2", second); err != nil {
		t.Fatalf("SavePersonalConversationRef(user-2) error = %v", err)
	}
	first, ok, err := store.PersonalConversationRef(ctx, "tenant-1", "user-1")
	if err != nil {
		t.Fatalf("PersonalConversationRef(user-1) error = %v", err)
	}
	if !ok || first.ConversationID != "conv-1" {
		t.Fatalf("PersonalConversationRef(user-1) = %+v, ok = %v, want conv-1 preserved", first, ok)
	}
}

// TestSavePersonalConversationRefRequiresTenantRow keeps the missing-tenant
// guard meaningful: the best-effort seed must not create the row on its own.
func TestSavePersonalConversationRefRequiresTenantRow(t *testing.T) {
	store, _ := newPolicyTestStore()

	ref := &PersonalConversationRef{ServiceURL: "https://service.example.test", ConversationID: "conv-1"}
	err := store.SavePersonalConversationRef(context.Background(), "tenant-missing", "user-1", ref)
	if err == nil {
		t.Fatal("SavePersonalConversationRef on absent tenant row = nil, want error")
	}
}
