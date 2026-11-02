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

const errCodePolicyPurgeWorkspaceChanged = "policy_purge_workspace_changed"

const policyConditionFailed = "ConditionalCheckFailed"

// PolicyAuthorization binds one operation to its installation and caller.
// Only AuthorizePolicyWrite can populate it; no user-supplied generation is trusted.
type PolicyAuthorization struct {
	store          *Store
	teamID         string
	userID         string
	ownerID        string
	generation     string
	installation   string
	legacyPolicies bool
}

type policyAuthorizationContextKey struct{}

// Context carries the authorization through an independently bounded async worker.
func (a *PolicyAuthorization) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, policyAuthorizationContextKey{}, *a)
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
	generation, generationPresent, err := policyIdentity(out.Item, attrPolicyGeneration)
	if err != nil {
		return PolicyAuthorization{}, err
	}
	installation, installationPresent, err := policyIdentity(out.Item, attrPolicyInstallation)
	if err != nil {
		return PolicyAuthorization{}, err
	}
	if generationPresent != installationPresent {
		return PolicyAuthorization{}, &Error{StatusCode: http.StatusInternalServerError, Title: "Workspace policy identity is incomplete"}
	}
	legacyPolicies := false
	if raw, present := out.Item[attrPolicyLegacy]; present {
		flag, valid := raw.(*ddbtypes.AttributeValueMemberBOOL)
		if !valid {
			return PolicyAuthorization{}, &Error{StatusCode: http.StatusInternalServerError, Title: "Workspace policy migration state is invalid"}
		}
		legacyPolicies = flag.Value
	}
	if !generationPresent {
		// Initialize legacy lineage once. Do not move the lifecycle purge cutoff
		// or silently adopt an installation that changed after the strong read.
		initialized, initErr := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String(s.WorkspaceMappingsName),
			Key:                       map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID)},
			UpdateExpression:          aws.String("SET #generation = :generation, #installation = :installation, #legacy = :legacy"),
			ConditionExpression:       aws.String(policyCallerCondition + " AND attribute_not_exists(#generation) AND attribute_not_exists(#installation)"),
			ExpressionAttributeNames:  map[string]string{"#owner": attrOwnerID, "#admins": attrAdminSlackUserIDs, "#generation": attrPolicyGeneration, "#installation": attrPolicyInstallation, "#legacy": attrPolicyLegacy},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":owner": stringAttr(owner), ":caller": stringAttr(userID), ":generation": stringAttr(rand.Text()), ":installation": stringAttr(rand.Text()), ":legacy": &ddbtypes.AttributeValueMemberBOOL{Value: true}},
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
		installation = readString(initialized.Attributes, attrPolicyInstallation)
		legacy, valid := initialized.Attributes[attrPolicyLegacy].(*ddbtypes.AttributeValueMemberBOOL)
		if generation == "" || installation == "" || !valid || !legacy.Value {
			return PolicyAuthorization{}, &Error{StatusCode: http.StatusInternalServerError, Title: "Workspace policy identity was not confirmed"}
		}
		legacyPolicies = true
	}
	return PolicyAuthorization{store: s, teamID: teamID, userID: userID, ownerID: owner, generation: generation, installation: installation, legacyPolicies: legacyPolicies}, nil
}

func policyIdentity(item map[string]ddbtypes.AttributeValue, name string) (identity string, present bool, err error) {
	raw, present := item[name]
	if !present {
		return "", false, nil
	}
	value, valid := raw.(*ddbtypes.AttributeValueMemberS)
	if !valid || value.Value == "" {
		return "", true, &Error{StatusCode: http.StatusInternalServerError, Title: "Workspace policy identity is invalid"}
	}
	return value.Value, true, nil
}

type policyTransactionClient interface {
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

// updatePolicy commits the caller/installation check and policy mutation together.
func (s *Store) updatePolicy(ctx context.Context, in *dynamodb.UpdateItemInput) error {
	authorization, ok := ctx.Value(policyAuthorizationContextKey{}).(PolicyAuthorization)
	if !ok || authorization.store != s || authorization.generation == "" || authorization.installation == "" || in == nil ||
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
				ConditionExpression:       aws.String(policyCallerCondition + " AND #generation = :generation AND #installation = :installation"),
				ExpressionAttributeNames:  map[string]string{"#owner": attrOwnerID, "#admins": attrAdminSlackUserIDs, "#generation": attrPolicyGeneration, "#installation": attrPolicyInstallation},
				ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":owner": stringAttr(authorization.ownerID), ":caller": stringAttr(authorization.userID), ":generation": stringAttr(authorization.generation), ":installation": stringAttr(authorization.installation)},
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
		if len(canceled.CancellationReasons) > 0 && aws.ToString(canceled.CancellationReasons[0].Code) == policyConditionFailed {
			return policyWorkspaceChanged()
		}
		if len(canceled.CancellationReasons) > 1 && aws.ToString(canceled.CancellationReasons[1].Code) == policyConditionFailed {
			// Existing alias callers distinguish their condition from a workspace fence.
			return &ddbtypes.ConditionalCheckFailedException{Message: aws.String("Channel policy condition changed")}
		}
	}
	return err
}

// deleteUnboundPolicy removes late old writes regardless of their wall-clock
// stamp. The binding check must commit with the delete, so reinstall wins safely.
func (s *Store) deleteUnboundPolicy(ctx context.Context, teamID, channelID string) error {
	transactional, ok := s.Client.(policyTransactionClient)
	if !ok {
		return &Error{StatusCode: http.StatusServiceUnavailable, Title: "Atomic policy cleanup is unavailable"}
	}
	_, err := transactional.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		ClientRequestToken: aws.String(rand.Text()),
		TransactItems: []ddbtypes.TransactWriteItem{
			{ConditionCheck: &ddbtypes.ConditionCheck{TableName: aws.String(s.WorkspaceMappingsName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID)}, ConditionExpression: aws.String("attribute_not_exists(#team)"), ExpressionAttributeNames: map[string]string{"#team": attrSlackTeamID}}},
			{Delete: &ddbtypes.Delete{TableName: aws.String(s.ChannelPoliciesName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID), attrSlackChannelID: stringAttr(channelID)}}},
		},
	})
	var canceled *ddbtypes.TransactionCanceledException
	if errors.As(err, &canceled) && len(canceled.CancellationReasons) > 0 && aws.ToString(canceled.CancellationReasons[0].Code) == policyConditionFailed {
		return &Error{StatusCode: http.StatusServiceUnavailable, Code: errCodePolicyPurgeWorkspaceChanged, Title: "Workspace changed during policy cleanup; retry"}
	}
	return ddbToError("PurgeTeamChannelPolicies.unbound", err)
}
