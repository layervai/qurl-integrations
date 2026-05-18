// Package aliasstore is the DynamoDB-backed implementation of
// internal.AliasStore that backs the /qurl setalias and
// /qurl unsetalias verbs (#347). It writes the per-channel
// `alias_bindings` Map attribute on the `channel_policies` table
// via two atomic UpdateItem calls (one to lazy-init the Map, one
// to add/remove the alias key under a conditional expression).
//
// This package exists because #347's AliasStore interface landed on
// main before the larger `slackdata` package (#231/#233) that would
// otherwise hold the implementation. Once #231 merges and the
// slackdata Store has BindChannelAlias/UnbindChannelAlias methods,
// fold this package into slackdata and delete it; the contract here
// matches the interface in apps/slack/internal/handler_alias.go so
// the migration is mechanical (move two methods + delete package +
// update one import).
//
// Schema: the row is keyed on (slack_team_id, slack_channel_id);
// aliases live as an app-managed Map attribute `alias_bindings`
// with alias name as key and resource id as value. Many aliases
// coexist on one channel (each is a Map entry). Coexists with the
// legacy `allowed_resource_ids` String Set used by /qurl admin
// allow/disallow (orthogonal surface, untouched here).
//
// Why two UpdateItem calls per Bind: DynamoDB rejects an
// UpdateExpression that references both a Map and a sub-path of that
// Map ("overlapping paths"). The first call seeds an empty Map only
// when absent (ConditionalCheckFailed on the seed is the "already
// there" success case and is swallowed). The second call performs the
// real, race-sensitive write — `SET alias_bindings.#a = :rid`
// conditional on `attribute_not_exists(alias_bindings.#a)`. Concurrent
// binds of different alias names succeed; concurrent binds of the same
// alias name collapse to one success + one ErrAliasAlreadyBound. The
// seed call is the steady-state cost (one extra round-trip per Bind),
// not a one-time setup — the alternatives (try-write-first or a
// GetItem cache) trade clean CCF semantics or write-races for the
// extra call, neither worth it for an admin-volume verb.
//
// Partial-failure footprint: if the seed succeeds but the write fails
// with a non-CCF error (throttling, network), the empty
// `alias_bindings` map is left on the row. Harmless — the next Bind
// observes it as already-seeded and the seed becomes a no-op. Within
// a single Bind, readers see no partial state; across attempts, an
// empty Map can briefly persist.
//
// Retry semantics: if a Bind write succeeds but the response is lost
// (network drop), the caller's retry surfaces as ErrAliasAlreadyBound
// (→ 409 user copy). That's the correct posture for admin verbs —
// "you already did this, don't worry about the duplicate" — and the
// idempotency cost is zero because the Map entry is set to the same
// resource_id.
//
// Alias-name validation is the caller's responsibility — this
// package writes whatever string it receives as a DDB Map key.
// `handler_alias.go::aliasCharsetPattern` is the upstream chokepoint
// for /qurl setalias; a future direct caller (CLI, migration script)
// MUST pre-validate before calling BindChannelAlias.
package aliasstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
)

// EnvChannelPoliciesTable is the operator-set env var that names the
// DDB table. Matches the name slackdata uses on the #231 branch so
// the eventual merge is wire-compatible — no operator action needed
// beyond what qurl-bot-slack/terraform/main.tf already sets.
const EnvChannelPoliciesTable = "QURL_CHANNEL_POLICIES_TABLE"

// DDB attribute and expression-placeholder names. Mirrored on the
// qurl-bot-slack-infra side in modules/qurl-slack-ddb's TF table
// schema; changing these here requires a coordinated TF change.
const (
	attrSlackTeamID    = "slack_team_id"
	attrSlackChannelID = "slack_channel_id"
	attrAliasBindings  = "alias_bindings"

	exprAliasBindings = "#ab"
	exprAliasName     = "#a"
	exprResourceID    = ":rid"
	exprEmptyMap      = ":empty"
)

