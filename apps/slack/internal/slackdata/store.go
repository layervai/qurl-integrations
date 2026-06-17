// Package slackdata is the DDB-direct replacement for the old
// admin_client.go HTTP wrapper around qurl-service `/internal/v1/admin/*`.
//
// Justin's 2026-05-12 pivot (see SLACK_QURL_ROLLOUT.md and
// qurl-integrations-infra #523) moved the Slack-keyed DynamoDB
// tables — workspace_mappings and channel_policies — out of
// qurl-service into qurl-bot-slack-owned terraform
// (`modules/qurl-slack-ddb/`). The bot now reads/writes those tables
// directly with in-account IAM. There is no longer any HTTP surface
// in qurl-service for admin/policy state; calling
// `/internal/v1/admin/*` returns 404.
//
// /qurl setup also seeds the workspace admin row from the OAuth
// callback (the installer becomes the seed admin), so the previously
// separate bootstrap_codes table and the `/qurl-admin admin claim` redeem
// step are gone — this package owns only workspace_mappings +
// channel_policies.
//
// This package exposes a `Store` facade with the method shapes the
// post-pivot Slack handlers depend on (CheckAdmin, ResolvePolicy,
// GetChannelPolicy, AllowedResourceIDsForChannel, LookupChannelAlias,
// BindWorkspace, AddAdmin, RemoveAdmin, ListAdmins). Errors carry an
// HTTP-shaped StatusCode on `*Error` so handlers branch via errors.As
// without caring about the underlying DDB exception shape.
//
// Two env vars wire the tables on Fargate (set by
// qurl-bot-slack/terraform via modules/qurl-slack-ddb's outputs):
//
//   - QURL_WORKSPACE_MAPPINGS_TABLE
//   - QURL_CHANNEL_POLICIES_TABLE
//
// Encryption note: both tables use customer-managed SSE on the
// qurl-bot-slack CMK. The task role's IAM grant is `kms:ViaService =
// dynamodb.<region>` so the bot never sees the data key directly;
// SSE is transparent at the SDK layer. (Field-level envelope
// encryption like shared/auth.DDBProvider does for `qurl_api_key`
// is NOT used here — workspace identity + admin user IDs + alias
// mappings are not customer-secret-grade payloads, and the per-row
// re-encrypt would burn KMS quota for no posture gain.)
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
// `output.table_names`). The tables are named with the env-scoped
// prefix `qurl-bot-slack-<env>-<table>` so a misrouted call hits
// NoSuchTable instead of the wrong env's data — see the "blast-radius
// control" rationale in modules/qurl-slack-ddb/main.tf.
const (
	// EnvWorkspaceMappingsTable holds the DDB table name for the
	// 1:1 Slack team_id → qurl owner mapping table.
	EnvWorkspaceMappingsTable = "QURL_WORKSPACE_MAPPINGS_TABLE"
	// EnvChannelPoliciesTable holds the DDB table name for the per-
	// channel allowed-resource-id policy table.
	EnvChannelPoliciesTable = "QURL_CHANNEL_POLICIES_TABLE"
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
	// Query backs [Store.ChannelsForResource] — the only Query caller — which
	// pages every channel_policies row for a team to find the channels a
	// resource is exposed to. It requires the dynamodb:Query action on the
	// channel_policies table; the other ops only need item-level grants.
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// Store is the DDB-direct replacement for the old `AdminClient`. It
// owns two DDB tables (workspace_mappings, channel_policies) and
// surfaces the same method set the old HTTP client had so the
// slash-command handlers don't need restructuring.
//
// The zero value is NOT usable — construct via [NewStore] or set
// every field explicitly in tests.
type Store struct {
	Client                DynamoDBClient
	WorkspaceMappingsName string
	ChannelPoliciesName   string
	RateLimitEnabled      bool
	RateLimitLimit        int
	RateLimitWindow       time.Duration

	// Now is injected so tests can pin the clock for created_at /
	// updated_at assertions without poking a package-global.
	// Defaults to time.Now.
	Now func() time.Time
}

// StoreOption configures [NewStore].
type StoreOption func(*storeOptions)

type storeOptions struct {
	workspaceMappingsName string
	channelPoliciesName   string
	ddbClient             DynamoDBClient
	rateLimitEnabled      bool
	awsConfigFns          []func(*awsconfig.LoadOptions) error
}

// WithDynamoDBClient injects a DDB client. Primarily for tests; in
// production [NewStore] loads aws config and instantiates one.
func WithDynamoDBClient(c DynamoDBClient) StoreOption {
	return func(o *storeOptions) { o.ddbClient = c }
}

// WithTableNames overrides the per-table names. Any empty argument
// falls back to the matching env var. Primarily for tests.
func WithTableNames(workspaceMappings, channelPolicies string) StoreOption {
	return func(o *storeOptions) {
		o.workspaceMappingsName = workspaceMappings
		o.channelPoliciesName = channelPolicies
	}
}

// WithRateLimitEnabled gates the in-bot per-user Slack command rate limit.
// Keep disabled for sandbox/no-prod deploys; production opts in once the DDB
// write path has table/IAM headroom confirmed.
func WithRateLimitEnabled(enabled bool) StoreOption {
	return func(o *storeOptions) { o.rateLimitEnabled = enabled }
}

// NewStore constructs a [Store], loading AWS config from the ambient
// environment unless overridden via options. Returns an error if a
// table name is missing — there is no sane fallback for "which env's
// data am I supposed to write to" so failing fast at boot is the
// only safe move.
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

	switch {
	case o.workspaceMappingsName == "":
		return nil, fmt.Errorf("slackdata.NewStore: %s is required", EnvWorkspaceMappingsTable)
	case o.channelPoliciesName == "":
		return nil, fmt.Errorf("slackdata.NewStore: %s is required", EnvChannelPoliciesTable)
	}

	if o.ddbClient == nil {
		cfg, err := awsconfig.LoadDefaultConfig(ctx, o.awsConfigFns...)
		if err != nil {
			return nil, fmt.Errorf("slackdata.NewStore: load AWS config: %w", err)
		}
		o.ddbClient = dynamodb.NewFromConfig(cfg)
	}

	return &Store{
		Client:                o.ddbClient,
		WorkspaceMappingsName: o.workspaceMappingsName,
		ChannelPoliciesName:   o.channelPoliciesName,
		RateLimitEnabled:      o.rateLimitEnabled,
		RateLimitLimit:        defaultRateLimitLimit,
		RateLimitWindow:       defaultRateLimitWindow,
		Now:                   time.Now,
	}, nil
}

