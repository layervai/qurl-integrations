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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/layervai/qurl-integrations/internal/ttlcache"

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
	apiKeyCacheTTL                     = 5 * time.Minute
	apiKeyCacheSweepEvery              = time.Minute
	apiKeySharedContextErrorRetryLimit = 1
	apiKeyValidationRecheckLimit       = 3

	apiKeyValidationProjectionKey        = "#api_key"
	apiKeyValidationProjectionDataKey    = "#data_key"
	apiKeyValidationProjectionExpression = apiKeyValidationProjectionKey + ", " + apiKeyValidationProjectionDataKey
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
// column. It owns mutex-protected cache state and must not be copied after
// first use; construct and share it as *DDBProvider.
type DDBProvider struct {
	Client    DynamoDBClient
	TableName string
	Encryptor FieldEncryptor

	// Cache successful APIKey lookups at the DDB/decrypt boundary with the
	// shared TTL singleflight helper. Cache hits still perform a strongly
	// consistent DDB projection read of the encrypted key material before
	// returning plaintext; that trades steady-state DDB-read avoidance for
	// bounded cross-instance rotation/revocation staleness without putting KMS
	// Decrypt back on the steady-state slash-command path. Definitive
	// changed/deleted validation results evict the cache; validation transport
	// errors fall back to the cached key so a DDB blip does not become a full
	// Slack outage. The decrypted key remains in heap memory for the TTL as the
	// accepted KMS/latency trade-off. Validation reads are strongly consistent
	// so sibling rotations/deletes take effect immediately when DDB is reachable
	// rather than after eventual-read replication lag.
	// The validation token is intentionally tied to encrypted key material, so a
	// future same-plaintext rewrap would invalidate warm entries and re-run KMS
	// decrypts even though the API key value did not change.
	// SetAPIKey seeds this process after a successful write. DeleteAPIKey evicts
	// this process and forces strongly-consistent refills for one TTL so a
	// same-process eventually-consistent post-delete read cannot re-cache the
	// revoked key.
	apiKeyCacheOnce   sync.Once
	apiKeyLookupCache *ttlcache.Cache[cachedAPIKey]

	// Protected by apiKeyLookupCache's mutex through ttlcache hooks. Generation
	// entries are retained by the helper after invalidation even after an
	// in-flight slot is deleted: that slot deletion detaches the old owner,
	// which may still finish later, so resetting the generation to zero could
	// let it cache an old key. The generation map grows for each workspace
	// seeded or invalidated in this process and is retained for the process
	// lifetime.
	apiKeyStrongReadUntil map[string]time.Time
	apiKeyValidationCalls map[string]*apiKeyValidationCall

	// Now is injected so tests can pin the wall clock for configured_at /
	// updated_at assertions without poking package-global state. Defaults
	// to time.Now.
	Now func() time.Time
}

type cachedAPIKey struct {
	apiKey     string
	cacheToken apiKeyCacheToken
}

type apiKeyLookupStart struct {
	apiKey     string
	cacheToken apiKeyCacheToken
	// Cached API-key hits only store successful lookups, so err is expected
	// to be nil. Keeping the Result shape here mirrors ttlcache and makes
	// that contract explicit at the call site.
	err            error
	hit            bool
	call           *ttlcache.Call[cachedAPIKey]
	owner          bool
	generation     uint64
	consistentRead bool
}

