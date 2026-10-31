package slackdata

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	testRateLimitTeamID = "TTEAM001"
	testRateLimitUserID = "UUSER001"
)

func TestCheckRateLimit_DisabledSkipsValidationAndDDB(t *testing.T) {
	ddb := newRateLimitDDB(false)
	store := newRateLimitStore(ddb)

	allowed, retry, err := store.CheckRateLimit(context.Background(), "", "")
	if err != nil {
		t.Fatalf("CheckRateLimit disabled: %v", err)
	}
	if !allowed || retry != 0 {
		t.Fatalf("CheckRateLimit disabled = allowed %v retry %s, want allowed/no retry", allowed, retry)
	}
	if ddb.getCalls != 0 || len(ddb.updateKeys) != 0 {
		t.Fatalf("disabled CheckRateLimit touched DDB: gets=%d updates=%v", ddb.getCalls, ddb.updateKeys)
	}
}

func TestCheckRateLimit_EnabledRequiresTeamAndUser(t *testing.T) {
	ddb := newRateLimitDDB(true)
	store := newRateLimitStore(ddb)
	store.RateLimitEnabled = true

	allowed, retry, err := store.CheckRateLimit(context.Background(), "", testRateLimitTeamID)
	if err == nil {
		t.Fatal("CheckRateLimit missing user error = nil")
	}
	var storeErr *Error
	if !errors.As(err, &storeErr) || storeErr.StatusCode != 400 {
		t.Fatalf("CheckRateLimit missing user err = %#v, want 400", err)
	}
	if allowed || retry != 0 {
		t.Fatalf("CheckRateLimit missing user = allowed %v retry %s, want denied/no retry", allowed, retry)
	}
	if ddb.getCalls != 0 || len(ddb.updateKeys) != 0 {
		t.Fatalf("missing user touched DDB: gets=%d updates=%v", ddb.getCalls, ddb.updateKeys)
	}
}

func TestCheckRateLimit_UsesSyntheticCounterItemAndResetsWindow(t *testing.T) {
	ddb := newRateLimitDDB(true)
	store := newRateLimitStore(ddb)
	store.RateLimitEnabled = true
	store.RateLimitLimit = 2
	store.RateLimitWindow = time.Hour

	now := time.Date(2026, 6, 17, 12, 5, 0, 0, time.UTC)
	store.Now = func() time.Time { return now }

	for i := 1; i <= 2; i++ {
		allowed, retry, err := store.CheckRateLimit(context.Background(), testRateLimitUserID, testRateLimitTeamID)
		if err != nil {
			t.Fatalf("CheckRateLimit call %d: %v", i, err)
		}
		if !allowed || retry != 0 {
			t.Fatalf("CheckRateLimit call %d = allowed %v retry %s, want allowed/no retry", i, allowed, retry)
		}
		if got := readString(ddb.items[rateLimitKey(testRateLimitTeamID, testRateLimitUserID)], attrRateLimitPartition); got != "7083a9b052bfa93936e9d6bdd90eb21cf52832dad031599bb7e6c507ba927385#a" {
			t.Fatalf("stored partition on call %d = %q", i, got)
		}
	}

	allowed, retry, err := store.CheckRateLimit(context.Background(), testRateLimitUserID, testRateLimitTeamID)
	if err != nil {
		t.Fatalf("CheckRateLimit denied call: %v", err)
	}
	if allowed {
		t.Fatal("third call allowed, want denied at limit")
	}
	if retry != 55*time.Minute {
		t.Fatalf("retry = %s, want 55m", retry)
	}

	counterKey := rateLimitKey(testRateLimitTeamID, testRateLimitUserID)
	if strings.Contains(counterKey, testRateLimitUserID) {
		t.Fatalf("counter key %q leaked raw Slack user ID", counterKey)
	}
	if _, ok := ddb.items[testRateLimitTeamID]; ok {
		t.Fatalf("rate limit mutated the real workspace row %q", testRateLimitTeamID)
	}
	for _, key := range ddb.updateKeys {
		if key != counterKey {
			t.Fatalf("UpdateItem key = %q, want synthetic counter key %q", key, counterKey)
		}
	}

	now = now.Add(time.Hour)
	allowed, retry, err = store.CheckRateLimit(context.Background(), testRateLimitUserID, testRateLimitTeamID)
	if err != nil {
		t.Fatalf("CheckRateLimit after rollover: %v", err)
	}
	if !allowed || retry != 0 {
		t.Fatalf("CheckRateLimit after rollover = allowed %v retry %s, want allowed/no retry", allowed, retry)
	}
	if got := readString(ddb.items[counterKey], attrRateLimitPartition); got != "7083a9b052bfa93936e9d6bdd90eb21cf52832dad031599bb7e6c507ba927385#a" {
		t.Fatalf("stored partition after reset = %q", got)
	}
	if got := readNumber(ddb.items[counterKey], attrRateLimitCount); got != 1 {
		t.Fatalf("counter after rollover = %d, want reset to 1", got)
	}
	if got, want := readNumber(ddb.items[counterKey], attrRateLimitExpiresAt), now.Truncate(time.Hour).Add(2*time.Hour).Unix(); got != want {
		t.Fatalf("counter expires_at = %d, want %d", got, want)
	}
	if got, want := readNumber(ddb.items[counterKey], attrUpdatedAtNano), now.UnixNano(); got != want {
		t.Fatalf("counter updated_at_unix_nano = %d, want %d", got, want)
	}
}

