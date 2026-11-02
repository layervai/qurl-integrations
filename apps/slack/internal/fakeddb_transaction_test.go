package internal

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestFakePolicyTransactionIsAtomic(t *testing.T) {
	for _, scenario := range []string{"commit", "condition failure", "invalid later update"} {
		t.Run(scenario, func(t *testing.T) {
			names := defaultTestTableNames()
			str := func(s string) ddbtypes.AttributeValue { return &ddbtypes.AttributeValueMemberS{Value: s} }
			key := func(channel string) map[string]ddbtypes.AttributeValue {
				return map[string]ddbtypes.AttributeValue{fAttrSlackTeamID: str("T1"), fAttrSlackChannelID: str(channel)}
			}
			original := key("C1")
			original["aliases"] = &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{"name": str("old")}}
			secondOriginal := key("C2")
			secondOriginal["marker"] = str("old")
			fake := newFakeDDB(t, names, map[string][]map[string]ddbtypes.AttributeValue{names.channelPolicy: {original, secondOriginal}})
			first := &ddbtypes.Update{TableName: aws.String(names.channelPolicy), Key: key("C1"), UpdateExpression: aws.String("SET aliases.#name = :new"), ExpressionAttributeNames: map[string]string{"#name": "name"}, ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":new": str("new")}}
			second := &ddbtypes.Update{TableName: aws.String(names.channelPolicy), Key: key("C2"), UpdateExpression: aws.String("SET marker = :new"), ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":new": str("committed")}}
			if scenario == "condition failure" {
				second.ConditionExpression = aws.String("attribute_not_exists(slack_team_id)")
				second.ReturnValuesOnConditionCheckFailure = ddbtypes.ReturnValuesOnConditionCheckFailureAllOld
			}
			if scenario == "invalid later update" {
				second.UpdateExpression = aws.String("SET marker = :missing")
			}
			_, err := fake.TransactWriteItems(context.Background(), &dynamodb.TransactWriteItemsInput{TransactItems: []ddbtypes.TransactWriteItem{{Update: first}, {Update: second}}})
			if scenario == "commit" && err != nil {
				t.Fatal(err)
			}
			if scenario != "commit" && err == nil {
				t.Fatal("invalid transaction committed")
			}
			if scenario == "condition failure" {
				var canceled *ddbtypes.TransactionCanceledException
				if !errors.As(err, &canceled) || len(canceled.CancellationReasons) != 2 || aws.ToString(canceled.CancellationReasons[1].Code) != "ConditionalCheckFailed" || len(canceled.CancellationReasons[1].Item) == 0 {
					t.Fatalf("missing indexed cancellation snapshot: %v", err)
				}
			}
			out, err := fake.GetItem(context.Background(), &dynamodb.GetItemInput{TableName: aws.String(names.channelPolicy), Key: key("C1")})
			if err != nil {
				t.Fatal(err)
			}
			want := "old"
			if scenario == "commit" {
				want = "new"
			}
			got := out.Item["aliases"].(*ddbtypes.AttributeValueMemberM).Value["name"].(*ddbtypes.AttributeValueMemberS).Value
			if got != want {
				t.Fatalf("partial transaction changed alias: %s, want %s", got, want)
			}
		})
	}
}

func TestFakePolicyConditionsPreserveGrouping(t *testing.T) {
	item := map[string]ddbtypes.AttributeValue{"owner": &ddbtypes.AttributeValueMemberS{Value: "owner"}, "admins": &ddbtypes.AttributeValueMemberSS{Value: []string{"admin"}}, "generation": &ddbtypes.AttributeValueMemberS{Value: "current"}}
	values := map[string]ddbtypes.AttributeValue{":caller": &ddbtypes.AttributeValueMemberS{Value: "admin"}, ":current": &ddbtypes.AttributeValueMemberS{Value: "current"}, ":old": &ddbtypes.AttributeValueMemberS{Value: "old"}}
	for _, tc := range []struct {
		expression string
		want       bool
	}{
		{"(owner = :caller OR contains(admins, :caller)) AND generation = :current", true},
		{"(owner = :caller OR contains(admins, :caller)) AND generation = :old", false},
		{"owner = :caller OR contains(admins, :caller) AND generation = :old", false},
		{"attribute_not_exists(missing) OR (generation = :old AND contains(admins, :caller))", true},
	} {
		got, err := evalCondition(tc.expression, item, true, values, nil)
		if err != nil || got != tc.want {
			t.Fatalf("%s: %v %v, want %v", tc.expression, got, err, tc.want)
		}
	}
	if _, err := evalCondition("(generation = :old", item, true, values, nil); err == nil {
		t.Fatal("unbalanced condition accepted")
	}
}
