package slackdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type policyTestDDB struct {
	*stubDDB
	transact func(*dynamodb.TransactWriteItemsInput) error
}

func (d *policyTestDDB) TransactWriteItems(_ context.Context, in *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	if d.transact == nil {
		return nil, errors.New("unexpected policy transaction")
	}
	return &dynamodb.TransactWriteItemsOutput{}, d.transact(in)
}

func policyTestWorkspace() map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		attrOwnerID: stringAttr(testOwnerSlackID), attrPolicyGeneration: stringAttr("original-generation"), attrPolicyInstallation: stringAttr("original-installation"),
		attrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{testCallerSlackID}},
	}
}

func assertPolicyStatus(t *testing.T, err error, status int) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed.StatusCode != status {
		t.Fatalf("error = %v, want status %d", err, status)
	}
}

func TestAuthorizePolicyWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller string
		mutate func(map[string]ddbtypes.AttributeValue)
		status int
	}{
		{name: "owner", caller: testOwnerSlackID},
		{name: "admin", caller: testCallerSlackID},
		{name: "stranger", caller: testOtherSlackID, status: http.StatusForbidden},
		{name: "partial identity", caller: testCallerSlackID, mutate: func(m map[string]ddbtypes.AttributeValue) { delete(m, attrPolicyInstallation) }, status: http.StatusInternalServerError},
		{name: "invalid migration flag", caller: testCallerSlackID, mutate: func(m map[string]ddbtypes.AttributeValue) { m[attrPolicyLegacy] = stringAttr("true") }, status: http.StatusInternalServerError},
		{name: "missing binding", caller: testCallerSlackID, mutate: func(m map[string]ddbtypes.AttributeValue) { clear(m) }, status: http.StatusForbidden},
		{name: "empty generation", caller: testCallerSlackID, mutate: func(m map[string]ddbtypes.AttributeValue) { m[attrPolicyGeneration] = stringAttr("") }, status: http.StatusInternalServerError},
		{name: "wrong generation type", caller: testCallerSlackID, mutate: func(m map[string]ddbtypes.AttributeValue) {
			m[attrPolicyGeneration] = &ddbtypes.AttributeValueMemberBOOL{Value: true}
		}, status: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := policyTestWorkspace()
			if tc.mutate != nil {
				tc.mutate(item)
			}
			store := newStore(&stubDDB{getItemFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				if !aws.ToBool(in.ConsistentRead) || aws.ToString(in.TableName) != "ws" || readString(in.Key, attrSlackTeamID) != "T1" {
					t.Fatal("authorization must strongly read the correct workspace")
				}
				return &dynamodb.GetItemOutput{Item: item}, nil
			}, updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
				t.Fatal("modern or rejected binding must not be rewritten")
				return nil, errors.New("unexpected unguarded update")
			}})
			authorization, err := store.AuthorizePolicyWrite(context.Background(), "T1", tc.caller)
			if tc.status != 0 {
				assertPolicyStatus(t, err, tc.status)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if authorization.store != store || authorization.teamID != "T1" || authorization.userID != tc.caller || authorization.ownerID != testOwnerSlackID || authorization.generation != "original-generation" || authorization.installation != "original-installation" {
				t.Fatalf("wrong authorization snapshot: %+v", authorization)
			}
		})
	}
}

func TestAuthorizePolicyWriteInitializesLegacyBindingWithoutMovingPurgeCutoff(t *testing.T) {
	item := policyTestWorkspace()
	delete(item, attrPolicyGeneration)
	delete(item, attrPolicyInstallation)
	calls := 0
	store := newStore(&stubDDB{getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: item}, nil
	}, updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		calls++
		if aws.ToString(in.UpdateExpression) != "SET #generation = :generation, #installation = :installation, #legacy = :legacy" || in.ReturnValues != ddbtypes.ReturnValueAllNew {
			t.Fatal("legacy initialization must only set and return its policy identity")
		}
		if !strings.Contains(aws.ToString(in.ConditionExpression), "attribute_not_exists(#generation)") || !strings.Contains(aws.ToString(in.ConditionExpression), "contains(#admins, :caller)") || !strings.Contains(aws.ToString(in.ConditionExpression), "#owner = :owner") {
			t.Fatal("legacy initialization must not overwrite a newer installation or bypass current authority")
		}
		generation := readString(in.ExpressionAttributeValues, ":generation")
		if generation == "" {
			t.Fatal("missing new generation")
		}
		return &dynamodb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{attrPolicyGeneration: stringAttr(generation), attrPolicyInstallation: in.ExpressionAttributeValues[":installation"], attrPolicyLegacy: &ddbtypes.AttributeValueMemberBOOL{Value: true}}}, nil
	}})
	authorization, err := store.AuthorizePolicyWrite(context.Background(), "T1", testCallerSlackID)
	if err != nil || calls != 1 || authorization.generation == "" || authorization.installation == "" || !authorization.legacyPolicies {
		t.Fatalf("legacy authorization: %+v calls=%d err=%v", authorization, calls, err)
	}
}

