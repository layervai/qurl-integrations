// Package auth — DDB-backed Provider for per-workspace API keys.
//
// DDBProvider stores per-workspace qURL API keys and Slack bot tokens in the
// `workspace_state` DynamoDB table, with secret columns encrypted at the field
// level. The PK (`team_id`) stays plaintext so GetItem can dispatch on it;
// `qurl_api_key` and `slack_bot_token` are wrapped in ciphertext.
//
// Encryption strategy (envelope encryption via AWS KMS):
//
//   - On write, we ask KMS for a fresh 32-byte data key
//     (GenerateDataKey, key-spec AES_256). KMS returns the plaintext
//     data key + the ciphertext data key (the latter is what we persist
//     alongside the encrypted value — KMS will unwrap it on read).
//   - We seal the plaintext API key under that data key with AES-256-GCM
//     and zero the plaintext data key from memory immediately after.
//   - On read, we hand the ciphertext data key to KMS Decrypt, get the
//     plaintext data key back, and open the GCM-sealed API key. Again
//     the plaintext data key is zeroed once we're done.
//
// This is materially the same posture as the proprietary AWS Database
// Encryption SDK for DynamoDB but implemented directly against
// crypto/cipher + the KMS SDK because that SDK doesn't ship a
// maintained Go binding. If AWS ever publishes one, port to it for the
// canonical attribute-level encrypt/sign/material-provider story
// (column-level "do not encrypt" / "sign only" / "encrypt and sign"
// declarations) — tracked at #269.
package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// kmsWorkspaceAADKey is the CloudTrail-visible name for the workspace
// identifier in KMS EncryptionContext. The fixed key matches what
// CloudTrail records on every Decrypt call, which is the discriminator
// incident responders use to attribute a Decrypt to a workspace without
// needing to fetch the DDB row.
const kmsWorkspaceAADKey = "workspace_id"

// Attribute names on the `workspace_state` DDB table. Mirrored on the
// qurl-integrations-infra side in the TF for the table schema; changing
// these here means a coordinated TF change.
const (
	attrTeamID       = "team_id"
	attrQURLAPIKey   = "qurl_api_key"    // GCM-sealed plaintext, base64 in JSON, raw bytes in DDB
	attrDataKeyCT    = "qurl_api_key_dk" // ciphertext data key returned by KMS GenerateDataKey
	attrConfiguredBy = "configured_by"
	attrConfiguredAt = "configured_at"
	attrUpdatedAt    = "updated_at"

	attrSlackBotToken       = "slack_bot_token"
	attrSlackBotTokenDK     = "slack_bot_token_dk"
	attrSlackBotInstalledBy = "slack_bot_installed_by"
	attrSlackBotInstalledAt = "slack_bot_installed_at"
	attrSlackBotUpdatedAt   = "slack_bot_updated_at"
	attrSlackBotUserID      = "slack_bot_user_id"
	attrSlackAppID          = "slack_app_id"
	attrSlackEnterpriseID   = "slack_enterprise_id"
	attrSlackBotScopes      = "slack_bot_scopes"
)

// Env var names — operator-set via the Fargate task definition (the
// qurl-integrations-infra TF wires these from module.runtime.environment).
const (
	// EnvWorkspaceStateTable holds the DDB table name (operator-set;
	// no default fallback to avoid the "ships pointing at the wrong
	// account" failure mode flagged in cmd/main.go for QURL_ENDPOINT).
	EnvWorkspaceStateTable = "WORKSPACE_STATE_TABLE"
	// EnvWorkspaceStateKMSKeyARN is the customer-managed CMK ARN used
	// for envelope-encrypting the qurl_api_key field. Required.
	EnvWorkspaceStateKMSKeyARN = "WORKSPACE_STATE_KMS_KEY_ARN"
)

const (
	apiKeyCacheTTL        = 5 * time.Minute
	apiKeyCacheSweepEvery = time.Minute
)

// ErrWorkspaceNotConfigured is the sentinel returned by APIKey when the
// workspace has no row in the workspace_state table (i.e. the admin has
// not yet completed /oauth/qurl/start → /callback). The Slack handler
// catches this distinctly so the bot can prompt the user to visit
// /oauth/qurl/start rather than rendering a generic "auth failed" error.
var ErrWorkspaceNotConfigured = errors.New("workspace not configured — admin must complete /oauth/qurl/start")

