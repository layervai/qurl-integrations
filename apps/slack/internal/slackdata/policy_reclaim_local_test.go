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

func TestPolicyReclaimDynamoDBLocal(t *testing.T) {
	for _, tagged := range []bool{false, true} {
		name := "untagged requires repair"
		if tagged {
			name = "tagged old grants retired"
		}
		t.Run(name, func(t *testing.T) {
			store, client := localPolicyStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			const team, channel = "TRECLAIM1", "CRECLAIM1"
			_, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(store.WorkspaceMappingsName), Item: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrOwnerID: stringAttr("auth0|pre-pivot"), attrPolicyGeneration: stringAttr("old-generation"), attrPolicyInstallation: stringAttr("old-installation"), attrPolicyLegacy: boolAttr(true)}})
			if err != nil {
				t.Fatal(err)
			}
			policy := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(team), attrSlackChannelID: stringAttr(channel), attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"r_previous"}}}
			if tagged {
				policy[attrPolicyInstallation] = stringAttr("old-installation")
			}
			if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(store.ChannelPoliciesName), Item: policy}); err != nil {
				t.Fatal(err)
			}
			if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: testOwnerSlackID}, testOwnerSlackID); err != nil {
				t.Fatal(err)
			}
			fresh, err := store.AuthorizePolicyWrite(ctx, team, testOwnerSlackID)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.installation == "old-installation" || fresh.legacyPolicies {
				t.Fatal("reclaim adopted prior authority")
			}
			err = store.ExposeResourceToChannel(fresh.Context(ctx), team, channel, "r_new")
			if tagged {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var typed *Error
				if !errors.As(err, &typed) || typed.Code != ErrCodePolicyReconciliationRequired {
					t.Fatalf("unknown ownership must require repair: %v", err)
				}
			}
			allowed, err := store.AllowedResourceIDsForChannel(ctx, team, channel)
			if err != nil {
				t.Fatal(err)
			}
			want := "r_previous"
			if tagged {
				want = "r_new"
			}
			if _, ok := allowed[want]; !ok || len(allowed) != 1 {
				t.Fatalf("wrong reclaim policy: %v", allowed)
			}
		})
	}
}
