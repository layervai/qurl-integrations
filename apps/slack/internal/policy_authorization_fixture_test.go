package internal

import (
	"context"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

func testPolicyContext(ctx context.Context, t *testing.T, store *slackdata.Store, teamID, userID string) context.Context {
	t.Helper()
	authorization, err := store.AuthorizePolicyWrite(ctx, teamID, userID)
	if err != nil {
		t.Fatalf("authorize policy test operation: %v", err)
	}
	return authorization.Context(ctx)
}

func testPolicyFixtureContext(t *testing.T, store *slackdata.Store, teamID string) context.Context {
	t.Helper()
	ctx := context.Background()
	owner, _, err := store.ListAdmins(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	return testPolicyContext(ctx, t, store, teamID, owner)
}
