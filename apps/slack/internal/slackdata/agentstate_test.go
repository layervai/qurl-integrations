package slackdata

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// agentFakeDDB is a focused in-memory DynamoDBClient for AgentStore tests. It
// models the shapes AgentStore uses: point GetItem, conditional PutItem with the
// `attribute_not_exists(pk)` and `attribute_not_exists(pk) OR conv_version = :ev`
// conditions, partition Query, and unconditional DeleteItem. Keyed by pk|sk.
type agentFakeDDB struct {
	items         map[string]map[string]ddbtypes.AttributeValue
	putErr        error
	getErr        error
	queryErr      error
	deleteErr     error
	queryPageSize int
	putCalls      int
	deleteCalls   int
	lastPutAt     map[string]string // sk -> ttl value, for assertions
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

func (f *agentFakeDDB) DeleteItem(_ context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.deleteCalls++
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	delete(f.items, keyOf(in.Key))
	return &dynamodb.DeleteItemOutput{}, nil
}

// Query models the two AgentStore shapes that tests need: ListAuditEntries emits
// pk equality + begins_with(sk) with ScanIndexForward + Limit, while
// PurgeWorkspaceAgentState emits pk equality only and pages over the whole
// partition. It reads the expression values directly rather than parsing the
// KeyConditionExpression text.
func (f *agentFakeDDB) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	vals := in.ExpressionAttributeValues
	pkv, _ := vals[":pk"].(*ddbtypes.AttributeValueMemberS)
	prefv, _ := vals[":prefix"].(*ddbtypes.AttributeValueMemberS)
	var matched []map[string]ddbtypes.AttributeValue
	for _, item := range f.items {
		pk, _ := item[attrAgentPK].(*ddbtypes.AttributeValueMemberS)
		sk, _ := item[attrAgentSK].(*ddbtypes.AttributeValueMemberS)
		if pk == nil || sk == nil || pkv == nil || pk.Value != pkv.Value {
			continue
		}
		if prefv != nil && !strings.HasPrefix(sk.Value, prefv.Value) {
			continue
		}
		matched = append(matched, item)
	}
	sort.Slice(matched, func(i, j int) bool {
		si := matched[i][attrAgentSK].(*ddbtypes.AttributeValueMemberS).Value
		sj := matched[j][attrAgentSK].(*ddbtypes.AttributeValueMemberS).Value
		if in.ScanIndexForward != nil && !*in.ScanIndexForward {
			return si > sj // descending — newest-first for the time-ordered audit sks
		}
		return si < sj
	})
	if in.Limit != nil && int(*in.Limit) < len(matched) {
		matched = matched[:*in.Limit]
	}
	if f.queryPageSize > 0 && f.queryPageSize < len(matched) {
		start := 0
		if len(in.ExclusiveStartKey) != 0 {
			startKey := keyOf(in.ExclusiveStartKey)
			for i, item := range matched {
				if keyOf(item) == startKey {
					start = i + 1
					break
				}
			}
		}
		end := start + f.queryPageSize
		if end > len(matched) {
			end = len(matched)
		}
		out := &dynamodb.QueryOutput{Items: matched[start:end]}
		if end < len(matched) {
			out.LastEvaluatedKey = map[string]ddbtypes.AttributeValue{
				attrAgentPK: matched[end-1][attrAgentPK],
				attrAgentSK: matched[end-1][attrAgentSK],
			}
		}
		return out, nil
	}
	return &dynamodb.QueryOutput{Items: matched}, nil
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

func TestThreadContext_RoundTripAndTTL(t *testing.T) {
	fake := newAgentFakeDDB()
	s := newTestAgentStore(fake)
	ctx := context.Background()

	// No context stored → not found, no error (a pane turn falls back to the DM).
	if ch, found, err := s.GetThreadContext(ctx, "T1", "D1:100.1"); err != nil || found || ch != "" {
		t.Fatalf("missing get: ch=%q found=%v err=%v", ch, found, err)
	}

	if err := s.PutThreadContext(ctx, "T1", "D1:100.1", "C9"); err != nil {
		t.Fatalf("put: %v", err)
	}
	ch, found, err := s.GetThreadContext(ctx, "T1", "D1:100.1")
	if err != nil || !found || ch != "C9" {
		t.Fatalf("roundtrip: ch=%q found=%v err=%v", ch, found, err)
	}
	// now=1_700_000_000, conversation TTL default 30m → 1_700_001_800: the context
	// tracks the lifetime of the conversation it scopes.
	if got := fake.lastPutAt[threadCtxSKPrefix+"D1:100.1"]; got != "1700001800" {
		t.Fatalf("context ttl = %q, want 1700001800", got)
	}
}

func TestThreadContext_LatestWins(t *testing.T) {
	// assistant_thread_context_changed overwrites the context as the user switches the
	// channel they're viewing — an unconditional Put, last write wins.
	s := newTestAgentStore(newAgentFakeDDB())
	ctx := context.Background()
	if err := s.PutThreadContext(ctx, "T1", "D1:100.1", "C1"); err != nil {
		t.Fatalf("put C1: %v", err)
	}
	if err := s.PutThreadContext(ctx, "T1", "D1:100.1", "C2"); err != nil {
		t.Fatalf("put C2: %v", err)
	}
	if ch, found, _ := s.GetThreadContext(ctx, "T1", "D1:100.1"); !found || ch != "C2" {
		t.Fatalf("latest write must win: ch=%q found=%v", ch, found)
	}
}

func TestGetThreadContext_ReadTimeExpiry(t *testing.T) {
	// Like LoadPendingAction, the TTL is enforced at read time so a long-stale context
	// (the reaper lags) reads as gone and the turn falls back to the DM.
	fake := newAgentFakeDDB()
	now := time.Unix(1_700_000_000, 0)
	s := &AgentStore{Client: fake, TableName: "agent_state", Now: func() time.Time { return now }, ConversationTTL: 30 * time.Minute}
	ctx := context.Background()

	if err := s.PutThreadContext(ctx, "T1", "D1:100.1", "C9"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, found, _ := s.GetThreadContext(ctx, "T1", "D1:100.1"); !found {
		t.Fatal("should be found within the TTL window")
	}
	now = now.Add(31 * time.Minute) // advance past the 30m TTL
	if _, found, _ := s.GetThreadContext(ctx, "T1", "D1:100.1"); found {
		t.Fatal("a past-TTL context must read as expired")
	}
}

func TestThreadContext_Validation(t *testing.T) {
	s := newTestAgentStore(newAgentFakeDDB())
	ctx := context.Background()
	if err := s.PutThreadContext(ctx, "", "k", "C1"); err == nil {
		t.Error("expected validation error for empty partition")
	}
	if err := s.PutThreadContext(ctx, "T1", "", "C1"); err == nil {
		t.Error("expected validation error for empty thread key")
	}
	if err := s.PutThreadContext(ctx, "T1", "k", ""); err == nil {
		t.Error("expected validation error for empty channel id")
	}
	if _, _, err := s.GetThreadContext(ctx, "", "k"); err == nil {
		t.Error("expected validation error for empty partition")
	}
	if _, _, err := s.GetThreadContext(ctx, "T1", ""); err == nil {
		t.Error("expected validation error for empty thread key")
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

func TestPurgeWorkspaceAgentState(t *testing.T) {
	fake := newAgentFakeDDB()
	fake.queryPageSize = 2
	s := newTestAgentStore(fake)
	ctx := context.Background()

	if err := s.SaveConversation(ctx, "T1", "C1:1", []byte(`[{"role":"user"}]`), 0); err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}
	if _, err := s.MarkEventSeen(ctx, "T1", "Ev1"); err != nil {
		t.Fatalf("MarkEventSeen: %v", err)
	}
	if err := s.PutThreadContext(ctx, "T1", "D1:1", "C1"); err != nil {
		t.Fatalf("PutThreadContext: %v", err)
	}
	if err := s.PutPendingAction(ctx, "T1", "pending1", []byte(`{"action":"get"}`)); err != nil {
		t.Fatalf("PutPendingAction: %v", err)
	}
	if _, err := s.ClaimPendingAction(ctx, "T1", "pending1"); err != nil {
		t.Fatalf("ClaimPendingAction: %v", err)
	}
	if err := s.PutAuditEntry(ctx, "T1", &AuditEntry{Actor: "U1", Action: "get", Target: "r_1"}); err != nil {
		t.Fatalf("PutAuditEntry: %v", err)
	}
	fake.items["T1|"+rateSKPrefix+"team#1700000000"] = map[string]ddbtypes.AttributeValue{
		attrAgentPK:   stringAttr("T1"),
		attrAgentSK:   stringAttr(rateSKPrefix + "team#1700000000"),
		attrTurnCount: numberAttr(1),
		attrAgentTTL:  numberAttr(1700003600),
	}
	if err := s.SaveConversation(ctx, "T2", "C2:1", []byte(`[{"role":"user"}]`), 0); err != nil {
		t.Fatalf("SaveConversation T2: %v", err)
	}

	if err := s.PurgeWorkspaceAgentState(ctx, "T1"); err != nil {
		t.Fatalf("PurgeWorkspaceAgentState: %v", err)
	}
	if fake.deleteCalls != 7 {
		t.Fatalf("DeleteItem calls = %d, want 7", fake.deleteCalls)
	}
	for key := range fake.items {
		if strings.HasPrefix(key, "T1|") {
			t.Fatalf("T1 agent-state row survived purge: %s", key)
		}
	}
	if _, ok := fake.items["T2|"+convSKPrefix+"C2:1"]; !ok {
		t.Fatal("purge removed another workspace's agent-state row")
	}

	if err := s.PurgeWorkspaceAgentState(ctx, "T1"); err != nil {
		t.Fatalf("second purge should be idempotent: %v", err)
	}
}

func TestPurgeWorkspaceAgentState_ValidationAndErrors(t *testing.T) {
	t.Run("empty partition", func(t *testing.T) {
		s := newTestAgentStore(newAgentFakeDDB())
		err := s.PurgeWorkspaceAgentState(context.Background(), "")
		var ae *Error
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusBadRequest {
			t.Fatalf("err = %v, want 400 *Error", err)
		}
	})

	t.Run("query error", func(t *testing.T) {
		fake := newAgentFakeDDB()
		fake.queryErr = errors.New("ddb query down")
		s := newTestAgentStore(fake)
		err := s.PurgeWorkspaceAgentState(context.Background(), "T1")
		var ae *Error
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("err = %v, want 503 *Error", err)
		}
	})

	t.Run("delete error", func(t *testing.T) {
		fake := newAgentFakeDDB()
		s := newTestAgentStore(fake)
		if err := s.SaveConversation(context.Background(), "T1", "C1:1", []byte("x"), 0); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		fake.deleteErr = errors.New("ddb delete down")
		err := s.PurgeWorkspaceAgentState(context.Background(), "T1")
		var ae *Error
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("err = %v, want 503 *Error", err)
		}
		if _, ok := fake.items["T1|"+convSKPrefix+"C1:1"]; !ok {
			t.Fatal("delete-error path should not remove the row in the fake")
		}
	})
}