func TestCheckRateLimit_RepairsConcurrentInitializeRace(t *testing.T) {
	ddb := newRateLimitDDB(true)
	ddb.raceInitializeWindow = true
	store := newRateLimitStore(ddb)
	store.RateLimitEnabled = true
	store.RateLimitLimit = 2

	allowed, retry, err := store.CheckRateLimit(context.Background(), testRateLimitUserID, testRateLimitTeamID)
	if err != nil {
		t.Fatalf("CheckRateLimit concurrent init: %v", err)
	}
	if !allowed || retry != 0 {
		t.Fatalf("CheckRateLimit concurrent init = allowed %v retry %s, want allowed/no retry", allowed, retry)
	}
	counterKey := rateLimitKey(testRateLimitTeamID, testRateLimitUserID)
	if got := readNumber(ddb.items[counterKey], attrRateLimitCount); got != 2 {
		t.Fatalf("counter after concurrent init repair = %d, want 2", got)
	}
	if len(ddb.updateKeys) != 3 {
		t.Fatalf("updates after concurrent init repair = %v, want initial increment, lost init, retry increment", ddb.updateKeys)
	}
}

func TestCheckRateLimit_WorkspaceNotBoundDoesNotCreateCounter(t *testing.T) {
	ddb := newRateLimitDDB(false)
	store := newRateLimitStore(ddb)
	store.RateLimitEnabled = true

	allowed, retry, err := store.CheckRateLimit(context.Background(), testRateLimitUserID, testRateLimitTeamID)
	if err == nil {
		t.Fatal("CheckRateLimit unbound workspace error = nil")
	}
	var storeErr *Error
	if !errors.As(err, &storeErr) || storeErr.StatusCode != 404 || storeErr.Code != ErrCodeWorkspaceNotBound {
		t.Fatalf("CheckRateLimit unbound err = %#v, want workspace-not-bound 404", err)
	}
	if allowed || retry != 0 {
		t.Fatalf("CheckRateLimit unbound = allowed %v retry %s, want denied/no retry", allowed, retry)
	}
	if len(ddb.updateKeys) != 0 {
		t.Fatalf("unbound workspace created/updated counters: %v", ddb.updateKeys)
	}
}

