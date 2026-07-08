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
	ok, item, updateErr := s.incrementCurrentRateLimitWindow(ctx, counterKey, windowUnix, limit)
	if updateErr != nil || ok {
		return ok, 0, updateErr
	}
	storedWindow, hasWindow := readRateLimitWindow(item)
	if hasWindow && storedWindow == windowUnix {
		return false, retry, nil
	}

	var resetOK bool
	if hasWindow {
		resetOK, updateErr = s.resetRateLimitWindow(ctx, counterKey, windowUnix, storedWindow)
	} else {
		resetOK, updateErr = s.initializeRateLimitWindow(ctx, counterKey, windowUnix)
	}
	if updateErr != nil || resetOK {
		return resetOK, 0, updateErr
	}

	// Another worker initialized/reset the window between our condition failure
	// and our repair write. Retry the normal current-window increment once.
	ok, item, updateErr = s.incrementCurrentRateLimitWindow(ctx, counterKey, windowUnix, limit)
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
	return rateLimitKeyPrefix + teamID + "#" + scope
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

func (s *Store) incrementCurrentRateLimitWindow(ctx context.Context, counterKey string, windowUnix int64, limit int) (allowed bool, item map[string]ddbtypes.AttributeValue, err error) {
	_, err = s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(counterKey),
		},
		UpdateExpression:    aws.String("ADD #count :one"),
		ConditionExpression: aws.String("#window = :window AND #count < :limit"),
		ExpressionAttributeNames: map[string]string{
			"#count":  attrRateLimitCount,
			"#window": attrRateLimitWindow,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":one":    numberAttr(1),
			":window": numberAttr(windowUnix),
			":limit":  numberAttr(int64(limit)),
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

func (s *Store) initializeRateLimitWindow(ctx context.Context, counterKey string, windowUnix int64) (bool, error) {
	return s.setRateLimitWindow(ctx, counterKey, windowUnix, "attribute_not_exists(#window)", nil)
}

func (s *Store) resetRateLimitWindow(ctx context.Context, counterKey string, windowUnix, oldWindow int64) (bool, error) {
	return s.setRateLimitWindow(ctx, counterKey, windowUnix, "#window = :old_window", map[string]ddbtypes.AttributeValue{
		":old_window": numberAttr(oldWindow),
	})
}

func (s *Store) setRateLimitWindow(ctx context.Context, counterKey string, windowUnix int64, condition string, extra map[string]ddbtypes.AttributeValue) (bool, error) {
	values := map[string]ddbtypes.AttributeValue{
		":expires_at": numberAttr(time.Unix(windowUnix, 0).Add(2 * s.rateLimitWindow()).Unix()),
		":one":        numberAttr(1),
		":window":     numberAttr(windowUnix),
	}
	for k, v := range extra {
		values[k] = v
	}
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(counterKey),
		},
		UpdateExpression:    aws.String("SET #window = :window, #count = :one, #expires_at = :expires_at"),
		ConditionExpression: aws.String(condition),
		ExpressionAttributeNames: map[string]string{
			"#count":      attrRateLimitCount,
			"#expires_at": attrRateLimitExpiresAt,
			"#window":     attrRateLimitWindow,
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

func readRateLimitWindow(item map[string]ddbtypes.AttributeValue) (int64, bool) {
	if _, ok := item[attrRateLimitWindow].(*ddbtypes.AttributeValueMemberN); !ok {
		return 0, false
	}
	return readNumber(item, attrRateLimitWindow), true
}
