package slackdata

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type noAtomicPolicyReadDDB struct{ DynamoDBClient }

func TestPolicyAtomicReadBoundary(t *testing.T) {
	for _, mode := range []string{"current", "captured current", "captured stale", "malformed context", "SDK failure", "nil snapshot", "partial snapshot", "unsupported client"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			stub := &stubDDB{getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				t.Fatal("atomic read fell back to GetItem")
				return nil, errors.New("forbidden fallback")
			}}
			store := newStore(stub)
			ctx := context.Background()
			if strings.HasPrefix(mode, "captured") {
				ctx = (&PolicyAuthorization{store: store, teamID: "T1", userID: testCallerSlackID, ownerID: testOwnerSlackID, generation: "original-generation", installation: "original-installation"}).Context(ctx)
			}
			if mode == "malformed context" {
				ctx = context.WithValue(ctx, policyAuthorizationContextKey{}, "invalid")
			}
			stub.transactGetItemsFn = func(in *dynamodb.TransactGetItemsInput) (*dynamodb.TransactGetItemsOutput, error) {
				calls++
				if len(in.TransactItems) != 2 || aws.ToString(in.TransactItems[0].Get.TableName) != "ws" || aws.ToString(in.TransactItems[1].Get.TableName) != "cp" {
					t.Fatal("wrong atomic read scope")
				}
				if strings.Contains(aws.ToString(in.TransactItems[0].Get.ProjectionExpression), "#admins") != (ctx.Value(policyAuthorizationContextKey{}) != nil) {
					t.Fatal("ordinary read loaded unnecessary admin roster")
				}
				if mode == "SDK failure" {
					return nil, errors.New("unavailable")
				}
				if mode == "nil snapshot" {
					return nil, nil //nolint:nilnil // Exercise a malformed SDK response.
				}
				if mode == "partial snapshot" {
					return &dynamodb.TransactGetItemsOutput{Responses: []ddbtypes.ItemResponse{{}}}, nil
				}
				binding := policyTestWorkspace()
				if mode == "captured stale" {
					binding[attrPolicyGeneration] = stringAttr("replacement")
				}
				return &dynamodb.TransactGetItemsOutput{Responses: []ddbtypes.ItemResponse{{Item: binding}, {Item: map[string]ddbtypes.AttributeValue{attrPolicyInstallation: stringAttr("original-installation"), attrAliasBindings: &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{"app": stringAttr("r_app")}}}}}}, nil
			}
			if mode == "unsupported client" {
				store.Client = &noAtomicPolicyReadDDB{DynamoDBClient: stub}
			}
			in := &dynamodb.GetItemInput{TableName: aws.String("cp"), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr("T1"), attrSlackChannelID: stringAttr("C1")}, ProjectionExpression: aws.String("#ab.#a"), ExpressionAttributeNames: map[string]string{"#ab": attrAliasBindings, "#a": "app"}}
			before := maps.Clone(in.ExpressionAttributeNames)
			out, err := store.readChannelPolicy(ctx, in)
			if mode == "current" || mode == "captured current" {
				if err != nil || readStringMap(out.Item, attrAliasBindings)["app"] != "r_app" {
					t.Fatalf("current read failed: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("invalid read succeeded")
				}
				if mode == "captured stale" || mode == "malformed context" {
					assertPolicyStatus(t, err, http.StatusConflict)
				}
			}
			want := 1
			if mode == "unsupported client" {
				want = 0
			}
			if calls != want {
				t.Fatalf("RPC calls=%d want=%d", calls, want)
			}
			if !reflect.DeepEqual(in.ExpressionAttributeNames, before) || aws.ToString(in.ProjectionExpression) != "#ab.#a" {
				t.Fatal("read changed caller projection")
			}
		})
	}
}