func TestPurgeTeamRateLimitCountersBefore(t *testing.T) {
	cutoff := time.Unix(1700000000, 0).UTC()
	prefix := rateLimitTeamPrefix("T1")
	first := prefix + strings.Repeat("0", 32)
	second := prefix + "0" + strings.Repeat("1", 31)
	t.Run("disabled makes no queries or scans", func(t *testing.T) {
		store := newRateLimitStore(&stubDDB{queryFn: func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			t.Fatal("disabled cleanup queried DDB")
			return nil, nil //nolint:nilnil // Exercise a malformed empty SDK response.
		}, scanFn: func(*dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
			t.Fatal("cleanup scanned the table")
			return nil, nil //nolint:nilnil // Exercise a malformed empty SDK response.
		}})
		store.RateLimitEnabled = false
		if err := store.PurgeTeamRateLimitCountersBefore(context.Background(), "T1", cutoff); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("queries bounded workspace shards and preserves pagination and cutoff", func(t *testing.T) {
		queries, deleted := 0, 0
		store := newRateLimitStore(&stubDDB{
			scanFn: func(*dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
				t.Fatal("cleanup must never scan the table")
				return nil, nil //nolint:nilnil // Exercise a malformed empty SDK response.
			},
			queryFn: func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
				queries++
				if aws.ToString(in.TableName) != "workspace_mappings" || aws.ToString(in.IndexName) != rateLimitIndexName || aws.ToInt32(in.Limit) != 100 || aws.ToBool(in.ConsistentRead) {
					t.Fatal("wrong index/query budget")
				}
				if aws.ToString(in.KeyConditionExpression) != "#partition = :partition AND begins_with(#tid, :prefix)" || aws.ToString(in.ProjectionExpression) != "#tid" {
					t.Fatal("query escaped its key-only workspace scope")
				}
				if readString(in.ExpressionAttributeValues, ":prefix") != prefix {
					t.Fatal("wrong workspace prefix")
				}
				if readString(in.ExpressionAttributeValues, ":partition") != rateLimitPartition("T1", "0") {
					return &dynamodb.QueryOutput{}, nil
				}
				if len(in.ExclusiveStartKey) == 0 {
					return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{{attrSlackTeamID: stringAttr(first)}}, LastEvaluatedKey: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(first), attrRateLimitPartition: stringAttr(rateLimitPartition("T1", "0"))}}, nil
				}
				if readString(in.ExclusiveStartKey, attrSlackTeamID) != first || readString(in.ExclusiveStartKey, attrRateLimitPartition) != rateLimitPartition("T1", "0") {
					t.Fatal("lost index pagination key")
				}
				return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{{attrSlackTeamID: stringAttr(second)}}}, nil
			},
			deleteItemFn: func(in *dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
				deleted++
				if aws.ToString(in.ConditionExpression) != purgeCutoffCondition || readNumber(in.ExpressionAttributeValues, exprPurgeCutoffNano) != cutoff.UnixNano() {
					t.Fatal("lost reinstall cutoff guard")
				}
				return &dynamodb.DeleteItemOutput{}, nil
			},
		})
		store.RateLimitEnabled = true
		if err := store.PurgeTeamRateLimitCountersBefore(context.Background(), "T1", cutoff); err != nil {
			t.Fatal(err)
		}
		if queries != rateLimitShardCount+1 || deleted != 2 {
			t.Fatalf("queries=%d deletes=%d", queries, deleted)
		}
	})
	t.Run("delete errors do not stop siblings or later shards", func(t *testing.T) {
		deleted := 0
		store := newRateLimitStore(&stubDDB{queryFn: func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			if readString(in.ExpressionAttributeValues, ":partition") != rateLimitPartition("T1", "0") {
				return &dynamodb.QueryOutput{}, nil
			}
			return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{{attrSlackTeamID: stringAttr(first)}, {attrSlackTeamID: stringAttr(second)}}}, nil
		}, deleteItemFn: func(*dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
			deleted++
			if deleted == 1 {
				return nil, errors.New("DDB unavailable")
			}
			return &dynamodb.DeleteItemOutput{}, nil
		}})
		store.RateLimitEnabled = true
		if err := store.PurgeTeamRateLimitCountersBefore(context.Background(), "T1", cutoff); err == nil {
			t.Fatal("lost delete failure")
		}
		if deleted != 2 {
			t.Fatal("a failed delete stopped sibling cleanup")
		}
	})
	t.Run("newer counter is retained after conditional delete refusal", func(t *testing.T) {
		store := newRateLimitStore(&stubDDB{queryFn: func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			if readString(in.ExpressionAttributeValues, ":partition") != rateLimitPartition("T1", "0") {
				return &dynamodb.QueryOutput{}, nil
			}
			return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{{attrSlackTeamID: stringAttr(first)}}}, nil
		}, deleteItemFn: func(*dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
			return nil, &ddbtypes.ConditionalCheckFailedException{}
		}})
		store.RateLimitEnabled = true
		if err := store.PurgeTeamRateLimitCountersBefore(context.Background(), "T1", cutoff); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("foreign index row is never deleted", func(t *testing.T) {
		store := newRateLimitStore(&stubDDB{queryFn: func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{{attrSlackTeamID: stringAttr(rateLimitTeamPrefix("T2") + "foreign")}}}, nil
		}, deleteItemFn: func(*dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
			t.Fatal("deleted a foreign counter")
			return nil, nil //nolint:nilnil // Exercise a malformed empty SDK response.
		}})
		store.RateLimitEnabled = true
		if err := store.PurgeTeamRateLimitCountersBefore(context.Background(), "T1", cutoff); err == nil {
			t.Fatal("foreign index data was accepted")
		}
	})
	t.Run("missing query response is an error", func(t *testing.T) {
		store := newRateLimitStore(&stubDDB{queryFn: func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			return nil, nil //nolint:nilnil // Exercise a malformed empty SDK response.
		}})
		store.RateLimitEnabled = true
		if err := store.PurgeTeamRateLimitCountersBefore(context.Background(), "T1", cutoff); err == nil {
			t.Fatal("missing response accepted")
		}
	})
}

