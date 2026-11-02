package slackdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/testddb"
)

// Trigger deletion at the last write boundary, including a forbidden direct-write
// fallback. The actual commit and condition evaluation still run in DynamoDB.
type policyCommitBoundaryDDB struct {
	*dynamodb.Client
	beforeCommit func()
	policyTable  string
}

func (d *policyCommitBoundaryDDB) interleave() {
	if d.beforeCommit != nil {
		f := d.beforeCommit
		d.beforeCommit = nil
		f()
	}
}

func (d *policyCommitBoundaryDDB) TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, options ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	d.interleave()
	return d.Client.TransactWriteItems(ctx, in, options...)
}

func (d *policyCommitBoundaryDDB) UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, options ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if aws.ToString(in.TableName) == d.policyTable {
		d.interleave()
	}
	return d.Client.UpdateItem(ctx, in, options...)
}

func localPolicyStore(t *testing.T) (*Store, *dynamodb.Client) {
	t.Helper()
	client, workspaceTable, policyTable := testddb.New(t)
	store := &Store{Client: client, WorkspaceMappingsName: workspaceTable, ChannelPoliciesName: policyTable, Now: func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }}
	return store, client
}

func TestPolicyAuthorizationDynamoDBLocal(t *testing.T) {
	store, client := localPolicyStore(t)
	index := 0
	for _, scenario := range []string{"current admin", "deleted binding", "reinstall at same clock time", "revoked admin", "owner transferred away and back", "legacy binding", "ownership keeps installation", "delete at commit boundary", "alias conflict", "retired policy row", "untagged modern policy row", "legacy existing policy", "invalid policy lineage"} {
		t.Run(scenario, func(t *testing.T) {
			index++
			teamID := fmt.Sprintf("T%08d", index)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			bind := func() {
				t.Helper()
				if err := store.BindWorkspace(ctx, &WorkspaceMapping{TeamID: teamID, OwnerID: testOwnerSlackID}, testOwnerSlackID); err != nil {
					t.Fatal(err)
				}
				if err := store.AddAdmin(nativeWorkspaceOwnerContext(ctx, t, store, teamID), teamID, testCallerSlackID); err != nil {
					t.Fatal(err)
				}
			}
			bind()
			workspaceKey := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID)}
			if scenario == "legacy binding" || scenario == "legacy existing policy" {
				if _, err := client.UpdateItem(ctx, &dynamodb.UpdateItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: workspaceKey, UpdateExpression: aws.String("REMOVE policy_generation, policy_installation")}); err != nil {
					t.Fatal(err)
				}
			}
			gateCtx, cancelGate := context.WithCancel(ctx)
			authorization, err := store.AuthorizePolicyWrite(gateCtx, teamID, testCallerSlackID)
			cancelGate()
			if err != nil {
				t.Fatal(err)
			}
			workerCtx := authorization.Context(ctx)
			mutation := &dynamodb.UpdateItemInput{TableName: aws.String(store.ChannelPoliciesName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID), attrSlackChannelID: stringAttr("C00000001")}, UpdateExpression: aws.String("SET allowed_resource_ids = :resources"), ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":resources": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_old"}}}}
			wantConflict := false
			wantResources := []string{"r_old"}
			switch scenario {
			case "retired policy row", "untagged modern policy row", "legacy existing policy", "invalid policy lineage":
				item := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID), attrSlackChannelID: stringAttr("C00000001"), attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"r_prior"}}}
				if scenario == "retired policy row" {
					item[attrPolicyInstallation] = stringAttr("retired")
				}
				if scenario == "invalid policy lineage" {
					item[attrPolicyInstallation] = &ddbtypes.AttributeValueMemberBOOL{Value: true}
				}
				if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: mutation.TableName, Item: item}); err != nil {
					t.Fatal(err)
				}
				mutation.UpdateExpression = aws.String("SET updated_at = :now ADD allowed_resource_ids :resources")
				mutation.ExpressionAttributeValues[":now"] = stringAttr("2026-11-02T12:00:00Z")
				wantResources = []string{"r_prior"}
				wantConflict = true
				if scenario == "retired policy row" {
					wantConflict = false
					wantResources = []string{"r_old"}
				}
				if scenario == "legacy existing policy" {
					wantConflict = false
					wantResources = []string{"r_old", "r_prior"}
				}
			case "deleted binding":
				if err := store.DeleteWorkspaceMapping(ctx, teamID); err != nil {
					t.Fatal(err)
				}
				wantConflict = true
				wantResources = nil
			case "reinstall at same clock time":
				if err := store.DeleteWorkspaceMapping(ctx, teamID); err != nil {
					t.Fatal(err)
				}
				bind() // Identical owner, admin and clock; only the installation changes.
				fresh, err := store.AuthorizePolicyWrite(ctx, teamID, testCallerSlackID)
				if err != nil {
					t.Fatal(err)
				}
				if fresh.generation == authorization.generation || fresh.installation == authorization.installation {
					t.Fatal("reinstall reused its policy identity")
				}
				newMutation := *mutation
				newMutation.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{":resources": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_new"}}}
				if err := store.updatePolicy(fresh.Context(ctx), &newMutation); err != nil {
					t.Fatal(err)
				}
				wantConflict = true
				wantResources = []string{"r_new"}
			case "revoked admin":
				if err := store.RemoveAdmin(nativeWorkspaceOwnerContext(ctx, t, store, teamID), teamID, testCallerSlackID); err != nil {
					t.Fatal(err)
				}
				wantConflict = true
				wantResources = nil
			case "owner transferred away and back":
				if err := store.TransferOwnership(nativeWorkspaceOwnerContext(ctx, t, store, teamID), teamID, testOwnerSlackID, testOtherSlackID); err != nil {
					t.Fatal(err)
				}
				if err := store.TransferOwnership(nativeWorkspaceOwnerContext(ctx, t, store, teamID), teamID, testOtherSlackID, testOwnerSlackID); err != nil {
					t.Fatal(err)
				}
				wantConflict = true
				wantResources = nil
			case "ownership keeps installation":
				if err := store.updatePolicy(workerCtx, mutation); err != nil {
					t.Fatal(err)
				}
				if err := store.TransferOwnership(nativeWorkspaceOwnerContext(ctx, t, store, teamID), teamID, testOwnerSlackID, testOtherSlackID); err != nil {
					t.Fatal(err)
				}
				fresh, err := store.AuthorizePolicyWrite(ctx, teamID, testCallerSlackID)
				if err != nil {
					t.Fatal(err)
				}
				if fresh.installation != authorization.installation || fresh.generation == authorization.generation {
					t.Fatal("ownership transfer must keep installation lineage and rotate pending authorization")
				}
				workerCtx = fresh.Context(ctx)
				mutation.UpdateExpression = aws.String("SET updated_at = :now ADD allowed_resource_ids :resources")
				mutation.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{":now": stringAttr("2026-11-02T12:00:00Z"), ":resources": &ddbtypes.AttributeValueMemberSS{Value: []string{"r_new"}}}
				wantResources = []string{"r_old", "r_new"}
			case "legacy binding":
				row, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: workspaceKey, ConsistentRead: aws.Bool(true)})
				if err != nil {
					t.Fatal(err)
				}
				if readString(row.Item, attrPolicyGeneration) == "" || !reflect.DeepEqual(row.Item[attrUpdatedAtNano], unixNanoAttr(store.Now())) {
					t.Fatal("legacy generation missing or purge cutoff changed")
				}
			case "delete at commit boundary":
				boundary := &policyCommitBoundaryDDB{Client: client, policyTable: store.ChannelPoliciesName, beforeCommit: func() {
					if _, err := client.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: workspaceKey}); err != nil {
						t.Fatal(err)
					}
				}}
				store.Client = boundary
				t.Cleanup(func() { store.Client = client })
				wantConflict = true
				wantResources = nil
			case "alias conflict":
				mutation.ConditionExpression = aws.String("attribute_not_exists(allowed_resource_ids)")
				if err := store.updatePolicy(workerCtx, mutation); err != nil {
					t.Fatal(err)
				}
				err = store.updatePolicy(workerCtx, mutation)
				var condition *ddbtypes.ConditionalCheckFailedException
				if !errors.As(err, &condition) {
					t.Fatalf("policy conflict lost its type: %v", err)
				}
				return
			case "current admin":
				// gateCtx is canceled above; authority must work with the worker's live ctx.
			}
			err = store.updatePolicy(workerCtx, mutation)
			if wantConflict {
				assertPolicyStatus(t, err, http.StatusConflict)
				if scenario == "invalid policy lineage" {
					var se *Error
					if !errors.As(err, &se) || se.Code != ErrCodePolicyReconciliationRequired {
						t.Fatalf("malformed lineage did not request repair: %v", err)
					}
				}
			} else if err != nil {
				t.Fatal(err)
			}
			row, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: mutation.TableName, Key: mutation.Key, ConsistentRead: aws.Bool(true)})
			if err != nil {
				t.Fatal(err)
			}
			if wantResources == nil && len(row.Item) != 0 {
				t.Fatal("refused write materialized an orphan policy row")
			}
			if !wantConflict && readString(row.Item, attrPolicyInstallation) != authorization.installation {
				t.Fatal("successful policy write did not stamp its installation")
			}
			got := readStringSet(row.Item, attrAllowedResourceIDs)
			slices.Sort(got)
			slices.Sort(wantResources)
			if !reflect.DeepEqual(got, wantResources) {
				t.Fatalf("policy after %s = %v, want %v", scenario, got, wantResources)
			}
			store.Client = client
		})
	}
}

func nativeWorkspaceOwnerContext(ctx context.Context, t *testing.T, store *Store, teamID string) context.Context {
	t.Helper()
	owner, _, err := store.ListAdmins(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := store.AuthorizePolicyWrite(ctx, teamID, owner)
	if err != nil {
		t.Fatal(err)
	}
	return authorization.Context(ctx)
}
