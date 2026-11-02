package slackdata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestPolicyRevokeDynamoDBLocalAliasReboundAtCommit(t *testing.T) {
	store, client := localPolicyStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const team, channel = "TREVOKE01", "CREVOKE01"
	if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testOwnerSlackID); err != nil {
		t.Fatal(err)
	}
	authorization, err := store.AuthorizePolicyWrite(ctx, team, testOwnerSlackID)
	if err != nil {
		t.Fatal(err)
	}
	ctx = authorization.Context(ctx)

	key := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel)}
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(store.ChannelPoliciesName), Item: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel), attrPolicyInstallation: stringAttr(authorization.installation), attrAliasBindings: &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{"reuse": stringAttr("r_dead")}}, attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"r_dead"}}}})
	if err != nil {
		t.Fatal(err)
	}
	store.Client = &policyCommitBoundaryDDB{Client: client, policyTable: store.ChannelPoliciesName, beforeCommit: func() {
		_, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(store.ChannelPoliciesName), Key: key, UpdateExpression: aws.String("SET alias_bindings.#a = :live"), ExpressionAttributeNames: map[string]string{"#a": "reuse"}, ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":live": stringAttr("r_live")}})
		if err != nil {
			t.Fatal(err)
		}
	}}
	_, err = store.PurgeResourceFromChannel(ctx, team, channel, "r_dead")
	var changed *ddbtypes.ConditionalCheckFailedException
	if !errors.As(err, &changed) {
		t.Fatalf("purge accepted a stale alias snapshot or failed for the wrong reason: %v", err)
	}
	store.Client = client
	// A fresh retry removes only the dead grant and preserves the live binding.
	removed, err := store.PurgeResourceFromChannel(ctx, team, channel, "r_dead")
	if err != nil || len(removed) != 0 {
		t.Fatalf("retry: %v %v", removed, err)
	}
	row, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(store.ChannelPoliciesName), Key: key, ConsistentRead: aws.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if got := readStringMap(row.Item, attrAliasBindings)["reuse"]; got != "r_live" {
		t.Fatalf("live alias lost: %q", got)
	}
	if len(readStringSet(row.Item, attrAllowedResourceIDs)) != 0 {
		t.Fatal("retry retained dead grant")
	}
}
