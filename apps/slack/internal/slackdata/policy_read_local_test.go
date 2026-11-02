package slackdata

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestPolicyReadLineageDynamoDBLocal(t *testing.T) {
	for _, mode := range []string{"current", "reinstalled", "unbound", "pre-upgrade legacy", "initialized legacy", "modern untagged", "invalid policy identity", "partial binding"} {
		t.Run(mode, func(t *testing.T) {
			store, client := localPolicyStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			const team, channel = "TREAD001", "CREAD001"
			bind := func() {
				t.Helper()
				if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testOwnerSlackID); err != nil {
					t.Fatal(err)
				}
			}
			bind()
			actor, err := store.AuthorizePolicyWrite(ctx, team, testOwnerSlackID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ExposeResourceToChannel(actor.Context(ctx), team, channel, "r_previous"); err != nil {
				t.Fatal(err)
			}
			if err := store.BindChannelAlias(actor.Context(ctx), team, channel, "old", "r_previous"); err != nil {
				t.Fatal(err)
			}
			update := func(table, expression string, key map[string]ddbtypes.AttributeValue, values map[string]ddbtypes.AttributeValue) {
				t.Helper()
				if _, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(table), Key: key, UpdateExpression: aws.String(expression), ExpressionAttributeValues: values}); err != nil {
					t.Fatal(err)
				}
			}
			workspaceKey := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team)}
			policyKey := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel)}
			switch mode {
			case "reinstalled":
				if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
					t.Fatal(err)
				}
				bind()
			case "unbound":
				if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
					t.Fatal(err)
				}
			case "pre-upgrade legacy":
				update(store.WorkspaceMappingsName, "REMOVE policy_generation, policy_installation", workspaceKey, nil)
				update(store.ChannelPoliciesName, "REMOVE policy_installation", policyKey, nil)
			case "initialized legacy":
				update(store.WorkspaceMappingsName, "SET policy_legacy = :yes", workspaceKey, map[string]ddbtypes.AttributeValue{":yes": boolAttr(true)})
				update(store.ChannelPoliciesName, "REMOVE policy_installation", policyKey, nil)
			case "modern untagged":
				update(store.ChannelPoliciesName, "REMOVE policy_installation", policyKey, nil)
			case "invalid policy identity":
				update(store.ChannelPoliciesName, "SET policy_installation = :bad", policyKey, map[string]ddbtypes.AttributeValue{":bad": boolAttr(true)})
			case "partial binding":
				update(store.WorkspaceMappingsName, "REMOVE policy_generation", workspaceKey, nil)
			}
			wantAllowed := mode == "current" || mode == "pre-upgrade legacy" || mode == "initialized legacy"
			wantError := mode == "invalid policy identity" || mode == "partial binding"
			for name, read := range map[string]func() (bool, error){
				"allowed": func() (bool, error) {
					ids, err := store.AllowedResourceIDsForChannel(ctx, team, channel)
					_, ok := ids["r_previous"]
					return ok, err
				},
				"enumeration": func() (bool, error) {
					channels, err := store.ChannelsForResource(ctx, team, "r_previous")
					return len(channels) != 0, err
				},
				"resolve": func() (bool, error) { return store.ResolvePolicy(ctx, team, channel, "r_previous") },
				"alias": func() (bool, error) {
					_, ok, err := store.LookupChannelAlias(ctx, team, channel, "old")
					return ok, err
				},
				"entries": func() (bool, error) {
					entries, err := store.GetChannelPolicy(ctx, team, channel)
					return len(entries) != 0, err
				},
			} {
				t.Run(name, func(t *testing.T) {
					allowed, err := read()
					if (err != nil) != wantError || allowed != wantAllowed {
						t.Fatalf("allowed=%v error=%v want allowed=%v error=%v", allowed, err, wantAllowed, wantError)
					}
				})
			}
		})
	}
}
