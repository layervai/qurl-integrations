package teamsdata

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	attrTenantID                 = "teams_tenant_id"
	attrOwnerID                  = "owner_id"
	attrAdminActorIDs            = "admin_actor_ids"
	attrPersonalConversationRefs = "personal_conversation_refs"
	attrCreatedAt                = "created_at"
	attrUpdatedAt                = "updated_at"
	attrUpdatedAtNano            = "updated_at_unix_nano"
)

// Teams workspace/admin error codes returned by the DynamoDB-backed store.
const (
	ErrCodeWorkspaceAlreadyBoundToCaller = "workspace_already_bound_to_caller"
	ErrCodeWorkspaceAlreadyBound         = "workspace_already_bound"
	ErrCodeWorkspaceNotBound             = "workspace_not_bound"
	ErrCodeAdminAlreadyExists            = "admin_already_exists"
	ErrCodeAdminNotFound                 = "admin_not_found"
	ErrCodeCannotRemoveOwner             = "cannot_remove_owner"
)

// WorkspaceMapping records the Teams tenant owner bound to qURL.
type WorkspaceMapping struct {
	TenantID  string
	OwnerID   string
	CreatedAt time.Time
}

// PersonalConversationRef identifies a user's personal Teams chat with the bot.
type PersonalConversationRef struct {
	ServiceURL     string `json:"service_url"`
	ConversationID string `json:"conversation_id"`
}

// CheckAdmin reports whether the actor is the owner or an admin for the tenant.
func (s *Store) CheckAdmin(ctx context.Context, tenantID, actorID string) (isAdmin bool, ownerID string, err error) {
	if tenantID == "" || actorID == "" {
		return false, "", &Error{StatusCode: http.StatusBadRequest, Title: "CheckAdmin: tenant_id and actor_id are required"}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
		},
	})
	if err != nil {
		return false, "", ddbToError("CheckAdmin", err)
	}
	if len(out.Item) == 0 {
		return false, "", nil
	}
	ownerID = readString(out.Item, attrOwnerID)
	if ownerID == actorID {
		return true, ownerID, nil
	}
	for _, adminID := range readStringSet(out.Item, attrAdminActorIDs) {
		if adminID == actorID {
			return true, ownerID, nil
		}
	}
	return false, ownerID, nil
}

// BindWorkspace creates the initial owner binding for a Teams tenant.
func (s *Store) BindWorkspace(ctx context.Context, m *WorkspaceMapping, seedAdmin string) error {
	if m == nil || m.TenantID == "" || m.OwnerID == "" || seedAdmin == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "BindWorkspace: tenant_id, owner_id, and seed_admin are required"}
	}
	now := s.nowOrDefault().UTC()
	created := m.CreatedAt
	if created.IsZero() {
		created = now
	}
	_, err := s.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Item: map[string]ddbtypes.AttributeValue{
			attrTenantID:      stringAttr(m.TenantID),
			attrOwnerID:       stringAttr(m.OwnerID),
			attrAdminActorIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{seedAdmin}},
			attrCreatedAt:     stringAttr(created.Format(time.RFC3339)),
			attrUpdatedAt:     stringAttr(now.Format(time.RFC3339)),
			attrUpdatedAtNano: unixNanoAttr(now),
		},
		ConditionExpression: aws.String("attribute_not_exists(" + attrTenantID + ")"),
	})
	if err == nil {
		return nil
	}
	var ccfe *ddbtypes.ConditionalCheckFailedException
	if !errors.As(err, &ccfe) {
		return ddbToError("BindWorkspace", err)
	}
	out, getErr := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.WorkspaceMappingsName),
		ConsistentRead: aws.Bool(true),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(m.TenantID),
		},
	})
	if getErr != nil {
		return ddbToError("BindWorkspace", getErr)
	}
	if readString(out.Item, attrOwnerID) == seedAdmin {
		return &Error{
			StatusCode: http.StatusConflict,
			Code:       ErrCodeWorkspaceAlreadyBoundToCaller,
			Title:      "BindWorkspace: caller is the existing workspace owner",
		}
	}
	return &Error{
		StatusCode: http.StatusConflict,
		Code:       ErrCodeWorkspaceAlreadyBound,
		Title:      "BindWorkspace: workspace is already owned by a different Teams user",
	}
}

// AddAdmin grants tenant admin access to the target Teams actor.
func (s *Store) AddAdmin(ctx context.Context, tenantID, actorID string) error {
	if tenantID == "" || actorID == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "AddAdmin: tenant_id and actor_id are required"}
	}
	now := s.nowOrDefault().UTC()
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
		},
		UpdateExpression:    aws.String("SET " + attrUpdatedAt + " = :now, " + attrUpdatedAtNano + " = :now_nano ADD " + attrAdminActorIDs + " :admins"),
		ConditionExpression: aws.String("attribute_exists(" + attrTenantID + ") AND NOT contains(" + attrAdminActorIDs + ", :actor)"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":admins":   &ddbtypes.AttributeValueMemberSS{Value: []string{actorID}},
			":actor":    stringAttr(actorID),
			":now":      stringAttr(now.Format(time.RFC3339)),
			":now_nano": unixNanoAttr(now),
		},
	})
	if err == nil {
		return nil
	}
	var ccfe *ddbtypes.ConditionalCheckFailedException
	if !errors.As(err, &ccfe) {
		return ddbToError("AddAdmin", err)
	}
	isAdmin, _, readErr := s.CheckAdmin(ctx, tenantID, actorID)
	if readErr == nil && isAdmin {
		return &Error{StatusCode: http.StatusConflict, Code: ErrCodeAdminAlreadyExists, Title: "AddAdmin: actor is already an admin"}
	}
	return &Error{StatusCode: http.StatusNotFound, Code: ErrCodeWorkspaceNotBound, Title: "AddAdmin: workspace is not bound"}
}

