package teamsdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	attrScopeID            = "teams_scope_id"
	attrAliasBindings      = "alias_bindings"
	attrAllowedResourceIDs = "allowed_resource_ids"
)

// ErrAliasAlreadyBound reports that a channel alias already exists.
var ErrAliasAlreadyBound = errors.New("alias already bound in this scope")

// ErrAliasNotFound reports that a channel alias does not exist.
var ErrAliasNotFound = errors.New("alias not bound in this scope")

// PolicyEntry describes a single alias binding in a Teams scope.
type PolicyEntry struct {
	ScopeID    string    `json:"scope_id"`
	Alias      string    `json:"alias"`
	ResourceID string    `json:"resource_id"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
}

// AllowedResourceIDsForScope returns the resource IDs currently reachable in a Teams scope.
func (s *Store) AllowedResourceIDsForScope(ctx context.Context, tenantID, scopeID string) (map[string]struct{}, error) {
	if tenantID == "" || scopeID == "" {
		return nil, &Error{StatusCode: http.StatusBadRequest, Title: "AllowedResourceIDsForScope: tenant_id and scope_id are required"}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
			attrScopeID:  stringAttr(scopeID),
		},
	})
	if err != nil {
		return nil, ddbToError("AllowedResourceIDsForScope", err)
	}
	outSet := map[string]struct{}{}
	for _, rid := range readStringSet(out.Item, attrAllowedResourceIDs) {
		outSet[rid] = struct{}{}
	}
	for _, rid := range readStringMap(out.Item, attrAliasBindings) {
		if rid != "" {
			outSet[rid] = struct{}{}
		}
	}
	return outSet, nil
}

// ResolvePolicy reports whether a resource is currently exposed to a Teams scope.
func (s *Store) ResolvePolicy(ctx context.Context, tenantID, scopeID, resourceID string) (bool, error) {
	allowed, err := s.AllowedResourceIDsForScope(ctx, tenantID, scopeID)
	if err != nil {
		return false, err
	}
	_, ok := allowed[resourceID]
	return ok, nil
}

// LookupScopeAlias resolves a channel alias to a resource ID.
func (s *Store) LookupScopeAlias(ctx context.Context, tenantID, scopeID, aliasName string) (resourceID string, found bool, err error) {
	if tenantID == "" || scopeID == "" || aliasName == "" {
		return "", false, &Error{StatusCode: http.StatusBadRequest, Title: "LookupScopeAlias: tenant_id, scope_id, and alias_name are required"}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
			attrScopeID:  stringAttr(scopeID),
		},
		ProjectionExpression: aws.String(attrAliasBindings + ".#alias"),
		ExpressionAttributeNames: map[string]string{
			"#alias": aliasName,
		},
	})
	if err != nil {
		return "", false, ddbToError("LookupScopeAlias", err)
	}
	resourceID = readStringMap(out.Item, attrAliasBindings)[aliasName]
	return resourceID, resourceID != "", nil
}

// GetScopePolicy lists alias bindings for a Teams scope.
func (s *Store) GetScopePolicy(ctx context.Context, tenantID, scopeID string) ([]PolicyEntry, error) {
	if tenantID == "" || scopeID == "" {
		return nil, &Error{StatusCode: http.StatusBadRequest, Title: "GetScopePolicy: tenant_id and scope_id are required"}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
			attrScopeID:  stringAttr(scopeID),
		},
	})
	if err != nil {
		return nil, ddbToError("GetScopePolicy", err)
	}
	createdAt, _ := time.Parse(time.RFC3339, readString(out.Item, attrCreatedAt))
	entries := make([]PolicyEntry, 0, len(out.Item))
	for alias, rid := range readStringMap(out.Item, attrAliasBindings) {
		entries = append(entries, PolicyEntry{
			ScopeID:    scopeID,
			Alias:      alias,
			ResourceID: rid,
			CreatedAt:  createdAt,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Alias < entries[j].Alias })
	return entries, nil
}

// BindScopeAlias binds an alias to a resource within a Teams scope.
func (s *Store) BindScopeAlias(ctx context.Context, tenantID, scopeID, aliasName, resourceID string) error {
	if tenantID == "" || scopeID == "" || aliasName == "" || resourceID == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "BindScopeAlias: tenant_id, scope_id, alias_name, and resource_id are required"}
	}
	now := s.nowOrDefault().UTC()
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
			attrScopeID:  stringAttr(scopeID),
		},
		UpdateExpression:    aws.String("SET " + attrAliasBindings + ".#alias = :rid, " + attrCreatedAt + " = if_not_exists(" + attrCreatedAt + ", :created_at), " + attrUpdatedAt + " = :updated_at, " + attrUpdatedAtNano + " = :updated_at_nano"),
		ConditionExpression: aws.String("attribute_not_exists(" + attrAliasBindings + ".#alias)"),
		ExpressionAttributeNames: map[string]string{
			"#alias": aliasName,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":rid":             stringAttr(resourceID),
			":created_at":      stringAttr(now.Format(time.RFC3339)),
			":updated_at":      stringAttr(now.Format(time.RFC3339)),
			":updated_at_nano": unixNanoAttr(now),
		},
	})
	if err == nil {
		return nil
	}
	var ccfe *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &ccfe) {
		return ErrAliasAlreadyBound
	}
	return ddbToError("BindScopeAlias", err)
}

// UnbindScopeAlias removes an alias from a Teams scope.
func (s *Store) UnbindScopeAlias(ctx context.Context, tenantID, scopeID, aliasName string) error {
	if tenantID == "" || scopeID == "" || aliasName == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "UnbindScopeAlias: tenant_id, scope_id, and alias_name are required"}
	}
	now := s.nowOrDefault().UTC()
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
			attrScopeID:  stringAttr(scopeID),
		},
		UpdateExpression:    aws.String("SET " + attrUpdatedAt + " = :updated_at, " + attrUpdatedAtNano + " = :updated_at_nano REMOVE " + attrAliasBindings + ".#alias"),
		ConditionExpression: aws.String("attribute_exists(" + attrAliasBindings + ".#alias)"),
		ExpressionAttributeNames: map[string]string{
			"#alias": aliasName,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":updated_at":      stringAttr(now.Format(time.RFC3339)),
			":updated_at_nano": unixNanoAttr(now),
		},
	})
	if err == nil {
		return nil
	}
	var ccfe *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &ccfe) {
		return ErrAliasNotFound
	}
	return ddbToError("UnbindScopeAlias", err)
}

// ExposeResourceToScope adds a resource to the scope's allowed set.
func (s *Store) ExposeResourceToScope(ctx context.Context, tenantID, scopeID, resourceID string) error {
	return s.updateScopeResourceSet(ctx, "ADD", tenantID, scopeID, resourceID)
}

// RevokeResourceFromScope removes a resource from the scope's allowed set.
func (s *Store) RevokeResourceFromScope(ctx context.Context, tenantID, scopeID, resourceID string) error {
	return s.updateScopeResourceSet(ctx, "DELETE", tenantID, scopeID, resourceID)
}

func (s *Store) updateScopeResourceSet(ctx context.Context, op, tenantID, scopeID, resourceID string) error {
	if tenantID == "" || scopeID == "" || resourceID == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "updateScopeResourceSet: tenant_id, scope_id, and resource_id are required"}
	}
	now := s.nowOrDefault().UTC()
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
			attrScopeID:  stringAttr(scopeID),
		},
		UpdateExpression: aws.String("SET " + attrCreatedAt + " = if_not_exists(" + attrCreatedAt + ", :created_at), " + attrUpdatedAt + " = :updated_at, " + attrUpdatedAtNano + " = :updated_at_nano " + op + " " + attrAllowedResourceIDs + " :resource_ids"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":created_at":      stringAttr(now.Format(time.RFC3339)),
			":updated_at":      stringAttr(now.Format(time.RFC3339)),
			":updated_at_nano": unixNanoAttr(now),
			":resource_ids":    &ddbtypes.AttributeValueMemberSS{Value: []string{resourceID}},
		},
	})
	if err != nil {
		return ddbToError("updateScopeResourceSet", err)
	}
	return nil
}

// ScopesForResource lists channel scopes that currently expose the resource.
func (s *Store) ScopesForResource(ctx context.Context, tenantID, resourceID string) ([]string, error) {
	if tenantID == "" || resourceID == "" {
		return nil, &Error{StatusCode: http.StatusBadRequest, Title: "ScopesForResource: tenant_id and resource_id are required"}
	}
	var scopes []string
	var startKey map[string]ddbtypes.AttributeValue
	for {
		out, err := s.Client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.ChannelPoliciesName),
			KeyConditionExpression: aws.String(attrTenantID + " = :tenant"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":tenant": stringAttr(tenantID),
			},
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return nil, ddbToError("ScopesForResource", err)
		}
		for _, item := range out.Items {
			scopeID := readString(item, attrScopeID)
			if scopeID == "" {
				continue
			}
			if channelItemAllowsResource(item, resourceID) {
				scopes = append(scopes, scopeID)
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		startKey = out.LastEvaluatedKey
	}
	sort.Strings(scopes)
	return scopes, nil
}

// PurgeResourceFromScope removes a resource and any matching aliases from a scope.
func (s *Store) PurgeResourceFromScope(ctx context.Context, tenantID, scopeID, resourceID string) ([]string, error) {
	if tenantID == "" || scopeID == "" || resourceID == "" {
		return nil, &Error{StatusCode: http.StatusBadRequest, Title: "PurgeResourceFromScope: tenant_id, scope_id, and resource_id are required"}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
			attrScopeID:  stringAttr(scopeID),
		},
	})
	if err != nil {
		return nil, ddbToError("PurgeResourceFromScope", err)
	}
	if len(out.Item) == 0 {
		return []string{}, nil
	}
	aliases := make([]string, 0)
	for alias, rid := range readStringMap(out.Item, attrAliasBindings) {
		if rid == resourceID {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)
	now := s.nowOrDefault().UTC()
	expr := "SET " + attrUpdatedAt + " = :updated_at, " + attrUpdatedAtNano + " = :updated_at_nano DELETE " + attrAllowedResourceIDs + " :resource_ids"
	names := map[string]string{}
	if len(aliases) > 0 {
		parts := make([]string, 0, len(aliases))
		for i, alias := range aliases {
			key := fmt.Sprintf("#a%d", i)
			names[key] = alias
			parts = append(parts, attrAliasBindings+"."+key)
		}
		expr = "SET " + attrUpdatedAt + " = :updated_at, " + attrUpdatedAtNano + " = :updated_at_nano REMOVE " + strings.Join(parts, ", ") + " DELETE " + attrAllowedResourceIDs + " :resource_ids"
	}
	_, err = s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTenantID: stringAttr(tenantID),
			attrScopeID:  stringAttr(scopeID),
		},
		UpdateExpression:         aws.String(expr),
		ExpressionAttributeNames: names,
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":updated_at":      stringAttr(now.Format(time.RFC3339)),
			":updated_at_nano": unixNanoAttr(now),
			":resource_ids":    &ddbtypes.AttributeValueMemberSS{Value: []string{resourceID}},
		},
	})
	if err != nil {
		return nil, ddbToError("PurgeResourceFromScope", err)
	}
	return aliases, nil
}

// PurgeTenantScopePolicies deletes every channel policy row for the tenant.
func (s *Store) PurgeTenantScopePolicies(ctx context.Context, tenantID string) error {
	if tenantID == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "PurgeTenantScopePolicies: tenant_id is required"}
	}
	var startKey map[string]ddbtypes.AttributeValue
	for {
		out, err := s.Client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.ChannelPoliciesName),
			KeyConditionExpression: aws.String(attrTenantID + " = :tenant"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":tenant": stringAttr(tenantID),
			},
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return ddbToError("PurgeTenantScopePolicies", err)
		}
		for _, item := range out.Items {
			scopeID := readString(item, attrScopeID)
			if scopeID == "" {
				continue
			}
			if _, err := s.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
				TableName: aws.String(s.ChannelPoliciesName),
				Key: map[string]ddbtypes.AttributeValue{
					attrTenantID: stringAttr(tenantID),
					attrScopeID:  stringAttr(scopeID),
				},
			}); err != nil {
				return ddbToError("PurgeTenantScopePolicies", err)
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		startKey = out.LastEvaluatedKey
	}
	return nil
}

func channelItemAllowsResource(item map[string]ddbtypes.AttributeValue, resourceID string) bool {
	for _, rid := range readStringSet(item, attrAllowedResourceIDs) {
		if rid == resourceID {
			return true
		}
	}
	for _, rid := range readStringMap(item, attrAliasBindings) {
		if rid == resourceID {
			return true
		}
	}
	return false
}