// resolveNow returns now() when set, else the wall clock. Shared by [Store]
// and [AgentStore] so the injectable-clock fallback lives in one place; both
// guard against a bare `&Store{}` / `&AgentStore{}` that didn't set Now.
func resolveNow(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}

// nowOrDefault guards against a bare `&Store{}` that didn't set
// Now — [NewStore] always sets it, but the fallback is cheap
// insurance against a future caller that constructs the struct
// directly.
func (s *Store) nowOrDefault() time.Time {
	return resolveNow(s.Now)
}

// Error mirrors the StatusCode/Code/Title/Detail shape the old
// `*AdminError` had so handlers that do
// `errors.As(err, &slackdata.Error) && e.StatusCode == 404` keep
// working unchanged after the swap. The StatusCode field is the
// load-bearing branch discriminator across the handler files —
// each DDB-direct op below maps native ddb errors to the
// equivalent HTTP-shaped status (404 for ConditionalCheckFailed
// on a "not-found"-style condition, 409 for "already exists",
// 503 for transport).
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

// PolicyEntry is one alias binding for a single (team, channel) row.
// Returned by [Store.GetChannelPolicy]. JSON tags retained from the
// pre-pivot wire shape so any caller that happens to marshal one
// (e.g. for slog) renders identically.
type PolicyEntry struct {
	ChannelID  string    `json:"channel_id"`
	Alias      string    `json:"alias"`
	ResourceID string    `json:"resource_id,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
}

// WorkspaceMapping describes the 1:1 workspace → owner row stored on
// the workspace_mappings table. Passed to `BindWorkspace` from the
// OAuth callback to seed the row.
//
// OwnerID is the Slack user ID of the Slack user who completed the
// first `/qurl setup` flow for this workspace — the "workspace
// owner" in the LayerV admin model. Only the owner can re-run
// `/qurl setup` after the first bind; other admins (added via
// `/qurl admin add`) can run the rest of the admin commands but
// cannot re-point the workspace's qURL credential. The OAuth
// callback gates this distinction: BindWorkspace classifies a
// rebind attempt as AlreadyBoundToCaller iff the OwnerID stored
// here matches the new caller's verified Slack user ID.
//
// Historical note: prior versions stored the JWKS-verified Auth0
// id_token sub here. That diverged from /qurl admin list's
// Slack-mention rendering (it expects a Slack user ID shape) and
// from /qurl setup's owner-only gate (which compares to the
// invoking Slack user). Switched to Slack user ID end-to-end —
// the Auth0 sub stays a runtime-only gate at OAuth callback time
// (id_token verification still refuses installs without a valid
// sub) but isn't persisted here.
type WorkspaceMapping struct {
	TeamID    string    `json:"team_id"`
	OwnerID   string    `json:"owner_id"`
	CreatedAt time.Time `json:"created_at"`
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
		//
		// CONTRACT: every existing call site (BindWorkspace,
		// AddAdmin, RemoveAdmin) catches
		// ConditionalCheckFailedException BEFORE calling
		// ddbToError, so this 412 branch is currently unreachable.
		// Any new op that calls ddbToError MUST do the same — the
		// handler layer doesn't dispatch on 412, so a leak through
		// here would surface to the user as the generic 503 copy
		// even when the underlying failure was a conditional check.
		// The 412 fallback exists for defense-in-depth, not as a
		// supported branch.
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

// stringAttr is a small helper for string DDB AttributeValues. Empty
// strings are NOT permitted in DDB (would 400 ValidationException);
// callers MUST guard upstream.
func stringAttr(v string) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberS{Value: v}
}

func boolAttr(v bool) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberBOOL{Value: v}
}

// readBoolPresent reads a BOOL attr. present is false when the attr is missing or
// the wrong type — the caller needs the three-state distinction (absent vs an
// explicit true/false) so an opt-out can survive a default flip.
func readBoolPresent(item map[string]ddbtypes.AttributeValue, key string) (value, present bool) {
	v, ok := item[key].(*ddbtypes.AttributeValueMemberBOOL)
	if !ok {
		return false, false
	}
	return v.Value, true
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
