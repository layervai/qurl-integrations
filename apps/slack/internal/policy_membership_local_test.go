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

type membershipBoundaryDDB struct {
	*dynamodb.Client
	beforeUpdate func()
}

func (d *membershipBoundaryDDB) UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, options ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if strings.Contains(aws.ToString(in.UpdateExpression), ":uids") && d.beforeUpdate != nil {
		interleave := d.beforeUpdate
		d.beforeUpdate = nil
		interleave()
	}
	return d.Client.UpdateItem(ctx, in, options...)
}

func TestPolicySignedMembershipDynamoDBLocal(t *testing.T) {
	for _, operation := range []string{"add", "remove"} {
		for _, mode := range []string{"unchanged", "caller removed", "reinstalled"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				client, workspaceTable, policyTable := testddb.New(t)
				boundary := &membershipBoundaryDDB{Client: client}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				store, err := slackdata.NewStore(ctx, slackdata.WithDynamoDBClient(boundary), slackdata.WithTableNames(workspaceTable, policyTable))
				if err != nil {
					t.Fatal(err)
				}
				const team = "TROLE001"
				if err := store.BindWorkspace(ctx, &slackdata.WorkspaceMapping{TeamID: team, OwnerID: testAdminOwnerID}, testAdminUserID); err != nil {
					t.Fatal(err)
				}
				owner, err := store.AuthorizePolicyWrite(ctx, team, testAdminOwnerID)
				if err != nil {
					t.Fatal(err)
				}
				if operation == "remove" {
					if err := store.AddAdmin(owner.Context(ctx), team, testTargetUserID); err != nil {
						t.Fatal(err)
					}
				}
				boundary.beforeUpdate = func() {
					switch mode {
					case "caller removed":
						if err := store.RemoveAdmin(owner.Context(ctx), team, testAdminUserID); err != nil {
							t.Error(err)
						}
					case "reinstalled":
						if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
							t.Error(err)
							return
						}
						if err := store.BindWorkspace(ctx, &slackdata.WorkspaceMapping{TeamID: team, OwnerID: "UOTHER001"}, "UOTHER001"); err != nil {
							t.Error(err)
							return
						}
						if operation == "remove" {
							fresh, err := store.AuthorizePolicyWrite(ctx, team, "UOTHER001")
							if err != nil {
								t.Error(err)
								return
							}
							if err := store.AddAdmin(fresh.Context(ctx), team, testTargetUserID); err != nil {
								t.Error(err)
							}
						}
					}
				}
				upstream := httptest.NewServer(http.NotFoundHandler())
				t.Cleanup(upstream.Close)
				h := newTestHandler(t, upstream)
				h.cfg.AdminStore = store
				status, reply := newAdminSlashInvoker(t, h).invokeAdmin(operation+" <@"+testTargetUserID+">", team, testAdminUserID)
				if status != http.StatusOK || boundary.beforeUpdate != nil {
					t.Fatalf("signed membership command missed commit boundary: status=%d reply=%q", status, reply)
				}
				row, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(workspaceTable), Key: map[string]ddbtypes.AttributeValue{"slack_team_id": &ddbtypes.AttributeValueMemberS{Value: team}}, ConsistentRead: aws.Bool(true)})
				if err != nil {
					t.Fatal(err)
				}
				admins, _ := row.Item["admin_slack_user_ids"].(*ddbtypes.AttributeValueMemberSS)
				found := false
				if admins != nil {
					for _, id := range admins.Value {
						found = found || id == testTargetUserID
					}
				}
				want := operation == "add"
				if mode != "unchanged" {
					want = !want
				}
				if found != want {
					t.Fatalf("stale membership command changed admin set: reply=%q", reply)
				}
			})
		}
	}
}
