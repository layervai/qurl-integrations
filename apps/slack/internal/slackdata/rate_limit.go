package slackdata

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Per-user mint rate limiting - strategy and tradeoffs.
//
// CheckRateLimit is the in-bot per-Slack-user mint-rate gate for `/qurl get`.
// Pre-pivot this was an HTTP call to qurl-service
// `/internal/v1/admin/rate-limit/check`; post-pivot (Justin's 2026-05-12
// review on qurl-integrations-infra#523) qurl-service is integration-agnostic
// and doesn't track per-Slack-user mint counts, so the rate-limit surface stays
// in-bot.
//
// IMPLEMENTATION: a DynamoDB-backed fixed-window counter keyed by Slack
// workspace + Slack user. The counter row lives in workspace_mappings under a
// reserved synthetic slack_team_id prefix, so the existing bot-owned table and
// IAM grants cover the write without an infra migration. Each successful mint
// attempt spends one count in the current one-hour window via an atomic
// conditional UpdateItem. The first task to observe a new hour resets the
// single synthetic row under a conditional guard; racing tasks retry through
// the normal increment path.
//
// Tradeoffs:
//   - This is global across ECS tasks and survives redeploys, matching the
//     multi-AZ production shape.
//   - It is a fixed hourly window rather than a smoothing token bucket. A user
//     can spend the full budget near the end of one hour and again at the
//     start of the next, but cannot exceed the configured count within any
//     single window.
//   - It adds one DynamoDB write per successful mint attempt. That is acceptable
//     here because `/qurl get` already performs DDB reads for workspace/policy
//     state before minting.

const (
	// mintRatePerHour is the default per-(slack_team_id, slack_user_id) mint
	// budget per fixed one-hour window. 30/hr is the pre-pivot enforcement
	// value the original HTTP gate applied. [NewStore] copies this into
	// [Store.MintRatePerHour]; callers can override the field to tune tests.
	mintRatePerHour = 30

	mintRateLimitWindow      = time.Hour
	mintRateLimitMaxAttempts = 3
	mintRateLimitKeyPrefix   = "__rate_limit#mint#"

	attrRateLimitKind          = "rate_limit_kind"
	attrRateLimitKindMint      = "slack_mint"
	attrRateLimitSubjectTeamID = "subject_slack_team_id"
	attrRateLimitSlackUserID   = "slack_user_id"
	attrRateLimitWindowStart   = "window_start"
	attrRateLimitMintCount     = "mint_count"
)

// CheckRateLimit reports whether slackUserID may mint another link right now.
// On denial it returns the wall-clock time until the current fixed window
// closes so the caller can tell the user when to retry.
func (s *Store) CheckRateLimit(ctx context.Context, slackUserID, teamID string) (allowed bool, retry time.Duration, err error) {
	if slackUserID == "" || teamID == "" {
		return false, 0, &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "CheckRateLimit: slack_user_id and team_id are required",
		}
	}
	if s.Client == nil || s.WorkspaceMappingsName == "" {
		return false, 0, &Error{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "ddb_not_configured",
			Title:      "CheckRateLimit: workspace_mappings store is not configured",
		}
	}

	limit := s.MintRatePerHour
	if limit <= 0 {
		limit = mintRatePerHour
	}

	now := s.nowOrDefault().UTC()
	windowStart := mintWindowStart(now)
	key := mintRateLimitKey(teamID, slackUserID)

	for attempt := 0; attempt < mintRateLimitMaxAttempts; attempt++ {
		err := s.incrementMintWindow(ctx, key, teamID, slackUserID, windowStart, now, limit)
		if err == nil {
			return true, 0, nil
		}
		if !isConditionalCheckFailed(err) {
			return false, 0, ddbToError("CheckRateLimit", err)
		}

		allowed, retry, shouldRetry, err := s.resolveMintRateLimitConflict(ctx, key, teamID, slackUserID, windowStart, now, limit)
		if err != nil || allowed || retry > 0 {
			return allowed, retry, err
		}
		if !shouldRetry {
			break
		}
	}

	return false, 0, &Error{
		StatusCode: http.StatusServiceUnavailable,
		Code:       "rate_limit_contention",
		Title:      "CheckRateLimit: too much concurrent rate-limit contention",
	}
}

