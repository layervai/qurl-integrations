package internal

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

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

// Read residue directly: authorization reads intentionally hide retired data.
func physicalPolicyResources(ctx context.Context, t *testing.T, db slackdata.DynamoDBClient, table, team, channel string) map[string]struct{} {
	t.Helper()
	out, err := db.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(table), Key: map[string]ddbtypes.AttributeValue{"slack_team_id": &ddbtypes.AttributeValueMemberS{Value: team}, "slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: channel}}, ConsistentRead: aws.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]struct{})
	if set, ok := out.Item["allowed_resource_ids"].(*ddbtypes.AttributeValueMemberSS); ok {
		for _, id := range set.Value {
			ids[id] = struct{}{}
		}
	}
	if aliases, ok := out.Item["alias_bindings"].(*ddbtypes.AttributeValueMemberM); ok {
		for _, raw := range aliases.Value {
			if value, ok := raw.(*ddbtypes.AttributeValueMemberS); ok {
				ids[value.Value] = struct{}{}
			}
		}
	}
	return ids
}
