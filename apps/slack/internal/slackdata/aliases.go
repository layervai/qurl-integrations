package slackdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ErrAliasAlreadyBound is returned by BindChannelAlias when an alias already
// exists in the channel's alias_bindings map.
var ErrAliasAlreadyBound = errors.New("alias already bound in this channel")

// ErrAliasNotFound is returned by UnbindChannelAlias when an alias is not bound
// in the channel's alias_bindings map.
var ErrAliasNotFound = errors.New("alias not bound in this channel")

const (
	exprAliasBindings = "#ab"
	exprAliasName     = "#a"
	exprResourceID    = ":rid"
	exprEmptyMap      = ":empty"
)

// BindChannelAlias binds aliasName to resourceID on the
// (teamID, channelID) channel_policies row.
//
// The first UpdateItem lazily seeds alias_bindings as an empty map. The second
// UpdateItem writes alias_bindings.#a under an attribute_not_exists guard so
// duplicate aliases fail without overwriting an existing binding.
func (s *Store) BindChannelAlias(ctx context.Context, teamID, channelID, aliasName, resourceID string) error {
	if teamID == "" || channelID == "" || aliasName == "" || resourceID == "" {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "BindChannelAlias: team_id, channel_id, alias_name, and resource_id are required",
		}
	}
	if err := s.ensureAliasBindingsMap(ctx, teamID, channelID); err != nil {
		return fmt.Errorf("ensure alias_bindings map: %w", err)
	}

	now := s.nowOrDefault()
	nowISO := now.UTC().Format(time.RFC3339)
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
		UpdateExpression:    aws.String("SET " + exprAliasBindings + "." + exprAliasName + " = " + exprResourceID + ", " + attrUpdatedAt + " = " + exprNow + ", " + attrUpdatedAtNano + " = " + exprNowNano),
		ConditionExpression: aws.String("attribute_not_exists(" + exprAliasBindings + "." + exprAliasName + ")"),
		ExpressionAttributeNames: map[string]string{
			exprAliasBindings: attrAliasBindings,
			exprAliasName:     aliasName,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			exprResourceID: stringAttr(resourceID),
			exprNow:        stringAttr(nowISO),
			exprNowNano:    unixNanoAttr(now),
		},
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return ErrAliasAlreadyBound
		}
		return ddbToError("BindChannelAlias", err)
	}
	return nil
}

// UnbindChannelAlias removes aliasName from the (teamID, channelID)
// channel_policies row.
func (s *Store) UnbindChannelAlias(ctx context.Context, teamID, channelID, aliasName string) error {
	if teamID == "" || channelID == "" || aliasName == "" {
		return &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "UnbindChannelAlias: team_id, channel_id, and alias_name are required",
		}
	}

	now := s.nowOrDefault()
	nowISO := now.UTC().Format(time.RFC3339)
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
		UpdateExpression:    aws.String("SET " + attrUpdatedAt + " = " + exprNow + ", " + attrUpdatedAtNano + " = " + exprNowNano + " REMOVE " + exprAliasBindings + "." + exprAliasName),
		ConditionExpression: aws.String("attribute_exists(" + exprAliasBindings + "." + exprAliasName + ")"),
		ExpressionAttributeNames: map[string]string{
			exprAliasBindings: attrAliasBindings,
			exprAliasName:     aliasName,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			exprNow:     stringAttr(nowISO),
			exprNowNano: unixNanoAttr(now),
		},
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return ErrAliasNotFound
		}
		return ddbToError("UnbindChannelAlias", err)
	}
	return nil
}

func (s *Store) ensureAliasBindingsMap(ctx context.Context, teamID, channelID string) error {
	now := s.nowOrDefault()
	nowISO := now.UTC().Format(time.RFC3339)
	_, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.ChannelPoliciesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    stringAttr(teamID),
			attrSlackChannelID: stringAttr(channelID),
		},
		UpdateExpression:    aws.String("SET " + exprAliasBindings + " = " + exprEmptyMap + ", " + attrUpdatedAt + " = " + exprNow + ", " + attrUpdatedAtNano + " = " + exprNowNano),
		ConditionExpression: aws.String("attribute_not_exists(" + exprAliasBindings + ")"),
		ExpressionAttributeNames: map[string]string{
			exprAliasBindings: attrAliasBindings,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			exprEmptyMap: &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{}},
			exprNow:      stringAttr(nowISO),
			exprNowNano:  unixNanoAttr(now),
		},
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return nil
		}
		return ddbToError("ensureAliasBindingsMap", err)
	}
	return nil
}
