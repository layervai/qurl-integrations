package internal

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestPolicySignedLifecycleDynamoDBLocal(t *testing.T) {
	for _, scenario := range []string{"late write before worker", "reinstall before worker", "unknown legacy retains delivery"} {
		t.Run(scenario, func(t *testing.T) {
			client, workspaceTable, policyTable := testddb.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			store, err := slackdata.NewStore(ctx, slackdata.WithDynamoDBClient(client), slackdata.WithTableNames(workspaceTable, policyTable))
			if err != nil {
				t.Fatal(err)
			}
			now := fixedNow.Add(-time.Minute)
			store.Now = func() time.Time { return now }
			const team = "TLIFEC01"
			bind := func() {
				t.Helper()
				if err := store.BindWorkspace(ctx, &slackdata.WorkspaceMapping{TeamID: team, OwnerID: testAdminOwnerID}, testAdminOwnerID); err != nil {
					t.Fatal(err)
				}
			}
			bind()
			old, err := store.AuthorizePolicyWrite(ctx, team, testAdminOwnerID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ExposeResourceToChannel(old.Context(ctx), team, "COLD", "r_existing"); err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(upstream.Close)
			h := newTestHandler(t, upstream)
			h.cfg.AdminStore = store
			h.cfg.AgentStore = &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: "local-agent-state"}
			provider := &recordingStateDeleter{recordingAuthProvider: recordingAuthProvider{apiKey: "local-test-key"}}
			h.cfg.AuthProvider = provider
			var persisted []byte
			queue := &lifecycleQueueStub{enqueue: func(_ context.Context, job *LifecyclePurgeJob) error {
				var err error
				persisted, err = json.Marshal(job)
				return err
			}}
			h.cfg.LifecyclePurgeQueue = queue
			body := fmt.Sprintf(`{"type":"event_callback","team_id":%q,"event_id":"EvNativePurge","event_time":%d,"event":{"type":"app_uninstalled"}}`, team, fixedNow.Unix())
			response := httptest.NewRecorder()
			h.ServeHTTP(response, newSignedRequest(t, pathSlackEvents, body, body))
			if response.Code != http.StatusOK || len(persisted) == 0 || provider.deleteStateCalls != 0 {
				t.Fatalf("teardown did not persist before ACK: %d", response.Code)
			}
			var job LifecyclePurgeJob
			if err := json.Unmarshal(persisted, &job); err != nil {
				t.Fatal(err)
			}
			if len(job.WorkspaceIDs) != 1 || job.WorkspaceIDs[0] != team || !job.Cutoff.Equal(time.Unix(fixedNow.Unix(), 0)) {
				t.Fatal("durable handoff lost original scope or cutoff")
			}
			now = fixedNow.Add(time.Second)
			if scenario == "late write before worker" {
				if err := store.AddAdmin(old.Context(ctx), team, testTargetUserID); err != nil {
					t.Fatal(err)
				}
				if err := store.ExposeResourceToChannel(old.Context(ctx), team, "COLD", "r_late"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
					t.Fatal(err)
				}
				bind()
				fresh, err := store.AuthorizePolicyWrite(ctx, team, testAdminOwnerID)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.ExposeResourceToChannel(fresh.Context(ctx), team, "CFRESH", "r_fresh"); err != nil {
					t.Fatal(err)
				}
				if scenario == "unknown legacy retains delivery" {
					if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(policyTable), Item: map[string]ddbtypes.AttributeValue{"slack_team_id": &ddbtypes.AttributeValueMemberS{Value: team}, "slack_channel_id": &ddbtypes.AttributeValueMemberS{Value: "CUNKNOWN"}, "allowed_resource_ids": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_unknown"}}}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			workerCtx, stopWorker := context.WithTimeout(ctx, 5*time.Second)
			defer stopWorker()
			received, deleted := false, 0
			queue.receive = func(ctx context.Context) (*LifecyclePurgeDelivery, error) {
				if received {
					stopWorker()
					return nil, ctx.Err()
				}
				received = true
				return &LifecyclePurgeDelivery{Job: &job, Receipt: "local-receipt"}, nil
			}
			queue.delete = func(_ context.Context, receipt string) error {
				if receipt != "local-receipt" {
					t.Error("wrong receipt")
				}
				deleted++
				stopWorker()
				return nil
			}
			h.runLifecyclePurgeWorker(workerCtx)
			if scenario == "unknown legacy retains delivery" {
				if deleted != 0 {
					t.Fatal("incomplete legacy cleanup acknowledged its delivery")
				}
				unknown, err := store.AllowedResourceIDsForChannel(ctx, team, "CUNKNOWN")
				if err != nil || len(unknown) != 1 {
					t.Fatalf("ambiguous policy was lost: %v", err)
				}
			} else if deleted != 1 {
				t.Fatal("completed cleanup did not acknowledge delivery")
			}
			oldRows, err := store.AllowedResourceIDsForChannel(ctx, team, "COLD")
			if err != nil || len(oldRows) != 0 {
				t.Fatalf("completed worker retained old policies: %v %v", oldRows, err)
			}
			if scenario != "late write before worker" {
				freshRows, err := store.AllowedResourceIDsForChannel(ctx, team, "CFRESH")
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := freshRows["r_fresh"]; !ok {
					t.Fatal("old delivery removed new installation policy")
				}
			}
			if err := store.ExposeResourceToChannel(old.Context(ctx), team, "CAFTER", "r_after"); err == nil {
				t.Fatal("old authorization wrote after lifecycle completion")
			}
			after, err := store.AllowedResourceIDsForChannel(ctx, team, "CAFTER")
			if err != nil || len(after) != 0 {
				t.Fatal("old worker recreated policy after cleanup")
			}
		})
	}
}
