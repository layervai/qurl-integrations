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

func TestCheckRateLimit_AllowsUntilHourlyLimitThenReturnsRetry(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 42, 0, 0, time.UTC)
	windowStart := now.Truncate(mintRateLimitWindow)
	counterKey := mintRateLimitCounterKey(testCallerSlackID)
	item := map[string]ddbtypes.AttributeValue{
		attrSlackTeamID:     stringAttr("T123"),
		attrSlackChannelID:  stringAttr(counterKey),
		attrMintWindowStart: numberAttr(windowStart.Unix()),
		attrMintCount:       numberAttr(mintRateLimitMax - 1),
	}
	store := newStore(&stubDDB{
		updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			if got := aws.ToString(in.UpdateExpression); got != "ADD mint_count :one SET #ttl = :ttl" {
				t.Fatalf("UpdateExpression = %q", got)
			}
			if got := in.ExpressionAttributeNames["#ttl"]; got != attrMintTTL {
				t.Fatalf("ExpressionAttributeNames[#ttl] = %q, want %q", got, attrMintTTL)
			}
			if got := readNumber(in.ExpressionAttributeValues, ":ttl"); got != mintCounterExpiresAt(windowStart.Unix()) {
				t.Fatalf(":ttl = %d, want %d", got, mintCounterExpiresAt(windowStart.Unix()))
			}
			if got := aws.ToString(in.ConditionExpression); got != "mint_window_start = :window AND mint_count < :limit" {
				t.Fatalf("ConditionExpression = %q", got)
			}
			if got := readString(in.Key, attrSlackChannelID); got != counterKey {
				t.Fatalf("counter key = %q, want %q", got, counterKey)
			}
			return &dynamodb.UpdateItemOutput{}, nil
		},
		getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
	})
	store.Now = func() time.Time { return now }

	allowed, retry, err := store.CheckRateLimit(context.Background(), testCallerSlackID, "T123")
	if err != nil {
		t.Fatalf("CheckRateLimit under limit error = %v", err)
	}
	if !allowed || retry != 0 {
		t.Fatalf("under limit allowed=%v retry=%s, want allowed with no retry", allowed, retry)
	}

	store.Client = &stubDDB{
		updateItemFn: func(_ *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("at limit")}
		},
		getItemFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if !aws.ToBool(in.ConsistentRead) {
				t.Fatalf("denied-path GetItem ConsistentRead = false, want true")
			}
			item[attrMintCount] = numberAttr(mintRateLimitMax)
			return &dynamodb.GetItemOutput{Item: item}, nil
		},
	}
	allowed, retry, err = store.CheckRateLimit(context.Background(), testCallerSlackID, "T123")
	if err != nil {
		t.Fatalf("CheckRateLimit at limit error = %v", err)
	}
	if allowed {
		t.Fatalf("at limit allowed = true, want false")
	}
	if retry != 18*time.Minute {
		t.Fatalf("retry = %s, want 18m", retry)
	}
}

func TestCheckRateLimit_ResetsStaleWindow(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 42, 0, 0, time.UTC)
	windowUnix := now.Truncate(mintRateLimitWindow).Unix()
	store := newStore(&stubDDB{
		updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			if got := aws.ToString(in.UpdateExpression); strings.HasPrefix(got, "ADD mint_count :one") {
				return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("stale window")}
			}
			if got := aws.ToString(in.UpdateExpression); got != "SET mint_window_start = :window, mint_count = :one, #ttl = :ttl" {
				t.Fatalf("reset UpdateExpression = %q", got)
			}
			if got := in.ExpressionAttributeNames["#ttl"]; got != attrMintTTL {
				t.Fatalf("reset ExpressionAttributeNames[#ttl] = %q, want %q", got, attrMintTTL)
			}
			if got := aws.ToString(in.ConditionExpression); got != "attribute_not_exists(mint_window_start) OR mint_window_start < :window" {
				t.Fatalf("reset ConditionExpression = %q", got)
			}
			if got := readNumber(in.ExpressionAttributeValues, ":window"); got != windowUnix {
				t.Fatalf(":window = %d, want %d", got, windowUnix)
			}
			if got := readNumber(in.ExpressionAttributeValues, ":ttl"); got != mintCounterExpiresAt(windowUnix) {
				t.Fatalf(":ttl = %d, want %d", got, mintCounterExpiresAt(windowUnix))
			}
			return &dynamodb.UpdateItemOutput{}, nil
		},
		getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
				attrMintWindowStart: numberAttr(windowUnix - int64(mintRateLimitWindow/time.Second)),
				attrMintCount:       numberAttr(mintRateLimitMax),
			}}, nil
		},
	})
	store.Now = func() time.Time { return now }

	allowed, retry, err := store.CheckRateLimit(context.Background(), testCallerSlackID, "T123")
	if err != nil {
		t.Fatalf("CheckRateLimit stale window error = %v", err)
	}
	if !allowed || retry != 0 {
		t.Fatalf("stale window allowed=%v retry=%s, want allowed/no retry", allowed, retry)
	}
}

