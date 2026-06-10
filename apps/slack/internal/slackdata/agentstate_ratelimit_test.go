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

	// Atomic ADD of the count + SET of the ttl, returning the new value.
	if expr := aws.ToString(got.UpdateExpression); !strings.Contains(expr, "ADD "+attrTurnCount+" :one") || !strings.Contains(expr, "SET "+attrAgentTTL+" = :ttl") {
		t.Errorf("UpdateExpression = %q, want ADD %s :one + SET %s = :ttl", expr, attrTurnCount, attrAgentTTL)
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