// DynamoDBClient is the narrow surface the Store uses. Matched against
// the live *dynamodb.Client; tests inject a fake. Kept here rather
// than as a package-public type so a future slackdata fold can drop
// it without breaking external callers.
type DynamoDBClient interface {
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// Store is the DDB-backed AliasStore. Zero value is not usable; use
// New.
type Store struct {
	client    DynamoDBClient
	tableName string
}

// New constructs a Store against the named DDB table from the
// ambient AWS config. Caller owns the env-var read — keeping it in
// cmd/main.go next to its sibling wirings (OAuth, DDB provider) means
// this package stays decoupled from the deployment shape, and the
// slackdata fold doesn't need a config seam.
func New(ctx context.Context, tableName string) (*Store, error) {
	if tableName == "" {
		return nil, errors.New("aliasstore: tableName is required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return &Store{
		client:    dynamodb.NewFromConfig(cfg),
		tableName: tableName,
	}, nil
}

// Static assertion: Store satisfies internal.AliasStore. Catches an
// interface drift on either side at compile time rather than at first
// slash-command invocation.
var _ internal.AliasStore = (*Store)(nil)

// BindChannelAlias binds aliasName→resourceID on the
// (teamID, channelID) row. Implements internal.AliasStore.
//
// Two-step write: lazy-init the Map if absent, then add the alias
// entry under a conditional that fences duplicates. See package doc
// for the rationale on splitting these.
func (s *Store) BindChannelAlias(ctx context.Context, teamID, channelID, aliasName, resourceID string) error {
	if err := s.ensureAliasBindingsMap(ctx, teamID, channelID); err != nil {
		return fmt.Errorf("ensure alias_bindings map: %w", err)
	}

	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    &ddbtypes.AttributeValueMemberS{Value: teamID},
			attrSlackChannelID: &ddbtypes.AttributeValueMemberS{Value: channelID},
		},
		UpdateExpression:    aws.String("SET " + exprAliasBindings + "." + exprAliasName + " = " + exprResourceID),
		ConditionExpression: aws.String("attribute_not_exists(" + exprAliasBindings + "." + exprAliasName + ")"),
		ExpressionAttributeNames: map[string]string{
			exprAliasBindings: attrAliasBindings,
			exprAliasName:     aliasName,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			exprResourceID: &ddbtypes.AttributeValueMemberS{Value: resourceID},
		},
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return internal.ErrAliasAlreadyBound
		}
		return fmt.Errorf("bind alias: %w", err)
	}
	return nil
}

// UnbindChannelAlias removes aliasName from the (teamID, channelID)
// row. Implements internal.AliasStore. ErrAliasNotFound when the
// alias (or the alias_bindings Map itself) is absent — both collapse
// to the same ConditionalCheckFailedException via the
// `attribute_exists(alias_bindings.#a)` guard.
func (s *Store) UnbindChannelAlias(ctx context.Context, teamID, channelID, aliasName string) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    &ddbtypes.AttributeValueMemberS{Value: teamID},
			attrSlackChannelID: &ddbtypes.AttributeValueMemberS{Value: channelID},
		},
		UpdateExpression:    aws.String("REMOVE " + exprAliasBindings + "." + exprAliasName),
		ConditionExpression: aws.String("attribute_exists(" + exprAliasBindings + "." + exprAliasName + ")"),
		ExpressionAttributeNames: map[string]string{
			exprAliasBindings: attrAliasBindings,
			exprAliasName:     aliasName,
		},
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return internal.ErrAliasNotFound
		}
		return fmt.Errorf("unbind alias: %w", err)
	}
	return nil
}

// ensureAliasBindingsMap is the lazy-init step of BindChannelAlias.
// SET-ifs the attribute to an empty Map only when it doesn't already
// exist; the CCF on the "already there" branch is the success case
// and is swallowed. The row itself is upserted by the same call
// (UpdateItem creates the item on first write), so a brand-new
// channel goes from no row → row with empty map → row with one alias
// in two atomic round-trips with no partial visible state for any
// reader (readers see either no row, the empty Map, or the Map with
// the alias).
func (s *Store) ensureAliasBindingsMap(ctx context.Context, teamID, channelID string) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    &ddbtypes.AttributeValueMemberS{Value: teamID},
			attrSlackChannelID: &ddbtypes.AttributeValueMemberS{Value: channelID},
		},
		UpdateExpression:    aws.String("SET " + exprAliasBindings + " = " + exprEmptyMap),
		ConditionExpression: aws.String("attribute_not_exists(" + exprAliasBindings + ")"),
		ExpressionAttributeNames: map[string]string{
			exprAliasBindings: attrAliasBindings,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			exprEmptyMap: &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{}},
		},
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return nil
		}
		return err
	}
	return nil
}
