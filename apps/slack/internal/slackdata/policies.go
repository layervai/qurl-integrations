package slackdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

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
// (teamID, channelID) channel_policies row authorizes in that channel —
// the channel-scoped set that gates both `/qurl get` mint (via
// handler_get.go's allowedResourceIDsForGet) and `/qurl list` disclosure
// (via handler_list.go's listChannelScope). The set is the union of two
// orthogonal surfaces on the same row:
//
//   - `allowed_resource_ids` SS — resources exposed to the channel (the
//     Edit modal / channel exposure) or carried over from pre-pivot
//     rows. A `/qurl get` token that resolves to one of these IDs (via
//     its `$<slug>` or a listed URL `$<alias>`) is mintable here.
//   - `alias_bindings` Map<alias_name, resource_id> — the channel-alias
//     surface `/qurl-admin set-alias` / `/qurl-admin unset-alias` mutate.
//     The bound resource_id joins the union, so the resource is mintable
//     via its `$<alias>` (the binding) or its `$<slug>`.
//
// Either surface allows the row to mint. As of #589 this set is also
// the `/qurl list` disclosure scope again (not just the mint gate) —
// see the TODO below and apps/slack/docs/list-disclosure.md. Single-row
// GetItem; no pagination needed.
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
// $r_<id>` (mint-time capability) consumed it. #234 made `/qurl list`
// channel-scoped; #459 reverted that (leaving only `get` on this set);
// #589 re-introduced the channel scope, so today BOTH disclosure and
// capability consume it again (see apps/slack/docs/list-disclosure.md).
// A name like `ChannelScopedResourceIDs` would reflect that shared role.
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
	return allowedResourceIDsFromItem(out.Item), nil
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

// ExposeResourceToChannel grants resourceID visibility-and-mintability in
// (teamID, channelID) by adding it to the channel_policies row's
// `allowed_resource_ids` SS. This is the explicit "extra channel" grant the
// `/qurl list` Edit modal writes when an admin exposes a tunnel beyond the
// channel it was installed in.
//
// Idempotent: `ADD` on a string set is set-union, so re-exposing is a no-op,
// and the UpdateItem materializes the row if it doesn't exist yet (matching
// BindChannelAlias's lazy-create posture). It does NOT touch alias_bindings —
// the install channel's implicit grant rides on the slug alias binding, and
// [AllowedResourceIDsForChannel] unions both surfaces, so a tunnel is
// "available in a channel" iff its id is in that union regardless of which
// surface carries it.
func (s *Store) ExposeResourceToChannel(ctx context.Context, teamID, channelID, resourceID string) error {
	if teamID == "" || channelID == "" || resourceID == "" {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "ExposeResourceToChannel: team_id, channel_id, and resource_id are required",
		}
	}
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
		UpdateExpression: aws.String("ADD " + attrAllowedResourceIDs + " :rids"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":rids": &ddbtypes.AttributeValueMemberSS{Value: []string{resourceID}},
		},
	})
	if err != nil {
		return ddbToError("ExposeResourceToChannel", err)
	}
	return nil
}

// RevokeResourceFromChannel removes resourceID from the (teamID, channelID)
// row's `allowed_resource_ids` SS — the inverse of [ExposeResourceToChannel],
// used when an admin de-selects a channel in the `/qurl list` Edit modal.
//
// Idempotent: `DELETE` on a set member that isn't present (or a missing
// attribute / row) is a no-op, and removing the last member drops the
// attribute (DDB forbids empty sets). It deliberately does NOT remove any
// alias_bindings entry: a channel that still has a `$alias` bound to this
// resource (e.g. the install channel's slug alias) stays in
// [AllowedResourceIDsForChannel]'s union and remains available there. Fully
// revoking such a channel means unbinding its aliases too (the Edit modal's
// aliases field, or `/qurl-admin unset-alias`).
func (s *Store) RevokeResourceFromChannel(ctx context.Context, teamID, channelID, resourceID string) error {
	if teamID == "" || channelID == "" || resourceID == "" {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "RevokeResourceFromChannel: team_id, channel_id, and resource_id are required",
		}
	}
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
		UpdateExpression: aws.String("DELETE " + attrAllowedResourceIDs + " :rids"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":rids": &ddbtypes.AttributeValueMemberSS{Value: []string{resourceID}},
		},
	})
	if err != nil {
		return ddbToError("RevokeResourceFromChannel", err)
	}
	return nil
}

