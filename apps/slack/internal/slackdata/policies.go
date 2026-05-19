package slackdata

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
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
// allowed_resource_ids set via a conditional UpdateItem. Orthogonal
// to alias_bindings — AllowResource only touches the allowed-set
// surface that ResolvePolicy gates on.
//
// Returns 409 (via *Error) on a duplicate add. The 409 is surfaced
// via DDB's ConditionalCheckFailedException on a
// `NOT contains(allowed_resource_ids, :rid)` condition. Folding the
// membership check into the conditional UpdateItem dodges the
// TOCTOU window the prior probe-then-write shape had — two
// concurrent admins running `/qurl admin allow` on the same
// channel/resource would now see the second one get the 409 signal
// instead of a false "Allowed" success.
//
// The condition fires only when the row exists AND the set already
// contains the resource. A missing row passes the condition (the
// attribute doesn't exist), and ADD creates the set on first write.
func (s *Store) AllowResource(ctx context.Context, teamID, channelID, resourceID string) error {
	if teamID == "" || channelID == "" || resourceID == "" {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "AllowResource: team_id, channel_id, resource_id are required",
		}
	}
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
		UpdateExpression: aws.String("ADD allowed_resource_ids :rids SET updated_at = :now"),
		// "attribute_not_exists(allowed_resource_ids) OR NOT
		// contains(...)" passes when the row is brand new (no set
		// yet) AND when the existing set lacks the target. Either
		// case is a legitimate add. A repeat add fails the
		// condition → ConditionalCheckFailedException → 409.
		ConditionExpression: aws.String("attribute_not_exists(allowed_resource_ids) OR NOT contains(allowed_resource_ids, :rid)"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":rids": &ddbtypes.AttributeValueMemberSS{Value: []string{resourceID}},
			":rid":  stringAttr(resourceID),
			exprNow: stringAttr(s.nowOrDefault().UTC().Format(timeFormat)),
		},
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return &Error{
				StatusCode: http.StatusConflict,
				Code:       "policy_already_exists",
				Title:      "AllowResource: policy already exists",
			}
		}
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
			// Cursor encoding shouldn't fail on a valid DDB key (PK+SK
			// are both String today), but don't blow up the listing —
			// surface HasMore without a cursor. Log loud so operators
			// can see the broken pagination instead of just observing
			// "next page is unreachable" via user reports.
			slog.Warn("ListPolicies: encodeCursor failed; pagination disabled for this page",
				"error", encErr, "team_id", teamID)
			return list, nil
		}
		list.NextCursor = cur
	}
	return list, nil
}

// LookupChannelAlias returns the resource id bound to aliasName on
// (teamID, channelID), or found=false when no binding exists. Issued
// as a targeted GetItem with a ProjectionExpression on the single
// `alias_bindings.#a` map key so the row's other attributes (the
// orthogonal `allowed_resource_ids` set, future audit columns) don't
// land in the read.
//
// Missing row, missing alias_bindings map, and missing map key all
// collapse to (resourceID="", found=false, err=nil) — the caller
// renders the same "`$X` is not configured for this channel. Run
// `/qurl aliases` to see what's available here, or contact your
// Slack admin to add it." copy for all three.
func (s *Store) LookupChannelAlias(ctx context.Context, teamID, channelID, aliasName string) (resourceID string, found bool, err error) {
	if teamID == "" || channelID == "" || aliasName == "" {
		return "", false, &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "LookupChannelAlias: team_id, channel_id, alias_name are required",
		}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
		ProjectionExpression: aws.String("#ab.#a"),
		ExpressionAttributeNames: map[string]string{
			"#ab": attrAliasBindings,
			"#a":  aliasName,
		},
	})
	if err != nil {
		return "", false, ddbToError("LookupChannelAlias", err)
	}
	if len(out.Item) == 0 {
		return "", false, nil
	}
	bindings := readStringMap(out.Item, attrAliasBindings)
	rid, ok := bindings[aliasName]
	if !ok {
		return "", false, nil
	}
	return rid, true, nil
}

// GetChannelPolicy returns every alias binding for a single
// (teamID, channelID) row via a single GetItem against the PK+SK.
// Replaces the previous "page team-wide then filter" shape that
// could miss the calling channel when its row sorted past page-
// boundary (the team-wide ListPolicies + post-filter pattern hid
// channel hits past the first `limit` rows).
//
// Returns an empty entries slice when no row exists or when the row
// has no alias_bindings — both render the same "no aliases" empty
// state at the handler. The caller does NOT need to distinguish
// row-absent from row-without-bindings (the user-visible signal is
// the same).
func (s *Store) GetChannelPolicy(ctx context.Context, teamID, channelID string) ([]PolicyEntry, error) {
	if teamID == "" || channelID == "" {
		return nil, &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "GetChannelPolicy: team_id and channel_id are required",
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
		return nil, ddbToError("GetChannelPolicy", err)
	}
	if len(out.Item) == 0 {
		return nil, nil
	}
	createdAt := readTime(out.Item, attrCreatedAt)
	bindings := readStringMap(out.Item, attrAliasBindings)
	entries := make([]PolicyEntry, 0, len(bindings))
	for alias, rid := range bindings {
		entries = append(entries, PolicyEntry{
			ChannelID:  channelID,
			Alias:      alias,
			ResourceID: rid,
			CreatedAt:  createdAt,
		})
	}
	return entries, nil
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
