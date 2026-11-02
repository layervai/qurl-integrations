package internal

import (
	"context"
	"log/slog"
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

type revokePolicyBoundaryDDB struct {
	*dynamodb.Client
	before func(int)
	calls  int
}

func (d *revokePolicyBoundaryDDB) TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, opts ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	d.calls++
	d.before(d.calls)
	return d.Client.TransactWriteItems(ctx, in, opts...)
}

func TestPolicyRevokeHandlerDynamoDBLocal(t *testing.T) {
	for _, mode := range []string{"one rebind", "two rebinds", "binding removed"} {
		t.Run(mode, func(t *testing.T) {
			client, workspace, policies := testddb.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			store, err := slackdata.NewStore(ctx, slackdata.WithDynamoDBClient(client), slackdata.WithTableNames(workspace, policies))
			if err != nil {
				t.Fatal(err)
			}
			const team, channel = "TRETRY001", "CRETRY001"
			if err := store.BindWorkspace(ctx, &slackdata.WorkspaceMapping{TeamID: team, OwnerID: testAdminOwnerID}, testAdminOwnerID); err != nil {
				t.Fatal(err)
			}
			actor, err := store.AuthorizePolicyWrite(ctx, team, testAdminOwnerID)
			if err != nil {
				t.Fatal(err)
			}
			ctx = actor.Context(ctx)
			for _, alias := range []string{"reuse", "second"} {
				if err := store.BindChannelAlias(ctx, team, channel, alias, "r_dead"); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.ExposeResourceToChannel(ctx, team, channel, "r_dead"); err != nil {
				t.Fatal(err)
			}
			db := &revokePolicyBoundaryDDB{Client: client}
			db.before = func(attempt int) {
				if mode == "binding removed" {
					if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
						t.Fatal(err)
					}
					return
				}
				if attempt > 1 && mode != "two rebinds" {
					return
				}
				alias := "reuse"
				if attempt == 2 {
					alias = "second"
				}
				_, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(policies), Key: map[string]ddbtypes.AttributeValue{"slack_team_id": &ddbtypes.AttributeValueMemberS{Value: team}, "slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: channel}}, UpdateExpression: aws.String("SET alias_bindings.#alias = :live"), ExpressionAttributeNames: map[string]string{"#alias": alias}, ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":live": &ddbtypes.AttributeValueMemberS{Value: "r_live"}}})
				if err != nil {
					t.Fatal(err)
				}
			}
			store.Client = db
			upstream := httptest.NewServer(http.NotFoundHandler())
			defer upstream.Close()
			h := newTestHandler(t, upstream)
			h.cfg.AdminStore = store
			h.purgeResourceBindings(ctx, slog.Default(), team, "r_dead")
			wantCalls := 2
			if mode == "binding removed" {
				wantCalls = 1
			}
			if db.calls != wantCalls {
				t.Fatalf("attempts=%d want=%d", db.calls, wantCalls)
			}
			allowed, err := store.AllowedResourceIDsForChannel(ctx, team, channel)
			if err != nil {
				t.Fatal(err)
			}
			_, dead := allowed["r_dead"]
			if dead != (mode != "one rebind") {
				t.Fatalf("dead grant retained=%v", dead)
			}
			if mode != "binding removed" {
				if _, live := allowed["r_live"]; !live {
					t.Fatal("live rebound alias lost")
				}
			}
		})
	}
}