// ErrSlackBotTokenNotConfigured is returned when a workspace has not completed
// the Slack app OAuth install/reinstall path that grants a per-workspace bot
// token. The Slack handler catches this distinctly from decrypt/transport
// failures so old installs can be pointed at the reinstall path.
var ErrSlackBotTokenNotConfigured = errors.New("workspace Slack bot token not configured — admin must reinstall the Slack app")

// DynamoDBClient is the slice of *dynamodb.Client the provider actually
// uses. Exposed as an interface so tests can inject a fake without
// spinning up localstack.
type DynamoDBClient interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

// FieldEncryptor seals/opens an attribute's plaintext using a customer-
// managed CMK. The Seal call returns the GCM-sealed payload plus the
// KMS-wrapped data key (both stored in DDB next to each other; both
// required to decrypt).
//
// Exposed as an interface so the DDBProvider can be tested with a pass-
// through encoder (no KMS calls) and the cmd-line entrypoint wires the
// production KMSEncryptor against the live key ARN.
type FieldEncryptor interface {
	Seal(ctx context.Context, plaintext []byte, aad []byte) (ciphertext, wrappedKey []byte, err error)
	Open(ctx context.Context, ciphertext, wrappedKey, aad []byte) ([]byte, error)
}

// DDBProvider implements Provider by reading per-workspace API keys from
// a DynamoDB table, with field-level envelope encryption on the key
// column.
type DDBProvider struct {
	Client    DynamoDBClient
	TableName string
	Encryptor FieldEncryptor

	// Cache successful APIKey lookups at the DDB/decrypt boundary so every
	// long-lived consumer avoids repeat KMS Decrypt calls during normal slash
	// command bursts. This mirrors newWorkspaceSlackTokenLookupWithInvalidation:
	// per-workspace in-flight fills, generation-guarded invalidation, and inline
	// expiry sweeps using Now instead of a background janitor. The cache is
	// process-local: rotation or disconnect on one serving instance cannot evict
	// sibling instances, so they may continue using a previously decrypted key,
	// including a revoked key, until this TTL expires. Issue #263 explicitly
	// accepts five minutes as the starting cross-instance staleness window; that
	// stays human-perceptible while collapsing the common multi-command session.
	// Slack bot-token caching stays shorter because it gates reinstall detection
	// rather than qURL API retry pressure.
	apiKeyCache map[string]*cachedAPIKey

	// The mutex coordinates the cache with in-flight fills and invalidation
	// generations. Generation entries are retained after invalidation so a
	// detached stale fill cannot observe a reset-to-zero generation and cache an
	// old key; the map only grows for workspaces that rotate or disconnect in
	// this process, matching the sibling Slack token lookup.
	apiKeyCacheMu         sync.Mutex
	apiKeyLookupInFlight  map[string]*apiKeyLookupCall
	apiKeyCacheGeneration map[string]uint64
	apiKeyCacheLastSweep  time.Time

	// Now is injected so tests can pin the wall clock for configured_at /
	// updated_at assertions without poking package-global state. Defaults
	// to time.Now.
	Now func() time.Time
}

type cachedAPIKey struct {
	apiKey    string
	expiresAt time.Time
}

type apiKeyLookupResult struct {
	apiKey string
	err    error
}

type apiKeyLookupCall struct {
	done chan struct{}
	apiKeyLookupResult
}

type apiKeyLookupStart struct {
	apiKey     string
	hit        bool
	call       *apiKeyLookupCall
	owner      bool
	generation uint64
}

// SlackBotTokenInstall is the workspace-scoped Slack OAuth material persisted
// after a customer installs or reauthorizes the Slack app. BotToken is stored
// encrypted; the remaining fields are operational metadata for audits and
// support triage.
type SlackBotTokenInstall struct {
	BotToken     string
	InstalledBy  string
	BotUserID    string
	AppID        string
	EnterpriseID string
	Scopes       []string
}

