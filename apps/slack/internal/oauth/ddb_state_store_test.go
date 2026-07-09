package oauth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/shared/auth"
)

type fakeOAuthStateDDB struct {
	putInput     *dynamodb.PutItemInput
	updateInput  *dynamodb.UpdateItemInput
	updateOutput *dynamodb.UpdateItemOutput
	updateErr    error
	deleteInput  *dynamodb.DeleteItemInput
	deleteOutput *dynamodb.DeleteItemOutput
	deleteErr    error
}

func (f *fakeOAuthStateDDB) GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return &dynamodb.GetItemOutput{}, nil
}

func (f *fakeOAuthStateDDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putInput = in
	if err := validateDDBExpressionBindings(
		[]string{aws.ToString(in.ConditionExpression)},
		in.ExpressionAttributeNames,
		in.ExpressionAttributeValues,
	); err != nil {
		return nil, err
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeOAuthStateDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.updateInput = in
	if err := validateDDBExpressionBindings(
		[]string{aws.ToString(in.UpdateExpression), aws.ToString(in.ConditionExpression)},
		in.ExpressionAttributeNames,
		in.ExpressionAttributeValues,
	); err != nil {
		return nil, err
	}
	if f.updateOutput != nil || f.updateErr != nil {
		return f.updateOutput, f.updateErr
	}
	return &dynamodb.UpdateItemOutput{Attributes: storedStateDDBItem()}, nil
}

func (f *fakeOAuthStateDDB) DeleteItem(_ context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.deleteInput = in
	if err := validateDDBExpressionBindings(
		[]string{aws.ToString(in.ConditionExpression)},
		in.ExpressionAttributeNames,
		in.ExpressionAttributeValues,
	); err != nil {
		return nil, err
	}
	if f.deleteOutput != nil || f.deleteErr != nil {
		return f.deleteOutput, f.deleteErr
	}
	return &dynamodb.DeleteItemOutput{Attributes: storedStateDDBItem()}, nil
}

var (
	ddbNamePlaceholderPattern  = regexp.MustCompile(`#[A-Za-z0-9_]+`)
	ddbValuePlaceholderPattern = regexp.MustCompile(`:[A-Za-z0-9_]+`)
)

func validateDDBExpressionBindings(expressions []string, names map[string]string, values map[string]ddbtypes.AttributeValue) error {
	joined := strings.Join(expressions, " ")
	usedNames := stringSet(ddbNamePlaceholderPattern.FindAllString(joined, -1))
	usedValues := stringSet(ddbValuePlaceholderPattern.FindAllString(joined, -1))
	for name := range names {
		if !usedNames[name] {
			return fmt.Errorf("unused expression attribute name %s", name)
		}
	}
	for name := range usedNames {
		if _, ok := names[name]; !ok {
			return fmt.Errorf("undeclared expression attribute name %s", name)
		}
	}
	for value := range values {
		if !usedValues[value] {
			return fmt.Errorf("unused expression attribute value %s", value)
		}
	}
	for value := range usedValues {
		if _, ok := values[value]; !ok {
			return fmt.Errorf("undeclared expression attribute value %s", value)
		}
	}
	return nil
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func storedStateDDBItem() map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		oauthStateAttrTeamID:   &ddbtypes.AttributeValueMemberS{Value: testStateTeamID},
		oauthStateAttrUserID:   &ddbtypes.AttributeValueMemberS{Value: testStateUserID},
		oauthStateAttrNonce:    &ddbtypes.AttributeValueMemberS{Value: strings.Repeat("a", stateNonceLen*2)},
		oauthStateAttrVerifier: &ddbtypes.AttributeValueMemberS{Value: strings.Repeat("b", statePKCEVerifierMinLen)},
		oauthStateAttrEmail:    &ddbtypes.AttributeValueMemberS{Value: testNormalizedSetupEmail},
		oauthStateAttrMode:     &ddbtypes.AttributeValueMemberS{Value: string(SetupModeRotate)},
	}
}

func TestNewDDBStateStoreValidatesWiringAtStartup(t *testing.T) {
	if _, err := NewDDBStateStore(nil); err == nil {
		t.Fatal("nil provider must fail")
	}
	if _, err := NewDDBStateStore(&auth.DDBProvider{Client: &fakeOAuthStateDDB{}}); err == nil {
		t.Fatal("empty table name must fail")
	}
	store, err := NewDDBStateStore(&auth.DDBProvider{Client: &fakeOAuthStateDDB{}, TableName: "workspace-state"})
	if err != nil {
		t.Fatalf("NewDDBStateStore: %v", err)
	}
	if store.TableName != "workspace-state" || store.Client == nil {
		t.Fatalf("store wiring = %+v", store)
	}
}

