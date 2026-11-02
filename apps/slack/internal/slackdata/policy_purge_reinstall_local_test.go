package slackdata

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestPolicyPurgeDynamoDBLocalOldInstallationAfterReinstall(t *testing.T) {
	store, client := localPolicyStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := store.Now()
	store.Now = func() time.Time { return now }
	const team = "TMIXED001"
	bind := func() {
		t.Helper()
		if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testOwnerSlackID); err != nil {
			t.Fatal(err)
		}
	}
	bind()
	old, err := store.AuthorizePolicyWrite(ctx, team, testOwnerSlackID)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(time.Second)
	now = cutoff.Add(time.Second)
	put := func(channel, resource, installation string) {
		t.Helper()
		_, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(store.ChannelPoliciesName), Item: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel), attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{resource}}, attrPolicyInstallation: stringAttr(installation), attrUpdatedAtNano: unixNanoAttr(now)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	put("COLD00001", "r_old", old.installation)
	if err := store.DeleteWorkspaceMappingBefore(ctx, team, cutoff); err != nil {
		t.Fatal(err)
	}
	now = cutoff.Add(2 * time.Second)
	bind()
	fresh, err := store.AuthorizePolicyWrite(ctx, team, testOwnerSlackID)
	if err != nil {
		t.Fatal(err)
	}
	put("CNEW00001", "r_new", fresh.installation)
	if err := store.PurgeTeamChannelPoliciesBefore(ctx, team, cutoff); err != nil {
		t.Fatal(err)
	}
	current, err := store.AllowedResourceIDsForChannel(ctx, team, "CNEW00001")
	if err != nil || len(current) != 1 {
		t.Fatalf("new installation policy was lost: %v", err)
	}
	prior, err := store.AllowedResourceIDsForChannel(ctx, team, "COLD00001")
	if err != nil {
		t.Fatal(err)
	}
	if len(prior) != 0 {
		t.Fatal("old installation row survived completed purge after reinstall")
	}
}

func TestPolicyPurgeDynamoDBLocalRetiredDeleteBoundary(t *testing.T) {
	for _, scenario := range []string{"row replaced", "binding replaced", "unmarked row"} {
		t.Run(scenario, func(t *testing.T) {
			store, client := localPolicyStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			const team, channel = "TBOUND001", "CBOUND001"
			if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testOwnerSlackID); err != nil {
				t.Fatal(err)
			}
			binding, err := store.policyPurgeBinding(ctx, team)
			if err != nil {
				t.Fatal(err)
			}
			row := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel), attrPolicyInstallation: stringAttr("retired"), attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"r_old"}}}
			if scenario == "unmarked row" {
				delete(row, attrPolicyInstallation)
			}
			put := func(item map[string]ddbtypes.AttributeValue) {
				t.Helper()
				if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(store.ChannelPoliciesName), Item: item}); err != nil {
					t.Fatal(err)
				}
			}
			put(row)
			store.Client = &policyCommitBoundaryDDB{Client: client, policyTable: store.ChannelPoliciesName, beforeCommit: func() {
				if scenario == "binding replaced" {
					_, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team)}, UpdateExpression: aws.String("SET #i = :i"), ExpressionAttributeNames: map[string]string{"#i": attrPolicyInstallation}, ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":i": stringAttr("replacement")}})
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				put(map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel), attrPolicyInstallation: stringAttr(binding.installation), attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"r_new"}}})
			}}
			err = store.deleteRetiredPolicy(ctx, team, channel, row, binding)
			if scenario == "row replaced" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				assertPolicyStatus(t, err, 503)
			}
			remaining, err := store.AllowedResourceIDsForChannel(ctx, team, channel)
			if err != nil || len(remaining) != 1 {
				t.Fatalf("cleanup lost policy: %v %v", remaining, err)
			}
			want := "r_old"
			if scenario == "row replaced" {
				want = "r_new"
			}
			if _, ok := remaining[want]; !ok {
				t.Fatalf("cleanup changed policy: %v", remaining)
			}
		})
	}
}