type apiKeyValidationCall struct {
	done       chan struct{}
	cacheToken apiKeyCacheToken
	current    bool
	err        error
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

func (p *DDBProvider) apiKeyCache() *ttlcache.Cache[cachedAPIKey] {
	p.apiKeyCacheOnce.Do(func() {
		p.apiKeyLookupCache = ttlcache.New[cachedAPIKey](ttlcache.Options[cachedAPIKey]{
			SweepEvery: apiKeyCacheSweepEvery,
			OnSweep: func(at time.Time) {
				// OnSweep runs under the ttlcache lock; keep this hook
				// non-reentrant and limited to the strong-read sidecar.
				for workspaceID, until := range p.apiKeyStrongReadUntil {
					if !at.Before(until) {
						delete(p.apiKeyStrongReadUntil, workspaceID)
					}
				}
			},
			OnEvict: func(workspaceID string, _ ttlcache.Result[cachedAPIKey]) {
				if p.apiKeyValidationCalls != nil {
					delete(p.apiKeyValidationCalls, workspaceID)
				}
			},
		})
	})
	return p.apiKeyLookupCache
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
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("DDBProvider.APIKey: %w", err)
	}

	sharedContextErrorRetries := 0
	validationContextErrorRetries := 0
	validationRechecks := 0
	for {
		now := p.nowOrDefault()
		start := p.getOrStartAPIKeyLookup(workspaceID, now)
		if start.hit {
			apiKey, done, err := p.apiKeyFromValidatedCache(ctx, workspaceID, &start, &validationContextErrorRetries, &validationRechecks)
			if !done {
				continue
			}
			return apiKey, err
		}
		if !start.owner {
			select {
			case <-start.call.Done():
				result := start.call.Result()
				if shouldRetryAPIKeyLookupAfterSharedError(ctx, result.Err, sharedContextErrorRetries) {
					sharedContextErrorRetries++
					continue
				}
				return result.Value.apiKey, result.Err
			case <-ctx.Done():
				return "", fmt.Errorf("DDBProvider.APIKey: %w", ctx.Err())
			}
		}

		return p.fetchAndFinishAPIKeyLookup(ctx, workspaceID, start.call, start.generation, start.consistentRead)
	}
}

// apiKeyFromValidatedCache returns done=false when validation observed a
// retryable state and the caller should re-enter APIKey's lookup loop. When
// done=true, apiKey/retErr are final for this APIKey call.
func (p *DDBProvider) apiKeyFromValidatedCache(ctx context.Context, workspaceID string, start *apiKeyLookupStart, validationContextErrorRetries, validationRechecks *int) (apiKey string, done bool, retErr error) {
	ok, err := p.validateCachedAPIKey(ctx, workspaceID, start.cacheToken)
	if err != nil {
		fallbackAPIKey, fallbackDone, fallbackErr := apiKeyAfterCacheValidationError(ctx, start.apiKey, err, validationContextErrorRetries)
		if !fallbackDone {
			return "", false, nil
		}
		if fallbackErr == nil && !p.cachedAPIKeyStillLocal(workspaceID, start.cacheToken) {
			if err := retryAPIKeyCacheValidationLoop(ctx, workspaceID, validationRechecks); err != nil {
				return "", true, err
			}
			return "", false, nil
		}
		return fallbackAPIKey, true, fallbackErr
	}
	if !ok {
		// Token mismatch/delete sets a strong-read refill marker, so the next
		// loop either observes current DDB state or a newer writer's token.
		p.evictAPIKeyCacheIfToken(workspaceID, start.cacheToken, p.nowOrDefault().Add(apiKeyCacheTTL))
		if err := retryAPIKeyCacheValidationLoop(ctx, workspaceID, validationRechecks); err != nil {
			return "", true, err
		}
		return "", false, nil
	}
	if !p.cachedAPIKeyStillLocal(workspaceID, start.cacheToken) {
		// A local writer replaced this token after validation; re-loop to
		// validate the live cache entry instead of returning stale plaintext.
		if err := retryAPIKeyCacheValidationLoop(ctx, workspaceID, validationRechecks); err != nil {
			return "", true, err
		}
		return "", false, nil
	}
	return start.apiKey, true, nil
}

