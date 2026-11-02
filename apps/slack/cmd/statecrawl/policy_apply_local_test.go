package main

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/apps/slack/internal/testddb"
)

type applyPolicyBoundaryDDB struct {
	*dynamodb.Client
	before    func()
	workspace string
	t         *testing.T
}

func (d *applyPolicyBoundaryDDB) GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if aws.ToString(in.TableName) == d.workspace && !aws.ToBool(in.ConsistentRead) {
		d.t.Fatal("operator authority used an eventual workspace read")
	}
	return d.Client.GetItem(ctx, in, opts...)
}
func (d *applyPolicyBoundaryDDB) TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, opts ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	if d.before != nil {
		hook := d.before
		d.before = nil
		hook()
	}
	return d.Client.TransactWriteItems(ctx, in, opts...)
}

func TestPolicyStatecrawlDynamoDBLocal(t *testing.T) {
	for _, mode := range []string{"current", "transfer at commit", "reinstall at commit", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			client, workspace, policies := testddb.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			store, err := slackdata.NewStore(ctx, slackdata.WithDynamoDBClient(client), slackdata.WithTableNames(workspace, policies))
			if err != nil {
				t.Fatal(err)
			}
			const team, channel, owner = "TAPPLY001", "CAPPLY001", "UOWNER001"
			bind := func() {
				t.Helper()
				if err := store.BindWorkspace(ctx, &slackdata.WorkspaceMapping{TeamID: team, OwnerID: owner}, owner); err != nil {
					t.Fatal(err)
				}
			}
			bind()
			actor, err := store.AuthorizePolicyWrite(ctx, team, owner)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.BindChannelAlias(actor.Context(ctx), team, channel, "dead", "r_dead"); err != nil {
				t.Fatal(err)
			}
			if err := store.BindChannelAlias(actor.Context(ctx), team, channel, "live", "r_live"); err != nil {
				t.Fatal(err)
			}
			db := &applyPolicyBoundaryDDB{Client: client, workspace: workspace, t: t}
			store.Client = db
			if mode == "unbound" {
				if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
					t.Fatal(err)
				}
			}
			db.before = func() {
				switch mode {
				case "transfer at commit":
					if err := store.TransferOwnership(actor.Context(ctx), team, owner, "UOTHER001"); err != nil {
						t.Fatal(err)
					}
				case "reinstall at commit":
					if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
						t.Fatal(err)
					}
					bind()
					fresh, err := store.AuthorizePolicyWrite(ctx, team, owner)
					if err != nil {
						t.Fatal(err)
					}
					if err := store.BindChannelAlias(fresh.Context(ctx), team, channel, "fresh", "r_fresh"); err != nil {
						t.Fatal(err)
					}
				}
			}
			rep := newReport(&flags{})
			if err := rep.applyPurge(ctx, store, discardLogger(), []purgeTarget{{teamID: team, channelID: channel, resourceID: "r_dead"}}); err != nil {
				t.Fatal(err)
			}
			wantPurged := int64(0)
			if mode == "current" {
				wantPurged = 1
			}
			if rep.stats.Purged.Load() != wantPurged || rep.stats.PurgeErrors.Load() != 1-wantPurged {
				t.Fatalf("apply counters: purged=%d errors=%d", rep.stats.Purged.Load(), rep.stats.PurgeErrors.Load())
			}
			allowed, err := store.AllowedResourceIDsForChannel(ctx, team, channel)
			if err != nil {
				t.Fatal(err)
			}
			_, dead := allowed["r_dead"]
			if dead != (mode == "transfer at commit" || mode == "unbound") {
				t.Fatalf("wrong dead grant state: %v", allowed)
			}
			keep := "r_live"
			if mode == "reinstall at commit" {
				keep = "r_fresh"
			}
			if _, ok := allowed[keep]; !ok {
				t.Fatalf("apply removed retained policy: %v", allowed)
			}
		})
	}
}