func TestCheckRateLimit_FollowsFutureWindowUnderClockSkew(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 59, 59, 0, time.UTC)
	windowUnix := now.Truncate(mintRateLimitWindow).Unix()
	futureWindowUnix := windowUnix + int64(mintRateLimitWindow/time.Second)
	var updates int
	store := newStore(&stubDDB{
		updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			updates++
			switch updates {
			case 1:
				if got := readNumber(in.ExpressionAttributeValues, ":window"); got != windowUnix {
					t.Fatalf("first :window = %d, want local window %d", got, windowUnix)
				}
				return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("future window")}
			case 2:
				if got := readNumber(in.ExpressionAttributeValues, ":window"); got != futureWindowUnix {
					t.Fatalf("second :window = %d, want future window %d", got, futureWindowUnix)
				}
				return &dynamodb.UpdateItemOutput{}, nil
			default:
				t.Fatalf("unexpected UpdateItem call %d", updates)
				return nil, errors.New("unreachable")
			}
		},
		getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
				attrMintWindowStart: numberAttr(futureWindowUnix),
				attrMintCount:       numberAttr(mintRateLimitMax - 1),
			}}, nil
		},
	})
	store.Now = func() time.Time { return now }

	allowed, retry, err := store.CheckRateLimit(context.Background(), testCallerSlackID, "T123")
	if err != nil {
		t.Fatalf("CheckRateLimit future window error = %v", err)
	}
	if !allowed || retry != 0 {
		t.Fatalf("future window allowed=%v retry=%s, want allowed/no retry", allowed, retry)
	}
}

func TestCheckRateLimit_DeniesWhenCurrentWindowRaceConsumesCapacity(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 42, 0, 0, time.UTC)
	windowUnix := now.Truncate(mintRateLimitWindow).Unix()
	var updates int
	store := newStore(&stubDDB{
		updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			updates++
			if got := readNumber(in.ExpressionAttributeValues, ":window"); got != windowUnix {
				t.Fatalf("update %d :window = %d, want %d", updates, got, windowUnix)
			}
			return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("race")}
		},
		getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
				attrMintWindowStart: numberAttr(windowUnix),
				attrMintCount:       numberAttr(mintRateLimitMax - 1),
			}}, nil
		},
	})
	store.Now = func() time.Time { return now }

	allowed, retry, err := store.CheckRateLimit(context.Background(), testCallerSlackID, "T123")
	if err != nil {
		t.Fatalf("CheckRateLimit current-window race error = %v", err)
	}
	if allowed {
		t.Fatal("current-window race allowed = true, want conservative deny")
	}
	if retry != 18*time.Minute {
		t.Fatalf("retry = %s, want 18m", retry)
	}
	if updates != 2 {
		t.Fatalf("UpdateItem calls = %d, want 2", updates)
	}
}

func TestCheckRateLimit_DeniesWhenFutureWindowRaceConsumesCapacity(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 59, 59, 0, time.UTC)
	windowUnix := now.Truncate(mintRateLimitWindow).Unix()
	futureWindowUnix := windowUnix + int64(mintRateLimitWindow/time.Second)
	var updates int
	store := newStore(&stubDDB{
		updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			updates++
			switch updates {
			case 1:
				if got := readNumber(in.ExpressionAttributeValues, ":window"); got != windowUnix {
					t.Fatalf("first :window = %d, want local window %d", got, windowUnix)
				}
			case 2:
				if got := readNumber(in.ExpressionAttributeValues, ":window"); got != futureWindowUnix {
					t.Fatalf("second :window = %d, want future window %d", got, futureWindowUnix)
				}
			default:
				t.Fatalf("unexpected UpdateItem call %d", updates)
			}
			return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("future race")}
		},
		getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
				attrMintWindowStart: numberAttr(futureWindowUnix),
				attrMintCount:       numberAttr(mintRateLimitMax - 1),
			}}, nil
		},
	})
	store.Now = func() time.Time { return now }

	allowed, retry, err := store.CheckRateLimit(context.Background(), testCallerSlackID, "T123")
	if err != nil {
		t.Fatalf("CheckRateLimit future-window race error = %v", err)
	}
	if allowed {
		t.Fatal("future-window race allowed = true, want conservative deny")
	}
	wantRetry := time.Unix(futureWindowUnix, 0).UTC().Add(mintRateLimitWindow).Sub(now)
	if retry != wantRetry {
		t.Fatalf("retry = %s, want %s", retry, wantRetry)
	}
	if updates != 2 {
		t.Fatalf("UpdateItem calls = %d, want 2", updates)
	}
}

func TestCheckRateLimit_Validation(t *testing.T) {
	store := newStore(&stubDDB{})
	if _, _, err := store.CheckRateLimit(context.Background(), "", "T123"); err == nil {
		t.Fatalf("missing user err = nil, want validation error")
	}
}

func TestCheckRateLimit_FailsClosedOnDynamoError(t *testing.T) {
	store := newStore(&stubDDB{
		updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			return nil, errors.New("ddb down")
		},
		getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			t.Fatal("GetItem must not run after a non-conditional counter write error")
			return nil, errors.New("unreachable")
		},
	})

	allowed, retry, err := store.CheckRateLimit(context.Background(), testCallerSlackID, "T123")
	if err == nil {
		t.Fatalf("CheckRateLimit error = nil, want DDB failure")
	}
	if allowed || retry != 0 {
		t.Fatalf("allowed=%v retry=%s, want fail-closed denial with no retry hint", allowed, retry)
	}
}
