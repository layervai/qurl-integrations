package slackdata

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestPolicyPurgeDynamoDBLocalSignalCutoff(t *testing.T) {
	store, client := localPolicyStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	store.Now = func() time.Time { return now }
	const team = "TCUTOFF01"
	if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testOwnerSlackID); err != nil {
		t.Fatal(err)
	}
	authorization, err := store.AuthorizePolicyWrite(ctx, team, testOwnerSlackID)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(time.Second)
	now = cutoff.Add(time.Second)
	if err := store.ExposeResourceToChannel(authorization.Context(ctx), team, "C00000001", "r_late"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteWorkspaceMappingBefore(ctx, team, cutoff); err != nil {
		t.Fatal(err)
	}
	binding, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(store.WorkspaceMappingsName), ConsistentRead: aws.Bool(true), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team)}})
	if err != nil || len(binding.Item) != 0 {
		t.Fatalf("workspace binding was not removed: %v", err)
	}
	if err := store.PurgeTeamChannelPoliciesBefore(ctx, team, cutoff); err != nil {
		t.Fatal(err)
	}
	allowed, err := store.AllowedResourceIDsForChannel(ctx, team, "C00000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != 0 {
		t.Fatal("policy survived successful purge: write committed after event cutoff but before binding deletion")
	}
}

func TestPolicyPurgeDynamoDBLocalReinstallAtDeleteBoundary(t *testing.T) {
	store, client := localPolicyStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const team = "TRESTART1"
	cutoff := store.Now().Add(time.Second)
	store.Now = func() time.Time { return cutoff.Add(time.Second) }
	if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(store.ChannelPoliciesName), Item: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr("C00000001"), attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"r_old"}}, attrUpdatedAtNano: unixNanoAttr(store.Now())}}); err != nil {
		t.Fatal(err)
	}
	boundary := &policyCommitBoundaryDDB{Client: client, policyTable: store.ChannelPoliciesName}
	boundary.beforeCommit = func() {
		store.Now = func() time.Time { return cutoff.Add(2 * time.Second) }
		if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testOwnerSlackID); err != nil {
			t.Fatal(err)
		}
		binding, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team)}, ConsistentRead: aws.Bool(true)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(store.ChannelPoliciesName), Item: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr("C00000001"), attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"r_new"}}, attrUpdatedAtNano: unixNanoAttr(store.Now()), attrPolicyInstallation: binding.Item[attrPolicyInstallation]}})
		if err != nil {
			t.Fatal(err)
		}
	}
	store.Client = boundary
	err := store.PurgeTeamChannelPoliciesBefore(ctx, team, cutoff)
	assertPolicyStatus(t, err, 503)
	store.Client = client
	if err := store.PurgeTeamChannelPoliciesBefore(ctx, team, cutoff); err != nil {
		t.Fatal(err)
	}
	allowed, err := store.AllowedResourceIDsForChannel(ctx, team, "C00000001")
	if err != nil || len(allowed) != 1 {
		t.Fatalf("reinstall policies changed: %v %v", allowed, err)
	}
	if _, ok := allowed["r_new"]; !ok {
		t.Fatal("late old purge deleted new installation policy")
	}
}