// nowOrDefault is the safe clock accessor — NewDDBProvider always sets
// Now, but a bare &DDBProvider{} construction (e.g. for tests that
// satisfy a type signature without exercising writes) would nil-deref
// on p.Now(). Defaulting here keeps the bare-struct path safe.
func (p *DDBProvider) nowOrDefault() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// DDBProviderOption configures NewDDBProvider.
type DDBProviderOption func(*ddbProviderOptions)

type ddbProviderOptions struct {
	tableName    string
	kmsKeyARN    string
	ddbClient    DynamoDBClient
	encryptor    FieldEncryptor
	awsConfigFns []func(*awsconfig.LoadOptions) error
}

// WithTableName overrides the DDB table name. If empty, NewDDBProvider
// falls back to the WORKSPACE_STATE_TABLE env var.
func WithTableName(name string) DDBProviderOption {
	return func(o *ddbProviderOptions) { o.tableName = name }
}

// WithKMSKeyARN overrides the CMK ARN used for field encryption. If
// empty, NewDDBProvider falls back to the WORKSPACE_STATE_KMS_KEY_ARN
// env var.
func WithKMSKeyARN(arn string) DDBProviderOption {
	return func(o *ddbProviderOptions) { o.kmsKeyARN = arn }
}

// WithDynamoDBClient injects a DDB client. Primarily for tests; in
// production the constructor loads aws config and instantiates one.
func WithDynamoDBClient(c DynamoDBClient) DDBProviderOption {
	return func(o *ddbProviderOptions) { o.ddbClient = c }
}

// WithFieldEncryptor injects a field encryptor. Primarily for tests;
// in production the constructor instantiates a KMSEncryptor pointed at
// WORKSPACE_STATE_KMS_KEY_ARN.
func WithFieldEncryptor(e FieldEncryptor) DDBProviderOption {
	return func(o *ddbProviderOptions) { o.encryptor = e }
}

// NewDDBProvider constructs a DDBProvider, loading AWS config from the
// ambient environment unless overridden via options.
func NewDDBProvider(ctx context.Context, opts ...DDBProviderOption) (*DDBProvider, error) {
	o := &ddbProviderOptions{}
	for _, fn := range opts {
		fn(o)
	}

	if o.tableName == "" {
		o.tableName = os.Getenv(EnvWorkspaceStateTable)
	}
	if o.tableName == "" {
		return nil, fmt.Errorf("DDBProvider: table name is required (set %s or use WithTableName)", EnvWorkspaceStateTable)
	}
	if o.kmsKeyARN == "" {
		o.kmsKeyARN = os.Getenv(EnvWorkspaceStateKMSKeyARN)
	}

	if o.ddbClient == nil || o.encryptor == nil {
		cfg, err := awsconfig.LoadDefaultConfig(ctx, o.awsConfigFns...)
		if err != nil {
			return nil, fmt.Errorf("DDBProvider: load AWS config: %w", err)
		}
		if o.ddbClient == nil {
			o.ddbClient = dynamodb.NewFromConfig(cfg)
		}
		if o.encryptor == nil {
			if o.kmsKeyARN == "" {
				return nil, fmt.Errorf("DDBProvider: KMS key ARN is required (set %s or use WithKMSKeyARN)", EnvWorkspaceStateKMSKeyARN)
			}
			o.encryptor = &KMSEncryptor{
				Client: kms.NewFromConfig(cfg),
				KeyID:  o.kmsKeyARN,
			}
		}
	}

	return &DDBProvider{
		Client:    o.ddbClient,
		TableName: o.tableName,
		Encryptor: o.encryptor,
		Now:       time.Now,
	}, nil
}