func newRateLimitStore(client DynamoDBClient) *Store {
	return &Store{
		Client:                client,
		WorkspaceMappingsName: "workspace_mappings",
		ChannelPoliciesName:   "channel_policies",
		RateLimitLimit:        defaultRateLimitLimit,
		RateLimitWindow:       defaultRateLimitWindow,
		Now:                   func() time.Time { return time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC) },
	}
}

type rateLimitDDB struct {
	workspaceBound       bool
	raceInitializeWindow bool
	items                map[string]map[string]ddbtypes.AttributeValue
	getCalls             int
	updateKeys           []string
}

func newRateLimitDDB(workspaceBound bool) *rateLimitDDB {
	return &rateLimitDDB{
		workspaceBound: workspaceBound,
		items:          map[string]map[string]ddbtypes.AttributeValue{},
	}
}

func (f *rateLimitDDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.getCalls++
	key := rateLimitTestKey(in.Key)
	if key == testRateLimitTeamID {
		if !f.workspaceBound {
			return &dynamodb.GetItemOutput{}, nil
		}
		return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(testRateLimitTeamID),
		}}, nil
	}
	item, ok := f.items[key]
	if !ok {
		return &dynamodb.GetItemOutput{}, nil
	}
	return &dynamodb.GetItemOutput{Item: cloneRateLimitTestItem(item)}, nil
}

