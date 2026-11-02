package slackdata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	exprRatePartition      = "#partition"
	exprRatePartitionValue = ":partition"
	rateLimitIndexName     = "rate-limit-partition"
	attrRateLimitPartition = "rate_limit_partition"
	rateLimitShardCount    = 16
	defaultRateLimitLimit  = 30
	defaultRateLimitWindow = time.Hour
	rateLimitKeyPrefix     = "__slack_rate_limit#"
	attrRateLimitCount     = "rate_limit_count"
	// Table-wide DDB TTL reaps any workspace_mappings item carrying a numeric
	// expires_at. Only synthetic counter items may write this attribute; real
	// workspace binding rows must never carry it.
	attrRateLimitExpiresAt = "expires_at"
	attrRateLimitWindow    = "rate_limit_window"
)

// CheckRateLimit is the in-bot per-(team, user) qURL command gate. When the
// feature flag is off it preserves the historical sandbox/open-gate behavior.
// When enabled it stores one synthetic counter item per Slack team/user in the
// workspace_mappings table, leaving the real workspace row bounded and avoiding
// a shared hot write item for large workspaces.
//
// The workspace existence read is deliberately kept inside this method even
// though current handlers authenticate first. That preserves the public method's
// invariant that unbound teams never create synthetic counter rows, including if
// a future call site invokes it before auth.
func (s *Store) CheckRateLimit(ctx context.Context, slackUserID, teamID string) (allowed bool, retry time.Duration, err error) {
	if !s.RateLimitEnabled {
		return true, 0, nil
	}
	if slackUserID == "" || teamID == "" {
		return false, 0, &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "CheckRateLimit: team_id and user_id are required",
		}
	}
	if err := s.ensureRateLimitWorkspaceBound(ctx, teamID); err != nil {
		return false, 0, err
	}

	window := s.rateLimitWindow()
	limit := s.rateLimitLimit()
	now := s.nowOrDefault().UTC()
	windowStart := now.Truncate(window)
	windowUnix := windowStart.Unix()
	retry = windowStart.Add(window).Sub(now)

	counterKey := rateLimitKey(teamID, slackUserID)
	userScope := strings.TrimPrefix(counterKey, rateLimitTeamPrefix(teamID))
	partition := rateLimitPartition(teamID, userScope[:1])
	ok, item, updateErr := s.incrementCurrentRateLimitWindow(ctx, counterKey, partition, now, windowUnix, limit)
	if updateErr != nil || ok {
		return ok, 0, updateErr
	}
	storedWindow, hasWindow := readRateLimitWindow(item)
	if hasWindow && storedWindow == windowUnix {
		return false, retry, nil
	}

	var resetOK bool
	if hasWindow {
		resetOK, updateErr = s.resetRateLimitWindow(ctx, counterKey, partition, now, windowUnix, storedWindow)
	} else {
		resetOK, updateErr = s.initializeRateLimitWindow(ctx, counterKey, partition, now, windowUnix)
	}
	if updateErr != nil || resetOK {
		return resetOK, 0, updateErr
	}

	// Another worker initialized/reset the window between our condition failure
	// and our repair write. Retry the normal current-window increment once.
	ok, item, updateErr = s.incrementCurrentRateLimitWindow(ctx, counterKey, partition, now, windowUnix, limit)
	if updateErr != nil || ok {
		return ok, 0, updateErr
	}
	if storedWindow, hasWindow = readRateLimitWindow(item); hasWindow && storedWindow == windowUnix {
		return false, retry, nil
	}
	return false, 0, &Error{
		StatusCode: http.StatusServiceUnavailable,
		Title:      "CheckRateLimit: concurrent counter update did not settle",
	}
}

func (s *Store) rateLimitLimit() int {
	if s.RateLimitLimit > 0 {
		return s.RateLimitLimit
	}
	return defaultRateLimitLimit
}

func (s *Store) rateLimitWindow() time.Duration {
	if s.RateLimitWindow > 0 {
		return s.RateLimitWindow
	}
	return defaultRateLimitWindow
}

