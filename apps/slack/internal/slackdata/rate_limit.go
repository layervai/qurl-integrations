package slackdata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	mintRateLimitWindow = time.Hour
	mintRateLimitMax    = int64(30)
	attrMintWindowStart = "mint_window_start"
	attrMintCount       = "mint_count"
)

// CheckRateLimit is the in-bot per-user mint-rate gate. Pre-pivot
// this was an HTTP call to qurl-service `/internal/v1/admin/rate-
// limit/check`; post-pivot (Justin's 2026-05-12 review on
// qurl-integrations-infra#523) qurl-service is integration-agnostic
// and doesn't track per-Slack-user mint counts. The rate-limit
// surface stays in-bot.
//
// The chosen strategy is a DynamoDB fixed-window counter item in the
// channel_policies table. Each Slack user gets one hashed counter key
// whose mint_window_start/mint_count fields are updated atomically, so
// the gate is shared by every Fargate task and survives restarts without
// adding a table or creating one row per user per hour.
func (s *Store) CheckRateLimit(ctx context.Context, slackUserID, teamID string) (allowed bool, retry time.Duration, err error) {
	if slackUserID == "" || teamID == "" {
		return false, 0, &Error{StatusCode: http.StatusBadRequest, Title: "CheckRateLimit: slack_user_id and team_id are required"}
	}
	now := s.nowOrDefault().UTC()
	windowStart := now.Truncate(mintRateLimitWindow)
	windowEnd := windowStart.Add(mintRateLimitWindow)
	windowUnix := windowStart.Unix()
	counterKey := mintRateLimitCounterKey(slackUserID)

	if allowed, err := mintCounterWriteResult(s.incrementMintCounter(ctx, teamID, counterKey, windowUnix)); allowed || err != nil {
		return allowed, 0, err
	}

	item, err := s.getMintCounter(ctx, teamID, counterKey)
	if err != nil {
		return false, 0, ddbToError("CheckRateLimit", err)
	}
	storedWindow := readNumber(item, attrMintWindowStart)
	count := readNumber(item, attrMintCount)
	// If another task has already advanced the counter into a later window
	// (hour-boundary race or clock skew), follow that authoritative item instead
	// of resetting it backward or denying while capacity remains.
	if storedWindow > windowUnix {
		futureWindowEnd := time.Unix(storedWindow, 0).UTC().Add(mintRateLimitWindow)
		if count >= mintRateLimitMax {
			return false, futureWindowEnd.Sub(now), nil
		}
		if allowed, err := mintCounterWriteResult(s.incrementMintCounter(ctx, teamID, counterKey, storedWindow)); allowed || err != nil {
			return allowed, 0, err
		}
		return false, futureWindowEnd.Sub(now), nil
	}
	if storedWindow == windowUnix && count >= mintRateLimitMax {
		return false, windowEnd.Sub(now), nil
	}
	if storedWindow == windowUnix {
		if allowed, err := mintCounterWriteResult(s.incrementMintCounter(ctx, teamID, counterKey, windowUnix)); allowed || err != nil {
			return allowed, 0, err
		}
		// A conditional miss after a fresh under-limit read means another writer
		// won the remaining capacity or advanced the window. Deny conservatively
		// rather than spend another read chasing a narrow race.
		return false, windowEnd.Sub(now), nil
	}

	if allowed, err := mintCounterWriteResult(s.resetMintCounter(ctx, teamID, counterKey, windowUnix)); allowed || err != nil {
		return allowed, 0, err
	}
	if allowed, err := mintCounterWriteResult(s.incrementMintCounter(ctx, teamID, counterKey, windowUnix)); allowed || err != nil {
		return allowed, 0, err
	}
	return false, windowEnd.Sub(now), nil
}

func mintCounterWriteResult(err error) (allowed bool, fatal error) {
	if err == nil {
		return true, nil
	}
	if isConditionalCheckFailed(err) {
		return false, nil
	}
	return false, ddbToError("CheckRateLimit", err)
}

func (s *Store) incrementMintCounter(ctx context.Context, teamID, counterKey string, windowUnix int64) error {
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(counterKey),
		},
		UpdateExpression:    aws.String("ADD " + attrMintCount + " :one"),
		ConditionExpression: aws.String(attrMintWindowStart + " = :window AND " + attrMintCount + " < :limit"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":one":    numberAttr(1),
			":window": numberAttr(windowUnix),
			":limit":  numberAttr(mintRateLimitMax),
		},
	})
	return err
}

func (s *Store) getMintCounter(ctx context.Context, teamID, counterKey string) (map[string]ddbtypes.AttributeValue, error) {
	out, getErr := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.ChannelPoliciesName),
		ConsistentRead: aws.Bool(true),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(counterKey),
		},
	})
	if getErr != nil {
		return nil, getErr
	}
	return out.Item, nil
}

func (s *Store) resetMintCounter(ctx context.Context, teamID, counterKey string, windowUnix int64) error {
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(counterKey),
		},
		UpdateExpression:    aws.String("SET " + attrMintWindowStart + " = :window, " + attrMintCount + " = :one"),
		ConditionExpression: aws.String("attribute_not_exists(" + attrMintWindowStart + ") OR " + attrMintWindowStart + " < :window"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":one":    numberAttr(1),
			":window": numberAttr(windowUnix),
		},
	})
	return err
}

func isConditionalCheckFailed(err error) bool {
	var ccfe *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &ccfe)
}

func mintRateLimitCounterKey(slackUserID string) string {
	sum := sha256.Sum256([]byte(slackUserID))
	return "rate_limit#" + hex.EncodeToString(sum[:])[:16]
}
