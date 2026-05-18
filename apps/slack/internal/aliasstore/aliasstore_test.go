package aliasstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
)

const (
	testTeamID    = "T123ABCDEF"
	testChannelID = "C456GHIJKL"
	testAlias     = "grafana"
	testResource  = "r_abc123"
	testTable     = "qurl-bot-slack-test-channel_policies"
)

// recordedCall captures the input handed to UpdateItem so tests can
// assert on table name, key, expression shape, and substitutions.
type recordedCall struct {
	Input *dynamodb.UpdateItemInput
}

// fakeDDB is a programmable UpdateItem stub. Each call pops one
// scripted response off `outputs`/`errs`; tests script the seed/write
// pair separately so the two-step Bind path can be exercised
// independently from the single-step Unbind path.
type fakeDDB struct {
	calls   []recordedCall
	errs    []error
	outputs []*dynamodb.UpdateItemOutput
}

func (f *fakeDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.calls = append(f.calls, recordedCall{Input: in})
	idx := len(f.calls) - 1
	var err error
	if idx < len(f.errs) {
		err = f.errs[idx]
	}
	var out *dynamodb.UpdateItemOutput
	if idx < len(f.outputs) {
		out = f.outputs[idx]
	} else {
		out = &dynamodb.UpdateItemOutput{}
	}
	return out, err
}

func newStore(f *fakeDDB) *Store {
	return &Store{Client: f, TableName: testTable}
}

func ccf() error {
	return &ddbtypes.ConditionalCheckFailedException{}
}

func TestBindChannelAlias_SeedsMapAndWritesEntry(t *testing.T) {
	f := &fakeDDB{}
	s := newStore(f)

	if err := s.BindChannelAlias(context.Background(), testTeamID, testChannelID, testAlias, testResource); err != nil {
		t.Fatalf("BindChannelAlias: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("expected 2 UpdateItem calls (seed + write), got %d", len(f.calls))
	}

	seed := f.calls[0].Input
	if got := *seed.TableName; got != testTable {
		t.Errorf("seed: TableName = %q, want %q", got, testTable)
	}
	if !strings.Contains(*seed.UpdateExpression, "SET #ab = :empty") {
		t.Errorf("seed: UpdateExpression = %q, want SET #ab = :empty", *seed.UpdateExpression)
	}
	if !strings.Contains(*seed.ConditionExpression, "attribute_not_exists(#ab)") {
		t.Errorf("seed: ConditionExpression = %q, want attribute_not_exists(#ab)", *seed.ConditionExpression)
	}

	write := f.calls[1].Input
	if got := *write.UpdateExpression; got != "SET #ab.#a = :rid" {
		t.Errorf("write: UpdateExpression = %q, want SET #ab.#a = :rid", got)
	}
	if got := *write.ConditionExpression; got != "attribute_not_exists(#ab.#a)" {
		t.Errorf("write: ConditionExpression = %q, want attribute_not_exists(#ab.#a)", got)
	}
	if got := write.ExpressionAttributeNames["#a"]; got != testAlias {
		t.Errorf("write: #a substitution = %q, want %q", got, testAlias)
	}
	rid, ok := write.ExpressionAttributeValues[":rid"].(*ddbtypes.AttributeValueMemberS)
	if !ok || rid.Value != testResource {
		t.Errorf("write: :rid substitution = %#v, want S(%q)", write.ExpressionAttributeValues[":rid"], testResource)
	}
}

func TestBindChannelAlias_SeedAlreadyExistsIsSwallowed(t *testing.T) {
	f := &fakeDDB{errs: []error{ccf(), nil}}
	s := newStore(f)

	if err := s.BindChannelAlias(context.Background(), testTeamID, testChannelID, testAlias, testResource); err != nil {
		t.Fatalf("BindChannelAlias: CCF on seed must be swallowed; got %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(f.calls))
	}
}

func TestBindChannelAlias_DuplicateAliasReturnsErrAliasAlreadyBound(t *testing.T) {
	f := &fakeDDB{errs: []error{nil, ccf()}}
	s := newStore(f)

	err := s.BindChannelAlias(context.Background(), testTeamID, testChannelID, testAlias, testResource)
	if !errors.Is(err, internal.ErrAliasAlreadyBound) {
		t.Fatalf("want ErrAliasAlreadyBound, got %v", err)
	}
}

func TestBindChannelAlias_SeedNonCCFErrorAborts(t *testing.T) {
	want := errors.New("boom")
	f := &fakeDDB{errs: []error{want}}
	s := newStore(f)

	err := s.BindChannelAlias(context.Background(), testTeamID, testChannelID, testAlias, testResource)
	if err == nil || !strings.Contains(err.Error(), "ensure alias_bindings map") {
		t.Fatalf("want wrapped seed error, got %v", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("write call must not fire after seed error; got %d calls", len(f.calls))
	}
}

func TestBindChannelAlias_WriteNonCCFErrorBubbles(t *testing.T) {
	want := errors.New("throttled")
	f := &fakeDDB{errs: []error{nil, want}}
	s := newStore(f)

	err := s.BindChannelAlias(context.Background(), testTeamID, testChannelID, testAlias, testResource)
	if err == nil || !strings.Contains(err.Error(), "bind alias") {
		t.Fatalf("want wrapped bind error, got %v", err)
	}
}

func TestUnbindChannelAlias_HappyPath(t *testing.T) {
	f := &fakeDDB{}
	s := newStore(f)

	if err := s.UnbindChannelAlias(context.Background(), testTeamID, testChannelID, testAlias); err != nil {
		t.Fatalf("UnbindChannelAlias: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(f.calls))
	}
	in := f.calls[0].Input
	if got := *in.UpdateExpression; got != "REMOVE #ab.#a" {
		t.Errorf("UpdateExpression = %q, want REMOVE #ab.#a", got)
	}
	if got := *in.ConditionExpression; got != "attribute_exists(#ab.#a)" {
		t.Errorf("ConditionExpression = %q, want attribute_exists(#ab.#a)", got)
	}
	if got := in.ExpressionAttributeNames["#a"]; got != testAlias {
		t.Errorf("#a substitution = %q, want %q", got, testAlias)
	}
}

func TestUnbindChannelAlias_NotPresentReturnsErrAliasNotFound(t *testing.T) {
	f := &fakeDDB{errs: []error{ccf()}}
	s := newStore(f)

	err := s.UnbindChannelAlias(context.Background(), testTeamID, testChannelID, testAlias)
	if !errors.Is(err, internal.ErrAliasNotFound) {
		t.Fatalf("want ErrAliasNotFound, got %v", err)
	}
}

func TestUnbindChannelAlias_NonCCFErrorBubbles(t *testing.T) {
	want := errors.New("network")
	f := &fakeDDB{errs: []error{want}}
	s := newStore(f)

	err := s.UnbindChannelAlias(context.Background(), testTeamID, testChannelID, testAlias)
	if err == nil || !strings.Contains(err.Error(), "unbind alias") {
		t.Fatalf("want wrapped unbind error, got %v", err)
	}
}

// Compile-time guard: the package-doc claim that *Store satisfies
// internal.AliasStore should never silently regress. The const-assign
// to a typed nil var would fail compilation if the contract drifts.
var _ internal.AliasStore = (*Store)(nil)

func TestNew_ReturnsErrorWhenEnvVarUnset(t *testing.T) {
	t.Setenv(EnvChannelPoliciesTable, "")
	_, err := New(context.Background())
	if err == nil || !strings.Contains(err.Error(), EnvChannelPoliciesTable) {
		t.Fatalf("want error naming %s, got %v", EnvChannelPoliciesTable, err)
	}
}
