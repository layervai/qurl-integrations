// Package slackdata is the DDB-direct replacement for the old
// admin_client.go HTTP wrapper around qurl-service `/internal/v1/admin/*`.
//
// Justin's 2026-05-12 pivot (see SLACK_QURL_ROLLOUT.md and
// qurl-integrations-infra #523) moved the three Slack-keyed DynamoDB
// tables — workspace_mappings, channel_policies, bootstrap_codes —
// out of qurl-service into qurl-bot-slack-owned terraform
// (`modules/qurl-slack-ddb/`). The bot now reads/writes those tables
// directly with in-account IAM. There is no longer any HTTP surface
// in qurl-service for admin/policy state; calling
// `/internal/v1/admin/*` returns 404.
//
// This package exposes a `Store` facade with the same method shapes
// the old AdminClient had (CheckAdmin, ResolvePolicy, AllowResource,
// DisallowResource, ListPolicies, GetWorkspaceConfig, RedeemBootstrap)
// so the slash-command handlers in apps/slack/internal change as
// little as possible — same call sites, same error-shape contract
// (`*Error` with a StatusCode that handlers branch on via errors.As).
//
// Three env vars wire the tables on Fargate (set by
// qurl-bot-slack/terraform via modules/qurl-slack-ddb's outputs):
//
//   - QURL_WORKSPACE_MAPPINGS_TABLE
//   - QURL_CHANNEL_POLICIES_TABLE
//   - QURL_BOOTSTRAP_CODES_TABLE
//
// Encryption note: the three tables use customer-managed SSE on the
// qurl-bot-slack CMK. The task role's IAM grant is `kms:ViaService =
// dynamodb.<region>` so the bot never sees the data key directly;
// SSE is transparent at the SDK layer. (Field-level envelope
// encryption like shared/auth.DDBProvider does for `qurl_api_key`
// is NOT used here — workspace identity + admin user IDs + alias
// mappings are not customer-secret-grade payloads, and the per-row
// re-encrypt would burn KMS quota for no posture gain. The
// bootstrap_codes table stores only sha256(plaintext) so the
// plaintext never lives at rest.)
package slackdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Env var names — operator-set via the Fargate task definition (the
// qurl-bot-slack TF wires these from modules/qurl-slack-ddb's
// `output.table_names`). The three tables are named with the env-
// scoped prefix `qurl-bot-slack-<env>-<table>` so a misrouted call
// hits NoSuchTable instead of the wrong env's data — see the
// "blast-radius control" rationale in modules/qurl-slack-ddb/main.tf.
const (
	// EnvWorkspaceMappingsTable holds the DDB table name for the
	// 1:1 Slack team_id → qurl owner mapping table.
	EnvWorkspaceMappingsTable = "QURL_WORKSPACE_MAPPINGS_TABLE"
	// EnvChannelPoliciesTable holds the DDB table name for the per-
	// channel allowed-resource-id policy table.
	EnvChannelPoliciesTable = "QURL_CHANNEL_POLICIES_TABLE"
	// EnvBootstrapCodesTable holds the DDB table name for the
	// single-use bootstrap-code table.
	EnvBootstrapCodesTable = "QURL_BOOTSTRAP_CODES_TABLE"
)

// DynamoDBClient is the slice of *dynamodb.Client the Store uses.
// Exposed as an interface so tests can inject a fake without
// spinning up localstack. Mirrors the shape in
// shared/auth/ddb_provider.go (DynamoDBClient there).
type DynamoDBClient interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// Store is the DDB-direct replacement for the old `AdminClient`. It
// owns three DDB tables (workspace, policies, bootstrap codes) and
// surfaces the same method set the old HTTP client had so the
// slash-command handlers don't need restructuring.
//
// The zero value is NOT usable — construct via [NewStore] or set
// every field explicitly in tests.
type Store struct {
	Client                DynamoDBClient
	WorkspaceMappingsName string
	ChannelPoliciesName   string
	BootstrapCodesName    string

	// Now is injected so tests can pin the clock for created_at /
	// updated_at / expires_at-vs-now assertions without poking a
	// package-global. Defaults to time.Now.
	Now func() time.Time

	// ExternalIdentityBindings is the optional client for the
	// new `POST /v1/external-identity-bindings` qurl-service surface
	// (see SLACK_QURL_ROLLOUT.md Appendix A). The endpoint doesn't
	// exist yet — the redeem path leaves this nil and skips the
	// HTTP call until the qurl-service PR lands. When non-nil,
	// RedeemBootstrap calls into it after the conditional UpdateItem
	// on bootstrap_codes succeeds.
	ExternalIdentityBindings ExternalIdentityBindingsClient
}

// StoreOption configures [NewStore].
type StoreOption func(*storeOptions)

type storeOptions struct {
	workspaceMappingsName    string
	channelPoliciesName      string
	bootstrapCodesName       string
	ddbClient                DynamoDBClient
	externalIdentityBindings ExternalIdentityBindingsClient
	awsConfigFns             []func(*awsconfig.LoadOptions) error
}

// WithDynamoDBClient injects a DDB client. Primarily for tests; in
// production [NewStore] loads aws config and instantiates one.
func WithDynamoDBClient(c DynamoDBClient) StoreOption {
	return func(o *storeOptions) { o.ddbClient = c }
}

// WithTableNames overrides the per-table names. Any empty argument
// falls back to the matching env var. Primarily for tests.
func WithTableNames(workspaceMappings, channelPolicies, bootstrapCodes string) StoreOption {
	return func(o *storeOptions) {
		o.workspaceMappingsName = workspaceMappings
		o.channelPoliciesName = channelPolicies
		o.bootstrapCodesName = bootstrapCodes
	}
}

// WithExternalIdentityBindings injects the qurl-service binding
// client for the redeem flow. Optional — leave nil until the
// `/v1/external-identity-bindings` endpoint ships (see Appendix A
// of SLACK_QURL_ROLLOUT.md).
func WithExternalIdentityBindings(c ExternalIdentityBindingsClient) StoreOption {
	return func(o *storeOptions) { o.externalIdentityBindings = c }
}

// NewStore constructs a [Store], loading AWS config from the ambient
// environment unless overridden via options. Returns an error if any
// of the three table names is missing — there is no sane fallback for
// "which env's data am I supposed to write to" so failing fast at
// boot is the only safe move.
func NewStore(ctx context.Context, opts ...StoreOption) (*Store, error) {
	o := &storeOptions{}
	for _, fn := range opts {
		fn(o)
	}

	if o.workspaceMappingsName == "" {
		o.workspaceMappingsName = os.Getenv(EnvWorkspaceMappingsTable)
	}
	if o.channelPoliciesName == "" {
		o.channelPoliciesName = os.Getenv(EnvChannelPoliciesTable)
	}
	if o.bootstrapCodesName == "" {
		o.bootstrapCodesName = os.Getenv(EnvBootstrapCodesTable)
	}

	switch {
	case o.workspaceMappingsName == "":
		return nil, fmt.Errorf("slackdata.NewStore: %s is required", EnvWorkspaceMappingsTable)
	case o.channelPoliciesName == "":
		return nil, fmt.Errorf("slackdata.NewStore: %s is required", EnvChannelPoliciesTable)
	case o.bootstrapCodesName == "":
		return nil, fmt.Errorf("slackdata.NewStore: %s is required", EnvBootstrapCodesTable)
	}

	if o.ddbClient == nil {
		cfg, err := awsconfig.LoadDefaultConfig(ctx, o.awsConfigFns...)
		if err != nil {
			return nil, fmt.Errorf("slackdata.NewStore: load AWS config: %w", err)
		}
		o.ddbClient = dynamodb.NewFromConfig(cfg)
	}

	return &Store{
		Client:                   o.ddbClient,
		WorkspaceMappingsName:    o.workspaceMappingsName,
		ChannelPoliciesName:      o.channelPoliciesName,
		BootstrapCodesName:       o.bootstrapCodesName,
		Now:                      time.Now,
		ExternalIdentityBindings: o.externalIdentityBindings,
	}, nil
}

// nowOrDefault guards against a bare `&Store{}` that didn't set
// Now — [NewStore] always sets it, but the fallback is cheap
// insurance against a future caller that constructs the struct
// directly.
func (s *Store) nowOrDefault() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Error mirrors the StatusCode/Code/Title/Detail shape the old
// `*AdminError` had so handlers that do
// `errors.As(err, &slackdata.Error) && e.StatusCode == 404` keep
// working unchanged after the swap. The StatusCode field is the
// load-bearing branch discriminator across the handler files —
// each DDB-direct op below maps native ddb errors to the
// equivalent HTTP-shaped status (404 for ConditionalCheckFailed
// on a "not-found"-style condition, 409 for "already exists",
// 410 for "expired/used bootstrap code", 503 for transport).
type Error struct {
	StatusCode int
	Code       string
	Title      string
	Detail     string
}

// Error returns a human-readable message. Mirrors the old
// AdminError.Error() format so log/grep across the cutover sees
// identical message shapes.
func (e *Error) Error() string {
	codeSuffix := ""
	if e.Code != "" {
		codeSuffix = " [" + e.Code + "]"
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s%s (%d): %s", e.Title, codeSuffix, e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("%s%s (%d)", e.Title, codeSuffix, e.StatusCode)
}

// PolicyEntry is one row of the channel_policies-by-team query.
// JSON tags retained from the old wire shape so any caller that
// happens to marshal one (e.g. for slog) renders identically.
type PolicyEntry struct {
	ChannelID  string    `json:"channel_id"`
	Alias      string    `json:"alias"`
	ResourceID string    `json:"resource_id,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
}

// PolicyList preserves the old envelope shape so handler.go's
// pagination-rendering code (HasMore, NextCursor) doesn't have to
// change. NextCursor is the base64 of the DDB LastEvaluatedKey;
// callers pass it back into ListPolicies for the next page.
type PolicyList struct {
	Entries    []PolicyEntry `json:"entries"`
	NextCursor string        `json:"next_cursor,omitempty"`
	HasMore    bool          `json:"has_more,omitempty"`
}

// WorkspaceMapping describes the 1:1 workspace → owner row stored on
// the workspace_mappings table. Returned from `RedeemBootstrap` so
// the modal-submit path can post the success DM with the owner ID.
type WorkspaceMapping struct {
	TeamID    string    `json:"team_id"`
	OwnerID   string    `json:"owner_id"`
	CreatedAt time.Time `json:"created_at"`
}

// WorkspaceConfig is the response shape `/qurl admin status` renders.
// Computed by the workspace-config read path: a single GetItem on
// workspace_mappings plus a count-only Query on channel_policies for
// PolicyCount.
//
// APIKeyFingerprint stays empty for now — pre-pivot it was a
// service-side sha256-prefix over the workspace API key, but the
// API key now lives in `workspace_state` (shared/auth/DDBProvider)
// which this package doesn't own. A follow-up will plumb a
// fingerprint accessor through handlerDeps without coupling
// slackdata to the encrypted-API-key surface.
type WorkspaceConfig struct {
	OwnerID           string    `json:"owner_id"`
	APIKeyFingerprint string    `json:"api_key_fingerprint"`
	SeedAdminUserID   string    `json:"seed_admin_user_id"`
	ConfiguredAt      time.Time `json:"configured_at"`
	PolicyCount       int       `json:"policy_count"`
}

// ddbToError normalizes an SDK error into an [*Error] with an HTTP-
// shaped StatusCode so handler callers can keep the same
// `errors.As(&Error) && StatusCode == X` branch shape they had
// against the old AdminError. Non-SDK errors return a generic 503.
func ddbToError(op string, err error) error {
	if err == nil {
		return nil
	}
	var ccfe *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &ccfe) {
		// Caller decides the status code via their wrapper — we
		// surface a sentinel string here so the caller can identify
		// it via errors.As + Code without parsing Detail. The
		// caller-supplied wrapper sets the right HTTP-shaped status.
		return &Error{
			StatusCode: http.StatusPreconditionFailed,
			Code:       "conditional_check_failed",
			Title:      op,
			Detail:     err.Error(),
		}
	}
	return &Error{
		StatusCode: http.StatusServiceUnavailable,
		Code:       "ddb_error",
		Title:      op,
		Detail:     err.Error(),
	}
}

// boolAttr is a small helper for boolean DDB AttributeValues.
func boolAttr(b bool) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberBOOL{Value: b}
}

// stringAttr is a small helper for string DDB AttributeValues. Empty
// strings are NOT permitted in DDB (would 400 ValidationException);
// callers MUST guard upstream.
func stringAttr(v string) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberS{Value: v}
}

// readString reads a string attr; returns "" if missing or wrong type.
func readString(item map[string]ddbtypes.AttributeValue, key string) string {
	v, ok := item[key].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return ""
	}
	return v.Value
}

// readStringSet reads an SS attr; returns nil if missing or wrong
// type. DDB string-set values are unordered.
func readStringSet(item map[string]ddbtypes.AttributeValue, key string) []string {
	v, ok := item[key].(*ddbtypes.AttributeValueMemberSS)
	if !ok {
		return nil
	}
	return v.Value
}

// readStringMap reads a Map<string,string> attr. Returns nil if
// missing or wrong type. Values that aren't strings are skipped
// rather than failing the whole read — a corrupt entry shouldn't
// drop the rest of the map. Iteration order of the returned map is
// non-deterministic; callers that render to UI must sort.
func readStringMap(item map[string]ddbtypes.AttributeValue, key string) map[string]string {
	m, ok := item[key].(*ddbtypes.AttributeValueMemberM)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m.Value))
	for k, v := range m.Value {
		s, ok := v.(*ddbtypes.AttributeValueMemberS)
		if !ok {
			continue
		}
		out[k] = s.Value
	}
	return out
}

// readTime parses an RFC3339 string attr into a time.Time. Returns
// zero time if missing/unparseable so the caller can fall back to
// "unknown" in the rendered output.
func readTime(item map[string]ddbtypes.AttributeValue, key string) time.Time {
	s := readString(item, key)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// exprNow is the DDB ExpressionAttributeValues placeholder for the
// current time, used as both an updated_at write and an
// expires_at > :now read across multiple UpdateExpression callers.
// Lifted to a constant to satisfy goconst.
const exprNow = ":now"

// notFoundError is the canonical 404 shape — emitted by the GetItem-
// returns-empty paths and the conditional-check-failed paths that
// mean "no such row to act on".
func notFoundError(title string) *Error {
	return &Error{
		StatusCode: http.StatusNotFound,
		Code:       "not_found",
		Title:      title,
	}
}
