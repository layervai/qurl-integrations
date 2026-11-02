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

func TestPolicyAtomicReadConflictRetry(t *testing.T) {
	for _, tc := range []struct {
		name                string
		codes               []string
		failures, wantCalls int
		canceled            bool
	}{
		{"one conflict", []string{"TransactionConflict", "None"}, 1, 2, false},
		{"both conflict", []string{"TransactionConflict", "TransactionConflict"}, 2, 3, false},
		{"exhausted", []string{"None", "TransactionConflict"}, 4, 3, false},
		{"mixed failure", []string{"TransactionConflict", "ValidationError"}, 1, 1, false},
		{"missing reason", []string{"TransactionConflict"}, 1, 1, false},
		{"empty reasons", nil, 1, 1, false},
		{"no conflict", []string{"None", "None"}, 1, 1, false},
		{"canceled wait", []string{"TransactionConflict", "None"}, 1, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			stub := &stubDDB{transactGetItemsFn: func(*dynamodb.TransactGetItemsInput) (*dynamodb.TransactGetItemsOutput, error) {
				calls++
				if calls <= tc.failures {
					reasons := make([]ddbtypes.CancellationReason, len(tc.codes))
					for i, code := range tc.codes {
						reasons[i].Code = aws.String(code)
					}
					if tc.canceled {
						cancel()
					}
					return nil, &ddbtypes.TransactionCanceledException{CancellationReasons: reasons}
				}
				return &dynamodb.TransactGetItemsOutput{Responses: []ddbtypes.ItemResponse{{Item: policyTestWorkspace()}, {Item: map[string]ddbtypes.AttributeValue{attrPolicyInstallation: stringAttr("original-installation")}}}}, nil
			}}
			store := newStore(stub)
			_, err := store.readChannelPolicy(ctx, &dynamodb.GetItemInput{TableName: aws.String("cp"), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr("T1"), attrSlackChannelID: stringAttr("C1")}})
			if calls != tc.wantCalls {
				t.Fatalf("calls=%d want=%d", calls, tc.wantCalls)
			}
			wantSuccess := tc.wantCalls > tc.failures
			if (err == nil) != wantSuccess {
				t.Fatalf("success=%t want=%t: %v", err == nil, wantSuccess, err)
			}
		})
	}
}

func TestPolicyMalformedLineageRequiresRepair(t *testing.T) {
	err := policyTransactionError(&ddbtypes.TransactionCanceledException{CancellationReasons: []ddbtypes.CancellationReason{
		{Code: aws.String("None")},
		{Code: aws.String(policyConditionFailed), Item: map[string]ddbtypes.AttributeValue{attrPolicyInstallation: &ddbtypes.AttributeValueMemberN{Value: "1"}}},
	}}, &PolicyAuthorization{})
	var se *Error
	if !errors.As(err, &se) || se.Code != ErrCodePolicyReconciliationRequired {
		t.Fatalf("wrong repair error: %v", err)
	}
}

func TestPolicyWorkspaceUpdateWithoutCallerCondition(t *testing.T) {
	calls := 0
	store := newStore(&stubDDB{updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		calls++
		condition := aws.ToString(in.ConditionExpression)
		if strings.Contains(condition, "()") || !strings.Contains(condition, "policy_installation = :authorized_installation") {
			t.Fatalf("invalid authority condition: %s", condition)
		}
		return &dynamodb.UpdateItemOutput{}, nil
	}})
	authorization := PolicyAuthorization{store: store, teamID: "T1", userID: testCallerSlackID, ownerID: testOwnerSlackID, generation: "generation", installation: "installation"}
	in := &dynamodb.UpdateItemInput{TableName: aws.String("ws"), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr("T1")}, UpdateExpression: aws.String("REMOVE agent_enabled")}
	if err := store.updateAuthorizedWorkspace(authorization.Context(context.Background()), in); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || in.ConditionExpression != nil || in.ExpressionAttributeValues != nil {
		t.Fatal("update mutated caller input or skipped authority")
	}
}