// APIKey looks up the per-workspace API key for workspaceID. Returns
// ErrWorkspaceNotConfigured (wrapped) if no row exists so the caller
// can route the user to /oauth/qurl/start. All other failures (decrypt,
// transport) return a generic wrapped error — the bot shouldn't leak
// which arm of the pipeline failed.
func (p *DDBProvider) APIKey(ctx context.Context, workspaceID string) (string, error) {
	if workspaceID == "" {
		return "", errors.New("DDBProvider.APIKey: workspaceID is empty")
	}

	now := p.nowOrDefault()
	start := p.getOrStartAPIKeyLookup(workspaceID, now)
	if start.hit {
		return start.apiKey, nil
	}
	if !start.owner {
		// Waiters share the owner result, including an owner context
		// cancellation. That matches the sibling Slack token cache and keeps
		// coalescing simple; the next request retries normally.
		select {
		case <-start.call.done:
			return start.call.apiKey, start.call.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	return p.fetchAndFinishAPIKeyLookup(ctx, workspaceID, start.call, now, start.generation)
}

func (p *DDBProvider) getOrStartAPIKeyLookup(workspaceID string, now time.Time) apiKeyLookupStart {
	p.apiKeyCacheMu.Lock()
	defer p.apiKeyCacheMu.Unlock()

	if p.apiKeyCache == nil {
		p.apiKeyCache = map[string]*cachedAPIKey{}
	}
	p.sweepExpiredAPIKeyCacheLocked(now)
	if entry, ok := p.apiKeyCache[workspaceID]; ok && now.Before(entry.expiresAt) {
		return apiKeyLookupStart{apiKey: entry.apiKey, hit: true}
	}

	if p.apiKeyCacheGeneration == nil {
		p.apiKeyCacheGeneration = map[string]uint64{}
	}
	generation := p.apiKeyCacheGeneration[workspaceID]
	if p.apiKeyLookupInFlight == nil {
		p.apiKeyLookupInFlight = map[string]*apiKeyLookupCall{}
	}
	if call, ok := p.apiKeyLookupInFlight[workspaceID]; ok {
		return apiKeyLookupStart{call: call}
	}
	call := &apiKeyLookupCall{done: make(chan struct{})}
	p.apiKeyLookupInFlight[workspaceID] = call
	return apiKeyLookupStart{call: call, owner: true, generation: generation}
}

func (p *DDBProvider) fetchAndFinishAPIKeyLookup(ctx context.Context, workspaceID string, call *apiKeyLookupCall, now time.Time, generation uint64) (string, error) {
	result := apiKeyLookupResult{}
	finished := false
	defer func() {
		if rec := recover(); rec != nil {
			if !finished {
				result = apiKeyLookupResult{err: errors.New("DDBProvider.APIKey: lookup panicked")}
				p.finishAPIKeyLookup(workspaceID, call, result, now, generation)
			}
			panic(rec)
		}
	}()

	apiKey, err := p.fetchAPIKey(ctx, workspaceID)
	result = apiKeyLookupResult{apiKey: apiKey, err: err}
	finished = true
	p.finishAPIKeyLookup(workspaceID, call, result, now, generation)
	return apiKey, err
}

func (p *DDBProvider) fetchAPIKey(ctx context.Context, workspaceID string) (string, error) {
	// Eventually-consistent read is correct here: the per-workspace key
	// only changes on (re-)install, and the few-ms propagation delay is
	// inside the same "click the setup link" window. Strong reads would
	// double RCU cost without changing the failure modes that matter.
	out, err := p.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(p.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTeamID: &ddbtypes.AttributeValueMemberS{Value: workspaceID},
		},
	})
	if err != nil {
		return "", fmt.Errorf("DDBProvider.APIKey: GetItem: %w", err)
	}
	if out == nil || len(out.Item) == 0 {
		return "", fmt.Errorf("DDBProvider.APIKey: workspace %q: %w", workspaceID, ErrWorkspaceNotConfigured)
	}

	ctBlob, ok := out.Item[attrQURLAPIKey].(*ddbtypes.AttributeValueMemberB)
	if !ok || len(ctBlob.Value) == 0 {
		return "", fmt.Errorf("DDBProvider.APIKey: workspace %q: %w", workspaceID, ErrWorkspaceNotConfigured)
	}
	wrappedKey, ok := out.Item[attrDataKeyCT].(*ddbtypes.AttributeValueMemberB)
	if !ok || len(wrappedKey.Value) == 0 {
		return "", errors.New("DDBProvider.APIKey: stored item missing or has wrong type for qurl_api_key_dk")
	}

	pt, err := p.Encryptor.Open(ctx, ctBlob.Value, wrappedKey.Value, []byte(workspaceID))
	if err != nil {
		return "", fmt.Errorf("DDBProvider.APIKey: decrypt: %w", err)
	}
	// Empty plaintext means the ciphertext decrypted but to zero bytes —
	// corruption / truncate / unsigned-store-bypass. Fail loud here rather
	// than handing the caller "" and watching qurl-service surface an
	// opaque 401.
	if len(pt) == 0 {
		return "", errors.New("DDBProvider.APIKey: decrypted plaintext is empty")
	}
	return string(pt), nil
}

