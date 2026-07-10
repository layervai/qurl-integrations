package slackdata

import (
	"context"
	"errors"
	"net/http"
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

func TestCheckRateLimit_FollowsFutureWindowUnderClockSkew(t *testing.T) {
	ddb := newRateLimitDDB(true)
	store := newRateLimitStore(ddb)
	store.RateLimitEnabled = true
	store.RateLimitLimit = 2
	store.RateLimitWindow = time.Hour

	now := time.Date(2026, 6, 17, 12, 59, 59, 0, time.UTC)
	store.Now = func() time.Time { return now }
	localWindowUnix := now.Truncate(time.Hour).Unix()
	futureWindowUnix := localWindowUnix + int64(time.Hour/time.Second)
	counterKey := rateLimitKey(testRateLimitTeamID, testRateLimitUserID)
	ddb.items[counterKey] = map[string]ddbtypes.AttributeValue{
		attrSlackTeamID:        stringAttr(counterKey),
		attrRateLimitWindow:    numberAttr(futureWindowUnix),
		attrRateLimitCount:     numberAttr(1),
		attrRateLimitExpiresAt: numberAttr(time.Unix(futureWindowUnix, 0).Add(2 * time.Hour).Unix()),
	}

	allowed, retry, err := store.CheckRateLimit(context.Background(), testRateLimitUserID, testRateLimitTeamID)
	if err != nil {
		t.Fatalf("CheckRateLimit future window: %v", err)
	}
	if !allowed || retry != 0 {
		t.Fatalf("CheckRateLimit future window = allowed %v retry %s, want allowed/no retry", allowed, retry)
	}
	if got := readNumber(ddb.items[counterKey], attrRateLimitWindow); got != futureWindowUnix {
		t.Fatalf("counter window = %d, want future window %d (must not reset backward to %d)", got, futureWindowUnix, localWindowUnix)
	}
	if got := readNumber(ddb.items[counterKey], attrRateLimitCount); got != 2 {
		t.Fatalf("counter count = %d, want 2", got)
	}
}

func TestCheckRateLimit_DeniesAtFutureWindowLimit(t *testing.T) {
	ddb := newRateLimitDDB(true)
	store := newRateLimitStore(ddb)
	store.RateLimitEnabled = true
	store.RateLimitLimit = 2
	store.RateLimitWindow = time.Hour

	now := time.Date(2026, 6, 17, 12, 59, 59, 0, time.UTC)
	store.Now = func() time.Time { return now }
	futureWindowUnix := now.Truncate(time.Hour).Add(time.Hour).Unix()
	counterKey := rateLimitKey(testRateLimitTeamID, testRateLimitUserID)
	ddb.items[counterKey] = map[string]ddbtypes.AttributeValue{
		attrSlackTeamID:        stringAttr(counterKey),
		attrRateLimitWindow:    numberAttr(futureWindowUnix),
		attrRateLimitCount:     numberAttr(2),
		attrRateLimitExpiresAt: numberAttr(time.Unix(futureWindowUnix, 0).Add(2 * time.Hour).Unix()),
	}

	allowed, retry, err := store.CheckRateLimit(context.Background(), testRateLimitUserID, testRateLimitTeamID)
	if err != nil {
		t.Fatalf("CheckRateLimit future window at limit: %v", err)
	}
	if allowed {
		t.Fatal("future-window at-limit call allowed, want denied")
	}
	wantRetry := time.Unix(futureWindowUnix, 0).Add(time.Hour).Sub(now)
	if retry != wantRetry {
		t.Fatalf("retry = %s, want %s", retry, wantRetry)
	}
	if got := readNumber(ddb.items[counterKey], attrRateLimitWindow); got != futureWindowUnix {
		t.Fatalf("counter window = %d, want future window %d", got, futureWindowUnix)
	}
}

func TestCheckRateLimit_DeniesFutureWindowRaceAfterRead(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 59, 59, 0, time.UTC)
	window := time.Hour
	localWindowUnix := now.Truncate(window).Unix()
	futureWindowUnix := localWindowUnix + int64(window/time.Second)
	limit := 2
	newerWindowUnix := futureWindowUnix + int64(window/time.Second)

	for _, tc := range []struct {
		name         string
		secondWindow int64
		secondCount  int64
		wantWindow   int64
	}{
		{
			name:         "same future window consumed capacity",
			secondWindow: futureWindowUnix,
			secondCount:  int64(limit),
			wantWindow:   futureWindowUnix,
		},
		{
			name:         "newer future window wins retry hint",
			secondWindow: newerWindowUnix,
			secondCount:  int64(limit),
			wantWindow:   newerWindowUnix,
		},
		{
			name:         "backward reset keeps conservative retry hint",
			secondWindow: localWindowUnix,
			secondCount:  int64(limit - 1),
			wantWindow:   futureWindowUnix,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updates := 0
			store := newRateLimitStore(&stubDDB{
				getItemFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
					if rateLimitTestKey(in.Key) != testRateLimitTeamID {
						t.Fatalf("unexpected GetItem key %#v", in.Key)
					}
					return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
						attrSlackTeamID: stringAttr(testRateLimitTeamID),
					}}, nil
				},
				updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
					updates++
					switch updates {
					case 1:
						if got := readNumber(in.ExpressionAttributeValues, ":window"); got != localWindowUnix {
							t.Fatalf("first update window = %d, want local %d", got, localWindowUnix)
						}
						return nil, &ddbtypes.ConditionalCheckFailedException{
							Message: aws.String("future window"),
							Item: map[string]ddbtypes.AttributeValue{
								attrRateLimitWindow: numberAttr(futureWindowUnix),
								attrRateLimitCount:  numberAttr(int64(limit - 1)),
							},
						}
					case 2:
						if got := readNumber(in.ExpressionAttributeValues, ":window"); got != futureWindowUnix {
							t.Fatalf("second update window = %d, want future %d", got, futureWindowUnix)
						}
						return nil, &ddbtypes.ConditionalCheckFailedException{
							Message: aws.String("future race"),
							Item: map[string]ddbtypes.AttributeValue{
								attrRateLimitWindow: numberAttr(tc.secondWindow),
								attrRateLimitCount:  numberAttr(tc.secondCount),
							},
						}
					default:
						t.Fatalf("unexpected UpdateItem call %d", updates)
						return nil, errors.New("unreachable")
					}
				},
			})
			store.RateLimitEnabled = true
			store.RateLimitLimit = limit
			store.RateLimitWindow = window
			store.Now = func() time.Time { return now }

			allowed, retry, err := store.CheckRateLimit(context.Background(), testRateLimitUserID, testRateLimitTeamID)
			if err != nil {
				t.Fatalf("CheckRateLimit future-window race: %v", err)
			}
			if allowed {
				t.Fatal("future-window race allowed, want denied")
			}
			wantRetry := time.Unix(tc.wantWindow, 0).UTC().Add(window).Sub(now)
			if retry != wantRetry {
				t.Fatalf("retry = %s, want %s", retry, wantRetry)
			}
			// These fixtures each consume the local-window attempt plus one
			// future-window follow-up; they are not asserting a global limit.
			if updates != 2 {
				t.Fatalf("updates = %d, want 2", updates)
			}
		})
	}
}