func (s *Store) incrementMintWindow(ctx context.Context, key, teamID, slackUserID string, windowStart, now time.Time, limit int) error {
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(key),
		},
		UpdateExpression: aws.String("SET #kind = :kind, #subject_team_id = :team_id, #slack_user_id = :slack_user_id, #window_start = if_not_exists(#window_start, :window_start), #updated_at = :now ADD #mint_count :one"),
		ConditionExpression: aws.String(
			"(attribute_not_exists(#window_start) OR #window_start = :window_start) AND " +
				"(attribute_not_exists(#mint_count) OR #mint_count < :limit)",
		),
		ExpressionAttributeNames: rateLimitExpressionNames(),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":kind":          stringAttr(attrRateLimitKindMint),
			":team_id":       stringAttr(teamID),
			":slack_user_id": stringAttr(slackUserID),
			":window_start":  numberAttr(windowStart.Unix()),
			":now":           stringAttr(now.Format(time.RFC3339)),
			":one":           numberAttr(1),
			":limit":         numberAttr(int64(limit)),
		},
	})
	return err
}

func (s *Store) resolveMintRateLimitConflict(ctx context.Context, key, teamID, slackUserID string, windowStart, now time.Time, limit int) (allowed bool, retry time.Duration, shouldRetry bool, err error) {
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(key),
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return false, 0, false, ddbToError("CheckRateLimit", err)
	}
	if len(out.Item) == 0 {
		return false, 0, true, nil
	}

	storedWindow := readNumber(out.Item, attrRateLimitWindowStart)
	count := readNumber(out.Item, attrRateLimitMintCount)
	currentWindow := windowStart.Unix()

	if storedWindow < currentWindow {
		err := s.resetMintWindow(ctx, key, teamID, slackUserID, windowStart, now)
		if err == nil {
			return true, 0, false, nil
		}
		if isConditionalCheckFailed(err) {
			return false, 0, true, nil
		}
		return false, 0, false, ddbToError("CheckRateLimit", err)
	}
	if storedWindow > currentWindow {
		return false, mintRetryAfter(now, time.Unix(storedWindow, 0).UTC()), false, nil
	}
	if count >= int64(limit) {
		return false, mintRetryAfter(now, time.Unix(storedWindow, 0).UTC()), false, nil
	}
	return false, 0, true, nil
}

func (s *Store) resetMintWindow(ctx context.Context, key, teamID, slackUserID string, windowStart, now time.Time) error {
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(key),
		},
		UpdateExpression:         aws.String("SET #kind = :kind, #subject_team_id = :team_id, #slack_user_id = :slack_user_id, #window_start = :window_start, #mint_count = :one, #updated_at = :now"),
		ConditionExpression:      aws.String("attribute_not_exists(#window_start) OR #window_start < :window_start"),
		ExpressionAttributeNames: rateLimitExpressionNames(),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":kind":          stringAttr(attrRateLimitKindMint),
			":team_id":       stringAttr(teamID),
			":slack_user_id": stringAttr(slackUserID),
			":window_start":  numberAttr(windowStart.Unix()),
			":one":           numberAttr(1),
			":now":           stringAttr(now.Format(time.RFC3339)),
		},
	})
	return err
}

func rateLimitExpressionNames() map[string]string {
	return map[string]string{
		"#kind":            attrRateLimitKind,
		"#subject_team_id": attrRateLimitSubjectTeamID,
		"#slack_user_id":   attrRateLimitSlackUserID,
		"#window_start":    attrRateLimitWindowStart,
		"#mint_count":      attrRateLimitMintCount,
		"#updated_at":      attrUpdatedAt,
	}
}

func mintRateLimitKey(teamID, slackUserID string) string {
	return mintRateLimitKeyPrefix + teamID + "#" + slackUserID
}

func mintWindowStart(now time.Time) time.Time {
	return now.UTC().Truncate(mintRateLimitWindow)
}

func mintRetryAfter(now, windowStart time.Time) time.Duration {
	retry := windowStart.Add(mintRateLimitWindow).Sub(now)
	if retry < time.Second {
		return time.Second
	}
	return retry
}

func isConditionalCheckFailed(err error) bool {
	var ccfe *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &ccfe)
}
