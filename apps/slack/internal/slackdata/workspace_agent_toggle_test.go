package slackdata

// Store-boundary tests for the per-workspace conversation-mode toggle
// (SetAgentEnabled / AgentEnabledFor). They fence the three-state read contract
// the handler layer depends on: absent (fall back to the org default) vs an
// explicit true/false (honored as-is, so an opt-out survives the GA default flip).

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestSetAgentEnabled_RequiresTeamID(t *testing.T) {
	called := false
	st := newStore(&stubDDB{updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		called = true
		return &dynamodb.UpdateItemOutput{}, nil
	}})
	err := st.SetAgentEnabled(context.Background(), "", true)
	var se *Error
	if !errors.As(err, &se) || se.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty teamID = %v, want 400 *Error", err)
	}
	if called {
		t.Error("UpdateItem reached despite the empty-teamID validation guard")
	}
}

func TestSetAgentEnabled_WritesExplicitBool(t *testing.T) {
	for _, enable := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[enable], func(t *testing.T) {
			var gotInput *dynamodb.UpdateItemInput
			st := newStore(&stubDDB{updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
				gotInput = in
				return &dynamodb.UpdateItemOutput{}, nil
			}})
			if err := st.SetAgentEnabled(context.Background(), "T1", enable); err != nil {
				t.Fatalf("SetAgentEnabled(%v) err: %v", enable, err)
			}
			if gotInput == nil {
				t.Fatal("UpdateItem was not called")
			}
			// The toggle is gated on an existing row so it can't be set for an
			// unbound workspace.
			if cond := aws.ToString(gotInput.ConditionExpression); !strings.Contains(cond, "attribute_exists("+attrSlackTeamID+")") {
				t.Errorf("ConditionExpression = %q, want attribute_exists guard", cond)
			}
			// The stored value must be an explicit BOOL (not a string/number) so the
			// three-state read can distinguish it from an absent attribute.
			v, ok := gotInput.ExpressionAttributeValues[":v"].(*ddbtypes.AttributeValueMemberBOOL)
			if !ok || v.Value != enable {
				t.Errorf(":v = %#v, want BOOL %v", gotInput.ExpressionAttributeValues[":v"], enable)
			}
			if upd := aws.ToString(gotInput.UpdateExpression); !strings.Contains(upd, attrAgentEnabled) || !strings.Contains(upd, attrUpdatedAt) {
				t.Errorf("UpdateExpression = %q, want it to SET %s and %s", upd, attrAgentEnabled, attrUpdatedAt)
			}
		})
	}
}

func TestSetAgentEnabled_UnboundWorkspaceIs404(t *testing.T) {
	// The conditional-check failure (no row) must surface as a 404 with setup
	// guidance — not the generic 503 — so the admin learns to run /qurl setup.
	st := newStore(&stubDDB{updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("no row")}
	}})
	err := st.SetAgentEnabled(context.Background(), "T1", true)
	var se *Error
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound {
		t.Fatalf("CCFE = %v, want 404 *Error", err)
	}
	if !strings.Contains(strings.ToLower(se.Title), "setup") {
		t.Errorf("404 Title = %q, want it to mention setup", se.Title)
	}
}

func TestSetAgentEnabled_TransportErrorMapsTo503(t *testing.T) {
	st := newStore(&stubDDB{updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
		return nil, errors.New("ddb unreachable")
	}})
	err := st.SetAgentEnabled(context.Background(), "T1", true)
	var se *Error
	if !errors.As(err, &se) || se.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("transport error = %v, want 503 *Error", err)
	}
}

func TestAgentEnabledFor_RequiresTeamID(t *testing.T) {
	called := false
	st := newStore(&stubDDB{getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		called = true
		return &dynamodb.GetItemOutput{}, nil
	}})
	_, _, err := st.AgentEnabledFor(context.Background(), "")
	var se *Error
	if !errors.As(err, &se) || se.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty teamID = %v, want 400 *Error", err)
	}
	if called {
		t.Error("GetItem reached despite the empty-teamID validation guard")
	}
}

func TestAgentEnabledFor_ThreeState(t *testing.T) {
	cases := []struct {
		name      string
		item      map[string]ddbtypes.AttributeValue
		wantValue bool
		wantSet   bool
	}{
		{
			name:    "missing row reads as absent",
			item:    nil, // empty GetItem output → no row
			wantSet: false,
		},
		{
			name:    "row without the attr reads as absent",
			item:    map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr("T1")},
			wantSet: false,
		},
		{
			name:      "explicit true",
			item:      map[string]ddbtypes.AttributeValue{attrAgentEnabled: boolAttr(true)},
			wantValue: true,
			wantSet:   true,
		},
		{
			// The opt-out case: an explicit false must read back as set=true so the
			// caller keeps it off even after the org default flips on at GA.
			name:      "explicit false survives as set",
			item:      map[string]ddbtypes.AttributeValue{attrAgentEnabled: boolAttr(false)},
			wantValue: false,
			wantSet:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStore(&stubDDB{getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: c.item}, nil
			}})
			value, set, err := st.AgentEnabledFor(context.Background(), "T1")
			if err != nil {
				t.Fatalf("AgentEnabledFor err: %v", err)
			}
			if value != c.wantValue || set != c.wantSet {
				t.Fatalf("AgentEnabledFor = (value=%v, set=%v), want (value=%v, set=%v)", value, set, c.wantValue, c.wantSet)
			}
		})
	}
}

func TestAgentEnabledFor_TransportErrorPropagates(t *testing.T) {
	// A read failure must surface as an error (not a silent false) so the gate can
	// fail closed rather than silently disabling a workspace that opted in.
	st := newStore(&stubDDB{getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return nil, errors.New("ddb unreachable")
	}})
	_, _, err := st.AgentEnabledFor(context.Background(), "T1")
	var se *Error
	if !errors.As(err, &se) || se.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("transport error = %v, want 503 *Error", err)
	}
}