func TestAuthorizePolicyWriteDoesNotAdoptConcurrentReinstall(t *testing.T) {
	item := policyTestWorkspace()
	delete(item, attrPolicyGeneration)
	delete(item, attrPolicyInstallation)
	reads := 0
	store := newStore(&stubDDB{getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		reads++
		return &dynamodb.GetItemOutput{Item: item}, nil
	}, updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		return nil, &ddbtypes.ConditionalCheckFailedException{}
	}})
	_, err := store.AuthorizePolicyWrite(context.Background(), "T1", testCallerSlackID)
	assertPolicyStatus(t, err, http.StatusConflict)
	if reads != 1 {
		t.Fatalf("silently retried authorization after installation changed: reads=%d", reads)
	}
}

func TestPolicyUpdateIsAtomicAndPreservesMutationCondition(t *testing.T) {
	var transaction *dynamodb.TransactWriteItemsInput
	client := &policyTestDDB{stubDDB: &stubDDB{getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: policyTestWorkspace()}, nil
	}, updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		t.Fatal("unguarded policy write")
		return nil, errors.New("unexpected unguarded update")
	}}, transact: func(in *dynamodb.TransactWriteItemsInput) error { transaction = in; return nil }}
	store := newStore(client)
	authorization, err := store.AuthorizePolicyWrite(context.Background(), "T1", testCallerSlackID)
	if err != nil {
		t.Fatal(err)
	}
	mutation := &dynamodb.UpdateItemInput{TableName: aws.String("cp"), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr("T1"), attrSlackChannelID: stringAttr("C1")}, UpdateExpression: aws.String("SET #alias = :resource"), ConditionExpression: aws.String("attribute_not_exists(#alias)"), ExpressionAttributeNames: map[string]string{"#alias": "alias_bindings"}, ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":resource": stringAttr("r_1")}}
	if err := store.updatePolicy(authorization.Context(context.Background()), mutation); err != nil {
		t.Fatal(err)
	}
	if transaction == nil || len(transaction.TransactItems) != 2 || aws.ToString(transaction.ClientRequestToken) == "" {
		t.Fatal("policy update must use a two-item idempotent transaction")
	}
	check := transaction.TransactItems[0].ConditionCheck
	if check == nil || aws.ToString(check.TableName) != "ws" || readString(check.Key, attrSlackTeamID) != "T1" {
		t.Fatal("wrong workspace condition")
	}
	for _, required := range []string{"#owner = :owner", "contains(#admins, :caller)", "#generation = :generation", "#installation = :installation"} {
		if !strings.Contains(aws.ToString(check.ConditionExpression), required) {
			t.Fatalf("missing authority fence %q", required)
		}
	}
	if readString(check.ExpressionAttributeValues, ":owner") != testOwnerSlackID || readString(check.ExpressionAttributeValues, ":caller") != testCallerSlackID || readString(check.ExpressionAttributeValues, ":generation") != "original-generation" {
		t.Fatal("transaction lost captured authority")
	}
	update := transaction.TransactItems[1].Update
	if update == nil || aws.ToString(update.TableName) != "cp" || !reflect.DeepEqual(update.Key, mutation.Key) ||
		aws.ToString(update.UpdateExpression) != "SET #policy_lineage = :policy_lineage, #alias = :resource" ||
		aws.ToString(update.ConditionExpression) != "(attribute_not_exists(#policy_team) OR #policy_lineage = :policy_lineage) AND (attribute_not_exists(#alias))" ||
		update.ExpressionAttributeNames["#alias"] != "alias_bindings" || readString(update.ExpressionAttributeValues, ":resource") != "r_1" ||
		readString(update.ExpressionAttributeValues, ":policy_lineage") != authorization.installation || update.ReturnValuesOnConditionCheckFailure != ddbtypes.ReturnValuesOnConditionCheckFailureAllOld {
		t.Fatal("transaction lost mutation semantics or installation condition")
	}
	if len(mutation.ExpressionAttributeNames) != 1 || len(mutation.ExpressionAttributeValues) != 1 || aws.ToString(mutation.UpdateExpression) != "SET #alias = :resource" || aws.ToString(mutation.ConditionExpression) != "attribute_not_exists(#alias)" {
		t.Fatal("transaction modified its caller's reusable input")
	}
}

func TestPolicyUpdateFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		kind          string
		reason        []ddbtypes.CancellationReason
		wantCondition bool
		wantStatus    int
	}{
		{name: "missing authorization", kind: "missing", wantStatus: http.StatusConflict},
		{name: "another team", kind: "team", wantStatus: http.StatusConflict},
		{name: "another store", kind: "store", wantStatus: http.StatusConflict},
		{name: "wrong table", kind: "table", wantStatus: http.StatusConflict},
		{name: "unsupported client", kind: "unsupported", wantStatus: http.StatusInternalServerError},
		{name: "unsupported return values", kind: "return", wantStatus: http.StatusInternalServerError},
		{name: "unsupported legacy condition", kind: "legacy-condition", wantStatus: http.StatusInternalServerError},
		{name: "changed installation or role", reason: []ddbtypes.CancellationReason{{Code: aws.String("ConditionalCheckFailed")}, {Code: aws.String("None")}}, wantStatus: http.StatusConflict},
		{name: "alias conflict", reason: []ddbtypes.CancellationReason{{Code: aws.String("None")}, {Code: aws.String("ConditionalCheckFailed")}}, wantCondition: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &policyTestDDB{stubDDB: &stubDDB{getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: policyTestWorkspace()}, nil
			}, updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
				t.Fatal("must not fall back to unguarded update")
				return nil, errors.New("unexpected unguarded update")
			}}, transact: func(*dynamodb.TransactWriteItemsInput) error {
				calls++
				return &ddbtypes.TransactionCanceledException{CancellationReasons: tc.reason}
			}}
			store := newStore(client)
			authorization, err := store.AuthorizePolicyWrite(context.Background(), "T1", testCallerSlackID)
			if err != nil {
				t.Fatal(err)
			}
			ctx := authorization.Context(context.Background())
			mutation := &dynamodb.UpdateItemInput{TableName: aws.String("cp"), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr("T1"), attrSlackChannelID: stringAttr("C1")}, UpdateExpression: aws.String("SET x = :v")}
			switch tc.kind {
			case "missing":
				ctx = context.Background()
			case "team":
				mutation.Key[attrSlackTeamID] = stringAttr("T2")
			case "store":
				copyStore := *store
				store = &copyStore
			case "table":
				mutation.TableName = aws.String("wrong")
			case "unsupported":
				store.Client = client.stubDDB
			case "legacy-condition":
				mutation.Expected = map[string]ddbtypes.ExpectedAttributeValue{"grant": {Exists: aws.Bool(false)}}
			case "return":
				mutation.ReturnValues = ddbtypes.ReturnValueAllOld
			}
			err = store.updatePolicy(ctx, mutation)
			if tc.wantCondition {
				var condition *ddbtypes.ConditionalCheckFailedException
				if !errors.As(err, &condition) {
					t.Fatalf("alias conflict lost its type: %v", err)
				}
			} else {
				assertPolicyStatus(t, err, tc.wantStatus)
			}
			expected := 0
			if tc.reason != nil {
				expected = 1
			}
			if calls != expected {
				t.Fatalf("transaction calls=%d want %d", calls, expected)
			}
		})
	}
}

func TestAuthorizePolicyWriteRequiresConfirmedLegacyGeneration(t *testing.T) {
	item := policyTestWorkspace()
	delete(item, attrPolicyGeneration)
	delete(item, attrPolicyInstallation)
	store := newStore(&stubDDB{
		getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
		updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			return &dynamodb.UpdateItemOutput{}, nil
		},
	})
	authorization, err := store.AuthorizePolicyWrite(context.Background(), "T1", testCallerSlackID)
	assertPolicyStatus(t, err, http.StatusInternalServerError)
	if authorization.store != nil {
		t.Fatal("unconfirmed initialization returned usable authority")
	}
}