// PurgeResourceFromChannel removes EVERY reference to resourceID from the
// (teamID, channelID) channel_policies row: it DELETEs the id from
// `allowed_resource_ids` AND REMOVEs each `alias_bindings` entry whose value is
// resourceID. It returns the alias names it unbound (the caller logs them); a
// nil/empty return means the row carried no alias pointing at resourceID.
//
// This is the revoke/delete cascade. [Store.RevokeResourceFromChannel] (the
// Edit-modal "de-select a channel" verb) deliberately PRESERVES alias bindings,
// because the resource still exists and may be reached from other channels. But
// when the resource itself is destroyed, a surviving binding has nothing left to
// resolve to — an orphaned `$alias` that [Store.BindChannelAlias] still rejects
// as "already bound" while `/qurl list` shows nothing (the dead id is filtered
// out of [allowedResourceIDsFromItem]'s union once the resource is gone). So a
// resource delete must purge BOTH surfaces; this is the verb that does it.
//
// Read-then-write: DynamoDB can only REMOVE map entries by key, so it GetItems
// the row, computes the alias keys whose value == resourceID, and issues a
// single UpdateItem combining the SS DELETE with those map REMOVEs (the channel
// never observes a half-purged row). A missing row short-circuits before the
// UpdateItem so the purge never materializes an empty row; on a present row a
// missing set member or absent map key each make their clause a harmless no-op,
// so the call is idempotent and safe to run for a channel that turns out not to
// reference the resource (the common case in a team-wide sweep).
//
// Best-effort by construction: the resource is already gone upstream when this
// runs, so a lost race or partial failure only leaves a recoverable orphan
// (clearable via `/qurl-admin unset-alias`), never data loss or a live-resource
// leak (resource IDs are unique and never reused, so a dangling id can't later
// point at a different resource).
//
// The map REMOVE is UNCONDITIONAL — DynamoDB can't express a per-key
// `alias_bindings.#a = :rid` guard across N keys in one expression. Alias NAMES,
// unlike resource IDs, are reusable, so in the sub-second window between this
// read and the write an admin who unbound then rebound the same name to a LIVE
// resource would have that fresh binding clobbered. The purge runs synchronously
// right after the upstream delete, so the window is tiny; the race is accepted
// rather than guarded.
func (s *Store) PurgeResourceFromChannel(ctx context.Context, teamID, channelID, resourceID string) ([]string, error) {
	if teamID == "" || channelID == "" || resourceID == "" {
		return nil, &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "PurgeResourceFromChannel: team_id, channel_id, and resource_id are required",
		}
	}
	// Full-row GetItem (no projection): a projected read of only alias_bindings
	// can't distinguish a missing row from a present row that carries an
	// allowed_resource_ids grant but no aliases — and that SS-only row still needs
	// purging. Matches the other reads on this table (small row).
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
	})
	if err != nil {
		return nil, ddbToError("PurgeResourceFromChannel", err)
	}
	// Row absent → nothing to purge. Returning here also keeps the unconditional
	// UpdateItem below from materializing an empty row for a channel that never
	// referenced the resource.
	if len(out.Item) == 0 {
		return nil, nil
	}

	var aliasKeys []string
	for alias, rid := range readStringMap(out.Item, attrAliasBindings) {
		if rid == resourceID {
			aliasKeys = append(aliasKeys, alias)
		}
	}
	sort.Strings(aliasKeys) // deterministic REMOVE order + return value

	// One UpdateItem clears both surfaces: DELETE drops resourceID from the
	// allowed_resource_ids SS (no-op if absent), and REMOVE deletes each
	// alias_bindings key that pointed at it. The DELETE uses the literal attribute
	// name (matching RevokeResourceFromChannel); only the user-controlled alias
	// keys are name-aliased.
	expr := "DELETE " + attrAllowedResourceIDs + " :rid"
	var names map[string]string
	if len(aliasKeys) > 0 {
		names = map[string]string{exprAliasBindings: attrAliasBindings}
		removes := make([]string, len(aliasKeys))
		for i, alias := range aliasKeys {
			nameRef := fmt.Sprintf("#a%d", i)
			names[nameRef] = alias
			removes[i] = exprAliasBindings + "." + nameRef
		}
		expr += " REMOVE " + strings.Join(removes, ", ")
	}

	in := &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
		UpdateExpression: aws.String(expr),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":rid": &ddbtypes.AttributeValueMemberSS{Value: []string{resourceID}},
		},
	}
	if names != nil {
		in.ExpressionAttributeNames = names
	}
	if _, err := s.Client.UpdateItem(ctx, in); err != nil {
		return nil, ddbToError("PurgeResourceFromChannel", err)
	}
	return aliasKeys, nil
}

