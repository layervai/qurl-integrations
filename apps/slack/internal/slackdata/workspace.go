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

// Error codes surfaced on [*Error.Code] by [Store.BindWorkspace]'s
// 409 paths. Exported so handlers pattern-match the field instead of
// redeclaring the literal — a future rename breaks the type-checker
// rather than silently desynchronizing the producer and consumer.
const (
	ErrCodeWorkspaceAlreadyBoundToCaller = "workspace_already_bound_to_caller"
	ErrCodeWorkspaceAlreadyBound         = "workspace_already_bound"
	// ErrCodeWorkspaceBindUnverified is the 409 variant surfaced when
	// the post-CCFE disambiguation GetItem itself fails (timeout,
	// transport blip). We know the binding is held — that's what
	// produced the ConditionalCheckFailed — but we couldn't read the
	// admin set to choose between the caller-already-bound and the
	// different-admin user copy. Routing to a "couldn't confirm,
	// please retry" message is more honest than defaulting to
	// "different admin" (which would tell a same-caller re-entry to
	// ask themselves for help).
	ErrCodeWorkspaceBindUnverified = "workspace_bind_unverified"
)

// bindDisambiguationBudget caps the post-CCFE GetItem that decides
// between the caller-already-bound and different-admin 409 message
// variants. The full BindWorkspace call already runs inside the
// view_submission [interactionAsyncBudget] (2.5s); this sub-budget
// keeps a slow disambiguating read from consuming whatever budget
// remains after the failed PutItem and forcing the post-409 PostDM
// off the wire.
//
// The wrapping [context.WithTimeout](ctx, budget) clamps against
// the parent ctx, so the effective budget is `min(300ms, parent
// remaining)` — a near-expired parent ctx yields a tighter cap.
// On timeout/transport failure the call surfaces
// [ErrCodeWorkspaceBindUnverified]; the handler renders a
// "couldn't confirm — please retry" copy.
const bindDisambiguationBudget = 300 * time.Millisecond

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

// BindWorkspace creates the workspace mapping row on first claim.
// Used by the admin-claim redeem flow once the bootstrap code has
// been atomically consumed. The seedAdmin becomes the only entry in
// admin_slack_user_ids — additional admins are added later via a
// separate "add admin" path (not in this PR).
//
// Returns 409 (via *Error) if the row already exists — a single-use
// bootstrap code can't legitimately produce a re-bind, so the
// existing-row case is treated as a fatal "this workspace is already
// claimed" failure. Surfacing same-owner as 409 (instead of silently
// overwriting) is intentional: PutItem would replace the entire row
// including any admin_slack_user_ids added after the first claim.
// If/when an explicit "rotate" verb lands, it gets its own SET-based
// UpdateItem rather than reusing this constructor path.
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
	// Capture `now` once: the same logical bind action's CreatedAt
	// fallback and updated_at attribute should reflect the same
	// instant rather than two sub-microsecond-apart readings of
	// time.Now.
	now := s.nowOrDefault().UTC()
	nowISO := now.Format(time.RFC3339)
	created := m.CreatedAt
	if created.IsZero() {
		created = now
	}

	// Conditional PutItem: refuse to overwrite any existing row. The
	// single-use bootstrap code means we can't legitimately re-enter
	// this path with the same workspace; an existing row is either a
	// different-owner conflict (the new owner shouldn't silently win)
	// OR a same-owner re-entry that would clobber later-added admins.
	// Both deserve the 409 signal.
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
			attrUpdatedAt: stringAttr(nowISO),
		},
		ConditionExpression: aws.String("attribute_not_exists(slack_team_id)"),
	})
	if err == nil {
		return nil
	}
	var ccfe *ddbtypes.ConditionalCheckFailedException
	if !errors.As(err, &ccfe) {
		return ddbToError("BindWorkspace", err)
	}
	// ConditionalCheckFailed here means the row already exists.
	// Distinguish "this caller is already the admin" from "another
	// admin holds this workspace" so the handler renders the right
	// user copy. A same-caller re-entry surfaces a distinct error
	// code so the handler can short-circuit to "you're already the
	// admin" instead of telling the user to ask themselves for help.
	//
	// Race: another admin could mutate admin_slack_user_ids between
	// the failed Put and this Get. The race is on which 409 *message*
	// the user sees (caller-already-bound vs different-admin); the
	// binding itself stays correctly held by the existing admin set
	// either way. Bounded impact → no extra locking.
	//
	// [bindDisambiguationBudget] caps this read so a slow DDB
	// response can't push the parent interaction over its 2.5s wall.
	disambigCtx, disambigCancel := context.WithTimeout(ctx, bindDisambiguationBudget)
	defer disambigCancel()
	check, getErr := s.Client.GetItem(disambigCtx, &dynamodb.GetItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(m.TeamID),
		},
		// ConsistentRead pairs with the PutItem-CCFE that just fired:
		// the row exists, but an eventually-consistent read on a
		// stale replica could miss it and send a same-caller re-entry
		// down the "different admin holds this workspace" branch.
		// Strong read avoids the cross-replica race; 2x RCUs on a
		// rare unhappy path.
		ConsistentRead: aws.Bool(true),
	})
	if getErr != nil {
		// Disambiguation read failed (timeout / transport). Surface
		// the unverified variant rather than defaulting to
		// "different admin", which would tell a same-caller re-entry
		// to ask themselves for help.
		return &Error{
			StatusCode: http.StatusConflict,
			Code:       ErrCodeWorkspaceBindUnverified,
			Title:      "BindWorkspace: bind held but disambiguation read failed",
		}
	}
	if len(check.Item) > 0 {
		for _, u := range readStringSet(check.Item, attrAdminSlackUserIDs) {
			if u == seedAdmin {
				return &Error{
					StatusCode: http.StatusConflict,
					Code:       ErrCodeWorkspaceAlreadyBoundToCaller,
					Title:      "BindWorkspace: caller is already on this workspace's admin set",
				}
			}
		}
	}
	return &Error{
		StatusCode: http.StatusConflict,
		Code:       ErrCodeWorkspaceAlreadyBound,
		Title:      "BindWorkspace: workspace is already claimed by a different admin",
	}
}
