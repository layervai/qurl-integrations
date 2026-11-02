package slackdata

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestPolicyLegacyTransferDynamoDBLocal(t *testing.T) {
	store, client := localPolicyStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const team, channel = "TOLDOWNER", "COLDOWNER"
	if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testOwnerSlackID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team)}, UpdateExpression: aws.String("REMOVE policy_generation, policy_installation")}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(store.ChannelPoliciesName), Item: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel), attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"r_existing"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.TransferOwnership(nativeWorkspaceOwnerContext(ctx, t, store, team), team, testOwnerSlackID, testOtherSlackID); err != nil {
		t.Fatal(err)
	}
	authorization, err := store.AuthorizePolicyWrite(ctx, team, testOtherSlackID)
	if err != nil {
		t.Fatalf("legacy transfer left unusable policy identity: %v", err)
	}
	if !authorization.legacyPolicies || authorization.installation == "" {
		t.Fatal("legacy transfer did not preserve legacy policy ownership")
	}
	if err := store.ExposeResourceToChannel(authorization.Context(ctx), team, channel, "r_new"); err != nil {
		t.Fatal(err)
	}
	if err := store.TransferOwnership(nativeWorkspaceOwnerContext(ctx, t, store, team), team, testOtherSlackID, testOwnerSlackID); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.AuthorizePolicyWrite(ctx, team, testOwnerSlackID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.installation != authorization.installation || fresh.generation == authorization.generation {
		t.Fatal("transfer must retain configuration lineage and invalidate pending authority")
	}
	if err := store.ExposeResourceToChannel(authorization.Context(ctx), team, channel, "r_stale"); err == nil {
		t.Fatal("pre-transfer authority wrote a policy")
	}
	allowed, err := store.AllowedResourceIDsForChannel(ctx, team, channel)
	if err != nil || len(allowed) != 2 {
		t.Fatalf("legacy transfer lost configuration: %v %v", allowed, err)
	}
	for _, resource := range []string{"r_existing", "r_new"} {
		if _, ok := allowed[resource]; !ok {
			t.Fatalf("missing transferred grant %s", resource)
		}
	}
}

func TestPolicyLegacyTransferRaceDynamoDBLocal(t *testing.T) {
	for _, scenario := range []string{"reinstall during initialization", "ownership changes before commit"} {
		t.Run(scenario, func(t *testing.T) {
			store, client := localPolicyStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			const team = "TOLDCHANG"
			bind := func() {
				t.Helper()
				if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testOwnerSlackID); err != nil {
					t.Fatal(err)
				}
			}
			bind()
			if _, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team)}, UpdateExpression: aws.String("REMOVE policy_generation, policy_installation")}); err != nil {
				t.Fatal(err)
			}
			boundary := &policyCommitBoundaryDDB{Client: client, policyTable: store.WorkspaceMappingsName}
			store.Client = boundary
			if scenario == "reinstall during initialization" {
				boundary.beforeCommit = func() {
					if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
						t.Fatal(err)
					}
					bind()
				}
			}
			authorization, err := store.AuthorizePolicyWrite(ctx, team, testOwnerSlackID)
			if err == nil {
				if scenario == "ownership changes before commit" {
					boundary.beforeCommit = func() {
						if err := store.TransferOwnership(nativeWorkspaceOwnerContext(ctx, t, store, team), team, testOwnerSlackID, testCallerSlackID); err != nil {
							t.Fatal(err)
						}
						if err := store.TransferOwnership(nativeWorkspaceOwnerContext(ctx, t, store, team), team, testCallerSlackID, testOwnerSlackID); err != nil {
							t.Fatal(err)
						}
					}
				}
				err = store.TransferOwnership(authorization.Context(ctx), team, testOwnerSlackID, testOtherSlackID)
			}
			assertPolicyStatus(t, err, 409)
			row, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team)}, ConsistentRead: aws.Bool(true)})
			if err != nil {
				t.Fatal(err)
			}
			if readString(row.Item, attrOwnerID) != testOwnerSlackID || readString(row.Item, attrPolicyInstallation) == "" {
				t.Fatal("stale transfer changed workspace identity")
			}
		})
	}
}
