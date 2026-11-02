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
	for key, value := range map[string]string{":authorized_owner": authorization.ownerID, ":authorized_caller": authorization.userID, ":authorized_generation": authorization.generation, ":authorized_installation": authorization.installation} {
		if _, present := guarded.ExpressionAttributeValues[key]; present {
			return policyWorkspaceChanged()
		}
		guarded.ExpressionAttributeValues[key] = stringAttr(value)
	}
	guarded.ConditionExpression = aws.String("(" + aws.ToString(in.ConditionExpression) + ") AND owner_id = :authorized_owner AND (owner_id = :authorized_caller OR contains(admin_slack_user_ids, :authorized_caller)) AND policy_generation = :authorized_generation AND policy_installation = :authorized_installation")
	guarded.ReturnValuesOnConditionCheckFailure = ddbtypes.ReturnValuesOnConditionCheckFailureAllOld
	_, err := s.Client.UpdateItem(ctx, &guarded)
	var conflict *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &conflict) && len(conflict.Item) != 0 && (readString(conflict.Item, attrOwnerID) != authorization.ownerID || readString(conflict.Item, attrPolicyGeneration) != authorization.generation || readString(conflict.Item, attrPolicyInstallation) != authorization.installation || !workspaceCallerIsAdmin(conflict.Item, authorization.userID)) {
		return policyWorkspaceChanged()
	}
	return err
}
