package slackdata

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// agentFakeDDB is a focused in-memory DynamoDBClient for AgentStore tests. It
// models only what AgentStore uses: GetItem and conditional PutItem with the
// `attribute_not_exists(pk)` and `attribute_not_exists(pk) OR conv_version = :ev`
// shapes. Keyed by pk|sk.
type agentFakeDDB struct {
	items     map[string]map[string]ddbtypes.AttributeValue
	putErr    error
	getErr    error
	putCalls  int
	lastPutAt map[string]string // sk -> ttl value, for assertions
}

func newAgentFakeDDB() *agentFakeDDB {
	return &agentFakeDDB{items: map[string]map[string]ddbtypes.AttributeValue{}, lastPutAt: map[string]string{}}
}

func keyOf(item map[string]ddbtypes.AttributeValue) string {
	pk, _ := item[attrAgentPK].(*ddbtypes.AttributeValueMemberS)
	sk, _ := item[attrAgentSK].(*ddbtypes.AttributeValueMemberS)
	return pk.Value + "|" + sk.Value
}

func (f *agentFakeDDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	k := keyOf(in.Key)
	item, ok := f.items[k]
	if !ok {
		return &dynamodb.GetItemOutput{}, nil
	}
	return &dynamodb.GetItemOutput{Item: item}, nil
}

func (f *agentFakeDDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putCalls++
	if f.putErr != nil {
		return nil, f.putErr
	}
	k := keyOf(in.Item)
	existing, present := f.items[k]
	if cond := aws.ToString(in.ConditionExpression); cond != "" {
		if !f.evalCond(cond, existing, present, in.ExpressionAttributeValues) {
			return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("conditional check failed")}
		}
	}
	f.items[k] = in.Item
	if ttl, ok := in.Item[attrAgentTTL].(*ddbtypes.AttributeValueMemberN); ok {
		sk, _ := in.Item[attrAgentSK].(*ddbtypes.AttributeValueMemberS)
		f.lastPutAt[sk.Value] = ttl.Value
	}
	return &dynamodb.PutItemOutput{}, nil
}

// evalCond models the two condition shapes AgentStore emits.
func (f *agentFakeDDB) evalCond(cond string, existing map[string]ddbtypes.AttributeValue, present bool, vals map[string]ddbtypes.AttributeValue) bool {
	notExists := !present
	if !strings.Contains(cond, " OR ") {
		// Single-term existence guard used by MarkEventSeen.
		return notExists
	}
	// attribute_not_exists(pk) OR conv_version = :ev
	if notExists {
		return true
	}
	want, ok := vals[":ev"].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return false
	}
	cur, ok := existing[attrAgentVersion].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return false
	}
	return cur.Value == want.Value
}

func (f *agentFakeDDB) UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	return nil, errors.New("not implemented")
}

func (f *agentFakeDDB) DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	return nil, errors.New("not implemented")
}

func (f *agentFakeDDB) Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	return nil, errors.New("not implemented")
}

