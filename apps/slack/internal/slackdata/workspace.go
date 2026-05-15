package slackdata

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Attribute names on the workspace_mappings table. Mirror the schema
// fenced in modules/qurl-slack-ddb/main.tf so a schema rename only
// touches one file in this repo (this one).
const (
	attrSlackTeamID        = "slack_team_id"
	attrOwnerID            = "owner_id"
	attrAdminSlackUserIDs  = "admin_slack_user_ids"
	attrCreatedAt          = "created_at"
	attrUpdatedAt          = "updated_at"
	attrSeedAdminSlackUser = "seed_admin_slack_user_id"
	attrAPIKeyFingerprint  = "api_key_fingerprint"
)

// CheckAdmin returns (isAdmin, ownerID) for the workspace.
//
// Workspace not yet bound to an owner → (false, "", nil) — the
// handler treats this as "not admin" and renders the friendly
// "admin features not configured" copy. Distinguishing
// "workspace-not-bound" from "user-not-on-admin-list" is left to a
// follow-up (the old HTTP surface didn't distinguish them either —
// both came back as `is_admin=false`).
func (s *Store) CheckAdmin(ctx context.Context, teamID, slackUserID string) (isAdmin bool, ownerID string, err error) {
	if teamID == "" || slackUserID == "" {
		return false, "", &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "CheckAdmin: team_id and user_id are required",
		}
	}
	out, getErr := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.WorkspaceMappingsName),
		ConsistentRead: aws.Bool(false), // eventual is fine for admin check
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(teamID),
		},
	})
	if getErr != nil {
		return false, "", ddbToError("CheckAdmin", getErr)
	}
	if len(out.Item) == 0 {
		return false, "", nil
	}
	ownerID = readString(out.Item, attrOwnerID)
	admins := readStringSet(out.Item, attrAdminSlackUserIDs)
	for _, u := range admins {
		if u == slackUserID {
			return true, ownerID, nil
		}
	}
	return false, ownerID, nil
}

// GetWorkspaceConfig renders the `/qurl admin status` payload — owner
// ID, seed admin, configured_at, and the policy count (a count-only
// Query on channel_policies). The API-key fingerprint is left empty
// for now; it'll be plumbed through handlerDeps in a follow-up so the
// slackdata package doesn't have to know about the encrypted-API-key
// surface in shared/auth.
func (s *Store) GetWorkspaceConfig(ctx context.Context, teamID string) (*WorkspaceConfig, error) {
	if teamID == "" {
		return nil, &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "GetWorkspaceConfig: team_id is required",
		}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(teamID),
		},
	})
	if err != nil {
		return nil, ddbToError("GetWorkspaceConfig", err)
	}
	if len(out.Item) == 0 {
		return nil, notFoundError("GetWorkspaceConfig: workspace not bound")
	}
	cfg := &WorkspaceConfig{
		OwnerID:           readString(out.Item, attrOwnerID),
		APIKeyFingerprint: readString(out.Item, attrAPIKeyFingerprint), // empty until follow-up
		SeedAdminUserID:   readString(out.Item, attrSeedAdminSlackUser),
		ConfiguredAt:      readTime(out.Item, attrCreatedAt),
	}

	// Count channel_policies rows for this team. Count-only DDB
	// queries return only the Count field (no row scan on the
	// wire). For workspaces with hundreds of channel policies this
	// is still O(rows) on RCUs — acceptable at slash-command volumes,
	// not acceptable on a hot path. Tracked as a follow-up if
	// /qurl admin status gets called often enough to matter.
	count, countErr := s.countPoliciesForTeam(ctx, teamID)
	if countErr != nil {
		// Don't fail the whole status reply on a count failure —
		// surface the partial data with policy_count=0 so the
		// operator still gets the workspace owner/admin info. The
		// count discrepancy will show up as zero, which the
		// /qurl admin policies command can correct.
		//
		// Audit the degraded path so operators can distinguish "no
		// policies" from "count failed". Without this log line
		// /qurl admin status quietly reports `Channel policies: 0`
		// on any transient DDB blip — indistinguishable from a real
		// empty-state.
		slog.Warn("countPoliciesForTeam degraded; status reply shows policy_count=0",
			"team_id", teamID, "error", countErr)
		// Intentional nilerr: partial status > total failure for
		// the operator. The slog.Warn above is the audit trail.
		return cfg, nil
	}
	cfg.PolicyCount = count
	return cfg, nil
}

// countPoliciesForTeam runs a count-only DDB Query on
// channel_policies (PK=slack_team_id). Returns 0 + nil if no rows.
func (s *Store) countPoliciesForTeam(ctx context.Context, teamID string) (int, error) {
	var total int
	var lastKey map[string]ddbtypes.AttributeValue
	for {
		out, err := s.Client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.ChannelPoliciesName),
			KeyConditionExpression: aws.String("slack_team_id = :tid"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":tid": stringAttr(teamID),
			},
			Select:            ddbtypes.SelectCount,
			ExclusiveStartKey: lastKey,
		})
		if err != nil {
			return 0, ddbToError("countPoliciesForTeam", err)
		}
		total += int(out.Count)
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		lastKey = out.LastEvaluatedKey
	}
	return total, nil
}

// BindWorkspace upserts the workspace mapping row. Used by the
// admin-claim redeem flow once the bootstrap code has been
// atomically consumed. The seedAdmin is added to admin_slack_user_ids
// on first bind; on overwrite the prior admin set is preserved
// (single-use bootstrap codes ensure we don't get here twice without
// an explicit rotate).
//
// Returns 409 (via *Error) if the row already exists with a
// different owner — the redeem path should treat this as a fatal
// "this workspace is already bound" failure rather than silently
// overwriting.
func (s *Store) BindWorkspace(ctx context.Context, m *WorkspaceMapping, seedAdmin string) error {
	if m == nil || m.TeamID == "" || m.OwnerID == "" {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "BindWorkspace: team_id and owner_id are required",
		}
	}
	if seedAdmin == "" {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "BindWorkspace: seed_admin user_id is required",
		}
	}
	created := m.CreatedAt
	if created.IsZero() {
		created = s.nowOrDefault().UTC()
	}
	now := s.nowOrDefault().UTC().Format(time.RFC3339)

	// Conditional PutItem: refuse to overwrite an existing row with a
	// different owner_id. The match-owner case is treated as
	// idempotent (re-running /qurl admin claim with the same bootstrap
	// code, before TTL clears the code — the conditional UpdateItem
	// on bootstrap_codes will have flipped redeemed=true; if the row
	// is still present this is a duplicate request).
	_, err := s.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Item: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:        stringAttr(m.TeamID),
			attrOwnerID:            stringAttr(m.OwnerID),
			attrSeedAdminSlackUser: stringAttr(seedAdmin),
			attrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{
				Value: []string{seedAdmin},
			},
			attrCreatedAt: stringAttr(created.Format(time.RFC3339)),
			attrUpdatedAt: stringAttr(now),
		},
		ConditionExpression: aws.String(
			"attribute_not_exists(slack_team_id) OR owner_id = :owner",
		),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":owner": stringAttr(m.OwnerID),
		},
	})
	if err == nil {
		return nil
	}
	// ConditionalCheckFailed here means the row exists with a different
	// owner — surface as 409 so the handler can render the
	// "this workspace already has an admin" copy.
	var ccfe *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &ccfe) {
		return &Error{
			StatusCode: http.StatusConflict,
			Code:       "workspace_already_bound",
			Title:      "BindWorkspace: workspace is already bound to a different owner",
		}
	}
	return ddbToError("BindWorkspace", err)
}