// ChannelsForResource returns the channel IDs in teamID whose channel_policies
// row makes resourceID available — i.e. resourceID is in
// [AllowedResourceIDsForChannel] for that channel (the union of
// `allowed_resource_ids` and `alias_bindings.values()`). It backs the
// `/qurl list` Edit modal's "expose to channels" pre-fill so an admin sees
// every channel a tunnel already reaches before adding more — and so the
// submit-side reconcile only revokes channels the admin actually saw.
//
// Issues a Query on the partition key (slack_team_id) and pages over
// LastEvaluatedKey. Unlike the package's other reads this needs the
// dynamodb:Query grant on the channel_policies table; callers treat a failure
// as best-effort (an empty/partial pre-fill never causes data loss because
// the reconcile only acts on the channels it returns). Result is sorted
// ascending for a deterministic modal pre-fill.
func (s *Store) ChannelsForResource(ctx context.Context, teamID, resourceID string) ([]string, error) {
	if teamID == "" || resourceID == "" {
		return nil, &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "ChannelsForResource: team_id and resource_id are required",
		}
	}
	var channels []string
	var startKey map[string]ddbtypes.AttributeValue
	for {
		out, err := s.Client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.ChannelPoliciesName),
			KeyConditionExpression: aws.String(attrSlackTeamID + " = :tid"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":tid": stringAttr(teamID),
			},
			// Project only the membership-test inputs (channelItemAllowsResource
			// reads the SS + the alias-bindings map; the loop reads the SK) so a
			// team-wide page doesn't drag every attribute over the wire. Mirrors
			// LookupChannelAlias's projected read on this table.
			ProjectionExpression: aws.String("#cid, #ari, #ab"),
			ExpressionAttributeNames: map[string]string{
				"#cid": attrSlackChannelID,
				"#ari": attrAllowedResourceIDs,
				"#ab":  attrAliasBindings,
			},
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return nil, ddbToError("ChannelsForResource", err)
		}
		for _, item := range out.Items {
			channelID := readString(item, attrSlackChannelID)
			if channelID != "" && channelItemAllowsResource(item, resourceID) {
				channels = append(channels, channelID)
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		startKey = out.LastEvaluatedKey
	}
	sort.Strings(channels)
	return channels, nil
}

