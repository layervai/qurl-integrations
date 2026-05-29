package slackdata

import (
	"context"
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
	// attrResourceID is the legacy per-row scalar shape that pre-pivot
	// rows carry. Net-new mutations land in the `allowed_resource_ids`
	// SS or `alias_bindings` Map, but hand-seeded rows or any row that
	// escapes the bot's mutation path may still carry the scalar.
	// ResolvePolicy falls back to the scalar so a legacy single-row
	// grant continues to resolve at `/qurl get`. Pinned by
	// TestResolvePolicy_LegacySingleRowShape.
	attrResourceID = "resource_id"
)

// AllowedResourceIDsForChannel returns the union of resource IDs the
// (teamID, channelID) channel_policies row authorizes for non-admin
// mint via the `$r_<id>` get path (handler_get.go's
// resourceAllowedForUser). The set is the union of two orthogonal
// surfaces on the same row:
//
//   - `allowed_resource_ids` SS — the legacy multi-resource gate
//     hand-seeded or carried over from pre-pivot rows. `/qurl get
//     $r_<id>` checks membership here.
//   - `alias_bindings` Map<alias_name, resource_id> — the alias
//     surface `/qurl setalias` / `/qurl unsetalias` mutate; the
//     binding's resource_id is also accepted on the `$r_<id>` path so
//     an aliased resource is mintable by its raw ID too.
//
// Either surface allows the row to mint. The `/qurl list` consumer of
// this set was removed in #459 (revert of #234): `/qurl list` is now
// workspace-wide and unfiltered, so this function survives only as the
// mint-time channel gate. Single-row GetItem; no pagination needed.
//
// Known asymmetry vs [ResolvePolicy]: this function does NOT read the
// legacy scalar `resource_id` attribute. ResolvePolicy falls back to
// the scalar so a hand-seeded pre-pivot row still resolves at `get`;
// the same row will not be mintable via this set. The asymmetry is
// intentional for now — the mint gate is being phased toward the
// post-pivot Map/SS shapes, and unioning the scalar here would
// re-expose pre-pivot rows that policies-migration is meant to drain.
// Revisit when the migration completes.
//
// Missing row → empty set (no policy = no access, fail-closed).
//
// TODO(#464): rename when next touched. The name dates from an era
// where both `/qurl list` (non-admin disclosure) and `/qurl get
// $r_<id>` (mint-time capability) consumed it; post-revert of #234 in
// #459 only the latter survives, so a name like
// `ChannelMintableResourceIDs` would better reflect today's role.
func (s *Store) AllowedResourceIDsForChannel(ctx context.Context, teamID, channelID string) (map[string]struct{}, error) {
	if teamID == "" || channelID == "" {
		return nil, &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "AllowedResourceIDsForChannel: team_id and channel_id are required",
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
		return nil, ddbToError("AllowedResourceIDsForChannel", err)
	}
	if len(out.Item) == 0 {
		return map[string]struct{}{}, nil
	}
	allowed := make(map[string]struct{})
	for _, rid := range readStringSet(out.Item, attrAllowedResourceIDs) {
		if rid != "" {
			allowed[rid] = struct{}{}
		}
	}
	for _, rid := range readStringMap(out.Item, attrAliasBindings) {
		if rid != "" {
			allowed[rid] = struct{}{}
		}
	}
	return allowed, nil
}

// ResolvePolicy returns true iff `resourceID` is in the
// channel_policies row's `allowed_resource_ids` set for
// (teamID, channelID). Missing row → false (no policy = no access).
//
// The old HTTP shape returned a `bool` and an error; same shape here.
// Eventual-consistency read is intentional — `/qurl get` and `/qurl
// aliases` tolerate ~few-second propagation lag after a fresh
// setalias mutation; strong reads would double RCU cost without
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
	// Legacy single-row shape: per-row `resource_id` scalar. Hand-seeded
	// rows that escaped the bot's mutation path may carry the grant
	// only in the scalar. Pinned by TestResolvePolicy_LegacySingleRowShape.
	if rid := readString(out.Item, attrResourceID); rid == resourceID {
		return true, nil
	}
	// Multi-resource shape: SS membership.
	for _, rid := range readStringSet(out.Item, attrAllowedResourceIDs) {
		if rid == resourceID {
			return true, nil
		}
	}
	return false, nil
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
