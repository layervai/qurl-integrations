package slackdata

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ErrCodePolicyWorkspaceChanged means the operation must obtain fresh authorization.
const ErrCodePolicyWorkspaceChanged = "workspace_policy_authorization_changed"

// PolicyAuthorization binds one operation to its installation and caller.
// Only AuthorizePolicyWrite can populate it; no user-supplied generation is trusted.
type PolicyAuthorization struct {
	store      *Store
	teamID     string
	userID     string
	ownerID    string
	generation string
}

type policyAuthorizationContextKey struct{}

// Context carries the authorization through an independently bounded async worker.
func (a PolicyAuthorization) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, policyAuthorizationContextKey{}, a)
}

func policyWorkspaceChanged() error {
	return &Error{StatusCode: http.StatusConflict, Code: ErrCodePolicyWorkspaceChanged,
		Title: "Workspace authorization changed; run the command again"}
}

const policyCallerCondition = "#owner = :owner AND (#owner = :caller OR contains(#admins, :caller))"

// AuthorizePolicyWrite captures current authority before upstream work starts.
// Legacy bindings receive a generation once, without moving their purge cutoff.
func (s *Store) AuthorizePolicyWrite(ctx context.Context, teamID, userID string) (PolicyAuthorization, error) {
	if teamID == "" || userID == "" {
		return PolicyAuthorization{}, &Error{StatusCode: http.StatusBadRequest, Title: "AuthorizePolicyWrite: team_id and user_id are required"}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.WorkspaceMappingsName), ConsistentRead: aws.Bool(true),
		Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID)},
	})
	if err != nil {
		return PolicyAuthorization{}, ddbToError("AuthorizePolicyWrite", err)
	}
	owner := readString(out.Item, attrOwnerID)
	if owner == "" || !workspaceCallerIsAdmin(out.Item, userID) {
		return PolicyAuthorization{}, &Error{StatusCode: http.StatusForbidden, Title: "This command is admin-only"}
	}
	generation := ""
	if raw, exists := out.Item[attrPolicyGeneration]; exists {
		value, ok := raw.(*ddbtypes.AttributeValueMemberS)
		if !ok || value.Value == "" {
			return PolicyAuthorization{}, &Error{StatusCode: http.StatusInternalServerError, Title: "Workspace policy generation is invalid"}
		}
		generation = value.Value
	} else {
		// A concurrent initialization or reinstall loses this condition. Do not adopt
		// another installation by silently retrying the original operation.
		initialized, initErr := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String(s.WorkspaceMappingsName),
			Key:                       map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID)},
			UpdateExpression:          aws.String("SET #generation = :generation"),
			ConditionExpression:       aws.String(policyCallerCondition + " AND attribute_not_exists(#generation)"),
			ExpressionAttributeNames:  map[string]string{"#owner": attrOwnerID, "#admins": attrAdminSlackUserIDs, "#generation": attrPolicyGeneration},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":owner": stringAttr(owner), ":caller": stringAttr(userID), ":generation": stringAttr(rand.Text())},
			ReturnValues:              ddbtypes.ReturnValueAllNew,
		})
		if initErr != nil {
			var condition *ddbtypes.ConditionalCheckFailedException
			if errors.As(initErr, &condition) {
				return PolicyAuthorization{}, policyWorkspaceChanged()
			}
			return PolicyAuthorization{}, ddbToError("AuthorizePolicyWrite.initialize", initErr)
		}
		generation = readString(initialized.Attributes, attrPolicyGeneration)
		if generation == "" {
			return PolicyAuthorization{}, &Error{StatusCode: http.StatusInternalServerError, Title: "Workspace policy generation was not confirmed"}
		}
	}
	return PolicyAuthorization{store: s, teamID: teamID, userID: userID, ownerID: owner, generation: generation}, nil
}

type policyTransactionClient interface {
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

// updatePolicy commits the caller/installation check and policy mutation together.
func (s *Store) updatePolicy(ctx context.Context, in *dynamodb.UpdateItemInput) error {
	authorization, ok := ctx.Value(policyAuthorizationContextKey{}).(PolicyAuthorization)
	if !ok || authorization.store != s || authorization.generation == "" || in == nil ||
		aws.ToString(in.TableName) != s.ChannelPoliciesName || readString(in.Key, attrSlackTeamID) != authorization.teamID {
		return policyWorkspaceChanged()
	}
	if len(in.Expected) != 0 || len(in.AttributeUpdates) != 0 || in.ConditionalOperator != "" {
		return &Error{StatusCode: http.StatusInternalServerError, Title: "Policy transactions require expression-based updates and conditions"}
	}
	if in.ReturnValues != "" && in.ReturnValues != ddbtypes.ReturnValueNone {
		return &Error{StatusCode: http.StatusInternalServerError, Title: "Policy transactions do not return item values"}
	}
	transactional, ok := s.Client.(policyTransactionClient)
	if !ok {
		return &Error{StatusCode: http.StatusInternalServerError, Title: "Atomic policy writes are unavailable"}
	}
	_, err := transactional.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		ClientRequestToken: aws.String(rand.Text()),
		TransactItems: []ddbtypes.TransactWriteItem{
			{ConditionCheck: &ddbtypes.ConditionCheck{
				TableName:                 aws.String(s.WorkspaceMappingsName),
				Key:                       map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(authorization.teamID)},
				ConditionExpression:       aws.String(policyCallerCondition + " AND #generation = :generation"),
				ExpressionAttributeNames:  map[string]string{"#owner": attrOwnerID, "#admins": attrAdminSlackUserIDs, "#generation": attrPolicyGeneration},
				ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":owner": stringAttr(authorization.ownerID), ":caller": stringAttr(authorization.userID), ":generation": stringAttr(authorization.generation)},
			}},
			{Update: &ddbtypes.Update{
				TableName: in.TableName, Key: in.Key, UpdateExpression: in.UpdateExpression,
				ConditionExpression:      in.ConditionExpression,
				ExpressionAttributeNames: in.ExpressionAttributeNames, ExpressionAttributeValues: in.ExpressionAttributeValues,
				ReturnValuesOnConditionCheckFailure: in.ReturnValuesOnConditionCheckFailure,
			}},
		},
	})
	var canceled *ddbtypes.TransactionCanceledException
	if errors.As(err, &canceled) {
		if len(canceled.CancellationReasons) > 0 && aws.ToString(canceled.CancellationReasons[0].Code) == "ConditionalCheckFailed" {
			return policyWorkspaceChanged()
		}
		if len(canceled.CancellationReasons) > 1 && aws.ToString(canceled.CancellationReasons[1].Code) == "ConditionalCheckFailed" {
			// Existing alias callers distinguish their condition from a workspace fence.
			return &ddbtypes.ConditionalCheckFailedException{Message: aws.String("Channel policy condition changed")}
		}
	}
	return err
}
