package oauth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	workspaceStatePKAttr = "team_id"

	oauthStateKeyPrefix     = "oauth_state#"
	oauthStateAttrItemType  = "item_type"
	oauthStateItemType      = "oauth_state"
	oauthStateAttrTeamID    = "oauth_team_id"
	oauthStateAttrUserID    = "oauth_user_id"
	oauthStateAttrNonce     = "oauth_nonce"
	oauthStateAttrVerifier  = "oauth_code_verifier"
	oauthStateAttrEmail     = "oauth_email"
	oauthStateAttrMode      = "oauth_mode"
	oauthStateAttrCreatedAt = "oauth_created_at"
	oauthStateAttrExpiresAt = "oauth_expires_at"
	oauthStateAttrStartedAt = "oauth_started_at"
	oauthStateAttrTTL       = "ttl"
)

// DDBStateStore stores short-lived OAuth state in the existing workspace_state
// table under reserved oauth_state# keys. It does not use the workspace API-key
// cache/encryptor path: these rows are not workspace credentials, carry a
// 1-hour TTL, and are deleted atomically on callback.
// TODO(upstream-contract): qurl-integrations-infra#1286 enables the table's
// native TTL on the numeric `ttl` attribute so abandoned rows are reaped. That
// attribute is reserved for short-lived oauth_state rows; durable workspace
// credential rows must never write it. Future scans, exports, or GSIs over this
// shared table must filter item_type so ephemeral rows are not treated as
// workspace credentials. State rows intentionally bypass credential encryption:
// the normalized email is needed for callback binding, nonce/verifier are
// short-lived protocol values, the rows retain the table's existing IAM access
// controls, and this store never logs row contents. None of these values can
// authorize an exchange without the browser-delivered Auth0 code and
// confidential-client key.
type DDBStateStore struct {
	Client    auth.DynamoDBClient
	TableName string
}

// NewDDBStateStore returns a StateStore backed by the same workspace_state table
// and DynamoDB client as the workspace API-key provider. Invalid wiring fails at
// process startup instead of turning every callback into a request-time 503.
func NewDDBStateStore(provider *auth.DDBProvider) (*DDBStateStore, error) {
	if provider == nil || provider.Client == nil || provider.TableName == "" {
		return nil, errors.New("oauth state store requires a configured DDB provider")
	}
	return &DDBStateStore{Client: provider.Client, TableName: provider.TableName}, nil
}

func oauthStateKey(handle string) string {
	return oauthStateKeyPrefix + handle
}

func (s *DDBStateStore) validate() error {
	if s == nil || s.Client == nil || s.TableName == "" {
		return errors.New("oauth state store is not configured")
	}
	return nil
}

// PutState stores a fresh opaque OAuth state row. The row is conditional so an
// impossible random-handle collision fails closed instead of overwriting.
func (s *DDBStateStore) PutState(ctx context.Context, handle string, state StoredState) error { //nolint:gocritic // StateStore value signature keeps callers immutable and simple.
	if err := s.validate(); err != nil {
		return err
	}
	if handle == "" {
		return errStateMalformed
	}
	if state.TeamID == "" || state.UserID == "" || state.Nonce == "" || state.CodeVerifier == "" || state.ExpiresAt.IsZero() {
		return errStateMalformed
	}
	_, err := s.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.TableName),
		Item: map[string]ddbtypes.AttributeValue{
			workspaceStatePKAttr:    &ddbtypes.AttributeValueMemberS{Value: oauthStateKey(handle)},
			oauthStateAttrItemType:  &ddbtypes.AttributeValueMemberS{Value: oauthStateItemType},
			oauthStateAttrTeamID:    &ddbtypes.AttributeValueMemberS{Value: state.TeamID},
			oauthStateAttrUserID:    &ddbtypes.AttributeValueMemberS{Value: state.UserID},
			oauthStateAttrNonce:     &ddbtypes.AttributeValueMemberS{Value: state.Nonce},
			oauthStateAttrVerifier:  &ddbtypes.AttributeValueMemberS{Value: state.CodeVerifier},
			oauthStateAttrEmail:     &ddbtypes.AttributeValueMemberS{Value: state.Email},
			oauthStateAttrMode:      &ddbtypes.AttributeValueMemberS{Value: string(state.Mode)},
			oauthStateAttrCreatedAt: &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(state.CreatedAt.Unix(), 10)},
			oauthStateAttrExpiresAt: &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(state.ExpiresAt.Unix(), 10)},
			oauthStateAttrTTL:       &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(state.ExpiresAt.Unix(), 10)},
		},
		ConditionExpression: aws.String("attribute_not_exists(#pk)"),
		ExpressionAttributeNames: map[string]string{
			"#pk": workspaceStatePKAttr,
		},
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return errStateCollision
		}
		return fmt.Errorf("oauth state store PutState: %w", err)
	}
	return nil
}

