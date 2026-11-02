package slackdata

import (
	"context"
	"maps"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type policyReadTransactionClient interface {
	TransactGetItems(context.Context, *dynamodb.TransactGetItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactGetItemsOutput, error)
}

// Read binding and policy in one snapshot so retired grants cannot authorize a
// new installation. There is no cache or separate workspace-read round trip.
func (s *Store) readChannelPolicy(ctx context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
	transactional, ok := s.Client.(policyReadTransactionClient)
	if !ok {
		return nil, &Error{StatusCode: http.StatusServiceUnavailable, Title: "Atomic policy reads are unavailable"}
	}
	teamID := readString(in.Key, attrSlackTeamID)
	names := maps.Clone(in.ExpressionAttributeNames)
	projection := in.ProjectionExpression
	if aws.ToString(projection) != "" {
		if names == nil {
			names = make(map[string]string)
		}
		const lineageName = "#read_policy_installation"
		if _, present := names[lineageName]; present {
			return nil, policyWorkspaceChanged()
		}
		names[lineageName] = attrPolicyInstallation
		projection = aws.String(aws.ToString(projection) + ", " + lineageName)
	}
	bindingProjection := "#owner, #generation, #installation, #legacy"
	bindingNames := map[string]string{
		exprPolicyOwner: attrOwnerID, exprPolicyGeneration: attrPolicyGeneration,
		"#installation": attrPolicyInstallation, exprPolicyLegacy: attrPolicyLegacy,
	}
	if ctx.Value(policyAuthorizationContextKey{}) != nil {
		bindingProjection += ", #admins"
		bindingNames[exprPolicyAdmins] = attrAdminSlackUserIDs
	}
	out, err := transactional.TransactGetItems(ctx, &dynamodb.TransactGetItemsInput{
		TransactItems: []ddbtypes.TransactGetItem{
			{Get: &ddbtypes.Get{
				TableName:                aws.String(s.WorkspaceMappingsName),
				Key:                      map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(teamID)},
				ProjectionExpression:     aws.String(bindingProjection),
				ExpressionAttributeNames: bindingNames,
			}},
			{Get: &ddbtypes.Get{
				TableName: in.TableName, Key: in.Key,
				ProjectionExpression: projection, ExpressionAttributeNames: names,
			}},
		},
	})
	if err != nil {
		return nil, ddbToError("ReadChannelPolicy", err)
	}
	if out == nil || len(out.Responses) != 2 {
		return nil, &Error{StatusCode: http.StatusServiceUnavailable, Title: "Atomic policy read returned an incomplete snapshot"}
	}
	binding, policy := out.Responses[0].Item, out.Responses[1].Item
	if raw := ctx.Value(policyAuthorizationContextKey{}); raw != nil {
		authorization, valid := raw.(PolicyAuthorization)
		if !valid || authorization.store != s || authorization.teamID != teamID || !matchesPolicyAuthorization(binding, &authorization) {
			return nil, policyWorkspaceChanged()
		}
	}
	allowed, err := policyReadBelongsTo(binding, policy)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return &dynamodb.GetItemOutput{}, nil
	}
	return &dynamodb.GetItemOutput{Item: policy}, nil
}

func policyReadBelongsTo(binding, policy map[string]ddbtypes.AttributeValue) (bool, error) {
	if len(policy) == 0 || readString(binding, attrOwnerID) == "" {
		return false, nil
	}
	_, generationPresent, err := policyIdentity(binding, attrPolicyGeneration)
	if err != nil {
		return false, err
	}
	installation, installationPresent, err := policyIdentity(binding, attrPolicyInstallation)
	if err != nil {
		return false, err
	}
	if generationPresent != installationPresent {
		return false, &Error{StatusCode: http.StatusServiceUnavailable, Title: "Workspace policy identity is incomplete"}
	}
	legacy := !installationPresent
	if raw, present := binding[attrPolicyLegacy]; present {
		flag, valid := raw.(*ddbtypes.AttributeValueMemberBOOL)
		if !valid {
			return false, &Error{StatusCode: http.StatusServiceUnavailable, Title: "Workspace policy migration state is invalid"}
		}
		legacy = legacy || flag.Value
	}
	policyInstallation, present, err := policyIdentity(policy, attrPolicyInstallation)
	if err != nil {
		return false, err
	}
	if (present && (!installationPresent || policyInstallation != installation)) || (!present && !legacy) {
		return false, nil
	}
	return true, nil
}