// RemoveAdmin revokes tenant admin access from the target Teams actor.
func (s *Store) RemoveAdmin(ctx context.Context, tenantID, actorID string) error {
	if tenantID == "" || actorID == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "RemoveAdmin: tenant_id and actor_id are required"}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.WorkspaceMappingsName),
		ConsistentRead: aws.Bool(true),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
		},
	})
	if err != nil {
		return ddbToError("RemoveAdmin", err)
	}
	if len(out.Item) == 0 {
		return &Error{StatusCode: http.StatusNotFound, Code: ErrCodeWorkspaceNotBound, Title: "RemoveAdmin: workspace is not bound"}
	}
	if readString(out.Item, attrOwnerID) == actorID {
		return &Error{StatusCode: http.StatusBadRequest, Code: ErrCodeCannotRemoveOwner, Title: "RemoveAdmin: cannot remove owner"}
	}
	now := s.nowOrDefault().UTC()
	_, err = s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
		},
		UpdateExpression:    aws.String("SET " + attrUpdatedAt + " = :now, " + attrUpdatedAtNano + " = :now_nano DELETE " + attrAdminActorIDs + " :admins"),
		ConditionExpression: aws.String("contains(" + attrAdminActorIDs + ", :actor)"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":admins":   &ddbtypes.AttributeValueMemberSS{Value: []string{actorID}},
			":actor":    stringAttr(actorID),
			":now":      stringAttr(now.Format(time.RFC3339)),
			":now_nano": unixNanoAttr(now),
		},
	})
	if err == nil {
		return nil
	}
	var ccfe *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &ccfe) {
		return &Error{StatusCode: http.StatusNotFound, Code: ErrCodeAdminNotFound, Title: "RemoveAdmin: actor is not an admin"}
	}
	return ddbToError("RemoveAdmin", err)
}

// ListAdmins returns the tenant owner and additional admin IDs.
func (s *Store) ListAdmins(ctx context.Context, tenantID string) (ownerID string, adminIDs []string, err error) {
	if tenantID == "" {
		return "", nil, &Error{StatusCode: http.StatusBadRequest, Title: "ListAdmins: tenant_id is required"}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
		},
	})
	if err != nil {
		return "", nil, ddbToError("ListAdmins", err)
	}
	if len(out.Item) == 0 {
		return "", nil, &Error{StatusCode: http.StatusNotFound, Code: ErrCodeWorkspaceNotBound, Title: "ListAdmins: workspace is not bound"}
	}
	ownerID = readString(out.Item, attrOwnerID)
	for _, adminID := range readStringSet(out.Item, attrAdminActorIDs) {
		if adminID != "" && adminID != ownerID {
			adminIDs = append(adminIDs, adminID)
		}
	}
	sort.Strings(adminIDs)
	return ownerID, adminIDs, nil
}

// SavePersonalConversationRef stores the user's personal bot chat reference.
func (s *Store) SavePersonalConversationRef(ctx context.Context, tenantID, actorID string, ref *PersonalConversationRef) error {
	if tenantID == "" || actorID == "" || ref == nil || ref.ServiceURL == "" || ref.ConversationID == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "SavePersonalConversationRef: tenant_id, actor_id, and personal reference are required"}
	}
	raw, err := json.Marshal(ref)
	if err != nil {
		return &Error{StatusCode: http.StatusBadRequest, Title: "SavePersonalConversationRef: marshal personal reference failed", Detail: err.Error()}
	}
	now := s.nowOrDefault().UTC()
	_, err = s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
		},
		UpdateExpression:    aws.String("SET " + attrPersonalConversationRefs + ".#actor = :ref, " + attrUpdatedAt + " = :now, " + attrUpdatedAtNano + " = :now_nano"),
		ConditionExpression: aws.String("attribute_exists(" + attrTenantID + ")"),
		ExpressionAttributeNames: map[string]string{
			"#actor": actorID,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":ref":      stringAttr(string(raw)),
			":now":      stringAttr(now.Format(time.RFC3339)),
			":now_nano": unixNanoAttr(now),
		},
	})
	if err != nil {
		return ddbToError("SavePersonalConversationRef", err)
	}
	return nil
}

// PersonalConversationRef loads the stored personal bot chat reference, if present.
func (s *Store) PersonalConversationRef(ctx context.Context, tenantID, actorID string) (*PersonalConversationRef, bool, error) {
	if tenantID == "" || actorID == "" {
		return nil, false, &Error{StatusCode: http.StatusBadRequest, Title: "PersonalConversationRef: tenant_id and actor_id are required"}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
		},
		ProjectionExpression: aws.String(attrPersonalConversationRefs + ".#actor"),
		ExpressionAttributeNames: map[string]string{
			"#actor": actorID,
		},
	})
	if err != nil {
		return nil, false, ddbToError("PersonalConversationRef", err)
	}
	raw := readStringMap(out.Item, attrPersonalConversationRefs)[actorID]
	if raw == "" {
		return nil, false, nil
	}
	var ref PersonalConversationRef
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		return nil, false, ddbToError("PersonalConversationRef", err)
	}
	if ref.ServiceURL == "" || ref.ConversationID == "" {
		return nil, false, nil
	}
	return &ref, true, nil
}

// DeleteWorkspace removes the Teams tenant binding row.
func (s *Store) DeleteWorkspace(ctx context.Context, tenantID string) error {
	if tenantID == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "DeleteWorkspace: tenant_id is required"}
	}
	_, err := s.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.WorkspaceMappingsName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
		},
	})
	if err != nil {
		return ddbToError("DeleteWorkspace", err)
	}
	return nil
}
