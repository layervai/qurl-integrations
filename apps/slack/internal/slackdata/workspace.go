package slackdata

import (
	"context"
	"errors"
	"net/http"
	"sort"
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
	// ErrCodeWorkspaceNotBound is surfaced by AddAdmin/RemoveAdmin/
	// ListAdmins when the (team_id) row hasn't been claimed yet —
	// the handler renders "run `/qurl setup` first" copy.
	ErrCodeWorkspaceNotBound = "workspace_not_bound"
	// ErrCodeAdminAlreadyExists is surfaced by AddAdmin when the
	// target user is already on the admin set — idempotent no-op
	// surface at the handler.
	ErrCodeAdminAlreadyExists = "admin_already_exists"
	// ErrCodeAdminAddUnverified is surfaced by AddAdmin when the
	// post-CCFE disambiguation read sees the workspace row but
	// admin_slack_user_ids is missing or wrong-typed (so the target
	// can't be confirmed as a current member). Distinct from
	// admin_already_exists because the user-visible copy should be
	// "couldn't confirm, please retry" rather than the misleading
	// "already an admin" surface.
	ErrCodeAdminAddUnverified = "admin_add_unverified"
	// ErrCodeAdminNotFound is surfaced by RemoveAdmin when the target
	// user isn't on the admin set — idempotent no-op surface at the
	// handler.
	ErrCodeAdminNotFound = "admin_not_found"
	// ErrCodeCannotRemoveOwner is surfaced by RemoveAdmin when the
	// caller targets the workspace owner. The owner is the OAuth
	// installer; demoting them via the bot would leave the workspace
	// in a half-claimed state. Re-install OAuth to transfer ownership.
	ErrCodeCannotRemoveOwner = "cannot_remove_owner"
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
// Workspace not yet bound to an owner → (false, "", nil). The handler
// treats this the same as "user not on admin set" and renders the
// generic ":warning: this command is admin-only" copy. The distinct
// "admin features not configured" copy is reserved for the
// AdminStore==nil branch (sandbox deploys without DDB), which never
// reaches this function. Distinguishing "workspace-not-bound" from
// "user-not-on-admin-list" is left to a follow-up (the old HTTP
// surface didn't distinguish them either — both came back as
// `is_admin=false`).
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
	// ErrCodeWorkspaceAlreadyBound covers two structurally distinct
	// conflicts: (a) the existing row's owner_id matches m.OwnerID but
	// the caller is not on the admin set, and (b) the existing row's
	// owner_id is a DIFFERENT owner entirely (a bootstrap code minted
	// against owner A landed on a row owned by B). Both produce the
	// same "different admin claims this workspace" user copy because
	// the user signal is the same — they cannot become admin via this
	// path regardless of which mismatch fired. Operators who need the
	// distinction read the workspace_mappings row directly.
	return &Error{
		StatusCode: http.StatusConflict,
		Code:       ErrCodeWorkspaceAlreadyBound,
		Title:      "BindWorkspace: workspace is already claimed by a different admin",
	}
}

// AddAdmin promotes targetUserID to bot admin on the (teamID)
// workspace_mappings row. A single conditional UpdateItem folds the
// "row exists + user not already on the set" check into the same DDB
// item-lock as the mutation:
//
//   - ConditionalCheckFailed → either no row OR the user is already on
//     the set. A follow-up GetItem disambiguates the two so the handler
//     can render the right copy (404 workspace_not_bound vs 409
//     admin_already_exists). Without this read the producer-side error
//     codes would conflate two structurally different states.
//   - Any other error surfaces via [ddbToError] as a 503.
//
// On success the existing admin_slack_user_ids SS gets the target user
// appended (ADD on an SS is set-union); updated_at is bumped.
func (s *Store) AddAdmin(ctx context.Context, teamID, targetUserID string) error {
	if teamID == "" || targetUserID == "" {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "AddAdmin: team_id and target_user_id are required",
		}
	}
	nowISO := s.nowOrDefault().UTC().Format(time.RFC3339)
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(teamID),
		},
		UpdateExpression: aws.String("ADD admin_slack_user_ids :uids SET updated_at = :now"),
		// Two clauses combined: refuse on a missing row (handler maps
		// to "workspace_not_bound") AND on an already-member user
		// (handler maps to "admin_already_exists"). DDB doesn't let
		// us discriminate which clause failed from the CCFE alone, so
		// the post-CCFE GetItem below makes the call.
		ConditionExpression: aws.String("attribute_exists(slack_team_id) AND NOT contains(admin_slack_user_ids, :uid)"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":uids": &ddbtypes.AttributeValueMemberSS{Value: []string{targetUserID}},
			":uid":  stringAttr(targetUserID),
			exprNow: stringAttr(nowISO),
		},
	})
	if err == nil {
		return nil
	}
	var ccfe *ddbtypes.ConditionalCheckFailedException
	if !errors.As(err, &ccfe) {
		return ddbToError("AddAdmin", err)
	}
	// Disambiguate: row missing → 404; row exists with user already on
	// the set → 409. A bare CCFE doesn't tell us which arm of the
	// AND fired; one GetItem is the cheapest way to surface the right
	// user-facing code.
	out, getErr := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.WorkspaceMappingsName),
		ConsistentRead: aws.Bool(true),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(teamID),
		},
	})
	if getErr != nil {
		// Disambiguation read failed; surface a generic 503 rather
		// than guess at the underlying state. The audit log on the
		// caller-side captures the original CCFE for triage.
		return ddbToError("AddAdmin", getErr)
	}
	if len(out.Item) == 0 {
		return &Error{
			StatusCode: http.StatusNotFound,
			Code:       ErrCodeWorkspaceNotBound,
			Title:      "AddAdmin: workspace is not bound",
		}
	}
	// Confirm the target is actually on the admin set before
	// returning the "already an admin" code. If the row exists but
	// admin_slack_user_ids is missing or the wrong type
	// (readStringSet returns empty/nil for both), the conditional
	// somehow fired without the target being a member — surface a
	// distinct unverified 409 so the handler can render a "retry"
	// hint instead of misleading "already an admin" copy.
	for _, u := range readStringSet(out.Item, attrAdminSlackUserIDs) {
		if u == targetUserID {
			return &Error{
				StatusCode: http.StatusConflict,
				Code:       ErrCodeAdminAlreadyExists,
				Title:      "AddAdmin: target user is already an admin",
			}
		}
	}
	return &Error{
		StatusCode: http.StatusConflict,
		Code:       ErrCodeAdminAddUnverified,
		Title:      "AddAdmin: conditional fired but target not confirmed on admin set",
	}
}