func TestPolicyUpdatePreservesUnknownFailureWithoutFallback(t *testing.T) {
	failure := errors.New("transaction transport interrupted")
	client := &policyTestDDB{stubDDB: &stubDDB{
		getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: policyTestWorkspace()}, nil
		},
		updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			t.Fatal("unguarded fallback after transaction failure")
			return nil, failure
		},
	}, transact: func(*dynamodb.TransactWriteItemsInput) error { return failure }}
	store := newStore(client)
	authorization, err := store.AuthorizePolicyWrite(context.Background(), "T1", testCallerSlackID)
	if err != nil {
		t.Fatal(err)
	}
	err = store.updatePolicy(authorization.Context(context.Background()), &dynamodb.UpdateItemInput{TableName: aws.String("cp"), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr("T1")}, UpdateExpression: aws.String("SET x = :v")})
	if !errors.Is(err, failure) {
		t.Fatalf("transaction failure was lost or misclassified: %v", err)
	}
}

func TestPolicyWorkspaceConflictSurvivesStoreErrorMapping(t *testing.T) {
	err := ddbToError("BindChannelAlias", policyWorkspaceChanged())
	assertPolicyStatus(t, err, http.StatusConflict)
	var mapped *Error
	if !errors.As(err, &mapped) || mapped.Code != ErrCodePolicyWorkspaceChanged {
		t.Fatalf("workspace conflict lost its code: %v", err)
	}
}

func TestPolicyRetiredRowRecoveryStopsOnUnknownFailure(t *testing.T) {
	for _, failAt := range []int{2, 3} {
		t.Run(fmt.Sprintf("transaction_%d", failAt), func(t *testing.T) {
			failure := errors.New("commit response interrupted")
			calls := 0
			tokens := map[string]bool{}
			client := &policyTestDDB{stubDDB: &stubDDB{}, transact: func(in *dynamodb.TransactWriteItemsInput) error {
				calls++
				token := aws.ToString(in.ClientRequestToken)
				if token == "" || tokens[token] {
					t.Fatal("recovery reused a transaction identity")
				}
				tokens[token] = true
				if calls == 1 {
					return &ddbtypes.TransactionCanceledException{CancellationReasons: []ddbtypes.CancellationReason{{Code: aws.String("None")}, {Code: aws.String(policyConditionFailed), Item: map[string]ddbtypes.AttributeValue{attrPolicyInstallation: stringAttr("retired")}}}}
				}
				if calls == failAt {
					return failure
				}
				return nil
			}}
			store := newStore(client)
			authorization := PolicyAuthorization{store: store, teamID: "T1", userID: testCallerSlackID, ownerID: testOwnerSlackID, generation: "current", installation: "current"}
			err := store.ExposeResourceToChannel(authorization.Context(context.Background()), "T1", "C1", "r_requested")
			if !errors.Is(err, failure) || calls != failAt {
				t.Fatalf("unknown failure was retried or lost: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestWorkspaceWritesRequireCapturedAuthority(t *testing.T) {
	for _, operation := range []string{"add", "remove", "toggle", "transfer"} {
		t.Run(operation, func(t *testing.T) {
			store := newStore(&stubDDB{getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: policyTestWorkspace()}, nil
			}, updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
				t.Fatal("membership write without captured authority")
				return nil, errors.New("unexpected unauthorized membership write")
			}})
			var err error
			switch operation {
			case "add":
				err = store.AddAdmin(context.Background(), "T1", testOtherSlackID)
			case "remove":
				err = store.RemoveAdmin(context.Background(), "T1", testOtherSlackID)
			case "toggle":
				err = store.SetAgentEnabled(context.Background(), "T1", true)
			case "transfer":
				err = store.TransferOwnership(context.Background(), "T1", testOwnerSlackID, testOtherSlackID)
			}
			assertPolicyStatus(t, err, http.StatusConflict)
		})
	}
}

func TestTransferRequiresCapturedOwnerRole(t *testing.T) {
	store := newStore(&stubDDB{updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		t.Fatal("non-owner transferred ownership")
		return nil, errors.New("unexpected transfer")
	}})
	authorization := PolicyAuthorization{store: store, teamID: "T1", userID: testCallerSlackID, ownerID: testOwnerSlackID, generation: "current", installation: "current"}
	if authorization.IsOwner() {
		t.Fatal("admin snapshot confers owner role")
	}
	assertPolicyStatus(t, store.TransferOwnership(authorization.Context(context.Background()), "T1", testOwnerSlackID, testOtherSlackID), http.StatusConflict)
}
