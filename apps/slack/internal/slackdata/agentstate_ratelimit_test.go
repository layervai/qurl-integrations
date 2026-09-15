package slackdata

// Store-boundary tests for the agent-turn rate counter (BumpTurnCount): the atomic
// ADD request shape, the UPDATED_NEW response parse, validation guards, and the
// transport-error mapping. Uses stubDDB (not the stateful agentFakeDDB) so the
// UpdateItem request can be inspected directly.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestBumpTurnCount_AtomicAddRequest(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	windowStart := now.Truncate(time.Hour)

	var got *dynamodb.UpdateItemInput
	st := &AgentStore{
		Client: &stubDDB{updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			got = in
			return &dynamodb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{attrTurnCount: numberAttr(7)}}, nil
		}},
		TableName: "agent_state",
		Now:       func() time.Time { return now },
	}

	count, err := st.BumpTurnCount(context.Background(), "T1", "user#U2", time.Hour)
	if err != nil {
		t.Fatalf("BumpTurnCount: %v", err)
	}
	if count != 7 {
		t.Fatalf("count = %d, want 7 (parsed from UPDATED_NEW)", count)
	}
	if got == nil {
		t.Fatal("UpdateItem was not called")
	}

	// Key: pk = teamID (a workspace, not the event partition), sk window-keyed.
	pk, _ := got.Key[attrAgentPK].(*ddbtypes.AttributeValueMemberS)
	sk, _ := got.Key[attrAgentSK].(*ddbtypes.AttributeValueMemberS)
	wantSK := "rate#user#U2#" + strconv.FormatInt(windowStart.Unix(), 10)
	if pk == nil || pk.Value != "T1" || sk == nil || sk.Value != wantSK {
		t.Fatalf("key = (%v, %v), want (T1, %s)", got.Key[attrAgentPK], got.Key[attrAgentSK], wantSK)
	}

	// Atomic ADD of the count + a SET of the ttl value, returning the new value. The
	// reserved-word aliasing of the ttl attribute is pinned separately by
	// TestBumpTurnCount_TTLReservedWordIsAliased; here we only assert the ADD and that a
	// SET of :ttl runs.
	if expr := aws.ToString(got.UpdateExpression); !strings.Contains(expr, "ADD "+attrTurnCount+" :one") || !strings.Contains(expr, "= :ttl") {
		t.Errorf("UpdateExpression = %q, want ADD %s :one + a SET of = :ttl", expr, attrTurnCount)
	}
	if got.ReturnValues != ddbtypes.ReturnValueUpdatedNew {
		t.Errorf("ReturnValues = %q, want UPDATED_NEW (the new count must come back)", got.ReturnValues)
	}
	if v, _ := got.ExpressionAttributeValues[":one"].(*ddbtypes.AttributeValueMemberN); v == nil || v.Value != "1" {
		t.Errorf(":one = %v, want N 1", got.ExpressionAttributeValues[":one"])
	}
	wantTTL := strconv.FormatInt(windowStart.Add(2*time.Hour).Unix(), 10)
	if v, _ := got.ExpressionAttributeValues[":ttl"].(*ddbtypes.AttributeValueMemberN); v == nil || v.Value != wantTTL {
		t.Errorf(":ttl = %v, want N %s (a window past the window end)", got.ExpressionAttributeValues[":ttl"], wantTTL)
	}
}

// TestBumpTurnCount_TTLReservedWordIsAliased pins the fix for a ValidationException that
// silently disabled the agent turn-rate cap. `ttl` is a DynamoDB reserved word, so a bare
// `SET ttl = :ttl` in an UpdateExpression 400s ("Attribute name is a reserved keyword;
// reserved keyword: ttl") — and because the rate gate is fail-open (handler_agent.go logs
// a Warn and allows the turn), a 400 on every call let every turn through uncapped. The
// attribute MUST be reached through an expression-attribute-name alias. The in-memory DDB
// fakes don't model reserved words, which is why the original bug passed the request-shape
// test above; this test asserts the alias directly so a revert to the bare form trips here.
func TestBumpTurnCount_TTLReservedWordIsAliased(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()

	var got *dynamodb.UpdateItemInput
	st := &AgentStore{
		Client: &stubDDB{updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			got = in
			return &dynamodb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{attrTurnCount: numberAttr(1)}}, nil
		}},
		TableName: "agent_state",
		Now:       func() time.Time { return now },
	}

	if _, err := st.BumpTurnCount(context.Background(), "T1", "team", time.Hour); err != nil {
		t.Fatalf("BumpTurnCount: %v", err)
	}
	if got == nil {
		t.Fatal("UpdateItem was not called")
	}

	// The reserved word reaches the table only through the `#ttl` alias...
	if got.ExpressionAttributeNames["#ttl"] != attrAgentTTL {
		t.Errorf("ExpressionAttributeNames[#ttl] = %q, want %q (ttl must be aliased, never SET bare)", got.ExpressionAttributeNames["#ttl"], attrAgentTTL)
	}
	// ...and the SET targets that alias. The buggy `SET ttl = :ttl` does not contain this
	// substring, so a revert to the bare reserved word fails here.
	if expr := aws.ToString(got.UpdateExpression); !strings.Contains(expr, "SET #ttl = :ttl") {
		t.Errorf("UpdateExpression = %q, want a `SET #ttl = :ttl` (reserved word aliased)", expr)
	}
}

func TestBumpTurnCount_ValidationGuards(t *testing.T) {
	called := false
	st := &AgentStore{
		Client: &stubDDB{updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			called = true
			return &dynamodb.UpdateItemOutput{}, nil
		}},
		TableName: "agent_state",
	}
	cases := []struct {
		name, team, scope string
		window            time.Duration
	}{
		{"empty team", "", "team", time.Hour},
		{"empty scope", "T1", "", time.Hour},
		{"non-positive window", "T1", "team", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := st.BumpTurnCount(context.Background(), c.team, c.scope, c.window)
			var se *Error
			if !errors.As(err, &se) || se.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s: err = %v, want 400 *Error", c.name, err)
			}
		})
	}
	if called {
		t.Error("UpdateItem reached despite a validation guard")
	}
}

func TestBumpTurnCount_TransportErrorMapsTo503(t *testing.T) {
	st := &AgentStore{
		Client: &stubDDB{updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			return nil, errors.New("ddb unreachable")
		}},
		TableName: "agent_state",
		Now:       func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
	_, err := st.BumpTurnCount(context.Background(), "T1", "team", time.Hour)
	var se *Error
	if !errors.As(err, &se) || se.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("transport error = %v, want 503 *Error", err)
	}
}