func rateLimitKey(teamID, slackUserID string) string {
	// Keep raw Slack user IDs out of table keys while preserving a stable
	// per-(team,user) counter item.
	sum := sha256.Sum256([]byte(slackUserID))
	scope := hex.EncodeToString(sum[:16])
	return rateLimitTeamPrefix(teamID) + scope
}

func rateLimitTeamPrefix(teamID string) string {
	return rateLimitKeyPrefix + teamID + "#"
}

func rateLimitPartition(teamID, shard string) string {
	sum := sha256.Sum256([]byte(teamID))
	return hex.EncodeToString(sum[:]) + "#" + shard
}

func (s *Store) ensureRateLimitWorkspaceBound(ctx context.Context, teamID string) error {
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:            aws.String(s.WorkspaceMappingsName),
		ProjectionExpression: aws.String(attrSlackTeamID),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(teamID),
		},
	})
	if err != nil {
		return ddbToError("CheckRateLimit", err)
	}
	if len(out.Item) == 0 {
		return &Error{
			StatusCode: http.StatusNotFound,
			Code:       ErrCodeWorkspaceNotBound,
			Title:      "CheckRateLimit: workspace is not bound",
		}
	}
	return nil
}

func (s *Store) incrementCurrentRateLimitWindow(ctx context.Context, counterKey, partition string, now time.Time, windowUnix int64, limit int) (allowed bool, item map[string]ddbtypes.AttributeValue, err error) {
	_, err = s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(counterKey),
		},
		UpdateExpression:    aws.String("SET " + exprUpdatedAtNano + " = " + exprNowNano + ", #partition = :partition ADD #count " + exprOne),
		ConditionExpression: aws.String("#window = :window AND #count < :limit"),
		ExpressionAttributeNames: map[string]string{
			"#count":          attrRateLimitCount,
			exprRatePartition: attrRateLimitPartition,
			exprUpdatedAtNano: attrUpdatedAtNano,
			"#window":         attrRateLimitWindow,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			exprOne:                numberAttr(1),
			":now_nano":            unixNanoAttr(now),
			exprRatePartitionValue: stringAttr(partition),
			":window":              numberAttr(windowUnix),
			":limit":               numberAttr(int64(limit)),
		},
		ReturnValuesOnConditionCheckFailure: ddbtypes.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if err == nil {
		return true, nil, nil
	}
	var ccfe *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &ccfe) {
		return false, ccfe.Item, nil
	}
	return false, nil, ddbToError("CheckRateLimit", err)
}

func (s *Store) initializeRateLimitWindow(ctx context.Context, counterKey, partition string, now time.Time, windowUnix int64) (bool, error) {
	return s.setRateLimitWindow(ctx, counterKey, partition, now, windowUnix, "attribute_not_exists(#window)", nil)
}

func (s *Store) resetRateLimitWindow(ctx context.Context, counterKey, partition string, now time.Time, windowUnix, oldWindow int64) (bool, error) {
	return s.setRateLimitWindow(ctx, counterKey, partition, now, windowUnix, "#window = :old_window", map[string]ddbtypes.AttributeValue{
		":old_window": numberAttr(oldWindow),
	})
}

func (s *Store) setRateLimitWindow(ctx context.Context, counterKey, partition string, now time.Time, windowUnix int64, condition string, extra map[string]ddbtypes.AttributeValue) (bool, error) {
	values := map[string]ddbtypes.AttributeValue{
		":expires_at":          numberAttr(time.Unix(windowUnix, 0).Add(2 * s.rateLimitWindow()).Unix()),
		":now_nano":            unixNanoAttr(now),
		exprRatePartitionValue: stringAttr(partition),
		exprOne:                numberAttr(1),
		":window":              numberAttr(windowUnix),
	}
	for k, v := range extra {
		values[k] = v
	}
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(counterKey),
		},
		UpdateExpression:    aws.String("SET #window = :window, #count = " + exprOne + ", #expires_at = :expires_at, " + exprUpdatedAtNano + " = " + exprNowNano + ", #partition = :partition"),
		ConditionExpression: aws.String(condition),
		ExpressionAttributeNames: map[string]string{
			"#count":          attrRateLimitCount,
			exprRatePartition: attrRateLimitPartition,
			"#expires_at":     attrRateLimitExpiresAt,
			exprUpdatedAtNano: attrUpdatedAtNano,
			"#window":         attrRateLimitWindow,
		},
		ExpressionAttributeValues: values,
	})
	if err == nil {
		return true, nil
	}
	var ccfe *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &ccfe) {
		return false, nil
	}
	return false, ddbToError("CheckRateLimit", err)
}