func (f *rateLimitDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	key := rateLimitTestKey(in.Key)
	f.updateKeys = append(f.updateKeys, key)

	existing, present := f.items[key]
	if f.raceInitializeWindow && aws.ToString(in.ConditionExpression) == "attribute_not_exists(#window)" {
		f.raceInitializeWindow = false
		f.items[key] = map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:        stringAttr(key),
			attrRateLimitWindow:    in.ExpressionAttributeValues[":window"],
			attrRateLimitCount:     in.ExpressionAttributeValues[":one"],
			attrRateLimitExpiresAt: in.ExpressionAttributeValues[":expires_at"],
		}
		return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("conditional check failed")}
	}
	if cond := aws.ToString(in.ConditionExpression); cond != "" {
		ok, err := f.rateLimitConditionOK(cond, existing, present, in.ExpressionAttributeValues)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, condFailure(in.ReturnValuesOnConditionCheckFailure, existing)
		}
	}

	item := cloneRateLimitTestItem(existing)
	if !present {
		item = map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(key)}
	}
	switch aws.ToString(in.UpdateExpression) {
	case "SET #updated_at_nano = :now_nano, #partition = :partition ADD #count :one":
		item[attrRateLimitCount] = numberAttr(readNumber(item, attrRateLimitCount) + 1)
		item[attrUpdatedAtNano] = in.ExpressionAttributeValues[":now_nano"]
		item[attrRateLimitPartition] = in.ExpressionAttributeValues[":partition"]
	case "SET #window = :window, #count = :one, #expires_at = :expires_at, #updated_at_nano = :now_nano, #partition = :partition":
		item[attrRateLimitWindow] = in.ExpressionAttributeValues[":window"]
		item[attrRateLimitCount] = in.ExpressionAttributeValues[":one"]
		item[attrRateLimitExpiresAt] = in.ExpressionAttributeValues[":expires_at"]
		item[attrUpdatedAtNano] = in.ExpressionAttributeValues[":now_nano"]
		item[attrRateLimitPartition] = in.ExpressionAttributeValues[":partition"]
	default:
		return nil, errors.New("unexpected UpdateExpression: " + aws.ToString(in.UpdateExpression))
	}
	f.items[key] = item
	return &dynamodb.UpdateItemOutput{Attributes: cloneRateLimitTestItem(item)}, nil
}

func (f *rateLimitDDB) rateLimitConditionOK(cond string, item map[string]ddbtypes.AttributeValue, present bool, vals map[string]ddbtypes.AttributeValue) (bool, error) {
	switch cond {
	case "#window = :window AND #count < :limit":
		return present &&
			readNumber(item, attrRateLimitWindow) == readNumber(vals, ":window") &&
			readNumber(item, attrRateLimitCount) < readNumber(vals, ":limit"), nil
	case "attribute_not_exists(#window)":
		if !present {
			return true, nil
		}
		_, ok := item[attrRateLimitWindow]
		return !ok, nil
	case "#window = :old_window":
		return present && readNumber(item, attrRateLimitWindow) == readNumber(vals, ":old_window"), nil
	default:
		return false, errors.New("unexpected ConditionExpression: " + cond)
	}
}

func (f *rateLimitDDB) PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	return nil, errors.New("not implemented")
}

func (f *rateLimitDDB) DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	return nil, errors.New("not implemented")
}

func (f *rateLimitDDB) Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	return nil, errors.New("not implemented")
}

func rateLimitTestKey(key map[string]ddbtypes.AttributeValue) string {
	return readString(key, attrSlackTeamID)
}

func cloneRateLimitTestItem(item map[string]ddbtypes.AttributeValue) map[string]ddbtypes.AttributeValue {
	out := make(map[string]ddbtypes.AttributeValue, len(item))
	for k, v := range item {
		out[k] = v
	}
	return out
}

func TestRateLimitIndexGoldenProtocol(t *testing.T) {
	data, err := os.ReadFile("testdata/rate-limit-partitions.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		TeamID     string `json:"team_id"`
		UserID     string `json:"user_id"`
		CounterKey string `json:"counter_key"`
		Partition  string `json:"index_partition"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("migration vectors are empty")
	}
	for _, fixture := range cases {
		key := rateLimitKey(fixture.TeamID, fixture.UserID)
		scope := strings.TrimPrefix(key, rateLimitTeamPrefix(fixture.TeamID))
		if key != fixture.CounterKey || rateLimitPartition(fixture.TeamID, scope[:1]) != fixture.Partition {
			t.Errorf("%s/%s: key=%q partition=%q, want %q / %q", fixture.TeamID, fixture.UserID, key, rateLimitPartition(fixture.TeamID, scope[:1]), fixture.CounterKey, fixture.Partition)
		}
	}
}