func TestCheckRateLimit_AllowsNewerFutureWindowRaceAfterRead(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 59, 59, 0, time.UTC)
	window := time.Hour
	localWindowUnix := now.Truncate(window).Unix()
	futureWindowUnix := localWindowUnix + int64(window/time.Second)
	newerWindowUnix := futureWindowUnix + int64(window/time.Second)
	limit := 2

	updates := 0
	store := newRateLimitStore(&stubDDB{
		getItemFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if rateLimitTestKey(in.Key) != testRateLimitTeamID {
				t.Fatalf("unexpected GetItem key %#v", in.Key)
			}
			return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
				attrSlackTeamID: stringAttr(testRateLimitTeamID),
			}}, nil
		},
		updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			updates++
			switch updates {
			case 1:
				if got := readNumber(in.ExpressionAttributeValues, ":window"); got != localWindowUnix {
					t.Fatalf("first update window = %d, want local %d", got, localWindowUnix)
				}
				return nil, &ddbtypes.ConditionalCheckFailedException{
					Message: aws.String("future window"),
					Item: map[string]ddbtypes.AttributeValue{
						attrRateLimitWindow: numberAttr(futureWindowUnix),
						attrRateLimitCount:  numberAttr(int64(limit - 1)),
					},
				}
			case 2:
				if got := readNumber(in.ExpressionAttributeValues, ":window"); got != futureWindowUnix {
					t.Fatalf("second update window = %d, want future %d", got, futureWindowUnix)
				}
				return nil, &ddbtypes.ConditionalCheckFailedException{
					Message: aws.String("newer future window"),
					Item: map[string]ddbtypes.AttributeValue{
						attrRateLimitWindow: numberAttr(newerWindowUnix),
						attrRateLimitCount:  numberAttr(int64(limit - 1)),
					},
				}
			case 3:
				if got := readNumber(in.ExpressionAttributeValues, ":window"); got != newerWindowUnix {
					t.Fatalf("third update window = %d, want newer %d", got, newerWindowUnix)
				}
				return &dynamodb.UpdateItemOutput{}, nil
			default:
				t.Fatalf("unexpected UpdateItem call %d", updates)
				return nil, errors.New("unreachable")
			}
		},
	})
	store.RateLimitEnabled = true
	store.RateLimitLimit = limit
	store.RateLimitWindow = window
	store.Now = func() time.Time { return now }

	allowed, retry, err := store.CheckRateLimit(context.Background(), testRateLimitUserID, testRateLimitTeamID)
	if err != nil {
		t.Fatalf("CheckRateLimit newer future-window race: %v", err)
	}
	if !allowed || retry != 0 {
		t.Fatalf("CheckRateLimit newer future-window race = allowed %v retry %s, want allowed/no retry", allowed, retry)
	}
	if updates != 3 {
		t.Fatalf("updates = %d, want 3", updates)
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
	t.Run("disabled skips full-table scan", func(t *testing.T) {
		scanned := false
		store := newRateLimitStore(&stubDDB{
			scanFn: func(*dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
				scanned = true
				return &dynamodb.ScanOutput{}, nil
			},
		})

		if err := store.PurgeTeamRateLimitCountersBefore(context.Background(), "T1", time.Now()); err != nil {
			t.Fatalf("PurgeTeamRateLimitCountersBefore disabled: %v", err)
		}
		if scanned {
			t.Fatal("disabled rate-limit purge issued a full-table scan")
		}
	})

	t.Run("scans prefix and deletes paged counters with cutoff guard", func(t *testing.T) {
		cutoff := time.Date(2026, 7, 8, 12, 0, 0, 123, time.UTC)
		prefix := rateLimitTeamPrefix("T1")
		var scans []*dynamodb.ScanInput
		var deleted []string
		store := newRateLimitStore(&stubDDB{
			scanFn: func(in *dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
				scans = append(scans, in)
				if aws.ToString(in.TableName) != "workspace_mappings" {
					t.Fatalf("Scan table = %q, want workspace_mappings", aws.ToString(in.TableName))
				}
				if got := aws.ToString(in.FilterExpression); got != "begins_with(#tid, :prefix)" {
					t.Fatalf("Scan FilterExpression = %q", got)
				}
				if got := aws.ToString(in.ProjectionExpression); got != "#tid" {
					t.Fatalf("Scan ProjectionExpression = %q", got)
				}
				if got := in.ExpressionAttributeNames["#tid"]; got != attrSlackTeamID {
					t.Fatalf("Scan #tid = %q, want %q", got, attrSlackTeamID)
				}
				if got := readString(map[string]ddbtypes.AttributeValue{attrSlackTeamID: in.ExpressionAttributeValues[":prefix"]}, attrSlackTeamID); got != prefix {
					t.Fatalf("Scan :prefix = %q, want %q", got, prefix)
				}
				switch len(scans) {
				case 1:
					if len(in.ExclusiveStartKey) != 0 {
						t.Fatalf("first scan ExclusiveStartKey = %v, want empty", in.ExclusiveStartKey)
					}
					return &dynamodb.ScanOutput{
						Items: []map[string]ddbtypes.AttributeValue{
							{attrSlackTeamID: stringAttr(prefix + "user-a")},
						},
						LastEvaluatedKey: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(prefix + "user-a")},
					}, nil
				case 2:
					if got := readString(in.ExclusiveStartKey, attrSlackTeamID); got != prefix+"user-a" {
						t.Fatalf("second scan ExclusiveStartKey = %q, want first page key", got)
					}
					return &dynamodb.ScanOutput{
						Items: []map[string]ddbtypes.AttributeValue{
							{attrSlackTeamID: stringAttr(prefix + "user-b")},
						},
					}, nil
				default:
					t.Fatalf("unexpected scan call %d", len(scans))
					return nil, errors.New("unexpected scan call")
				}
			},
			deleteItemFn: func(in *dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
				key := readString(in.Key, attrSlackTeamID)
				deleted = append(deleted, key)
				if got := aws.ToString(in.ConditionExpression); got != purgeCutoffCondition {
					t.Fatalf("Delete ConditionExpression = %q", got)
				}
				if got := in.ExpressionAttributeNames["#updated_at_nano"]; got != attrUpdatedAtNano {
					t.Fatalf("Delete #updated_at_nano = %q, want %q", got, attrUpdatedAtNano)
				}
				if got, want := readNumber(in.ExpressionAttributeValues, ":purge_cutoff_nano"), cutoff.UnixNano(); got != want {
					t.Fatalf("Delete cutoff = %d, want %d", got, want)
				}
				return &dynamodb.DeleteItemOutput{}, nil
			},
		})
		store.RateLimitEnabled = true

		if err := store.PurgeTeamRateLimitCountersBefore(context.Background(), "T1", cutoff); err != nil {
			t.Fatalf("PurgeTeamRateLimitCountersBefore: %v", err)
		}
		wantDeleted := []string{prefix + "user-a", prefix + "user-b"}
		if strings.Join(deleted, ",") != strings.Join(wantDeleted, ",") {
			t.Fatalf("deleted = %v, want %v", deleted, wantDeleted)
		}
	})

	t.Run("delete errors do not stop remaining counters", func(t *testing.T) {
		prefix := rateLimitTeamPrefix("T1")
		var deleted []string
		store := newRateLimitStore(&stubDDB{
			scanFn: func(*dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
				return &dynamodb.ScanOutput{
					Items: []map[string]ddbtypes.AttributeValue{
						{attrSlackTeamID: stringAttr(prefix + "fail")},
						{attrSlackTeamID: stringAttr(prefix + "ok")},
					},
				}, nil
			},
			deleteItemFn: func(in *dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
				key := readString(in.Key, attrSlackTeamID)
				deleted = append(deleted, key)
				if strings.HasSuffix(key, "fail") {
					return nil, errors.New("ddb delete down")
				}
				return &dynamodb.DeleteItemOutput{}, nil
			},
		})
		store.RateLimitEnabled = true

		err := store.PurgeTeamRateLimitCountersBefore(context.Background(), "T1", time.Time{})
		if err == nil {
			t.Fatal("PurgeTeamRateLimitCountersBefore: want joined delete error")
		}
		var storeErr *Error
		if !errors.As(err, &storeErr) || storeErr.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("PurgeTeamRateLimitCountersBefore err = %v, want joined 503 *Error", err)
		}
		wantDeleted := []string{prefix + "fail", prefix + "ok"}
		if strings.Join(deleted, ",") != strings.Join(wantDeleted, ",") {
			t.Fatalf("deleted = %v, want %v", deleted, wantDeleted)
		}
	})

	t.Run("guarded delete skips counters updated after cutoff", func(t *testing.T) {
		var deletes int
		store := newRateLimitStore(&stubDDB{
			scanFn: func(*dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
				return &dynamodb.ScanOutput{
					Items: []map[string]ddbtypes.AttributeValue{
						{attrSlackTeamID: stringAttr(rateLimitTeamPrefix("T1") + "new")},
					},
				}, nil
			},
			deleteItemFn: func(*dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
				deletes++
				return nil, &ddbtypes.ConditionalCheckFailedException{}
			},
		})
		store.RateLimitEnabled = true

		if err := store.PurgeTeamRateLimitCountersBefore(context.Background(), "T1", time.Now()); err != nil {
			t.Fatalf("PurgeTeamRateLimitCountersBefore newer row err = %v, want nil", err)
		}
		if deletes != 1 {
			t.Fatalf("DeleteItem calls = %d, want 1", deletes)
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
			condErr := &ddbtypes.ConditionalCheckFailedException{Message: aws.String("conditional check failed")}
			if in.ReturnValuesOnConditionCheckFailure == ddbtypes.ReturnValuesOnConditionCheckFailureAllOld && present {
				condErr.Item = cloneRateLimitTestItem(existing)
			}
			return nil, condErr
		}
	}

	item := cloneRateLimitTestItem(existing)
	if !present {
		item = map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(key)}
	}
	switch aws.ToString(in.UpdateExpression) {
	case "SET #updated_at_nano = :now_nano ADD #count :one":
		item[attrRateLimitCount] = numberAttr(readNumber(item, attrRateLimitCount) + 1)
		item[attrUpdatedAtNano] = in.ExpressionAttributeValues[":now_nano"]
	case "SET #window = :window, #count = :one, #expires_at = :expires_at, #updated_at_nano = :now_nano":
		item[attrRateLimitWindow] = in.ExpressionAttributeValues[":window"]
		item[attrRateLimitCount] = in.ExpressionAttributeValues[":one"]
		item[attrRateLimitExpiresAt] = in.ExpressionAttributeValues[":expires_at"]
		item[attrUpdatedAtNano] = in.ExpressionAttributeValues[":now_nano"]
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
