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

// attrResourceDefaultTTLs is the workspace_mappings Map<resource_id,
// duration-string> holding each resource's admin-set default link expiry
// (the `expires_in` `/qurl get` mints with). Lives on the workspace row —
// not channel_policies — because the setting is per-resource, not
// per-channel, and qurl-service has no resource field for it (the bot is
// the only consumer, so the bot's own table is the seam; same posture as
// alias_bindings). Absence of an entry means the bot's built-in default.
const attrResourceDefaultTTLs = "resource_default_ttls"

// Expression placeholders for the resource_default_ttls map mutations.
// The resource_id key MUST go through ExpressionAttributeNames — it's
// caller-supplied data, not a known-safe identifier.
const (
	exprResourceTTLs   = "#rttl"
	exprTTLResourceKey = "#rid"
	exprTTLValue       = ":ttl"
)

// GetResourceDefaultTTL returns the stored default link expiry for
// (teamID, resourceID), or "" when no override is set (missing row, missing
// map, or missing entry — callers fall back to their built-in default).
func (s *Store) GetResourceDefaultTTL(ctx context.Context, teamID, resourceID string) (string, error) {
	if teamID == "" || resourceID == "" {
		return "", &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "GetResourceDefaultTTL: team_id and resource_id are required",
		}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.WorkspaceMappingsName),
		ConsistentRead: aws.Bool(false), // eventual is fine: a just-written TTL showing up one mint late is benign
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(teamID),
		},
	})
	if err != nil {
		return "", ddbToError("GetResourceDefaultTTL", err)
	}
	return readStringMap(out.Item, attrResourceDefaultTTLs)[resourceID], nil
}

// SetResourceDefaultTTL writes (ttl != "") or clears (ttl == "") the
// (teamID, resourceID) default link expiry on the workspace_mappings row.
// The ttl value is stored verbatim; validating it against the allowed
// option set is the caller's job (the Edit modal only submits listed
// values).
//
// Writes require the workspace row to exist (404 workspace_not_bound
// otherwise) — an UpdateItem upsert would otherwise materialize a phantom
// unbound row keyed by teamID, breaking BindWorkspace's
// attribute_not_exists first-claim condition. Clears are idempotent: a
// CCFE (entry already absent) returns nil because the desired end state
// holds.
func (s *Store) SetResourceDefaultTTL(ctx context.Context, teamID, resourceID, ttl string) error {
	if teamID == "" || resourceID == "" {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "SetResourceDefaultTTL: team_id and resource_id are required",
		}
	}
	if ttl == "" {
		return s.clearResourceDefaultTTL(ctx, teamID, resourceID)
	}
	if err := s.ensureResourceTTLMap(ctx, teamID); err != nil {
		return err
	}
	nowISO := s.nowOrDefault().UTC().Format(time.RFC3339)
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(teamID),
		},
		UpdateExpression:    aws.String("SET " + exprResourceTTLs + "." + exprTTLResourceKey + " = " + exprTTLValue + ", updated_at = " + exprNow),
		ConditionExpression: aws.String("attribute_exists(slack_team_id)"),
		ExpressionAttributeNames: map[string]string{
			exprResourceTTLs:   attrResourceDefaultTTLs,
			exprTTLResourceKey: resourceID,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			exprTTLValue: stringAttr(ttl),
			exprNow:      stringAttr(nowISO),
		},
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return &Error{
				StatusCode: http.StatusNotFound,
				Code:       ErrCodeWorkspaceNotBound,
				Title:      "SetResourceDefaultTTL: workspace is not bound",
			}
		}
		return ddbToError("SetResourceDefaultTTL", err)
	}
	return nil
}

// clearResourceDefaultTTL removes the (teamID, resourceID) entry. The
// attribute_exists condition guards the nested REMOVE path (DDB rejects a
// REMOVE through a missing parent map); a CCFE means the entry — or the
// whole map, or the row — is already absent, which IS the desired end
// state, so it returns nil rather than a not-found error.
func (s *Store) clearResourceDefaultTTL(ctx context.Context, teamID, resourceID string) error {
	nowISO := s.nowOrDefault().UTC().Format(time.RFC3339)
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(teamID),
		},
		UpdateExpression:    aws.String("REMOVE " + exprResourceTTLs + "." + exprTTLResourceKey + " SET updated_at = " + exprNow),
		ConditionExpression: aws.String("attribute_exists(" + exprResourceTTLs + "." + exprTTLResourceKey + ")"),
		ExpressionAttributeNames: map[string]string{
			exprResourceTTLs:   attrResourceDefaultTTLs,
			exprTTLResourceKey: resourceID,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			exprNow: stringAttr(nowISO),
		},
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return nil
		}
		return ddbToError("SetResourceDefaultTTL.clear", err)
	}
	return nil
}

// ensureResourceTTLMap lazily seeds resource_default_ttls as an empty map
// (mirrors ensureAliasBindingsMap) so the nested SET path in
// SetResourceDefaultTTL has a parent to write into. Unlike the
// channel_policies variant it conditions on the row existing — see
// SetResourceDefaultTTL's phantom-row rationale. A CCFE is swallowed
// because it means either the map already exists (proceed) or the row is
// missing (the follow-up SET's own condition surfaces workspace_not_bound).
func (s *Store) ensureResourceTTLMap(ctx context.Context, teamID string) error {
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(teamID),
		},
		UpdateExpression:    aws.String("SET " + exprResourceTTLs + " = " + exprEmptyMap),
		ConditionExpression: aws.String("attribute_exists(slack_team_id) AND attribute_not_exists(" + exprResourceTTLs + ")"),
		ExpressionAttributeNames: map[string]string{
			exprResourceTTLs: attrResourceDefaultTTLs,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			exprEmptyMap: &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{}},
		},
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return nil
		}
		return ddbToError("ensureResourceTTLMap", err)
	}
	return nil
}