// PurgeTeamChannelPolicies deletes EVERY channel_policies row for teamID — all
// alias_bindings and allowed_resource_ids across every channel the bot was used
// in. It is part of the Slack-lifecycle (app_uninstalled / tokens_revoked) and
// `/qurl uninstall` cascade that forgets a workspace once the Slack install is
// gone and there is nothing left for those per-channel grants to authorize.
//
// Two phases against the partition key (slack_team_id):
//
//   - Query pages over the team's rows (LastEvaluatedKey loop, same shape as
//     [Store.ChannelsForResource]), projecting only the SK so a wide row doesn't
//     drag every attribute over the wire — the DeleteItem only needs the key.
//   - Each row is removed with an unconditional DeleteItem keyed by its
//     (slack_team_id, slack_channel_id). Unconditional ⇒ idempotent: a row that a
//     concurrent unset-alias already cleared makes its delete a DynamoDB no-op.
//
// Best-effort by construction: it attempts to delete every row it observes
// during the Query pass, even after an individual DeleteItem fails, then returns
// the joined delete errors at the end. A row created after the Query completes
// is not seen — acceptable here because once the Slack app is uninstalled the
// bot can no longer be invoked to create new policy rows, so the set is
// effectively frozen before the purge runs. A missed channel_policies row has no
// TTL and can persist unless a later uninstall/revoke delivery triggers another
// purge, so lifecycle callers should treat returned errors as meaningful cleanup
// signals even though the Slack ack has already been sent. Sequential DeleteItem
// (not BatchWriteItem) keeps the [DynamoDBClient] surface unchanged; per-team
// channel-policy rows are bounded by the channels a workspace used the bot in,
// so the round-trips stay modest.
func (s *Store) PurgeTeamChannelPolicies(ctx context.Context, teamID string) error {
	if teamID == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "PurgeTeamChannelPolicies: team_id is required"}
	}
	var startKey map[string]ddbtypes.AttributeValue
	var deleteErrs []error
	for {
		out, err := s.Client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.ChannelPoliciesName),
			KeyConditionExpression: aws.String(attrSlackTeamID + " = :tid"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":tid": stringAttr(teamID),
			},
			// Only the SK is needed to issue the per-row DeleteItem; the rest of
			// each row is irrelevant to a full delete. Mirrors the projected reads
			// elsewhere on this table.
			ProjectionExpression: aws.String("#cid"),
			ExpressionAttributeNames: map[string]string{
				"#cid": attrSlackChannelID,
			},
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return joinSweepErrors(deleteErrs, ddbToError("PurgeTeamChannelPolicies", err))
		}
		for _, item := range out.Items {
			channelID := readString(item, attrSlackChannelID)
			if channelID == "" {
				// A row without a readable SK can't be addressed for delete; record
				// cleanup residue rather than emit a malformed key (it would 400).
				// This should not happen for a well-formed table.
				deleteErrs = append(deleteErrs, &Error{
					StatusCode: http.StatusInternalServerError,
					Title:      "PurgeTeamChannelPolicies: queried row missing slack_channel_id",
				})
				continue
			}
			if _, err := s.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
				TableName: aws.String(s.ChannelPoliciesName),
				Key: map[string]ddbtypes.AttributeValue{
					attrSlackTeamID:    stringAttr(teamID),
					attrSlackChannelID: stringAttr(channelID),
				},
			}); err != nil {
				deleteErrs = append(deleteErrs, ddbToError("PurgeTeamChannelPolicies", err))
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		startKey = out.LastEvaluatedKey
	}
	return errors.Join(deleteErrs...)
}

// allowedResourceIDsFromItem returns the set of resource IDs a channel_policies
// item makes available: the union of its `allowed_resource_ids` SS and its
// `alias_bindings` map values (empty IDs dropped). This is the single
// definition of "a tunnel is available in a channel" — [Store.AllowedResourceIDsForChannel]
// returns it after a point GetItem, and [channelItemAllowsResource] membership-
// tests it inside the [Store.ChannelsForResource] Query loop. Keeping the union
// in one place stops those two surfaces from drifting if a third grant source
// is ever added.
func allowedResourceIDsFromItem(item map[string]ddbtypes.AttributeValue) map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, rid := range readStringSet(item, attrAllowedResourceIDs) {
		if rid != "" {
			allowed[rid] = struct{}{}
		}
	}
	for _, rid := range readStringMap(item, attrAliasBindings) {
		if rid != "" {
			allowed[rid] = struct{}{}
		}
	}
	return allowed
}

// channelItemAllowsResource reports whether a channel_policies item makes
// resourceID available, per the shared [allowedResourceIDsFromItem] union.
func channelItemAllowsResource(item map[string]ddbtypes.AttributeValue, resourceID string) bool {
	_, ok := allowedResourceIDsFromItem(item)[resourceID]
	return ok
}