func (p *DDBProvider) finishAPIKeyLookup(workspaceID string, call *apiKeyLookupCall, result apiKeyLookupResult, now time.Time, generation uint64) {
	p.apiKeyCacheMu.Lock()
	defer p.apiKeyCacheMu.Unlock()

	if result.err == nil && p.apiKeyCacheGeneration[workspaceID] == generation {
		p.cacheAPIKey(workspaceID, result.apiKey, now)
	}
	call.apiKeyLookupResult = result
	if p.apiKeyLookupInFlight[workspaceID] == call {
		delete(p.apiKeyLookupInFlight, workspaceID)
	}
	close(call.done)
}

func (p *DDBProvider) cacheAPIKey(workspaceID, apiKey string, now time.Time) {
	if p.apiKeyCache == nil {
		p.apiKeyCache = map[string]*cachedAPIKey{}
	}
	p.apiKeyCache[workspaceID] = &cachedAPIKey{
		apiKey:    apiKey,
		expiresAt: now.Add(apiKeyCacheTTL),
	}
}

func (p *DDBProvider) sweepExpiredAPIKeyCacheLocked(now time.Time) {
	if !p.apiKeyCacheLastSweep.IsZero() && now.Sub(p.apiKeyCacheLastSweep) < apiKeyCacheSweepEvery {
		return
	}
	// Minute-gated O(workspaces seen by this process), matching the Slack token
	// cache without adding a background goroutine or real-clock timer to tests.
	for workspaceID, entry := range p.apiKeyCache {
		if !now.Before(entry.expiresAt) {
			delete(p.apiKeyCache, workspaceID)
		}
	}
	p.apiKeyCacheLastSweep = now
}

func (p *DDBProvider) invalidateAPIKeyCache(workspaceID string) {
	p.apiKeyCacheMu.Lock()
	defer p.apiKeyCacheMu.Unlock()

	delete(p.apiKeyCache, workspaceID)
	if p.apiKeyLookupInFlight != nil {
		delete(p.apiKeyLookupInFlight, workspaceID)
	}
	if p.apiKeyCacheGeneration == nil {
		p.apiKeyCacheGeneration = map[string]uint64{}
	}
	p.apiKeyCacheGeneration[workspaceID]++
}

// SlackBotToken looks up the per-workspace Slack bot token captured during
// Slack app OAuth installation. Missing token attributes are treated as a
// reinstall-needed state instead of row corruption because older workspace rows
// legitimately predate Slack bot-token persistence.
func (p *DDBProvider) SlackBotToken(ctx context.Context, workspaceID string) (string, error) {
	if workspaceID == "" {
		return "", errors.New("DDBProvider.SlackBotToken: workspaceID is empty")
	}
	out, err := p.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(p.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTeamID: &ddbtypes.AttributeValueMemberS{Value: workspaceID},
		},
	})
	if err != nil {
		return "", fmt.Errorf("DDBProvider.SlackBotToken: GetItem: %w", err)
	}
	if out == nil || len(out.Item) == 0 {
		return "", fmt.Errorf("DDBProvider.SlackBotToken: workspace %q: %w", workspaceID, ErrSlackBotTokenNotConfigured)
	}

	// Rows can exist before a Slack app install because /qurl setup writes the
	// qURL API key first. Missing Slack columns are expected in that state.
	ctBlob, ok := out.Item[attrSlackBotToken].(*ddbtypes.AttributeValueMemberB)
	if !ok || len(ctBlob.Value) == 0 {
		return "", fmt.Errorf("DDBProvider.SlackBotToken: workspace %q: %w", workspaceID, ErrSlackBotTokenNotConfigured)
	}
	wrappedKey, ok := out.Item[attrSlackBotTokenDK].(*ddbtypes.AttributeValueMemberB)
	if !ok || len(wrappedKey.Value) == 0 {
		return "", fmt.Errorf("DDBProvider.SlackBotToken: workspace %q: %w", workspaceID, ErrSlackBotTokenNotConfigured)
	}

	pt, err := p.Encryptor.Open(ctx, ctBlob.Value, wrappedKey.Value, []byte(workspaceID))
	if err != nil {
		return "", fmt.Errorf("DDBProvider.SlackBotToken: decrypt: %w", err)
	}
	if len(pt) == 0 {
		return "", errors.New("DDBProvider.SlackBotToken: decrypted plaintext is empty")
	}
	return string(pt), nil
}

