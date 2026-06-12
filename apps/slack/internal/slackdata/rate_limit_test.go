package slackdata

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type rateLimitFakeDDB struct {
	mu          sync.Mutex
	items       map[string]map[string]ddbtypes.AttributeValue
	updateErr   error
	getErr      error
	updateCalls int
	getCalls    int
}

func newRateLimitFakeDDB() *rateLimitFakeDDB {
	return &rateLimitFakeDDB{
		items: make(map[string]map[string]ddbtypes.AttributeValue),
	}
}

func (f *rateLimitFakeDDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	key := readString(in.Key, attrSlackTeamID)
	return &dynamodb.GetItemOutput{Item: cloneRateLimitItem(f.items[key])}, nil
}

func (f *rateLimitFakeDDB) PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	return &dynamodb.PutItemOutput{}, nil
}

func (f *rateLimitFakeDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.updateCalls++
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	key := readString(in.Key, attrSlackTeamID)
	if key == "" {
		return nil, errors.New("missing slack_team_id key")
	}
	if aws.ToString(in.UpdateExpression) == "SET #kind = :kind, #subject_team_id = :team_id, #slack_user_id = :slack_user_id, #window_start = :window_start, #mint_count = :one, #updated_at = :now" {
		return f.resetWindow(in, key)
	}
	return f.incrementWindow(in, key)
}

func (f *rateLimitFakeDDB) DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	return &dynamodb.DeleteItemOutput{}, nil
}

func (f *rateLimitFakeDDB) Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	return &dynamodb.QueryOutput{}, nil
}

func (f *rateLimitFakeDDB) incrementWindow(in *dynamodb.UpdateItemInput, key string) (*dynamodb.UpdateItemOutput, error) {
	item, present := f.items[key]
	windowStart := readNumber(in.ExpressionAttributeValues, ":window_start")
	limit := readNumber(in.ExpressionAttributeValues, ":limit")

	_, hasWindowStart := item[attrRateLimitWindowStart]
	_, hasCount := item[attrRateLimitMintCount]
	currentWindowOK := !present || !hasWindowStart || readNumber(item, attrRateLimitWindowStart) == windowStart
	countOK := !present || !hasCount || readNumber(item, attrRateLimitMintCount) < limit
	if !currentWindowOK || !countOK {
		return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("rate limit exceeded")}
	}

	if !present {
		item = map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(key)}
		f.items[key] = item
	}
	item[attrRateLimitKind] = in.ExpressionAttributeValues[":kind"]
	item[attrRateLimitSubjectTeamID] = in.ExpressionAttributeValues[":team_id"]
	item[attrRateLimitSlackUserID] = in.ExpressionAttributeValues[":slack_user_id"]
	if !hasWindowStart {
		item[attrRateLimitWindowStart] = in.ExpressionAttributeValues[":window_start"]
	}
	item[attrUpdatedAt] = in.ExpressionAttributeValues[":now"]
	item[attrRateLimitMintCount] = numberAttr(readNumber(item, attrRateLimitMintCount) + 1)
	return &dynamodb.UpdateItemOutput{}, nil
}

func (f *rateLimitFakeDDB) resetWindow(in *dynamodb.UpdateItemInput, key string) (*dynamodb.UpdateItemOutput, error) {
	item, present := f.items[key]
	windowStart := readNumber(in.ExpressionAttributeValues, ":window_start")
	if present && readNumber(item, attrRateLimitWindowStart) >= windowStart {
		return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("window already reset")}
	}
	if !present {
		item = map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(key)}
		f.items[key] = item
	}
	item[attrRateLimitKind] = in.ExpressionAttributeValues[":kind"]
	item[attrRateLimitSubjectTeamID] = in.ExpressionAttributeValues[":team_id"]
	item[attrRateLimitSlackUserID] = in.ExpressionAttributeValues[":slack_user_id"]
	item[attrRateLimitWindowStart] = in.ExpressionAttributeValues[":window_start"]
	item[attrRateLimitMintCount] = in.ExpressionAttributeValues[":one"]
	item[attrUpdatedAt] = in.ExpressionAttributeValues[":now"]
	return &dynamodb.UpdateItemOutput{}, nil
}

func (f *rateLimitFakeDDB) item(key string) map[string]ddbtypes.AttributeValue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneRateLimitItem(f.items[key])
}