func newTestAgentStore(client DynamoDBClient) *AgentStore {
	return &AgentStore{
		Client:    client,
		TableName: "agent_state",
		Now:       func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
}

func TestNewAgentStore(t *testing.T) {
	if _, err := NewAgentStore(newAgentFakeDDB(), ""); err == nil {
		t.Error("expected error when table name and env are empty")
	}
	// A whitespace-only env value must be rejected as empty, not used verbatim:
	// the empty tableName triggers the env fallback, which trims before checking.
	t.Setenv(EnvAgentStateTable, "   ")
	if _, err := NewAgentStore(newAgentFakeDDB(), ""); err == nil {
		t.Error("expected error when env table is whitespace-only")
	}
	t.Setenv(EnvAgentStateTable, "")
	if _, err := NewAgentStore(nil, "t"); err == nil {
		t.Error("expected error when client is nil")
	}
	s, err := NewAgentStore(newAgentFakeDDB(), "t")
	if err != nil {
		t.Fatalf("NewAgentStore: %v", err)
	}
	if s.ConversationTTL != defaultConversationTTL || s.DedupeTTL != defaultDedupeTTL {
		t.Error("defaults not applied")
	}
}

func TestMarkEventSeen_DedupesRetries(t *testing.T) {
	s := newTestAgentStore(newAgentFakeDDB())
	ctx := context.Background()

	first, err := s.MarkEventSeen(ctx, "T1", "Ev123")
	if err != nil || !first {
		t.Fatalf("first sighting: first=%v err=%v", first, err)
	}
	again, err := s.MarkEventSeen(ctx, "T1", "Ev123")
	if err != nil || again {
		t.Fatalf("retry should not be first: again=%v err=%v", again, err)
	}
	// A different event id is independent.
	other, err := s.MarkEventSeen(ctx, "T1", "Ev999")
	if err != nil || !other {
		t.Fatalf("distinct event should be first: %v %v", other, err)
	}
}

func TestMarkEventSeen_Validation(t *testing.T) {
	s := newTestAgentStore(newAgentFakeDDB())
	if _, err := s.MarkEventSeen(context.Background(), "", "e"); err == nil {
		t.Error("expected validation error for empty partition")
	}
	if _, err := s.MarkEventSeen(context.Background(), "T1", ""); err == nil {
		t.Error("expected validation error for empty event id")
	}
}

func TestConversation_RoundTripAndVersioning(t *testing.T) {
	fake := newAgentFakeDDB()
	s := newTestAgentStore(fake)
	ctx := context.Background()
	const part, thread = "T1", "C1:1700.0001"

	// Empty thread.
	blob, ver, err := s.LoadConversation(ctx, part, thread)
	if err != nil || blob != nil || ver != 0 {
		t.Fatalf("empty load: blob=%q ver=%d err=%v", blob, ver, err)
	}

	// First save (expectedVersion 0 → stored version 1).
	if err := s.SaveConversation(ctx, part, thread, []byte(`[{"role":"user"}]`), 0); err != nil {
		t.Fatalf("first save: %v", err)
	}
	blob, ver, err = s.LoadConversation(ctx, part, thread)
	if err != nil || ver != 1 || string(blob) != `[{"role":"user"}]` {
		t.Fatalf("after first save: blob=%q ver=%d err=%v", blob, ver, err)
	}

	// Second save with the matching version succeeds and bumps to 2.
	if err := s.SaveConversation(ctx, part, thread, []byte(`[{"role":"user"},{"role":"assistant"}]`), 1); err != nil {
		t.Fatalf("second save: %v", err)
	}
	_, ver, _ = s.LoadConversation(ctx, part, thread)
	if ver != 2 {
		t.Fatalf("version should be 2, got %d", ver)
	}

	// A stale writer (expectedVersion 1 again) must conflict, not clobber.
	err = s.SaveConversation(ctx, part, thread, []byte(`[{"role":"stale"}]`), 1)
	if !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("expected ErrConversationConflict, got %v", err)
	}
	// The clobber didn't land.
	blob, _, _ = s.LoadConversation(ctx, part, thread)
	if strings.Contains(string(blob), "stale") {
		t.Fatalf("stale write clobbered the conversation: %s", blob)
	}
}

func TestConversation_Validation(t *testing.T) {
	s := newTestAgentStore(newAgentFakeDDB())
	if _, _, err := s.LoadConversation(context.Background(), "", "t"); err == nil {
		t.Error("expected validation error")
	}
	if err := s.SaveConversation(context.Background(), "p", "", nil, 0); err == nil {
		t.Error("expected validation error")
	}
}

func TestConversation_TTLRefreshedOnSave(t *testing.T) {
	fake := newAgentFakeDDB()
	s := newTestAgentStore(fake)
	if err := s.SaveConversation(context.Background(), "T1", "C1:1", []byte("x"), 0); err != nil {
		t.Fatalf("save: %v", err)
	}
	// now=1_700_000_000, conversation TTL default 30m → 1_700_001_800.
	if got := fake.lastPutAt[convSKPrefix+"C1:1"]; got != "1700001800" {
		t.Fatalf("conversation ttl = %q, want 1700001800", got)
	}
}

