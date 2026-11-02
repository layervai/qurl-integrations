package slackdata

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestPolicyWritersDynamoDBLocal(t *testing.T) {
	for _, operation := range []string{"bind alias", "unbind alias", "grant", "revoke grant", "purge resource"} {
		for _, mode := range []string{"allowed", "binding deleted", "admin removed", "missing authorization"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				store, client := localPolicyStore(t)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				const team, channel = "TWRITER01", "CWRITER01"
				if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testCallerSlackID); err != nil {
					t.Fatal(err)
				}
				authorization, err := store.AuthorizePolicyWrite(ctx, team, testCallerSlackID)
				if err != nil {
					t.Fatal(err)
				}
				key := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel)}
				original := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel), attrPolicyInstallation: stringAttr(authorization.installation), attrAliasBindings: &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{"old": stringAttr("r_old")}}, attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"r_old"}}}
				if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(store.ChannelPoliciesName), Item: original}); err != nil {
					t.Fatal(err)
				}
				worker := authorization.Context(ctx)
				if mode == "missing authorization" {
					worker = ctx
				}
				store.Client = &policyCommitBoundaryDDB{Client: client, policyTable: store.ChannelPoliciesName, beforeCommit: func() {
					switch mode {
					case "binding deleted":
						if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
							t.Fatal(err)
						}
					case "admin removed":
						if err := store.RemoveAdmin(nativeWorkspaceOwnerContext(ctx, t, store, team), team, testCallerSlackID); err != nil {
							t.Fatal(err)
						}
					case "missing authorization":
						t.Fatal("unauthorized writer reached a commit")
					}
				}}
				switch operation {
				case "bind alias":
					err = store.BindChannelAlias(worker, team, channel, "new", "r_new")
				case "unbind alias":
					err = store.UnbindChannelAlias(worker, team, channel, "old")
				case "grant":
					err = store.ExposeResourceToChannel(worker, team, channel, "r_new")
				case "revoke grant":
					err = store.RevokeResourceFromChannel(worker, team, channel, "r_old")
				case "purge resource":
					_, err = store.PurgeResourceFromChannel(worker, team, channel, "r_old")
				}
				if mode == "allowed" {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					assertPolicyStatus(t, err, 409)
				}
				row, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(store.ChannelPoliciesName), Key: key, ConsistentRead: aws.Bool(true)})
				if err != nil {
					t.Fatal(err)
				}
				if mode != "allowed" {
					if !reflect.DeepEqual(row.Item, original) {
						t.Fatal("refused writer changed stored policy")
					}
					return
				}
				aliases := readStringMap(row.Item, attrAliasBindings)
				resources := readStringSet(row.Item, attrAllowedResourceIDs)
				switch operation {
				case "bind alias":
					if aliases["new"] != "r_new" || aliases["old"] != "r_old" {
						t.Fatal("alias bind lost configuration")
					}
				case "unbind alias":
					if len(aliases) != 0 || len(resources) != 1 {
						t.Fatal("alias unbind changed grant")
					}
				case "grant":
					if len(resources) != 2 || aliases["old"] != "r_old" {
						t.Fatal("grant lost configuration")
					}
				case "revoke grant":
					if len(resources) != 0 || aliases["old"] != "r_old" {
						t.Fatal("grant revoke changed alias")
					}
				case "purge resource":
					if len(resources) != 0 || len(aliases) != 0 {
						t.Fatal("purge left a dead reference")
					}
				}
			})
		}
	}
}