func TestDDBStateStorePutStateWritesOpaqueStateRow(t *testing.T) {
	ddb := &fakeOAuthStateDDB{}
	store := &DDBStateStore{Client: ddb, TableName: "workspace-state"}
	now := time.Unix(1700000000, 0).UTC()
	state := StoredState{
		VerifiedState: VerifiedState{
			TeamID:       testStateTeamID,
			UserID:       testStateUserID,
			Nonce:        strings.Repeat("a", stateNonceLen*2),
			CodeVerifier: strings.Repeat("b", statePKCEVerifierMinLen),
			Email:        testNormalizedSetupEmail,
			Mode:         SetupModeRotate,
		},
		CreatedAt: now,
		ExpiresAt: now.Add(stateMaxAge),
	}
	if err := store.PutState(context.Background(), "opaque-handle", state); err != nil {
		t.Fatalf("PutState: %v", err)
	}
	if ddb.putInput == nil {
		t.Fatal("expected PutItem")
	}
	if got := aws.ToString(ddb.putInput.TableName); got != "workspace-state" {
		t.Fatalf("table = %q", got)
	}
	if v, ok := ddb.putInput.Item[workspaceStatePKAttr].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != oauthStateKey("opaque-handle") {
		t.Fatalf("pk = %v", ddb.putInput.Item[workspaceStatePKAttr])
	}
	if v, ok := ddb.putInput.Item[oauthStateAttrVerifier].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != state.CodeVerifier {
		t.Fatalf("code verifier not persisted server-side: %v", ddb.putInput.Item[oauthStateAttrVerifier])
	}
	if _, ok := ddb.putInput.Item[oauthStateAttrTTL].(*ddbtypes.AttributeValueMemberN); !ok {
		t.Fatalf("ttl attr missing/wrong: %v", ddb.putInput.Item[oauthStateAttrTTL])
	}
	if got := ddb.putInput.Item[oauthStateAttrTTL].(*ddbtypes.AttributeValueMemberN).Value; got != "1700003600" {
		t.Fatalf("ttl attr = %q, want one-hour expiry epoch", got)
	}
	if got := aws.ToString(ddb.putInput.ConditionExpression); !strings.Contains(got, "attribute_not_exists") {
		t.Fatalf("PutState must be conditional, got %q", got)
	}
}

func TestDDBStateStoreConsumeStateIsConditionalOneShot(t *testing.T) {
	ddb := &fakeOAuthStateDDB{}
	store := &DDBStateStore{Client: ddb, TableName: "workspace-state"}
	got, err := store.ConsumeState(context.Background(), "opaque-handle", time.Unix(1700000030, 0))
	if err != nil {
		t.Fatalf("ConsumeState: %v", err)
	}
	if got.TeamID != testStateTeamID || got.Email != testNormalizedSetupEmail || got.Mode != SetupModeRotate {
		t.Fatalf("verified state mismatch: %+v", got)
	}
	if ddb.updateInput != nil {
		t.Fatalf("ConsumeState must delete, not update: %+v", ddb.updateInput)
	}
	if ddb.deleteInput == nil {
		t.Fatal("expected DeleteItem")
	}
	cond := aws.ToString(ddb.deleteInput.ConditionExpression)
	for _, want := range []string{"attribute_exists(#started_at)", "#expires_at > :now_epoch"} {
		if !strings.Contains(cond, want) {
			t.Fatalf("consume condition missing %q in %q", want, cond)
		}
	}
	if ddb.deleteInput.ReturnValues != ddbtypes.ReturnValueAllOld {
		t.Fatalf("consume ReturnValues = %v, want ALL_OLD", ddb.deleteInput.ReturnValues)
	}
	if ddb.deleteInput.ReturnValuesOnConditionCheckFailure != ddbtypes.ReturnValuesOnConditionCheckFailureAllOld {
		t.Fatalf("consume failure ReturnValues = %v, want ALL_OLD", ddb.deleteInput.ReturnValuesOnConditionCheckFailure)
	}
}

func TestDDBStateStoreStartStateUsesValidExpressionBindings(t *testing.T) {
	ddb := &fakeOAuthStateDDB{}
	store := &DDBStateStore{Client: ddb, TableName: "workspace-state"}
	got, err := store.StartState(context.Background(), "opaque-handle", time.Unix(1700000030, 0))
	if err != nil {
		t.Fatalf("StartState: %v", err)
	}
	if got.TeamID != testStateTeamID || got.CodeVerifier == "" {
		t.Fatalf("verified state mismatch: %+v", got)
	}
	if ddb.updateInput == nil {
		t.Fatal("expected UpdateItem")
	}
	if len(ddb.updateInput.ExpressionAttributeValues) != 1 {
		t.Fatalf("StartState values = %v, want one shared epoch value", ddb.updateInput.ExpressionAttributeValues)
	}
	if _, ok := ddb.updateInput.ExpressionAttributeValues[":now_epoch"].(*ddbtypes.AttributeValueMemberN); !ok {
		t.Fatalf("StartState timestamp must be numeric epoch: %v", ddb.updateInput.ExpressionAttributeValues)
	}
}

func TestDDBStateStoreConsumeStateDistinguishesNotStarted(t *testing.T) {
	item := storedStateDDBItem()
	item[workspaceStatePKAttr] = &ddbtypes.AttributeValueMemberS{Value: oauthStateKey("opaque-handle")}
	ddb := &fakeOAuthStateDDB{
		deleteErr: &ddbtypes.ConditionalCheckFailedException{Item: item},
	}
	store := &DDBStateStore{Client: ddb, TableName: "workspace-state"}
	if _, err := store.ConsumeState(context.Background(), "opaque-handle", time.Unix(1700000030, 0)); !errors.Is(err, errStateNotStarted) {
		t.Fatalf("ConsumeState error = %v, want errStateNotStarted", err)
	}
}

func TestDDBStateStoreConsumeStateDistinguishesMissingOrReplayed(t *testing.T) {
	ddb := &fakeOAuthStateDDB{
		deleteErr: &ddbtypes.ConditionalCheckFailedException{},
	}
	store := &DDBStateStore{Client: ddb, TableName: "workspace-state"}
	if _, err := store.ConsumeState(context.Background(), "opaque-handle", time.Unix(1700000030, 0)); !errors.Is(err, errStateMissing) {
		t.Fatalf("ConsumeState error = %v, want errStateMissing", err)
	}
}
