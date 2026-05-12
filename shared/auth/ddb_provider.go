// Package auth — DDB-backed Provider for per-workspace API keys.
//
// DDBProvider stores per-workspace qURL API keys in the `workspace_state`
// DynamoDB table, with the key column encrypted at the field level. The
// PK (`team_id`) stays plaintext so GetItem can dispatch on it; only
// `qurl_api_key` is wrapped in ciphertext.
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
// crypto/cipher + the KMS SDK because that SDK doesn't ship a maintained
// Go binding. TODO(claude-followup): if AWS publishes a Go binding of
// the DB Encryption SDK, port this to it so that we get the
// canonical attribute-level encrypt/sign/material-provider story
// (column-level "do not encrypt" / "sign only" / "encrypt and sign"
// declarations) for free.
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

// ErrWorkspaceNotConfigured is the sentinel returned by APIKey when the
// workspace has no row in the workspace_state table (i.e. the admin has
// not yet completed /oauth/qurl/start → /callback). The Slack handler
// catches this distinctly so the bot can prompt the user to visit
// /oauth/qurl/start rather than rendering a generic "auth failed" error.
var ErrWorkspaceNotConfigured = errors.New("workspace not configured — admin must complete /oauth/qurl/start")

// DynamoDBClient is the slice of *dynamodb.Client the provider actually
// uses. Exposed as an interface so tests can inject a fake without
// spinning up localstack.
type DynamoDBClient interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
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

	// Now is injected so tests can pin the wall clock for configured_at /
	// updated_at assertions without poking package-global state. Defaults
	// to time.Now.
	Now func() time.Time
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
	if len(out.Item) == 0 {
		return "", fmt.Errorf("DDBProvider.APIKey: workspace %q: %w", workspaceID, ErrWorkspaceNotConfigured)
	}

	ctBlob, ok := out.Item[attrQURLAPIKey].(*ddbtypes.AttributeValueMemberB)
	if !ok || len(ctBlob.Value) == 0 {
		return "", errors.New("DDBProvider.APIKey: stored item missing or has wrong type for qurl_api_key")
	}
	wrappedKey, ok := out.Item[attrDataKeyCT].(*ddbtypes.AttributeValueMemberB)
	if !ok || len(wrappedKey.Value) == 0 {
		return "", errors.New("DDBProvider.APIKey: stored item missing or has wrong type for qurl_api_key_dk")
	}

	pt, err := p.Encryptor.Open(ctx, ctBlob.Value, wrappedKey.Value, []byte(workspaceID))
	if err != nil {
		return "", fmt.Errorf("DDBProvider.APIKey: decrypt: %w", err)
	}
	return string(pt), nil
}

// SetAPIKey upserts the per-workspace API key. The configuredBy field
// is informational (the Slack user_id of the admin who completed
// /oauth/qurl/callback) and is persisted plaintext.
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

	// Pre-flight GetItem serves two purposes on this rare path:
	//   1. Preserve the original configured_at on key rotation — otherwise
	//      every rotation would overwrite the first-install timestamp and
	//      the audit trail loses the install date.
	//   2. Emit a warn line on overwrite so operator dashboards can
	//      distinguish first-installs from rotation churn.
	// The install path is invoked once per workspace, so doubling the RTT
	// here is a non-issue.
	existing, getErr := p.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(p.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrTeamID: &ddbtypes.AttributeValueMemberS{Value: workspaceID},
		},
	})
	overwriting := getErr == nil && existing != nil && len(existing.Item) > 0

	now := p.Now().UTC().Format(time.RFC3339)
	configuredAt := now
	if overwriting {
		if prev, ok := existing.Item[attrConfiguredAt].(*ddbtypes.AttributeValueMemberS); ok && prev.Value != "" {
			configuredAt = prev.Value
		}
	}
	item := map[string]ddbtypes.AttributeValue{
		attrTeamID:       &ddbtypes.AttributeValueMemberS{Value: workspaceID},
		attrQURLAPIKey:   &ddbtypes.AttributeValueMemberB{Value: ct},
		attrDataKeyCT:    &ddbtypes.AttributeValueMemberB{Value: wrapped},
		attrConfiguredBy: &ddbtypes.AttributeValueMemberS{Value: configuredBy},
		attrUpdatedAt:    &ddbtypes.AttributeValueMemberS{Value: now},
		attrConfiguredAt: &ddbtypes.AttributeValueMemberS{Value: configuredAt},
	}

	if _, err := p.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(p.TableName),
		Item:      item,
	}); err != nil {
		return fmt.Errorf("DDBProvider.SetAPIKey: PutItem: %w", err)
	}
	if overwriting {
		slog.Warn("DDBProvider.SetAPIKey overwrote existing workspace row",
			"workspace_id", workspaceID,
			"configured_by", configuredBy)
	}
	return nil
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
// with DDB write access swapping ciphertexts between rows.
func (e *KMSEncryptor) Seal(ctx context.Context, plaintext, aad []byte) (ciphertext, wrappedKey []byte, err error) {
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
// plaintext data key.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