func TestPendingAction_RoundTripAndTTL(t *testing.T) {
	fake := newAgentFakeDDB()
	s := newTestAgentStore(fake)
	ctx := context.Background()

	// Missing id → not found, no error (treated as "expired").
	if payload, found, err := s.LoadPendingAction(ctx, "T1", "missing"); err != nil || found || payload != nil {
		t.Fatalf("missing load: payload=%q found=%v err=%v", payload, found, err)
	}

	if err := s.PutPendingAction(ctx, "T1", "abc123", []byte(`{"action":"revoke"}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	payload, found, err := s.LoadPendingAction(ctx, "T1", "abc123")
	if err != nil || !found || string(payload) != `{"action":"revoke"}` {
		t.Fatalf("roundtrip: payload=%q found=%v err=%v", payload, found, err)
	}
	// now=1_700_000_000, pending TTL default 10m → 1_700_000_600.
	if got := fake.lastPutAt[pendSKPrefix+"abc123"]; got != "1700000600" {
		t.Fatalf("pending ttl = %q, want 1700000600", got)
	}
}

func TestLoadPendingAction_ReadTimeExpiry(t *testing.T) {
	// The DynamoDB TTL reaper lags, so LoadPendingAction enforces the TTL at read
	// time: a past-TTL item reads as gone even though the fake never reaps.
	fake := newAgentFakeDDB()
	now := time.Unix(1_700_000_000, 0)
	s := &AgentStore{Client: fake, TableName: "agent_state", Now: func() time.Time { return now }, PendingActionTTL: 10 * time.Minute}
	ctx := context.Background()

	if err := s.PutPendingAction(ctx, "T1", "id1", []byte("x")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, found, _ := s.LoadPendingAction(ctx, "T1", "id1"); !found {
		t.Fatal("should be found within the TTL window")
	}
	now = now.Add(11 * time.Minute) // advance past the 10m TTL
	if _, found, _ := s.LoadPendingAction(ctx, "T1", "id1"); found {
		t.Fatal("a past-TTL pending action must read as expired")
	}
}

func TestClaimPendingAction_ConsumeOnce(t *testing.T) {
	s := newTestAgentStore(newAgentFakeDDB())
	ctx := context.Background()

	// First claim wins; every later claim (double-click / replay) loses — even
	// without a prior PutPendingAction, since the claim marker is independent.
	first, err := s.ClaimPendingAction(ctx, "T1", "id1")
	if err != nil || !first {
		t.Fatalf("first claim: claimed=%v err=%v", first, err)
	}
	again, err := s.ClaimPendingAction(ctx, "T1", "id1")
	if err != nil || again {
		t.Fatalf("second claim must lose: claimed=%v err=%v", again, err)
	}
	// A distinct id is independent.
	other, err := s.ClaimPendingAction(ctx, "T1", "id2")
	if err != nil || !other {
		t.Fatalf("distinct id claim: claimed=%v err=%v", other, err)
	}
}

func TestPendingAction_PartitionIsolatesTeams(t *testing.T) {
	// Pending actions key on team id; one team's id must not resolve under another
	// (and the claim markers are independent across teams).
	s := newTestAgentStore(newAgentFakeDDB())
	ctx := context.Background()
	if err := s.PutPendingAction(ctx, "TA", "shared", []byte("a")); err != nil {
		t.Fatalf("put TA: %v", err)
	}
	if _, found, _ := s.LoadPendingAction(ctx, "TB", "shared"); found {
		t.Fatal("team TB must not see team TA's pending action")
	}
	claimedA, _ := s.ClaimPendingAction(ctx, "TA", "shared")
	claimedB, _ := s.ClaimPendingAction(ctx, "TB", "shared")
	if !claimedA || !claimedB {
		t.Fatalf("each team claims its own id independently: A=%v B=%v", claimedA, claimedB)
	}
}

func TestPendingAction_Validation(t *testing.T) {
	s := newTestAgentStore(newAgentFakeDDB())
	ctx := context.Background()
	if err := s.PutPendingAction(ctx, "", "id", nil); err == nil {
		t.Error("PutPendingAction: expected validation error for empty partition")
	}
	if _, _, err := s.LoadPendingAction(ctx, "T1", ""); err == nil {
		t.Error("LoadPendingAction: expected validation error for empty id")
	}
	if _, err := s.ClaimPendingAction(ctx, "", "id"); err == nil {
		t.Error("ClaimPendingAction: expected validation error for empty partition")
	}
}
