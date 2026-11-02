package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/apps/slack/internal/testddb"
)

func TestPolicySignedOwnerTransferDynamoDBLocal(t *testing.T) {
	for _, mode := range []string{"unchanged", "same-owner reinstall", "owner away and back"} {
		t.Run(mode, func(t *testing.T) {
			client, workspaceTable, policyTable := testddb.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			store, err := slackdata.NewStore(ctx, slackdata.WithDynamoDBClient(client), slackdata.WithTableNames(workspaceTable, policyTable))
			if err != nil {
				t.Fatal(err)
			}
			const team = "TOWNER001"
			bind := func() {
				t.Helper()
				if err := store.BindWorkspace(ctx, &slackdata.WorkspaceMapping{TeamID: team, OwnerID: testAdminOwnerID}, testAdminOwnerID); err != nil {
					t.Fatal(err)
				}
			}
			bind()
			upstream := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(upstream.Close)
			h := newTestHandler(t, upstream)
			h.cfg.AdminStore = store
			reached := false
			h.cfg.SlackUserLookup = func(_ context.Context, gotTeam, _, user string) (bool, error) {
				reached = true
				if gotTeam != team || user != testTargetUserID {
					t.Fatal("transfer lookup used wrong scope")
				}
				if mode == "same-owner reinstall" {
					if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
						return false, err
					}
					bind()
				}
				if mode == "owner away and back" {
					if err := store.TransferOwnership(testPolicyContext(ctx, t, store, team, testAdminOwnerID), team, testAdminOwnerID, "UOTHER001"); err != nil {
						return false, err
					}
					if err := store.TransferOwnership(testPolicyContext(ctx, t, store, team, "UOTHER001"), team, "UOTHER001", testAdminOwnerID); err != nil {
						return false, err
					}
				}
				return true, nil
			}
			status, reply := newAdminSlashInvoker(t, h).invokeAdmin("transfer-ownership <@"+testTargetUserID+">", team, testAdminOwnerID)
			if status != http.StatusOK || !reached {
				t.Fatalf("signed transfer missed lookup boundary: %d %q", status, reply)
			}
			row, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(workspaceTable), Key: map[string]ddbtypes.AttributeValue{"slack_team_id": &ddbtypes.AttributeValueMemberS{Value: team}}, ConsistentRead: aws.Bool(true)})
			if err != nil {
				t.Fatal(err)
			}
			owner, _ := row.Item["owner_id"].(*ddbtypes.AttributeValueMemberS)
			want := testAdminOwnerID
			if mode == "unchanged" {
				want = testTargetUserID
			}
			if owner == nil || owner.Value != want {
				t.Fatalf("stale owner command transferred a changed installation: %q", reply)
			}
			if mode != "unchanged" && !strings.Contains(reply, "Workspace permissions changed") {
				t.Fatal("stale transfer reported success")
			}
		})
	}
}