// StartState marks an opaque state as started and returns the backend payload
// needed to build the Auth0 authorization URL.
func (s *DDBStateStore) StartState(ctx context.Context, handle string, now time.Time) (VerifiedState, error) {
	return s.updateAndReadState(ctx, handle, now, oauthStateAttrStartedAt,
		"attribute_exists(#pk) AND #expires_at > :now_epoch",
		ddbtypes.ReturnValueAllNew)
}

// ConsumeState atomically deletes an opaque state and returns the prior payload,
// including the PKCE verifier needed for Auth0 token exchange.
func (s *DDBStateStore) ConsumeState(ctx context.Context, handle string, now time.Time) (VerifiedState, error) {
	if err := s.validate(); err != nil {
		return VerifiedState{}, err
	}
	if handle == "" {
		return VerifiedState{}, errStateMalformed
	}
	out, err := s.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			workspaceStatePKAttr: &ddbtypes.AttributeValueMemberS{Value: oauthStateKey(handle)},
		},
		ConditionExpression: aws.String(
			"attribute_exists(#pk) AND attribute_exists(#started_at) AND #expires_at > :now_epoch",
		),
		ExpressionAttributeNames: map[string]string{
			"#pk":         workspaceStatePKAttr,
			"#started_at": oauthStateAttrStartedAt,
			"#expires_at": oauthStateAttrExpiresAt,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":now_epoch": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now.Unix(), 10)},
		},
		ReturnValues:                        ddbtypes.ReturnValueAllOld,
		ReturnValuesOnConditionCheckFailure: ddbtypes.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			if len(ccfe.Item) == 0 {
				return VerifiedState{}, errStateMissing
			}
			if _, started := ccfe.Item[oauthStateAttrStartedAt]; !started {
				return VerifiedState{}, errStateNotStarted
			}
			return VerifiedState{}, errStateExpired
		}
		return VerifiedState{}, fmt.Errorf("oauth state store delete: %w", err)
	}
	return verifiedStateFromDDBItem(out.Attributes)
}

func (s *DDBStateStore) updateAndReadState(ctx context.Context, handle string, now time.Time, markAttr, condition string, returnValues ddbtypes.ReturnValue) (VerifiedState, error) {
	if err := s.validate(); err != nil {
		return VerifiedState{}, err
	}
	if handle == "" {
		return VerifiedState{}, errStateMalformed
	}
	names := map[string]string{
		"#pk":         workspaceStatePKAttr,
		"#mark":       markAttr,
		"#expires_at": oauthStateAttrExpiresAt,
	}
	out, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			workspaceStatePKAttr: &ddbtypes.AttributeValueMemberS{Value: oauthStateKey(handle)},
		},
		UpdateExpression:         aws.String("SET #mark = if_not_exists(#mark, :now_epoch)"),
		ConditionExpression:      aws.String(condition),
		ExpressionAttributeNames: names,
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":now_epoch": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now.Unix(), 10)},
		},
		ReturnValues:                        returnValues,
		ReturnValuesOnConditionCheckFailure: ddbtypes.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			if len(ccfe.Item) == 0 {
				return VerifiedState{}, errStateMissing
			}
			return VerifiedState{}, errStateExpired
		}
		return VerifiedState{}, fmt.Errorf("oauth state store update: %w", err)
	}
	return verifiedStateFromDDBItem(out.Attributes)
}

func verifiedStateFromDDBItem(item map[string]ddbtypes.AttributeValue) (VerifiedState, error) {
	if item == nil {
		return VerifiedState{}, errStateMalformed
	}
	mode, err := normalizeSetupMode(SetupMode(readDDBString(item, oauthStateAttrMode)))
	if err != nil {
		return VerifiedState{}, errStateMalformed
	}
	v := VerifiedState{
		TeamID:       readDDBString(item, oauthStateAttrTeamID),
		UserID:       readDDBString(item, oauthStateAttrUserID),
		Nonce:        readDDBString(item, oauthStateAttrNonce),
		CodeVerifier: readDDBString(item, oauthStateAttrVerifier),
		Email:        readDDBString(item, oauthStateAttrEmail),
		Mode:         mode,
	}
	if v.TeamID == "" || v.UserID == "" || v.Nonce == "" || !validPKCEVerifier(v.CodeVerifier) {
		return VerifiedState{}, errStateMalformed
	}
	if v.Mode.Explicit() && v.Email == "" {
		return VerifiedState{}, errStateMalformed
	}
	if v.Email != "" && !stateEmailNormalized(v.Email) {
		return VerifiedState{}, errStateMalformed
	}
	return v, nil
}

func readDDBString(item map[string]ddbtypes.AttributeValue, key string) string {
	if v, ok := item[key].(*ddbtypes.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}