// PurgeTeamRateLimitCountersBefore queries only this workspace's sparse index
// shards. The original cutoff still protects counters from a newer install.
// TODO(upstream-contract): deploy/backfill rate-limit-partition before this
// reader; old images may still create unindexed counters until drained (#930).
func (s *Store) PurgeTeamRateLimitCountersBefore(ctx context.Context, teamID string, cutoff time.Time) error {
	if teamID == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "PurgeTeamRateLimitCounters: team_id is required"}
	}
	if !s.RateLimitEnabled {
		return nil
	}
	var failures []error
	for shard := 0; shard < rateLimitShardCount; shard++ {
		if err := s.purgeRateLimitShard(ctx, teamID, fmt.Sprintf("%x", shard), cutoff); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Store) purgeRateLimitShard(ctx context.Context, teamID, shard string, cutoff time.Time) error {
	prefix := rateLimitTeamPrefix(teamID)
	var deleteErrs []error
	var startKey map[string]ddbtypes.AttributeValue
	for {
		out, err := s.Client.Query(ctx, &dynamodb.QueryInput{
			TableName: aws.String(s.WorkspaceMappingsName), IndexName: aws.String(rateLimitIndexName),
			KeyConditionExpression: aws.String("#partition = :partition AND begins_with(#tid, :prefix)"),
			ProjectionExpression:   aws.String("#tid"), Limit: aws.Int32(100),
			ExpressionAttributeNames:  map[string]string{exprRatePartition: attrRateLimitPartition, "#tid": attrSlackTeamID},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{exprRatePartitionValue: stringAttr(rateLimitPartition(teamID, shard)), ":prefix": stringAttr(prefix)},
			ExclusiveStartKey:         startKey,
		})
		if err != nil {
			return joinSweepErrors(deleteErrs, ddbToError("PurgeTeamRateLimitCounters", err))
		}
		if out == nil {
			return joinSweepErrors(deleteErrs, &Error{StatusCode: http.StatusServiceUnavailable, Title: "PurgeTeamRateLimitCounters: index returned no response"})
		}
		for _, item := range out.Items {
			counterKey := readString(item, attrSlackTeamID)
			if !strings.HasPrefix(counterKey, prefix) {
				deleteErrs = append(deleteErrs, &Error{
					StatusCode: http.StatusInternalServerError,
					Title:      "PurgeTeamRateLimitCounters: index returned a counter outside the requested workspace",
				})
				continue
			}
			deleteInput := &dynamodb.DeleteItemInput{
				TableName: aws.String(s.WorkspaceMappingsName),
				Key: map[string]ddbtypes.AttributeValue{
					attrSlackTeamID: stringAttr(counterKey),
				},
			}
			if !cutoff.IsZero() {
				deleteInput.ConditionExpression = aws.String(purgeCutoffCondition)
				deleteInput.ExpressionAttributeNames = map[string]string{
					exprUpdatedAtNano: attrUpdatedAtNano,
				}
				deleteInput.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{
					exprPurgeCutoffNano: unixNanoAttr(cutoff),
				}
			}
			if _, err := s.Client.DeleteItem(ctx, deleteInput); err != nil {
				var ccfe *ddbtypes.ConditionalCheckFailedException
				if !cutoff.IsZero() && errors.As(err, &ccfe) {
					continue
				}
				deleteErrs = append(deleteErrs, ddbToError("PurgeTeamRateLimitCounters", err))
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		startKey = out.LastEvaluatedKey
	}
	return errors.Join(deleteErrs...)
}

func readRateLimitWindow(item map[string]ddbtypes.AttributeValue) (int64, bool) {
	if _, ok := item[attrRateLimitWindow].(*ddbtypes.AttributeValueMemberN); !ok {
		return 0, false
	}
	return readNumber(item, attrRateLimitWindow), true
}
