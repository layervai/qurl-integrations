package slackdata

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Attribute names on the channel_policies table. PK=slack_team_id,
// SK=slack_channel_id. The row carries two orthogonal surfaces:
//
//   - `alias_bindings`: app-managed DDB Map<alias_name, resource_id>.
//     A channel can carry many alias bindings simultaneously;
//     `/qurl aliases` lists every binding for the channel.
//   - `allowed_resource_ids`: SS attribute; the multi-resource gate
//     `/qurl get` checks via ResolvePolicy. Orthogonal to the alias
//     map — a resource can be in the allowed set without an alias,
//     and an alias can be bound without being in the allowed set
//     (the two surfaces serve different commands).
//
// Schema decision locked 2026-05-17: app-managed Map, no GSI, no
// SK reshape. channel_policies table is empty pending Slack bot
// sandbox deploy so no data migration is required.
const (
	attrSlackChannelID     = "slack_channel_id"
	attrAliasBindings      = "alias_bindings"
	attrAllowedResourceIDs = "allowed_resource_ids"
)

// ResolvePolicy returns true iff `resourceID` is in the
// channel_policies row's `allowed_resource_ids` set for
// (teamID, channelID). Missing row → false (no policy = no access).
//
// The old HTTP shape returned a `bool` and an error; same shape here.
// Eventual-consistency read is intentional — `/qurl get` and `/qurl
// aliases` tolerate ~few-second propagation lag after a fresh
// allow/disallow; strong reads would double RCU cost without
// changing failure modes that matter.
func (s *Store) ResolvePolicy(ctx context.Context, teamID, channelID, resourceID string) (bool, error) {
	if teamID == "" || channelID == "" || resourceID == "" {
		return false, &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "ResolvePolicy: team_id, channel_id, resource_id are required",
		}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
	})
	if err != nil {
		return false, ddbToError("ResolvePolicy", err)
	}
	if len(out.Item) == 0 {
		return false, nil
	}
	for _, rid := range readStringSet(out.Item, attrAllowedResourceIDs) {
		if rid == resourceID {
			return true, nil
		}
	}
	return false, nil
}

// AllowResource adds `resourceID` to the (teamID, channelID) row's
// allowed_resource_ids set. Uses `UpdateItem ADD` so DDB handles
// set-create-or-add atomically. Orthogonal to alias_bindings —
// AllowResource only touches the allowed-set surface that
// ResolvePolicy gates on.
//
// Returns 409 (via *Error) on a duplicate add — this is "operator's
// mental model already satisfied," and the handler maps it to the
// "already allowed" copy.
func (s *Store) AllowResource(ctx context.Context, teamID, channelID, resourceID string) error {
	if teamID == "" || channelID == "" || resourceID == "" {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "AllowResource: team_id, channel_id, resource_id are required",
		}
	}
	// First: probe the row to surface the idempotent "already
	// allowed" path as 409 rather than silently no-op'ing. ADD on a
	// set is idempotent in DDB, but the handler relies on the 409
	// status to render the right user copy ("already allowed in
	// <#channel>"). Without this probe we'd silently no-op and
	// render the "Allowed" success copy on a duplicate add — wrong
	// signal to the operator.
	get, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
	})
	if err != nil {
		return ddbToError("AllowResource(probe)", err)
	}
	if len(get.Item) > 0 {
		for _, rid := range readStringSet(get.Item, attrAllowedResourceIDs) {
			if rid == resourceID {
				return &Error{
					StatusCode: http.StatusConflict,
					Code:       "policy_already_exists",
					Title:      "AllowResource: policy already exists",
				}
			}
		}
	}
	_, err = s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
		UpdateExpression: aws.String("ADD allowed_resource_ids :rids SET updated_at = :now"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":rids": &ddbtypes.AttributeValueMemberSS{Value: []string{resourceID}},
			exprNow: stringAttr(s.nowOrDefault().UTC().Format(timeFormat)),
		},
	})
	if err != nil {
		return ddbToError("AllowResource", err)
	}
	return nil
}

// DisallowResource removes `resourceID` from the (teamID, channelID)
// row's allowed_resource_ids set. Returns 404 (via *Error) if no
// matching row/member exists — the handler maps 404 to the
// idempotent "nothing to remove" copy.
func (s *Store) DisallowResource(ctx context.Context, teamID, channelID, resourceID string) error {
	if teamID == "" || channelID == "" || resourceID == "" {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "DisallowResource: team_id, channel_id, resource_id are required",
		}
	}
	// Probe the existing membership so the no-op path surfaces as
	// 404 (handler maps to "nothing to remove"). DDB's DELETE on a
	// set is silent on absent members — without the probe we'd
	// render the success copy on a no-op.
	get, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
	})
	if err != nil {
		return ddbToError("DisallowResource(probe)", err)
	}
	if len(get.Item) == 0 {
		return notFoundError("DisallowResource: no policy for channel")
	}
	found := false
	for _, rid := range readStringSet(get.Item, attrAllowedResourceIDs) {
		if rid == resourceID {
			found = true
			break
		}
	}
	if !found {
		return notFoundError("DisallowResource: resource_id not in allowed set")
	}
	_, err = s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
		UpdateExpression: aws.String("DELETE allowed_resource_ids :rids SET updated_at = :now"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":rids": &ddbtypes.AttributeValueMemberSS{Value: []string{resourceID}},
			exprNow: stringAttr(s.nowOrDefault().UTC().Format(timeFormat)),
		},
	})
	if err != nil {
		return ddbToError("DisallowResource", err)
	}
	return nil
}