func retryAPIKeyCacheValidationLoop(ctx context.Context, workspaceID string, rechecks *int) error {
	// One budget covers all validation re-loop reasons because each means this
	// caller has not yet validated the currently local cache token.
	if *rechecks >= apiKeyValidationRecheckLimit {
		slog.WarnContext(ctx, "DDBProvider.APIKey cache validation did not converge",
			slog.String("workspace_id", workspaceID),
			slog.Int("rechecks", *rechecks),
			slog.Int("limit", apiKeyValidationRecheckLimit),
		)
		return errors.New("DDBProvider.APIKey: cache validation did not converge")
	}
	*rechecks++
	return nil
}

func apiKeyAfterCacheValidationError(ctx context.Context, cachedKey string, err error, contextErrorRetries *int) (apiKey string, done bool, retErr error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", true, fmt.Errorf("DDBProvider.APIKey: %w", ctxErr)
	}
	if shouldRetryAPIKeyLookupAfterSharedError(ctx, err, *contextErrorRetries) {
		*contextErrorRetries++
		return "", false, nil
	}
	return cachedKey, true, nil
}

func shouldRetryAPIKeyLookupAfterSharedError(ctx context.Context, err error, retries int) bool {
	if err == nil || ctx.Err() != nil || retries >= apiKeySharedContextErrorRetryLimit {
		return false
	}
	// We cannot distinguish the owner's caller being canceled from a lower
	// layer surfacing the same context error during a DDB brownout. Retrying
	// keeps healthy waiters from inheriting a dead owner's context, and each
	// retry re-enters singleflight as a new owner so extra DDB pressure is
	// sequential rather than a fan-out spike. Keep the retry count tiny so a
	// persistent context-like lower-layer error cannot spin on a healthy caller.
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (p *DDBProvider) getOrStartAPIKeyLookup(workspaceID string, now time.Time) apiKeyLookupStart {
	start, consistentRead := ttlcache.GetOrStartWith[cachedAPIKey, bool](p.apiKeyCache(), workspaceID, now, func() bool {
		// GetOrStartWith runs this hook under the ttlcache lock; keep it
		// non-reentrant and limited to the strong-read sidecar.
		// The hook also runs for waiters so all callers observe and clean up
		// strong-read sidecar state under the same lock, even though only a
		// fill owner passes the flag into DDB.
		if p.apiKeyStrongReadUntil == nil {
			return false
		}
		until, ok := p.apiKeyStrongReadUntil[workspaceID]
		if !ok {
			return false
		}
		if now.Before(until) {
			return true
		}
		delete(p.apiKeyStrongReadUntil, workspaceID)
		return false
	})
	if start.Hit {
		return apiKeyLookupStart{apiKey: start.Result.Value.apiKey, cacheToken: start.Result.Value.cacheToken, err: start.Result.Err, hit: true}
	}
	if !start.Owner {
		return apiKeyLookupStart{call: start.Call}
	}
	return apiKeyLookupStart{call: start.Call, owner: true, generation: start.Generation, consistentRead: consistentRead}
}

func (p *DDBProvider) fetchAndFinishAPIKeyLookup(ctx context.Context, workspaceID string, call *ttlcache.Call[cachedAPIKey], generation uint64, consistentRead bool) (string, error) {
	result := ttlcache.Result[cachedAPIKey]{}
	finished := false
	defer func() {
		if rec := recover(); rec != nil {
			if !finished {
				result = ttlcache.Result[cachedAPIKey]{Err: errors.New("DDBProvider.APIKey: lookup panicked")}
				p.apiKeyCache().Finish(workspaceID, call, result, 0, p.nowOrDefault(), generation)
			}
			panic(rec)
		}
	}()

	apiKey, cacheToken, err := p.fetchAPIKey(ctx, workspaceID, consistentRead)
	result = ttlcache.Result[cachedAPIKey]{Value: cachedAPIKey{apiKey: apiKey, cacheToken: cacheToken}, Err: err}
	finished = true
	cacheTTL := time.Duration(0)
	if err == nil {
		cacheTTL = apiKeyCacheTTL
	}
	p.apiKeyCache().Finish(workspaceID, call, result, cacheTTL, p.nowOrDefault(), generation)
	return apiKey, err
}

func (p *DDBProvider) fetchAPIKey(ctx context.Context, workspaceID string, consistentRead bool) (string, apiKeyCacheToken, error) {
	// Eventually-consistent read is correct here: the per-workspace key
	// only changes on (re-)install, and the few-ms propagation delay is
	// inside the same "click the setup link" window. Cache-hit validation is
	// stricter because #766 is specifically about prompt cross-instance
	// revocation/rotation visibility. DeleteAPIKey temporarily asks for a strong
	// read so this process cannot re-cache a just-deleted key from an
	// eventually-consistent replica.
	input := &dynamodb.GetItemInput{
		TableName: aws.String(p.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTeamID: &ddbtypes.AttributeValueMemberS{Value: workspaceID},
		},
	}
	if consistentRead {
		input.ConsistentRead = aws.Bool(true)
	}
	out, err := p.Client.GetItem(ctx, input)
	if err != nil {
		return "", apiKeyCacheToken{}, fmt.Errorf("DDBProvider.APIKey: GetItem: %w", err)
	}
	if out == nil || len(out.Item) == 0 {
		return "", apiKeyCacheToken{}, fmt.Errorf("DDBProvider.APIKey: workspace %q: %w", workspaceID, ErrWorkspaceNotConfigured)
	}

	ctBlob, ok := out.Item[attrQURLAPIKey].(*ddbtypes.AttributeValueMemberB)
	if !ok || len(ctBlob.Value) == 0 {
		return "", apiKeyCacheToken{}, fmt.Errorf("DDBProvider.APIKey: workspace %q: %w", workspaceID, ErrWorkspaceNotConfigured)
	}
	wrappedKey, ok := out.Item[attrDataKeyCT].(*ddbtypes.AttributeValueMemberB)
	if !ok || len(wrappedKey.Value) == 0 {
		return "", apiKeyCacheToken{}, errors.New("DDBProvider.APIKey: stored item missing or has wrong type for qurl_api_key_dk")
	}

	pt, err := p.Encryptor.Open(ctx, ctBlob.Value, wrappedKey.Value, []byte(workspaceID))
	if err != nil {
		return "", apiKeyCacheToken{}, fmt.Errorf("DDBProvider.APIKey: decrypt: %w", err)
	}
	// Empty plaintext means the ciphertext decrypted but to zero bytes —
	// corruption / truncate / unsigned-store-bypass. Fail loud here rather
	// than handing the caller "" and watching qurl-service surface an
	// opaque 401.
	if len(pt) == 0 {
		return "", apiKeyCacheToken{}, errors.New("DDBProvider.APIKey: decrypted plaintext is empty")
	}
	return string(pt), newAPIKeyCacheToken(ctBlob.Value, wrappedKey.Value), nil
}

type apiKeyCacheToken [sha256.Size]byte

func newAPIKeyCacheToken(ciphertext, wrappedKey []byte) apiKeyCacheToken {
	h := sha256.New()
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(ciphertext)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(ciphertext)
	binary.BigEndian.PutUint64(length[:], uint64(len(wrappedKey)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(wrappedKey)
	var token apiKeyCacheToken
	copy(token[:], h.Sum(nil))
	return token
}

func (p *DDBProvider) cachedAPIKeyStillCurrent(ctx context.Context, workspaceID string, cacheToken apiKeyCacheToken) (bool, error) {
	out, err := p.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(p.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTeamID: &ddbtypes.AttributeValueMemberS{Value: workspaceID},
		},
		ProjectionExpression: aws.String(apiKeyValidationProjectionExpression),
		ExpressionAttributeNames: map[string]string{
			apiKeyValidationProjectionKey:     attrQURLAPIKey,
			apiKeyValidationProjectionDataKey: attrDataKeyCT,
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return false, fmt.Errorf("DDBProvider.APIKey: cache validation GetItem: %w", err)
	}
	if out == nil || len(out.Item) == 0 {
		return false, nil
	}
	ctBlob, ok := out.Item[attrQURLAPIKey].(*ddbtypes.AttributeValueMemberB)
	if !ok || len(ctBlob.Value) == 0 {
		return false, nil
	}
	wrappedKey, ok := out.Item[attrDataKeyCT].(*ddbtypes.AttributeValueMemberB)
	if !ok || len(wrappedKey.Value) == 0 {
		return false, nil
	}
	return newAPIKeyCacheToken(ctBlob.Value, wrappedKey.Value) == cacheToken, nil
}

func (p *DDBProvider) validateCachedAPIKey(ctx context.Context, workspaceID string, cacheToken apiKeyCacheToken) (bool, error) {
	call, owner := p.getOrStartAPIKeyValidation(workspaceID, cacheToken)
	if !owner {
		select {
		case <-call.done:
			return call.current, call.err
		case <-ctx.Done():
			return false, fmt.Errorf("DDBProvider.APIKey: %w", ctx.Err())
		}
	}

	finished := false
	defer func() {
		if rec := recover(); rec != nil {
			if !finished {
				p.finishAPIKeyValidation(workspaceID, call, false, errors.New("DDBProvider.APIKey: cache validation panicked"))
			}
			panic(rec)
		}
	}()
	current, err := p.cachedAPIKeyStillCurrent(ctx, workspaceID, cacheToken)
	finished = true
	p.finishAPIKeyValidation(workspaceID, call, current, err)
	return current, err
}

func (p *DDBProvider) getOrStartAPIKeyValidation(workspaceID string, cacheToken apiKeyCacheToken) (*apiKeyValidationCall, bool) {
	var call *apiKeyValidationCall
	owner := false
	p.apiKeyCache().WithLock(func() {
		if p.apiKeyValidationCalls == nil {
			p.apiKeyValidationCalls = map[string]*apiKeyValidationCall{}
		}
		if existing, ok := p.apiKeyValidationCalls[workspaceID]; ok && existing.cacheToken == cacheToken {
			call = existing
			return
		}
		call = &apiKeyValidationCall{
			done:       make(chan struct{}),
			cacheToken: cacheToken,
		}
		p.apiKeyValidationCalls[workspaceID] = call
		owner = true
	})
	return call, owner
}

func (p *DDBProvider) finishAPIKeyValidation(workspaceID string, call *apiKeyValidationCall, current bool, err error) {
	p.apiKeyCache().WithLock(func() {
		call.current = current
		call.err = err
		if p.apiKeyValidationCalls[workspaceID] == call {
			delete(p.apiKeyValidationCalls, workspaceID)
		}
		close(call.done)
	})
}

func (p *DDBProvider) invalidateAPIKeyCache(workspaceID string, strongReadUntil time.Time) {
	if strings.TrimSpace(workspaceID) == "" {
		return
	}
	p.apiKeyCache().InvalidateWith(workspaceID, func() {
		// InvalidateWith runs this hook under the ttlcache lock; keep it
		// non-reentrant and limited to the strong-read sidecar.
		if !strongReadUntil.IsZero() {
			if p.apiKeyStrongReadUntil == nil {
				p.apiKeyStrongReadUntil = map[string]time.Time{}
			}
			p.apiKeyStrongReadUntil[workspaceID] = strongReadUntil
		}
	})
}

func (p *DDBProvider) evictAPIKeyCacheIfToken(workspaceID string, cacheToken apiKeyCacheToken, strongReadUntil time.Time) {
	if strings.TrimSpace(workspaceID) == "" {
		return
	}
	p.apiKeyCache().InvalidateIfWith(workspaceID, func(result ttlcache.Result[cachedAPIKey]) bool {
		return result.Value.cacheToken == cacheToken
	}, func() {
		if !strongReadUntil.IsZero() {
			if p.apiKeyStrongReadUntil == nil {
				p.apiKeyStrongReadUntil = map[string]time.Time{}
			}
			p.apiKeyStrongReadUntil[workspaceID] = strongReadUntil
		}
	})
}

func (p *DDBProvider) cachedAPIKeyStillLocal(workspaceID string, cacheToken apiKeyCacheToken) bool {
	return p.apiKeyCache().CachedResultMatches(workspaceID, func(result ttlcache.Result[cachedAPIKey]) bool {
		return result.Value.cacheToken == cacheToken
	})
}

func (p *DDBProvider) seedAPIKeyCache(workspaceID, apiKey string, cacheToken apiKeyCacheToken, now time.Time) {
	if strings.TrimSpace(workspaceID) == "" {
		return
	}
	p.apiKeyCache().SeedWith(workspaceID, ttlcache.Result[cachedAPIKey]{Value: cachedAPIKey{apiKey: apiKey, cacheToken: cacheToken}}, apiKeyCacheTTL, now, func() {
		// SeedWith runs this hook under the ttlcache lock; keep it
		// non-reentrant and limited to the strong-read sidecar.
		if p.apiKeyStrongReadUntil != nil {
			delete(p.apiKeyStrongReadUntil, workspaceID)
		}
	})
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
// of PutItem so Slack app install metadata in the same row is preserved. The
// apiKey value is stored exactly as minted by qurl-service; APIKey returns the
// same plaintext without trimming.
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

	now := p.nowOrDefault()
	nowString := now.UTC().Format(time.RFC3339)
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
			":now": &ddbtypes.AttributeValueMemberS{Value: nowString},
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
	p.seedAPIKeyCache(workspaceID, apiKey, newAPIKeyCacheToken(ct, wrapped), now)
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

// SupportsDeleteAPIKey reports that DDBProvider can mutate workspace key state.
func (p *DDBProvider) SupportsDeleteAPIKey() bool {
	return true
}

// DeleteAPIKey removes the qURL API key columns while preserving Slack app
// install metadata in the same row. It returns [ErrWorkspaceNotConfigured] when
// the workspace has no stored qURL key.
//
// TODO(#792): this local disconnect cannot revoke the upstream qURL key until
// setup persists the qurl-service key_id for workspace keys.
func (p *DDBProvider) DeleteAPIKey(ctx context.Context, workspaceID string) error {
	if workspaceID == "" {
		return errors.New("DDBProvider.DeleteAPIKey: workspaceID is empty")
	}
	now := p.nowOrDefault()
	_, err := p.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(p.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTeamID: &ddbtypes.AttributeValueMemberS{Value: workspaceID},
		},
		UpdateExpression: aws.String("SET #updated_at = :now REMOVE #qurl_api_key, #qurl_api_key_dk, #configured_by, #configured_at"),
		ConditionExpression: aws.String(
			"attribute_exists(#qurl_api_key)",
		),
		ExpressionAttributeNames: map[string]string{
			"#qurl_api_key":    attrQURLAPIKey,
			"#qurl_api_key_dk": attrDataKeyCT,
			"#configured_by":   attrConfiguredBy,
			"#configured_at":   attrConfiguredAt,
			"#updated_at":      attrUpdatedAt,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":now": &ddbtypes.AttributeValueMemberS{Value: now.UTC().Format(time.RFC3339)},
		},
	})
	if err != nil {
		var missing *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &missing) {
			return fmt.Errorf("DDBProvider.DeleteAPIKey: workspace %q: %w", workspaceID, ErrWorkspaceNotConfigured)
		}
		return fmt.Errorf("DDBProvider.DeleteAPIKey: UpdateItem: %w", err)
	}
	p.invalidateAPIKeyCache(workspaceID, now.Add(apiKeyCacheTTL))
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
