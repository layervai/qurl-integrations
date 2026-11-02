package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/apps/slack/internal/testddb"
	"github.com/layervai/qurl-integrations/shared/client"
)

func TestPolicySignedHandlerDynamoDBLocal(t *testing.T) {
	for _, scenario := range []string{"unchanged", "admin removed during lookup", "workspace deleted during lookup", "reinstalled during lookup", "owner transferred away and back"} {
		t.Run(scenario, func(t *testing.T) {
			ddb, workspaceTable, policyTable := testddb.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			store, err := slackdata.NewStore(ctx, slackdata.WithDynamoDBClient(ddb), slackdata.WithTableNames(workspaceTable, policyTable))
			if err != nil {
				t.Fatal(err)
			}
			const team, channel = "TSIGNED01", "CSIGNED01"
			bind := func() error {
				return store.BindWorkspace(ctx, &slackdata.WorkspaceMapping{TeamID: team, OwnerID: testAdminOwnerID}, testAdminUserID)
			}
			if err := bind(); err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			mutationResult := make(chan error, 1)
			mutate := func() error {
				switch scenario {
				case "admin removed during lookup":
					return store.RemoveAdmin(testPolicyContext(ctx, t, store, team, testAdminOwnerID), team, testAdminUserID)
				case "workspace deleted during lookup":
					return store.DeleteWorkspaceMapping(ctx, team)
				case "reinstalled during lookup":
					if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
						return err
					}
					if err := bind(); err != nil {
						return err
					}
					authorization, err := store.AuthorizePolicyWrite(ctx, team, testAdminUserID)
					if err != nil {
						return err
					}
					return store.BindChannelAlias(authorization.Context(ctx), team, channel, "fresh", "r_fresh")
				case "owner transferred away and back":
					if err := store.TransferOwnership(testPolicyContext(ctx, t, store, team, testAdminOwnerID), team, testAdminOwnerID, "UOTHER001"); err != nil {
						return err
					}
					return store.TransferOwnership(testPolicyContext(ctx, t, store, team, "UOTHER001"), team, "UOTHER001", testAdminOwnerID)
				}
				return nil
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != testResourcesPath {
					http.Error(w, "unexpected lookup", http.StatusNotFound)
					return
				}
				once.Do(func() { mutationResult <- mutate() })
				respondQURLEnvelope(t, w, []map[string]any{{testKeyResourceID: "r_app", testKeyType: client.ResourceTypeTunnel, testKeySlug: "app", testKeyStatus: client.StatusActive}})
			}))
			t.Cleanup(upstream.Close)
			t.Setenv("QURL_API_KEY", "local-handler-key")
			h := newTestHandler(t, upstream)
			h.cfg.AdminStore = store
			h.SetAliasStore(store)
			status, ack, reply := newAdminSlashInvokerOnChannel(t, h, channel).invokeAdminAsync("set-alias $requested $app", team, testAdminUserID)
			if status != http.StatusOK || ack == "" {
				t.Fatalf("signed Slack request was not acknowledged: %d %q", status, ack)
			}
			select {
			case err := <-mutationResult:
				if err != nil {
					t.Fatalf("lookup interleave failed: %v", err)
				}
			default:
				t.Fatal("request did not reach actual upstream lookup")
			}
			row, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(policyTable), Key: map[string]ddbtypes.AttributeValue{"slack_team_id": &ddbtypes.AttributeValueMemberS{Value: team}, "slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: channel}}, ConsistentRead: aws.Bool(true)})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "unchanged" {
				aliases, valid := row.Item["alias_bindings"].(*ddbtypes.AttributeValueMemberM)
				var requested *ddbtypes.AttributeValueMemberS
				if valid {
					requested, _ = aliases.Value["requested"].(*ddbtypes.AttributeValueMemberS)
				}
				if !strings.Contains(reply, "now points to") || requested == nil || requested.Value != "r_app" {
					t.Fatalf("authorized signed request failed: %q", reply)
				}
				return
			}
			if !strings.Contains(reply, "Failed to update alias") {
				t.Fatalf("stale authorization reached a successful reply: %q", reply)
			}
			if scenario == "reinstalled during lookup" {
				aliases, ok := row.Item["alias_bindings"].(*ddbtypes.AttributeValueMemberM)
				var fresh *ddbtypes.AttributeValueMemberS
				if ok {
					fresh, _ = aliases.Value["fresh"].(*ddbtypes.AttributeValueMemberS)
				}
				if !ok || len(aliases.Value) != 1 || fresh == nil || fresh.Value != "r_fresh" {
					t.Fatalf("old worker changed reinstalled policy: %v", row.Item)
				}
			} else if len(row.Item) != 0 {
				t.Fatalf("stale worker materialized policy: %v", row.Item)
			}
		})
	}
}
