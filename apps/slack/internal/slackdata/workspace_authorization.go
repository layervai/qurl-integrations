package slackdata

import (
	"context"
	"errors"
	"maps"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// updateAuthorizedWorkspace checks captured authority in the single-row update.
// A transaction cannot check and update the same DynamoDB item twice.
func (s *Store) updateAuthorizedWorkspace(ctx context.Context, in *dynamodb.UpdateItemInput) error {
	authorization, ok := ctx.Value(policyAuthorizationContextKey{}).(PolicyAuthorization)
	if !ok || authorization.store != s || authorization.generation == "" || authorization.installation == "" || aws.ToString(in.TableName) != s.WorkspaceMappingsName || readString(in.Key, attrSlackTeamID) != authorization.teamID {
		return policyWorkspaceChanged()
	}
	guarded := *in
	guarded.ExpressionAttributeValues = maps.Clone(in.ExpressionAttributeValues)
	if guarded.ExpressionAttributeValues == nil {
		guarded.ExpressionAttributeValues = make(map[string]ddbtypes.AttributeValue)
	}
	for key, value := range map[string]string{":authorized_owner": authorization.ownerID, ":authorized_caller": authorization.userID, ":authorized_generation": authorization.generation, ":authorized_installation": authorization.installation} {
		if _, present := guarded.ExpressionAttributeValues[key]; present {
			return policyWorkspaceChanged()
		}
		guarded.ExpressionAttributeValues[key] = stringAttr(value)
	}
	condition := ""
	if aws.ToString(in.ConditionExpression) != "" {
		condition = "(" + aws.ToString(in.ConditionExpression) + ") AND "
	}
	guarded.ConditionExpression = aws.String(condition + "owner_id = :authorized_owner AND (owner_id = :authorized_caller OR contains(admin_slack_user_ids, :authorized_caller)) AND policy_generation = :authorized_generation AND policy_installation = :authorized_installation")
	guarded.ReturnValuesOnConditionCheckFailure = ddbtypes.ReturnValuesOnConditionCheckFailureAllOld
	_, err := s.Client.UpdateItem(ctx, &guarded)
	var conflict *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &conflict) && !matchesPolicyAuthorization(conflict.Item, &authorization) {
		return policyWorkspaceChanged()
	}
	return err
}

// ValidatePolicyAuthorization checks captured mutation scope before selecting
// credentials. Ordinary read contexts do not incur a workspace read.
func ValidatePolicyAuthorization(ctx context.Context, store *Store, teamID string) error {
	raw := ctx.Value(policyAuthorizationContextKey{})
	if raw == nil {
		return nil
	}
	authorization, ok := raw.(PolicyAuthorization)
	if !ok || store == nil || authorization.store != store || authorization.teamID != teamID || authorization.generation == "" || authorization.installation == "" {
		return policyWorkspaceChanged()
	}
	out, err := store.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(store.WorkspaceMappingsName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID)}, ConsistentRead: aws.Bool(true)})
	if err != nil {
		return ddbToError("ValidatePolicyAuthorization", err)
	}
	if !matchesPolicyAuthorization(out.Item, &authorization) {
		return policyWorkspaceChanged()
	}
	return nil
}

func matchesPolicyAuthorization(item map[string]ddbtypes.AttributeValue, authorization *PolicyAuthorization) bool {
	return readString(item, attrOwnerID) == authorization.ownerID && readString(item, attrPolicyGeneration) == authorization.generation && readString(item, attrPolicyInstallation) == authorization.installation && workspaceCallerIsAdmin(item, authorization.userID)
}
