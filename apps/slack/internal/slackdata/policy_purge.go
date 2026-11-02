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

const errCodePolicyPurgeLegacyUnknown = "policy_purge_legacy_ownership_unknown"

type policyPurgeBinding struct {
	bound        bool
	installation string
	legacy       bool
}

func (s *Store) policyPurgeBinding(ctx context.Context, teamID string) (policyPurgeBinding, error) {
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(s.WorkspaceMappingsName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID)}, ConsistentRead: aws.Bool(true)})
	if err != nil {
		return policyPurgeBinding{}, ddbToError("PurgeTeamChannelPolicies.binding", err)
	}
	binding := policyPurgeBinding{bound: len(out.Item) != 0}
	binding.installation, _, err = policyIdentity(out.Item, attrPolicyInstallation)
	if err != nil {
		return policyPurgeBinding{}, ddbToError("PurgeTeamChannelPolicies.binding", err)
	}
	if raw, present := out.Item[attrPolicyLegacy]; present {
		flag, valid := raw.(*ddbtypes.AttributeValueMemberBOOL)
		if !valid {
			return policyPurgeBinding{}, &Error{StatusCode: http.StatusServiceUnavailable, Title: "Policy cleanup migration state is invalid"}
		}
		binding.legacy = flag.Value
	}
	return binding, nil
}

func policyPurgeChanged() error {
	return &Error{StatusCode: http.StatusServiceUnavailable, Code: errCodePolicyPurgeWorkspaceChanged, Title: "Workspace changed during policy cleanup; retry"}
}

// TODO(upstream-contract): DynamoDB cancellation reasons follow item order;
// conditional deletes must preserve the workspace and policy checks in that order.
func (s *Store) deleteRetiredPolicy(ctx context.Context, teamID, channelID string, item map[string]ddbtypes.AttributeValue, binding policyPurgeBinding) error {
	installation, present, err := policyIdentity(item, attrPolicyInstallation)
	if err != nil {
		return ddbToError("PurgeTeamChannelPolicies.identity", err)
	}
	if present && installation == binding.installation {
		return nil
	}
	if !present {
		if binding.legacy {
			return nil
		}
		// A mixed-version writer may have created this row. Retain the durable job
		// for reconciliation rather than guessing ownership or deleting new data.
		if !s.legacyPolicyWritersRetired {
			return &Error{StatusCode: http.StatusServiceUnavailable, Code: errCodePolicyPurgeLegacyUnknown, Title: "Legacy policy ownership needs reconciliation"}
		}
	}
	rowCondition := "attribute_not_exists(" + exprPolicyInstallation + ")"
	var rowValues map[string]ddbtypes.AttributeValue
	if present {
		rowCondition = exprPolicyInstallation + " = " + valuePolicyInstallation
		rowValues = map[string]ddbtypes.AttributeValue{valuePolicyInstallation: stringAttr(installation)}
	}
	transactional, ok := s.Client.(policyTransactionClient)
	if !ok {
		return &Error{StatusCode: http.StatusServiceUnavailable, Title: "Atomic policy cleanup is unavailable"}
	}
	_, err = transactional.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{ClientRequestToken: aws.String(rand.Text()), TransactItems: []ddbtypes.TransactWriteItem{
		{ConditionCheck: &ddbtypes.ConditionCheck{TableName: aws.String(s.WorkspaceMappingsName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID)}, ConditionExpression: aws.String(exprPolicyInstallation + " = " + valuePolicyInstallation), ExpressionAttributeNames: map[string]string{exprPolicyInstallation: attrPolicyInstallation}, ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{valuePolicyInstallation: stringAttr(binding.installation)}}},
		{Delete: &ddbtypes.Delete{TableName: aws.String(s.ChannelPoliciesName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID), attrSlackChannelID: stringAttr(channelID)}, ConditionExpression: aws.String(rowCondition), ExpressionAttributeNames: map[string]string{exprPolicyInstallation: attrPolicyInstallation}, ExpressionAttributeValues: rowValues}},
	}})
	var canceled *ddbtypes.TransactionCanceledException
	if errors.As(err, &canceled) {
		if len(canceled.CancellationReasons) > 0 && aws.ToString(canceled.CancellationReasons[0].Code) == policyConditionFailed {
			return policyPurgeChanged()
		}
		if len(canceled.CancellationReasons) > 1 && aws.ToString(canceled.CancellationReasons[1].Code) == policyConditionFailed {
			current, readErr := s.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(s.ChannelPoliciesName), Key: map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID), attrSlackChannelID: stringAttr(channelID)}, ConsistentRead: aws.Bool(true)})
			if readErr != nil {
				return ddbToError("PurgeTeamChannelPolicies.reconcile", readErr)
			}
			if len(current.Item) == 0 || readString(current.Item, attrPolicyInstallation) == binding.installation {
				return nil
			}
			return policyPurgeChanged()
		}
	}
	return ddbToError("PurgeTeamChannelPolicies.retired", err)
}