// SetAPIKey upserts the per-workspace qURL API key. The configuredBy field
// is informational (the Slack user_id of the admin who completed
// /oauth/qurl/callback) and is persisted plaintext. UpdateItem is used instead
// of PutItem so Slack app install metadata in the same row is preserved.
func (p *DDBProvider) SetAPIKey(ctx context.Context, workspaceID, apiKey, configuredBy string) error {
	if workspaceID == "" {
		return errors.New("DDBProvider.SetAPIKey: workspaceID is empty")
	}
	if apiKey == "" {
		return errors.New("DDBProvider.SetAPIKey: apiKey is empty")
	}

	ct, wrapped, err := p.Encryptor.Seal(ctx, []byte(apiKey), []byte(workspaceID))
	if err != nil {
		return fmt.Errorf("DDBProvider.SetAPIKey: encrypt: %w", err)
	}

	now := p.nowOrDefault().UTC().Format(time.RFC3339)
	updateExpr := fmt.Sprintf("SET %s = :key, %s = :dk, %s = :by, %s = :now, %s = if_not_exists(%s, :now)",
		attrQURLAPIKey, attrDataKeyCT, attrConfiguredBy, attrUpdatedAt, attrConfiguredAt, attrConfiguredAt)
	// TODO(#265): this UpdateItem closes the old GetItem+PutItem row-clobber
	// window and preserves Slack install metadata, but the upstream qurl-service
	// mint still happens before this write. If concurrent admins mint different
	// keys, the earlier key can still be overwritten here and left orphaned
	// upstream because there is no losing-write signal to trigger a revoke.
	out, err := p.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(p.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTeamID: &ddbtypes.AttributeValueMemberS{Value: workspaceID},
		},
		UpdateExpression: aws.String(updateExpr),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":key": &ddbtypes.AttributeValueMemberB{Value: ct},
			":dk":  &ddbtypes.AttributeValueMemberB{Value: wrapped},
			":by":  &ddbtypes.AttributeValueMemberS{Value: configuredBy},
			":now": &ddbtypes.AttributeValueMemberS{Value: now},
		},
		ReturnValues: ddbtypes.ReturnValueUpdatedOld,
	})
	if err != nil {
		return fmt.Errorf("DDBProvider.SetAPIKey: UpdateItem: %w", err)
	}
	if out != nil {
		if _, rotated := out.Attributes[attrQURLAPIKey]; rotated {
			slog.Warn("DDBProvider.SetAPIKey overwrote existing workspace API key",
				"workspace_id", workspaceID,
				"configured_by", configuredBy)
		}
	}
	p.invalidateAPIKeyCache(workspaceID)
	return nil
}