// RemoveAdmin demotes targetUserID from bot admin on the (teamID)
// workspace_mappings row.
//
// Refuses to demote the workspace owner — the owner is the OAuth
// installer, and removing them via the bot would leave the workspace
// in a half-claimed state where no one can re-promote (the new admin
// set has no relationship to the OAuth identity). The owner check is
// a read-before-write: a GetItem precedes the conditional UpdateItem
// so the 400 cannot_remove_owner surfaces before any mutation
// attempt. The read isn't strongly consistent because owner_id is
// currently immutable post-bind (BindWorkspace is the only writer).
//
// TODO(ownership-transfer): if/when an OAuth-re-install path lands
// that mutates owner_id, this read needs ConsistentRead=true OR the
// owner check needs to fold into the conditional UpdateItem
// (`AND owner_id <> :uid`). Today the cross-replica lag window only
// matters during the seconds between BindWorkspace and the user's
// first RemoveAdmin call.
//
// CCFE on the UpdateItem maps to 404 admin_not_found — either the row
// vanished (race with a concurrent OAuth re-install) or the target
// wasn't on the admin set in the first place. Both render the same
// idempotent "nothing to do" copy at the handler.
func (s *Store) RemoveAdmin(ctx context.Context, teamID, targetUserID string) error {
	if teamID == "" || targetUserID == "" {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "RemoveAdmin: team_id and target_user_id are required",
		}
	}
	// Owner-check read precedes the mutation. Use the same GetItem as
	// ListAdmins so the 404 path is identical — row missing surfaces
	// as workspace_not_bound rather than racing into the UpdateItem's
	// CCFE-classified-as-admin_not_found (which would render the
	// wrong copy).
	out, getErr := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(teamID),
		},
	})
	if getErr != nil {
		return ddbToError("RemoveAdmin", getErr)
	}
	if len(out.Item) == 0 {
		return &Error{
			StatusCode: http.StatusNotFound,
			Code:       ErrCodeWorkspaceNotBound,
			Title:      "RemoveAdmin: workspace is not bound",
		}
	}
	if ownerID := readString(out.Item, attrOwnerID); ownerID != "" && ownerID == targetUserID {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Code:       ErrCodeCannotRemoveOwner,
			Title:      "RemoveAdmin: cannot remove the workspace owner",
		}
	}
	nowISO := s.nowOrDefault().UTC().Format(time.RFC3339)
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(teamID),
		},
		UpdateExpression: aws.String("DELETE admin_slack_user_ids :uids SET updated_at = :now"),
		// `contains(...)` guarantees the user is in the set at
		// mutation time — a CCFE here is the "nothing to remove"
		// case (either the row vanished mid-flight, or the user
		// wasn't a member). attribute_exists pin protects against
		// a row delete-race in the same code path.
		ConditionExpression: aws.String("contains(admin_slack_user_ids, :uid) AND attribute_exists(slack_team_id)"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":uids": &ddbtypes.AttributeValueMemberSS{Value: []string{targetUserID}},
			":uid":  stringAttr(targetUserID),
			exprNow: stringAttr(nowISO),
		},
	})
	if err == nil {
		return nil
	}
	var ccfe *ddbtypes.ConditionalCheckFailedException
	if !errors.As(err, &ccfe) {
		return ddbToError("RemoveAdmin", err)
	}
	return &Error{
		StatusCode: http.StatusNotFound,
		Code:       ErrCodeAdminNotFound,
		Title:      "RemoveAdmin: target user is not on the admin set",
	}
}

// ListAdmins returns the workspace owner ID and the sorted set of
// admin Slack user IDs. The owner is always present (BindWorkspace
// stamps it on first claim); admin_slack_user_ids may include the
// owner alongside additional admins added via AddAdmin.
//
// 404 workspace_not_bound when the row is missing. The slice is
// sorted ascending so callers (the `/qurl admin list` handler in
// particular) render a deterministic order across calls — operators
// audit-via-paste, and a re-ordered listing reads as state churn
// that didn't actually happen.
func (s *Store) ListAdmins(ctx context.Context, teamID string) (ownerID string, adminIDs []string, err error) {
	if teamID == "" {
		return "", nil, &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "ListAdmins: team_id is required",
		}
	}
	out, getErr := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID: stringAttr(teamID),
		},
	})
	if getErr != nil {
		return "", nil, ddbToError("ListAdmins", getErr)
	}
	if len(out.Item) == 0 {
		return "", nil, &Error{
			StatusCode: http.StatusNotFound,
			Code:       ErrCodeWorkspaceNotBound,
			Title:      "ListAdmins: workspace is not bound",
		}
	}
	ownerID = readString(out.Item, attrOwnerID)
	adminIDs = readStringSet(out.Item, attrAdminSlackUserIDs)
	sort.Strings(adminIDs)
	return ownerID, adminIDs, nil
}
