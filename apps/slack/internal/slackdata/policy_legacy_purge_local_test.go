package slackdata

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestPolicyLegacyPurgeDynamoDBLocal(t *testing.T) {
	for _, scenario := range []string{"retirement unconfirmed", "retirement confirmed", "active legacy binding", "fresh writer wins cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			store, client := localPolicyStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			const team, channel = "TLEGACY01", "CLEGACY01"
			if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testCallerSlackID); err != nil {
				t.Fatal(err)
			}
			key := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel)}
			if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(store.ChannelPoliciesName), Item: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel), attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"r_legacy"}}}}); err != nil {
				t.Fatal(err)
			}
			store.legacyPolicyWritersRetired = scenario != "retirement unconfirmed"
			if scenario == "active legacy binding" {
				if _, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team)}, UpdateExpression: aws.String("SET policy_legacy = :true"), ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":true": &ddbtypes.AttributeValueMemberBOOL{Value: true}}}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "fresh writer wins cleanup" {
				authorization, err := store.AuthorizePolicyWrite(ctx, team, testCallerSlackID)
				if err != nil {
					t.Fatal(err)
				}
				store.Client = &policyCommitBoundaryDDB{Client: client, policyTable: store.ChannelPoliciesName, beforeCommit: func() {
					// The concurrent writer has already reconciled the old row and stamped new config.
					if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(store.ChannelPoliciesName), Item: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel), attrPolicyInstallation: stringAttr(authorization.installation), attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"r_fresh"}}}}); err != nil {
						t.Error(err)
					}
				}}
			}
			err := store.PurgeTeamChannelPoliciesBefore(ctx, team, store.Now().Add(time.Second))
			if scenario == "retirement unconfirmed" {
				assertPolicyStatus(t, err, 503)
			} else if err != nil {
				t.Fatal(err)
			}
			row, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(store.ChannelPoliciesName), Key: key, ConsistentRead: aws.Bool(true)})
			if err != nil {
				t.Fatal(err)
			}
			resources := readStringSet(row.Item, attrAllowedResourceIDs)
			switch scenario {
			case "retirement confirmed":
				if len(row.Item) != 0 {
					t.Fatal("confirmed retired legacy policy remained")
				}
			case "fresh writer wins cleanup":
				if len(resources) != 1 || resources[0] != "r_fresh" {
					t.Fatal("cleanup deleted fresh configuration")
				}
			default:
				if len(resources) != 1 || resources[0] != "r_legacy" {
					t.Fatal("cleanup guessed ambiguous ownership")
				}
			}
		})
	}
}