// SetSlackBotToken upserts the encrypted Slack bot token captured during Slack
// app install or reinstall. It intentionally updates only Slack-specific
// attributes so the qURL API key columns survive app reauthorization.
func (p *DDBProvider) SetSlackBotToken(ctx context.Context, workspaceID string, install *SlackBotTokenInstall) error {
	if workspaceID == "" {
		return errors.New("DDBProvider.SetSlackBotToken: workspaceID is empty")
	}
	if install == nil {
		return errors.New("DDBProvider.SetSlackBotToken: install is nil")
	}
	botToken := strings.TrimSpace(install.BotToken)
	if botToken == "" {
		return errors.New("DDBProvider.SetSlackBotToken: bot token is empty")
	}
	if err := ValidateSlackBotTokenShape(botToken); err != nil {
		return fmt.Errorf("DDBProvider.SetSlackBotToken: invalid bot token: %w", err)
	}

	ct, wrapped, err := p.Encryptor.Seal(ctx, []byte(botToken), []byte(workspaceID))
	if err != nil {
		return fmt.Errorf("DDBProvider.SetSlackBotToken: encrypt: %w", err)
	}
	now := p.nowOrDefault().UTC().Format(time.RFC3339)

	setParts := []string{
		attrSlackBotToken + " = :token",
		attrSlackBotTokenDK + " = :dk",
		attrSlackBotUpdatedAt + " = :now",
		attrSlackBotInstalledAt + " = if_not_exists(" + attrSlackBotInstalledAt + ", :now)",
	}
	values := map[string]ddbtypes.AttributeValue{
		":token": &ddbtypes.AttributeValueMemberB{Value: ct},
		":dk":    &ddbtypes.AttributeValueMemberB{Value: wrapped},
		":now":   &ddbtypes.AttributeValueMemberS{Value: now},
	}
	var removeParts []string
	setStringAttr := func(attr, token, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			removeParts = append(removeParts, attr)
			return
		}
		setParts = append(setParts, attr+" = "+token)
		values[token] = &ddbtypes.AttributeValueMemberS{Value: value}
	}
	setStringAttr(attrSlackBotInstalledBy, ":installed_by", install.InstalledBy)
	setStringAttr(attrSlackBotUserID, ":bot_user_id", install.BotUserID)
	setStringAttr(attrSlackAppID, ":app_id", install.AppID)
	setStringAttr(attrSlackEnterpriseID, ":enterprise_id", install.EnterpriseID)

	scopes := normalizedStringSet(install.Scopes)
	if len(scopes) > 0 {
		setParts = append(setParts, attrSlackBotScopes+" = :scopes")
		values[":scopes"] = &ddbtypes.AttributeValueMemberSS{Value: scopes}
	} else {
		removeParts = append(removeParts, attrSlackBotScopes)
	}

	expr := "SET " + strings.Join(setParts, ", ")
	if len(removeParts) > 0 {
		expr += " REMOVE " + strings.Join(removeParts, ", ")
	}

	if _, err := p.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(p.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTeamID: &ddbtypes.AttributeValueMemberS{Value: workspaceID},
		},
		UpdateExpression:          aws.String(expr),
		ExpressionAttributeValues: values,
	}); err != nil {
		return fmt.Errorf("DDBProvider.SetSlackBotToken: UpdateItem: %w", err)
	}
	slog.Info("DDBProvider.SetSlackBotToken stored Slack app bot token metadata", // #nosec G706 -- Slack IDs are structured slog attributes; JSON handlers escape control bytes.
		"workspace_id", workspaceID,
		"installed_by", install.InstalledBy,
		"bot_user_id", install.BotUserID,
		"app_id", install.AppID,
		"enterprise_id", install.EnterpriseID,
		"scope_count", len(scopes))
	return nil
}

func normalizedStringSet(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// DeleteAPIKey removes the per-workspace row. Used by the uninstall /
// disconnect flow (not implemented yet — left here so the next PR can
// wire `/qurl uninstall` to it without re-opening this file).
func (p *DDBProvider) DeleteAPIKey(ctx context.Context, workspaceID string) error {
	if workspaceID == "" {
		return errors.New("DDBProvider.DeleteAPIKey: workspaceID is empty")
	}
	if _, err := p.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(p.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTeamID: &ddbtypes.AttributeValueMemberS{Value: workspaceID},
		},
	}); err != nil {
		return fmt.Errorf("DDBProvider.DeleteAPIKey: DeleteItem: %w", err)
	}
	p.invalidateAPIKeyCache(workspaceID)
	return nil
}

// --- KMSEncryptor ----------------------------------------------------------

