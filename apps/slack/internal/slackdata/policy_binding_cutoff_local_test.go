package slackdata

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestPolicyBindingCutoffDynamoDBLocal(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, operation := range []string{"add admin", "remove admin", "transfer owner", "agent toggle"} {
			name := operation
			if legacy {
				name = "legacy/" + name
			}
			t.Run(name, func(t *testing.T) {
				store, client := localPolicyStore(t)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				now := store.Now()
				store.Now = func() time.Time { return now }
				const team = "TBINDTIME"
				key := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team)}
				if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testCallerSlackID); err != nil {
					t.Fatal(err)
				}
				if legacy {
					if _, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: key, UpdateExpression: aws.String("REMOVE policy_generation, policy_installation, policy_bound_at_unix_nano")}); err != nil {
						t.Fatal(err)
					}
				}
				cutoff := now.Add(time.Second)
				now = cutoff.Add(time.Second)
				var err error
				switch operation {
				case "add admin":
					err = store.AddAdmin(nativeWorkspaceOwnerContext(ctx, t, store, team), team, testOtherSlackID)
				case "remove admin":
					err = store.RemoveAdmin(nativeWorkspaceOwnerContext(ctx, t, store, team), team, testCallerSlackID)
				case "transfer owner":
					err = store.TransferOwnership(ctx, team, testOwnerSlackID, testOtherSlackID)
				case "agent toggle":
					err = store.SetAgentEnabled(nativeWorkspaceOwnerContext(ctx, t, store, team), team, true)
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := store.DeleteWorkspaceMappingBefore(ctx, team, cutoff); err != nil {
					t.Fatal(err)
				}
				row, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: key, ConsistentRead: aws.Bool(true)})
				if err != nil {
					t.Fatal(err)
				}
				if len(row.Item) != 0 {
					t.Fatal("late settings update disguised old binding as a reinstall")
				}
			})
		}
	}
}

func TestPolicyNewBindingCutoffDynamoDBLocal(t *testing.T) {
	store, client := localPolicyStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const team = "TFRESHBND"
	cutoff := store.Now().Add(-time.Second)
	if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID, CreatedAt: cutoff.Add(-time.Hour)}, testCallerSlackID); err != nil {
		t.Fatal(err)
	}
	key := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team)}
	// Imported audit timestamps cannot backdate a newly bound installation.
	if _, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: key, UpdateExpression: aws.String("SET updated_at_unix_nano = :old"), ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":old": unixNanoAttr(cutoff.Add(-time.Second))}}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteWorkspaceMappingBefore(ctx, team, cutoff); err != nil {
		t.Fatal(err)
	}
	row, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: key, ConsistentRead: aws.Bool(true)})
	if err != nil || len(row.Item) == 0 {
		t.Fatalf("old purge deleted a fresh binding: %v", err)
	}
}