func cloneRateLimitItem(in map[string]ddbtypes.AttributeValue) map[string]ddbtypes.AttributeValue {
	if in == nil {
		return nil
	}
	out := make(map[string]ddbtypes.AttributeValue, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func newRateLimitStore(clk *time.Time, ddb *rateLimitFakeDDB) *Store {
	s := newStore(ddb)
	s.Now = func() time.Time { return *clk }
	return s
}

func TestCheckRateLimit_FirstMintWritesGlobalCounter(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	ddb := newRateLimitFakeDDB()
	s := newRateLimitStore(&clk, ddb)

	allowed, retry, err := s.CheckRateLimit(context.Background(), "U1", "T1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("first mint denied; want allowed")
	}
	if retry != 0 {
		t.Errorf("retry = %v on allowed mint, want 0", retry)
	}

	item := ddb.item(mintRateLimitKey("T1", "U1"))
	if got := readNumber(item, attrRateLimitMintCount); got != 1 {
		t.Errorf("mint_count = %d, want 1", got)
	}
	if got := readNumber(item, attrRateLimitWindowStart); got != mintWindowStart(clk).Unix() {
		t.Errorf("window_start = %d, want %d", got, mintWindowStart(clk).Unix())
	}
	if got := readString(item, attrRateLimitSubjectTeamID); got != "T1" {
		t.Errorf("subject team = %q, want T1", got)
	}
	if got := readString(item, attrRateLimitSlackUserID); got != "U1" {
		t.Errorf("slack user = %q, want U1", got)
	}
}

func TestCheckRateLimit_BurstThenDeny(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	ddb := newRateLimitFakeDDB()
	s := newRateLimitStore(&clk, ddb)
	s.MintRatePerHour = 2

	for i := 0; i < 2; i++ {
		allowed, _, err := s.CheckRateLimit(context.Background(), "U1", "T1")
		if err != nil {
			t.Fatalf("mint %d: unexpected error: %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("mint %d/2 denied; want allowed", i+1)
		}
	}

	allowed, retry, err := s.CheckRateLimit(context.Background(), "U1", "T1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatal("third mint allowed under 2/hr budget; want denied")
	}
	if want := mintWindowStart(clk).Add(time.Hour).Sub(clk); retry != want {
		t.Errorf("retry = %v, want %v", retry, want)
	}
}

func TestCheckRateLimit_ResetsAtNextWindow(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	ddb := newRateLimitFakeDDB()
	s := newRateLimitStore(&clk, ddb)
	s.MintRatePerHour = 1

	if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); !allowed {
		t.Fatal("first mint denied; want allowed")
	}
	if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); allowed {
		t.Fatal("second mint in same 1/hr window allowed; want denied")
	}

	clk = mintWindowStart(clk).Add(time.Hour)
	allowed, retry, err := s.CheckRateLimit(context.Background(), "U1", "T1")
	if err != nil {
		t.Fatalf("unexpected error after window rollover: %v", err)
	}
	if !allowed {
		t.Fatal("mint after window rollover denied; want allowed")
	}
	if retry != 0 {
		t.Errorf("retry = %v after rollover, want 0", retry)
	}

	item := ddb.item(mintRateLimitKey("T1", "U1"))
	if got := readNumber(item, attrRateLimitMintCount); got != 1 {
		t.Errorf("mint_count after reset = %d, want 1", got)
	}
	if got := readNumber(item, attrRateLimitWindowStart); got != clk.Unix() {
		t.Errorf("window_start after reset = %d, want %d", got, clk.Unix())
	}
}

func TestCheckRateLimit_PerTeamUserIsolation(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	ddb := newRateLimitFakeDDB()
	s := newRateLimitStore(&clk, ddb)
	s.MintRatePerHour = 1

	if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); !allowed {
		t.Fatal("first T1/U1 mint denied; want allowed")
	}
	if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T1"); allowed {
		t.Fatal("second T1/U1 mint allowed; want denied")
	}
	if allowed, _, _ := s.CheckRateLimit(context.Background(), "U2", "T1"); !allowed {
		t.Fatal("T1/U2 mint denied after T1/U1 drained; want isolated budget")
	}
	if allowed, _, _ := s.CheckRateLimit(context.Background(), "U1", "T2"); !allowed {
		t.Fatal("T2/U1 mint denied after T1/U1 drained; want team-scoped budget")
	}
}

func TestCheckRateLimit_ConcurrentSingleUserNeverExceedsBudget(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	ddb := newRateLimitFakeDDB()
	s := newRateLimitStore(&clk, ddb)

	const goroutines = 64
	const perGoroutine = 4
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				ok, _, err := s.CheckRateLimit(context.Background(), "U1", "T1")
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				if ok {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != int64(mintRatePerHour) {
		t.Errorf("concurrent single-user allows = %d, want exactly %d", got, mintRatePerHour)
	}
}

func TestCheckRateLimit_DDBErrorSurfaces(t *testing.T) {
	clk := time.Unix(1_700_000_000, 0).UTC()
	ddb := newRateLimitFakeDDB()
	ddb.updateErr = errors.New("injected update failure")
	s := newRateLimitStore(&clk, ddb)

	allowed, retry, err := s.CheckRateLimit(context.Background(), "U1", "T1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if allowed {
		t.Error("allowed = true on DDB error, want false")
	}
	if retry != 0 {
		t.Errorf("retry = %v on DDB error, want 0", retry)
	}
}