// KMSClient is the slice of *kms.Client the encryptor uses. Exposed as
// an interface for the same testability reason as DynamoDBClient.
type KMSClient interface {
	GenerateDataKey(ctx context.Context, params *kms.GenerateDataKeyInput, optFns ...func(*kms.Options)) (*kms.GenerateDataKeyOutput, error)
	Decrypt(ctx context.Context, params *kms.DecryptInput, optFns ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

// KMSEncryptor implements FieldEncryptor using AWS KMS envelope
// encryption + AES-256-GCM. See package doc for the threat model.
type KMSEncryptor struct {
	Client KMSClient
	KeyID  string // CMK ARN
}

// gcmNonceSize is fixed at 12 bytes per RFC 5116 §3.2; we prepend it to
// the ciphertext on the wire.
const gcmNonceSize = 12

// Seal returns (nonce||ciphertext, wrappedDataKey, nil). aad is fed as
// AES-GCM additional-authenticated-data so a ciphertext stored under
// the wrong workspaceID won't decrypt — defense against an attacker
// with DDB write access swapping ciphertexts between rows. Empty aad
// is rejected: the workspace_id binding is the entire point of this
// surface and a Seal(...,nil) would burn a CMK quota for no posture.
func (e *KMSEncryptor) Seal(ctx context.Context, plaintext, aad []byte) (ciphertext, wrappedKey []byte, err error) {
	if len(plaintext) == 0 {
		// Symmetric with APIKey's empty-plaintext guard: refuse to
		// seal zero bytes so the contract lives at one layer rather
		// than discovering the failure at decrypt time.
		return nil, nil, errors.New("KMSEncryptor.Seal: empty plaintext")
	}
	if len(aad) == 0 {
		return nil, nil, errors.New("KMSEncryptor.Seal: aad is required for workspace_id binding")
	}
	dkOut, err := e.Client.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{
		KeyId:             aws.String(e.KeyID),
		KeySpec:           kmstypes.DataKeySpecAes256,
		EncryptionContext: map[string]string{kmsWorkspaceAADKey: string(aad)},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("KMSEncryptor.Seal: GenerateDataKey: %w", err)
	}
	defer zero(dkOut.Plaintext)

	block, err := aes.NewCipher(dkOut.Plaintext)
	if err != nil {
		return nil, nil, fmt.Errorf("KMSEncryptor.Seal: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("KMSEncryptor.Seal: new GCM: %w", err)
	}
	nonce := make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("KMSEncryptor.Seal: read nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, plaintext, aad)
	return sealed, dkOut.CiphertextBlob, nil
}

// Open undoes Seal. AAD must match the value passed at seal-time.
func (e *KMSEncryptor) Open(ctx context.Context, ciphertext, wrappedKey, aad []byte) ([]byte, error) {
	if len(aad) == 0 {
		return nil, errors.New("KMSEncryptor.Open: aad is required for workspace_id binding")
	}
	if len(ciphertext) < gcmNonceSize {
		return nil, errors.New("KMSEncryptor.Open: ciphertext shorter than nonce")
	}
	decOut, err := e.Client.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob:    wrappedKey,
		KeyId:             aws.String(e.KeyID),
		EncryptionContext: map[string]string{kmsWorkspaceAADKey: string(aad)},
	})
	if err != nil {
		return nil, fmt.Errorf("KMSEncryptor.Open: KMS Decrypt: %w", err)
	}
	defer zero(decOut.Plaintext)
	// Explicit length check: aes.NewCipher will error on wrong-sized
	// key (loud), but pinning the expected 32-byte (AES-256) size here
	// catches a future regression — e.g., a misconfigured key spec
	// that returned a 16-byte key — at a more useful stack frame.
	if len(decOut.Plaintext) != 32 {
		// 32 bytes is the AES_256 KeySpec contract. A different size
		// almost always means the KMS-side KeySpec is misconfigured;
		// surface that in the error to skip a layer of triage.
		return nil, fmt.Errorf("KMSEncryptor.Open: data key has wrong size %d (want 32 for AES_256 KeySpec)", len(decOut.Plaintext))
	}

	block, err := aes.NewCipher(decOut.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("KMSEncryptor.Open: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("KMSEncryptor.Open: new GCM: %w", err)
	}
	nonce, body := ciphertext[:gcmNonceSize], ciphertext[gcmNonceSize:]
	pt, err := aead.Open(nil, nonce, body, aad)
	if err != nil {
		return nil, fmt.Errorf("KMSEncryptor.Open: GCM Open: %w", err)
	}
	return pt, nil
}

// zero wipes a buffer in place. Defense-in-depth: a goroutine stack /
// heap dump immediately after a Seal/Open call shouldn't surface a
// plaintext data key in the SDK-returned slice.
//
// Caveat: this scrubs only the AWS-SDK-returned buffer. aes.NewCipher
// copies the key into the cipher's internal key schedule, and Go's
// compiler may keep that copy alive past the defer — Go has no
// guarantee on key-material zeroization. Treat this as best-effort,
// not a hard secrecy boundary.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
