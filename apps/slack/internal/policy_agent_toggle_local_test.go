package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/apps/slack/internal/testddb"
)

func TestPolicySignedAgentToggleDynamoDBLocal(t *testing.T) {
	for _, mode := range []string{"unchanged", "caller removed", "reinstalled"} {
		t.Run(mode, func(t *testing.T) {
			client, workspaceTable, policyTable := testddb.New(t)
			boundary := &workspaceMutationBoundaryDDB{Client: client}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			store, err := slackdata.NewStore(ctx, slackdata.WithDynamoDBClient(boundary), slackdata.WithTableNames(workspaceTable, policyTable))
			if err != nil {
				t.Fatal(err)
			}
			const team = "TTOGGLE01"
			if err := store.BindWorkspace(ctx, &slackdata.WorkspaceMapping{TeamID: team, OwnerID: testAdminOwnerID}, testAdminUserID); err != nil {
				t.Fatal(err)
			}
			owner, err := store.AuthorizePolicyWrite(ctx, team, testAdminOwnerID)
			if err != nil {
				t.Fatal(err)
			}
			boundary.beforeUpdate = func() {
				if mode == "caller removed" {
					if err := store.RemoveAdmin(owner.Context(ctx), team, testAdminUserID); err != nil {
						t.Error(err)
					}
				}
				if mode == "reinstalled" {
					if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
						t.Error(err)
						return
					}
					if err := store.BindWorkspace(ctx, &slackdata.WorkspaceMapping{TeamID: team, OwnerID: "UOTHER001"}, "UOTHER001"); err != nil {
						t.Error(err)
					}
				}
			}
			upstream := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(upstream.Close)
			h := newTestHandler(t, upstream)
			h.cfg.AdminStore = store
			status, reply := newAdminSlashInvoker(t, h).invokeAdmin("agent on", team, testAdminUserID)
			if status != http.StatusOK || boundary.beforeUpdate != nil {
				t.Fatalf("toggle missed its commit boundary: %d %q", status, reply)
			}
			row, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(workspaceTable), Key: map[string]ddbtypes.AttributeValue{"slack_team_id": &ddbtypes.AttributeValueMemberS{Value: team}}, ConsistentRead: aws.Bool(true)})
			if err != nil {
				t.Fatal(err)
			}
			value, _ := row.Item["agent_enabled"].(*ddbtypes.AttributeValueMemberBOOL)
			enabled := value != nil && value.Value
			if enabled != (mode == "unchanged") {
				t.Fatalf("stale toggle changed agent admission: %q", reply)
			}
		})
	}
}
