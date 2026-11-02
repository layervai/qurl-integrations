package slackdata

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestPolicyReinstallWriteDynamoDBLocal(t *testing.T) {
	for _, scenario := range []string{"grant replaces retired row", "alias replaces retired row", "fresh row wins cleanup", "revoked at cleanup", "revoked at retry", "fresh grant wins retry", "untagged before retirement", "untagged after retirement"} {
		t.Run(scenario, func(t *testing.T) {
			store, client := localPolicyStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			const team, channel = "TREPLACE1", "CREPLACE1"
			if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testCallerSlackID); err != nil {
				t.Fatal(err)
			}
			authorization, err := store.AuthorizePolicyWrite(ctx, team, testCallerSlackID)
			if err != nil {
				t.Fatal(err)
			}
			key := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel)}
			put := func(installation, resource string) {
				t.Helper()
				_, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(store.ChannelPoliciesName), Item: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel), attrPolicyInstallation: stringAttr(installation), attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{resource}}, attrAliasBindings: &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{"existing": stringAttr(resource)}}}})
				if err != nil {
					t.Fatal(err)
				}
			}
			put("retired", "r_retired")
			if scenario == "untagged before retirement" || scenario == "untagged after retirement" {
				if _, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(store.ChannelPoliciesName), Key: key, UpdateExpression: aws.String("REMOVE policy_installation")}); err != nil {
					t.Fatal(err)
				}
				store.legacyPolicyWritersRetired = scenario == "untagged after retirement"
			}

			revoke := func() {
				if err := store.RemoveAdmin(ctx, team, testCallerSlackID); err != nil {
					t.Fatal(err)
				}
			}
			boundary := &policyCommitBoundaryDDB{Client: client, policyTable: store.ChannelPoliciesName}
			boundary.beforeCommit = func() {
				boundary.beforeCommit = func() {
					switch scenario {
					case "fresh row wins cleanup":
						put(authorization.installation, "r_concurrent")
					case "revoked at cleanup":
						revoke()
					case "revoked at retry":
						boundary.beforeCommit = revoke
					case "fresh grant wins retry":
						boundary.beforeCommit = func() { put(authorization.installation, "r_concurrent") }
					}
				}
			}
			store.Client = boundary
			if scenario == "alias replaces retired row" {
				err = store.BindChannelAlias(authorization.Context(ctx), team, channel, "requested", "r_requested")
			} else {
				err = store.ExposeResourceToChannel(authorization.Context(ctx), team, channel, "r_requested")
			}
			if scenario == "fresh row wins cleanup" || scenario == "revoked at cleanup" || scenario == "revoked at retry" || scenario == "untagged before retirement" {
				assertPolicyStatus(t, err, 409)
			} else if err != nil {
				t.Fatal(err)
			}
			row, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(store.ChannelPoliciesName), Key: key, ConsistentRead: aws.Bool(true)})
			if err != nil {
				t.Fatal(err)
			}
			resources := allowedResourceIDsFromItem(row.Item)
			switch scenario {
			case "untagged before retirement":
				if len(resources) != 1 {
					t.Fatal("ambiguous policy changed before retirement")
				}
				if _, ok := resources["r_retired"]; !ok {
					t.Fatal("ambiguous grant was lost")
				}
			case "fresh row wins cleanup":
				if len(resources) != 1 {
					t.Fatal("cleanup changed concurrent row")
				}
				if _, ok := resources["r_concurrent"]; !ok {
					t.Fatal("cleanup deleted fresh policy")
				}
			case "revoked at cleanup":
				if len(resources) != 1 || readString(row.Item, attrPolicyInstallation) != "retired" {
					t.Fatal("revoked caller cleared a row")
				}
			case "revoked at retry":
				if len(row.Item) != 0 {
					t.Fatal("revoked retry wrote a policy")
				}
			default:
				if _, ok := resources["r_retired"]; ok {
					t.Fatal("new installation inherited a retired grant")
				}
				if _, ok := resources["r_requested"]; !ok {
					t.Fatal("new installation lost its requested grant")
				}
				if readString(row.Item, attrPolicyInstallation) != authorization.installation {
					t.Fatal("new policy lacks current ownership")
				}
				if scenario == "fresh grant wins retry" {
					if _, ok := resources["r_concurrent"]; !ok {
						t.Fatal("retry overwrote concurrent grant")
					}
				} else if len(resources) != 1 {
					t.Fatal("new installation retained unexpected configuration")
				}
			}
		})
	}
}