// ListPolicies pages channel_policies rows for teamID. Cursor is a
// base64-encoded JSON of the DDB LastEvaluatedKey; opaque to the
// caller. Limit caps the page size (DDB enforces its own max).
//
// Each row carries an `alias_bindings` Map<alias_name, resource_id>;
// ListPolicies flattens the map into one PolicyEntry per binding so
// `/qurl aliases` can render one line per (channel, alias). Rows
// without alias_bindings (only `allowed_resource_ids` populated)
// emit zero entries — they're orthogonal to the alias listing.
// `allowed_resource_ids` is the gate for `/qurl get` via
// ResolvePolicy and intentionally NOT mirrored here.
func (s *Store) ListPolicies(ctx context.Context, teamID, cursor string, limit int) (*PolicyList, error) {
	if teamID == "" {
		return nil, &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "ListPolicies: team_id is required",
		}
	}
	if limit <= 0 {
		limit = 50
	}
	in := &dynamodb.QueryInput{
		TableName:              aws.String(s.ChannelPoliciesName),
		KeyConditionExpression: aws.String("slack_team_id = :tid"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":tid": stringAttr(teamID),
		},
		Limit: aws.Int32(int32(limit)),
	}
	if cursor != "" {
		startKey, decErr := decodeCursor(cursor)
		if decErr != nil {
			return nil, &Error{
				StatusCode: http.StatusBadRequest,
				Code:       "invalid_cursor",
				Title:      "ListPolicies: cursor is malformed",
				Detail:     decErr.Error(),
			}
		}
		in.ExclusiveStartKey = startKey
	}

	out, err := s.Client.Query(ctx, in)
	if err != nil {
		return nil, ddbToError("ListPolicies", err)
	}

	list := &PolicyList{
		Entries: make([]PolicyEntry, 0, len(out.Items)),
	}
	for _, item := range out.Items {
		channelID := readString(item, attrSlackChannelID)
		createdAt := readTime(item, attrCreatedAt)
		// One PolicyEntry per alias_bindings binding. Rows without
		// an alias_bindings Map (or with an empty one) contribute
		// zero entries — `/qurl aliases` renders the empty-state
		// hint when the post-filter slice is empty.
		for alias, rid := range readStringMap(item, attrAliasBindings) {
			list.Entries = append(list.Entries, PolicyEntry{
				ChannelID:  channelID,
				Alias:      alias,
				ResourceID: rid,
				CreatedAt:  createdAt,
			})
		}
	}
	if len(out.LastEvaluatedKey) > 0 {
		list.HasMore = true
		cur, encErr := encodeCursor(out.LastEvaluatedKey)
		if encErr != nil {
			// Cursor encoding shouldn't fail on a valid DDB key, but
			// don't blow up the listing — surface HasMore without a
			// cursor so the handler shows the "more pages exist"
			// hint without a broken next-page token.
			return list, nil //nolint:nilerr // intentional degrade: surface partial list with HasMore but no cursor
		}
		list.NextCursor = cur
	}
	return list, nil
}

// timeFormat is the on-the-wire format for created_at/updated_at on
// the DDB rows. Mirrors shared/auth.ddb_provider's RFC3339 usage so
// a cross-table audit query renders timestamps uniformly.
const timeFormat = "2006-01-02T15:04:05Z07:00"

// encodeCursor serializes a DDB LastEvaluatedKey to an opaque
// base64-of-JSON token. The only string fields in the channel_policies
// PK/SK are slack_team_id + slack_channel_id; both are strings, so
// the JSON shape is small and stable.
func encodeCursor(key map[string]ddbtypes.AttributeValue) (string, error) {
	flat := make(map[string]string, len(key))
	for k, v := range key {
		s, ok := v.(*ddbtypes.AttributeValueMemberS)
		if !ok {
			// Non-string attr in the key — refuse to encode rather
			// than producing an inconsistent token. channel_policies'
			// PK + SK are both S today; future schema changes would
			// need to update this serializer.
			return "", errors.New("encodeCursor: non-string attribute in PK/SK")
		}
		flat[k] = s.Value
	}
	body, err := json.Marshal(flat)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(body), nil
}

// decodeCursor inverts encodeCursor. Bad/malformed cursors return an
// error so the handler can surface "cursor is malformed" rather than
// silently restarting at the top of the listing.
func decodeCursor(token string) (map[string]ddbtypes.AttributeValue, error) {
	body, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	flat := make(map[string]string)
	if err := json.Unmarshal(body, &flat); err != nil {
		return nil, err
	}
	out := make(map[string]ddbtypes.AttributeValue, len(flat))
	for k, v := range flat {
		out[k] = stringAttr(v)
	}
	return out, nil
}
