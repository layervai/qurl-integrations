package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/internal/ttlcache"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	testTeamID        = "T123ABCDEF"
	testSlackBotToken = "xoxb-123456789012345678901234567890"
	testOldAPIKey     = "lv_live_old"
	testNewAPIKey     = "lv_live_new"
	testKeyID         = "key_123"
	testKeyPrefix     = "lv_live_abcd"
	testQURLAccount   = "auth0|qurl-acct-1"
)

// fakeDDBClient is a hand-rolled stub the table tests configure with
// predetermined results. Captures write inputs for assertion.
type fakeDDBClient struct {
	getOutput    *dynamodb.GetItemOutput
	getFunc      func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error)
	getErr       error
	getCalls     int
	getInputs    []*dynamodb.GetItemInput
	putInput     *dynamodb.PutItemInput
	putErr       error
	updateInput  *dynamodb.UpdateItemInput
	updateFunc   func(context.Context, *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error)
	updateOutput *dynamodb.UpdateItemOutput
	updateErr    error
}

func (f *fakeDDBClient) GetItem(ctx context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.getCalls++
	f.getInputs = append(f.getInputs, in)
	if f.getFunc != nil {
		return f.getFunc(ctx, in)
	}
	return f.getOutput, f.getErr
}
func (f *fakeDDBClient) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putInput = in
	return &dynamodb.PutItemOutput{}, f.putErr
}
func (f *fakeDDBClient) UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.updateInput = in
	if f.updateFunc != nil {
		return f.updateFunc(ctx, in)
	}
	if f.updateOutput != nil {
		return f.updateOutput, f.updateErr
	}
	return &dynamodb.UpdateItemOutput{}, f.updateErr
}
func (f *fakeDDBClient) DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	return &dynamodb.DeleteItemOutput{}, nil
}

// emptyPlaintextEncryptor decrypts any ciphertext to zero bytes. Used
// to exercise the "decrypted plaintext is empty" guard in APIKey
// without touching the ciphertext-too-short DDB-side check.
type emptyPlaintextEncryptor struct{}

func (emptyPlaintextEncryptor) Seal(_ context.Context, _, _ []byte) (ciphertext, wrappedKey []byte, err error) {
	return []byte("ct"), []byte(passthroughWrappedKey), nil
}
func (emptyPlaintextEncryptor) Open(_ context.Context, _, _, _ []byte) ([]byte, error) {
	return []byte{}, nil
}

// passthroughEncryptor is a no-op FieldEncryptor for tests: ciphertext
// equals plaintext, wrappedKey is a fixed marker so Open can verify it
// was actually plumbed through.
type passthroughEncryptor struct {
	sealErr error
	openErr error

	sealCalls int
	openCalls int
}

const passthroughWrappedKey = "DK"

func (p *passthroughEncryptor) Seal(_ context.Context, plaintext, _ []byte) (ciphertext, wrappedKey []byte, err error) {
	p.sealCalls++
	if p.sealErr != nil {
		return nil, nil, p.sealErr
	}
	return append([]byte(nil), plaintext...), []byte(passthroughWrappedKey), nil
}
func (p *passthroughEncryptor) Open(_ context.Context, ciphertext, wrappedKey, _ []byte) ([]byte, error) {
	p.openCalls++
	if p.openErr != nil {
		return nil, p.openErr
	}
	if string(wrappedKey) != passthroughWrappedKey {
		return nil, errors.New("wrong wrapped key")
	}
	return append([]byte(nil), ciphertext...), nil
}

func getItemConsistentRead(in *dynamodb.GetItemInput) bool {
	return in != nil && in.ConsistentRead != nil && *in.ConsistentRead
}

func getItemUsesCacheValidationProjection(in *dynamodb.GetItemInput) bool {
	return in != nil && aws.ToString(in.ProjectionExpression) == apiKeyValidationProjectionExpression
}

func requireCacheValidationProjection(t *testing.T, in *dynamodb.GetItemInput) {
	t.Helper()
	if got := aws.ToString(in.ProjectionExpression); got != apiKeyValidationProjectionExpression {
		t.Fatalf("cache validation projection = %q", got)
	}
	if got := in.ExpressionAttributeNames[apiKeyValidationProjectionKey]; got != attrQURLAPIKey {
		t.Fatalf("cache validation key alias = %q, want %q", got, attrQURLAPIKey)
	}
	if got := in.ExpressionAttributeNames[apiKeyValidationProjectionDataKey]; got != attrDataKeyCT {
		t.Fatalf("cache validation data-key alias = %q, want %q", got, attrDataKeyCT)
	}
}

func cachedAPIKeyResult(apiKey string) ttlcache.Result[cachedAPIKey] {
	return ttlcache.Result[cachedAPIKey]{
		Value: cachedAPIKey{
			apiKey:     apiKey,
			cacheToken: newAPIKeyCacheToken([]byte(apiKey), []byte(passthroughWrappedKey)),
		},
	}
}

func TestDDBProviderAPIKey(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{
				Item: map[string]ddbtypes.AttributeValue{
					attrTeamID:     &ddbtypes.AttributeValueMemberS{Value: testTeamID},
					attrQURLAPIKey: &ddbtypes.AttributeValueMemberB{Value: []byte("lv_live_xxx")},
					attrDataKeyCT:  &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
				},
			},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return time.Unix(1700000000, 0) },
		}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey: %v", err)
		}
		if got != "lv_live_xxx" {
			t.Fatalf("got %q want %q", got, "lv_live_xxx")
		}
	})

	t.Run("workspace not configured", func(t *testing.T) {
		ddb := &fakeDDBClient{getOutput: &dynamodb.GetItemOutput{Item: nil}}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		_, err := p.APIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatalf("want ErrWorkspaceNotConfigured, got %v", err)
		}
	})

	t.Run("nil get output", func(t *testing.T) {
		ddb := &fakeDDBClient{}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		_, err := p.APIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatalf("want ErrWorkspaceNotConfigured, got %v", err)
		}
	})

	t.Run("decrypt error", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{
				Item: map[string]ddbtypes.AttributeValue{
					attrTeamID:     &ddbtypes.AttributeValueMemberS{Value: testTeamID},
					attrQURLAPIKey: &ddbtypes.AttributeValueMemberB{Value: []byte("ct")},
					attrDataKeyCT:  &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
				},
			},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{openErr: errors.New("kms denied")},
		}
		_, err := p.APIKey(context.Background(), testTeamID)
		if err == nil {
			t.Fatal("want error, got nil")
		}
		if errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatal("decrypt error should not be ErrWorkspaceNotConfigured")
		}
	})

	t.Run("empty workspaceID", func(t *testing.T) {
		p := &DDBProvider{Client: &fakeDDBClient{}, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		_, err := p.APIKey(context.Background(), "")
		if err == nil {
			t.Fatal("want error, got nil")
		}
	})

	t.Run("ddb transport error", func(t *testing.T) {
		ddb := &fakeDDBClient{getErr: errors.New("ddb down")}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		_, err := p.APIKey(context.Background(), testTeamID)
		if err == nil {
			t.Fatal("want error, got nil")
		}
		if errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatal("transport error should not be ErrWorkspaceNotConfigured")
		}
	})

	t.Run("empty plaintext after decrypt", func(t *testing.T) {
		// Corrupted row scenario: ciphertext is non-empty (passes the
		// type/length check at the DDB read) but decrypts to zero bytes.
		// APIKey must fail loud rather than hand the caller "" — qurl-
		// service would otherwise surface an opaque 401.
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{
				Item: map[string]ddbtypes.AttributeValue{
					attrTeamID:     &ddbtypes.AttributeValueMemberS{Value: testTeamID},
					attrQURLAPIKey: &ddbtypes.AttributeValueMemberB{Value: []byte("non-empty-ct")},
					attrDataKeyCT:  &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
				},
			},
		}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &emptyPlaintextEncryptor{}}
		_, err := p.APIKey(context.Background(), testTeamID)
		if err == nil {
			t.Fatal("expected error on empty plaintext")
		}
	})

	t.Run("missing qurl_api_key_dk attribute", func(t *testing.T) {
		// Inverse of "missing qurl_api_key": the wrapped data key column
		// is absent. Without the KMS-wrapped key we can't decrypt; fail
		// loud rather than falling through to a nil-pointer panic on
		// Open.
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{
				Item: map[string]ddbtypes.AttributeValue{
					attrTeamID:     &ddbtypes.AttributeValueMemberS{Value: testTeamID},
					attrQURLAPIKey: &ddbtypes.AttributeValueMemberB{Value: []byte("ct")},
					// attrDataKeyCT omitted.
				},
			},
		}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		_, err := p.APIKey(context.Background(), testTeamID)
		if err == nil {
			t.Fatal("want error when qurl_api_key_dk attribute is missing")
		}
	})

	t.Run("missing qurl_api_key attribute", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{
				Item: map[string]ddbtypes.AttributeValue{
					attrTeamID: &ddbtypes.AttributeValueMemberS{Value: testTeamID},
				},
			},
		}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		_, err := p.APIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatalf("want ErrWorkspaceNotConfigured for Slack-installed but qURL-unconfigured row, got %v", err)
		}
	})

	t.Run("does not cache workspace not configured", func(t *testing.T) {
		ddb := &fakeDDBClient{getOutput: &dynamodb.GetItemOutput{Item: nil}}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		_, err := p.APIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatalf("want ErrWorkspaceNotConfigured, got %v", err)
		}

		ddb.getOutput = &dynamodb.GetItemOutput{
			Item: map[string]ddbtypes.AttributeValue{
				attrTeamID:     &ddbtypes.AttributeValueMemberS{Value: testTeamID},
				attrQURLAPIKey: &ddbtypes.AttributeValueMemberB{Value: []byte("lv_live_after_install")},
				attrDataKeyCT:  &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
			},
		}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey after install: %v", err)
		}
		if got != "lv_live_after_install" {
			t.Fatalf("got %q want %q", got, "lv_live_after_install")
		}
		if ddb.getCalls != 2 {
			t.Fatalf("missing workspace should not be cached: GetItem calls = %d, want 2", ddb.getCalls)
		}
	})
}

func TestDDBProviderAPIKeyCache(t *testing.T) {
	itemForKey := func(apiKey string) map[string]ddbtypes.AttributeValue {
		return map[string]ddbtypes.AttributeValue{
			attrTeamID:     &ddbtypes.AttributeValueMemberS{Value: testTeamID},
			attrQURLAPIKey: &ddbtypes.AttributeValueMemberB{Value: []byte(apiKey)},
			attrDataKeyCT:  &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
		}
	}

	t.Run("hit validates DDB and skips decrypt", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey("lv_live_cached")},
		}
		encryptor := &passthroughEncryptor{}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: encryptor,
			Now:       func() time.Time { return time.Unix(1700000000, 0) },
		}

		for i := 0; i < 2; i++ {
			got, err := p.APIKey(context.Background(), testTeamID)
			if err != nil {
				t.Fatalf("APIKey call %d: %v", i+1, err)
			}
			if got != "lv_live_cached" {
				t.Fatalf("call %d got %q want %q", i+1, got, "lv_live_cached")
			}
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want 2", ddb.getCalls)
		}
		if len(ddb.getInputs) != 2 || !getItemConsistentRead(ddb.getInputs[1]) {
			t.Fatalf("cached hit should validate with strongly consistent read, inputs=%v", ddb.getInputs)
		}
		requireCacheValidationProjection(t, ddb.getInputs[1])
		if encryptor.openCalls != 1 {
			t.Fatalf("Open calls = %d, want 1", encryptor.openCalls)
		}
	})

	t.Run("cache hit honors canceled context without extra DDB", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey("lv_live_cached_after_cancel")},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return time.Unix(1700000000, 0) },
		}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("prime APIKey cache: %v", err)
		}
		if got != "lv_live_cached_after_cancel" {
			t.Fatalf("prime got %q want %q", got, "lv_live_cached_after_cancel")
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err = p.APIKey(ctx, testTeamID)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cached APIKey with canceled context err = %v, want context.Canceled", err)
		}
		if got != "" {
			t.Fatalf("cached APIKey with canceled context got %q want empty", got)
		}
		if ddb.getCalls != 1 {
			t.Fatalf("GetItem calls = %d, want 1", ddb.getCalls)
		}
	})

	t.Run("does not cache DDB transport errors", func(t *testing.T) {
		ddb := &fakeDDBClient{getErr: errors.New("ddb down")}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return time.Unix(1700000000, 0) },
		}
		if _, err := p.APIKey(context.Background(), testTeamID); err == nil {
			t.Fatal("want DDB error, got nil")
		}

		ddb.getErr = nil
		ddb.getOutput = &dynamodb.GetItemOutput{Item: itemForKey("lv_live_after_ddb_error")}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey after DDB recovery: %v", err)
		}
		if got != "lv_live_after_ddb_error" {
			t.Fatalf("got %q want %q", got, "lv_live_after_ddb_error")
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want 2", ddb.getCalls)
		}
	})

	t.Run("does not cache decrypt errors", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey("lv_live_after_decrypt_error")},
		}
		encryptor := &passthroughEncryptor{openErr: errors.New("kms down")}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: encryptor,
			Now:       func() time.Time { return time.Unix(1700000000, 0) },
		}
		if _, err := p.APIKey(context.Background(), testTeamID); err == nil {
			t.Fatal("want decrypt error, got nil")
		}

		encryptor.openErr = nil
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey after decrypt recovery: %v", err)
		}
		if got != "lv_live_after_decrypt_error" {
			t.Fatalf("got %q want %q", got, "lv_live_after_decrypt_error")
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want 2", ddb.getCalls)
		}
		if encryptor.openCalls != 2 {
			t.Fatalf("Open calls = %d, want 2", encryptor.openCalls)
		}
	})

	t.Run("expired entry refreshes from DDB", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		now := time.Unix(1700000000, 0)
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return now },
		}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("first APIKey: %v", err)
		}
		if got != testOldAPIKey {
			t.Fatalf("got %q want %q", got, testOldAPIKey)
		}

		ddb.getFunc = func(_ context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if getItemConsistentRead(in) {
				return &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}, nil
			}
			return &dynamodb.GetItemOutput{Item: itemForKey(testNewAPIKey)}, nil
		}
		now = now.Add(apiKeyCacheTTL - time.Second)
		got, err = p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("cached APIKey: %v", err)
		}
		if got != testOldAPIKey {
			t.Fatalf("cached call got %q want %q", got, testOldAPIKey)
		}

		now = now.Add(2 * time.Second)
		got, err = p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("expired APIKey: %v", err)
		}
		if got != testNewAPIKey {
			t.Fatalf("expired call got %q want %q", got, testNewAPIKey)
		}
		if ddb.getCalls != 3 {
			t.Fatalf("GetItem calls = %d, want 3", ddb.getCalls)
		}
	})

	t.Run("expired entry is deleted on read miss", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		ddb := &fakeDDBClient{getErr: errors.New("ddb down")}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return now },
		}
		p.apiKeyCache().Seed(testTeamID, cachedAPIKeyResult(testOldAPIKey), apiKeyCacheTTL, now.Add(-apiKeyCacheTTL-time.Second))

		if _, err := p.APIKey(context.Background(), testTeamID); err == nil {
			t.Fatal("want DDB error, got nil")
		}
		if ddb.getCalls != 1 {
			t.Fatalf("GetItem calls after expired miss = %d, want 1", ddb.getCalls)
		}

		ddb.getErr = nil
		ddb.getOutput = &dynamodb.GetItemOutput{Item: itemForKey(testNewAPIKey)}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey after expired miss recovery: %v", err)
		}
		if got != testNewAPIKey {
			t.Fatalf("got %q want %q", got, testNewAPIKey)
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls after recovery = %d, want 2", ddb.getCalls)
		}
	})

	t.Run("sweeps expired entries during lookup", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		ddb := &fakeDDBClient{}
		ddb.getFunc = func(_ context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if getItemConsistentRead(in) {
				return &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}, nil
			}
			return &dynamodb.GetItemOutput{Item: itemForKey(testNewAPIKey)}, nil
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return now },
		}
		p.apiKeyCache().Seed("T_expired", cachedAPIKeyResult(testOldAPIKey), apiKeyCacheTTL, now.Add(-apiKeyCacheTTL-time.Second))
		p.apiKeyCache().Seed("T_fresh", cachedAPIKeyResult(testOldAPIKey), apiKeyCacheTTL, now)
		p.apiKeyStrongReadUntil = map[string]time.Time{
			"T_expired": now.Add(-time.Second),
			"T_fresh":   now.Add(time.Minute),
		}

		got, err := p.APIKey(context.Background(), "T_new")
		if err != nil {
			t.Fatalf("APIKey: %v", err)
		}
		if got != testNewAPIKey {
			t.Fatalf("got %q want %q", got, testNewAPIKey)
		}
		if _, ok := p.apiKeyStrongReadUntil["T_expired"]; ok {
			t.Fatal("expired strong-read marker was not swept")
		}
		if _, ok := p.apiKeyStrongReadUntil["T_fresh"]; !ok {
			t.Fatal("fresh strong-read marker was swept")
		}
		got, err = p.APIKey(context.Background(), "T_fresh")
		if err != nil {
			t.Fatalf("fresh cached APIKey: %v", err)
		}
		if got != testOldAPIKey {
			t.Fatalf("fresh cached got %q want %q", got, testOldAPIKey)
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want 2", ddb.getCalls)
		}
	})

	t.Run("concurrent miss shares one DDB and decrypt fill", func(t *testing.T) {
		releaseGet := make(chan struct{})
		getStarted := make(chan struct{})
		closeGetStarted := sync.OnceFunc(func() { close(getStarted) })
		ddb := &fakeDDBClient{
			getFunc: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				closeGetStarted()
				<-releaseGet
				return &dynamodb.GetItemOutput{Item: itemForKey("lv_live_shared_fill")}, nil
			},
		}
		encryptor := &passthroughEncryptor{}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: encryptor,
			Now:       func() time.Time { return time.Unix(1700000000, 0) },
		}

		results := make(chan string, 2)
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := p.APIKey(context.Background(), testTeamID)
				if err != nil {
					errs <- err
					return
				}
				results <- got
			}()
		}
		<-getStarted
		close(releaseGet)
		wg.Wait()
		close(results)
		close(errs)

		for err := range errs {
			t.Fatalf("APIKey: %v", err)
		}
		for got := range results {
			if got != "lv_live_shared_fill" {
				t.Fatalf("got %q want %q", got, "lv_live_shared_fill")
			}
		}
		if ddb.getCalls < 1 || ddb.getCalls > 2 {
			t.Fatalf("GetItem calls = %d, want 1 fill plus optional cache validation", ddb.getCalls)
		}
		if encryptor.openCalls != 1 {
			t.Fatalf("Open calls = %d, want 1", encryptor.openCalls)
		}
	})

	t.Run("same token validation shares one DDB read", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return time.Unix(1700000000, 0) },
		}
		token := newAPIKeyCacheToken([]byte(testOldAPIKey), []byte(passthroughWrappedKey))

		ownerCall, owner := p.getOrStartAPIKeyValidation(testTeamID, token)
		if !owner {
			t.Fatal("first validation should own the DDB read")
		}
		waiterCall, owner := p.getOrStartAPIKeyValidation(testTeamID, token)
		if owner {
			t.Fatal("same-token validation should join the owner")
		}
		if waiterCall != ownerCall {
			t.Fatal("same-token validation did not return the existing call")
		}

		current, err := p.cachedAPIKeyStillCurrent(context.Background(), testTeamID, token)
		p.finishAPIKeyValidation(testTeamID, ownerCall, current, err)
		<-waiterCall.done
		if waiterCall.err != nil {
			t.Fatalf("waiter validation err = %v", waiterCall.err)
		}
		if !waiterCall.current {
			t.Fatal("waiter validation current = false, want true")
		}
		if ddb.getCalls != 1 {
			t.Fatalf("validation GetItem calls = %d, want 1", ddb.getCalls)
		}
		requireCacheValidationProjection(t, ddb.getInputs[0])
		if !getItemConsistentRead(ddb.getInputs[0]) {
			t.Fatal("validation read should be strongly consistent")
		}
	})

	t.Run("owner error is shared with coalesced waiter", func(t *testing.T) {
		ownerErr := errors.New("ddb down")
		ddb := &fakeDDBClient{getErr: ownerErr}
		now := time.Unix(1700000000, 0)
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return now },
		}

		owner := p.getOrStartAPIKeyLookup(testTeamID, now)
		if !owner.owner {
			t.Fatal("first lookup should own the fill")
		}
		waiter := p.getOrStartAPIKeyLookup(testTeamID, now)
		if waiter.owner || waiter.call != owner.call {
			t.Fatal("second lookup should wait on the owner fill")
		}

		waiterErr := make(chan error, 1)
		go func() {
			<-waiter.call.Done()
			waiterErr <- waiter.call.Result().Err
		}()

		_, err := p.fetchAndFinishAPIKeyLookup(context.Background(), testTeamID, owner.call, owner.generation, false)
		if !errors.Is(err, ownerErr) {
			t.Fatalf("owner err = %v, want %v", err, ownerErr)
		}
		select {
		case err := <-waiterErr:
			if !errors.Is(err, ownerErr) {
				t.Fatalf("waiter err = %v, want %v", err, ownerErr)
			}
		case <-time.After(time.Second):
			t.Fatal("waiter did not receive owner error")
		}
		if ddb.getCalls != 1 {
			t.Fatalf("GetItem calls = %d, want 1", ddb.getCalls)
		}
	})

	t.Run("owner cancellation asks healthy waiter to retry", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getFunc: func(ctx context.Context, _ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return nil, ctx.Err()
			},
		}
		now := time.Unix(1700000000, 0)
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return now },
		}

		owner := p.getOrStartAPIKeyLookup(testTeamID, now)
		if !owner.owner {
			t.Fatal("first lookup should own the fill")
		}
		waiter := p.getOrStartAPIKeyLookup(testTeamID, now)
		if waiter.owner || waiter.call != owner.call {
			t.Fatal("second lookup should wait on the owner fill")
		}

		waiterErr := make(chan error, 1)
		go func() {
			<-waiter.call.Done()
			waiterErr <- waiter.call.Result().Err
		}()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := p.fetchAndFinishAPIKeyLookup(ctx, testTeamID, owner.call, owner.generation, false)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("owner err = %v, want context.Canceled", err)
		}
		select {
		case err := <-waiterErr:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("waiter err = %v, want context.Canceled", err)
			}
			if !shouldRetryAPIKeyLookupAfterSharedError(context.Background(), err, 0) {
				t.Fatal("healthy waiter should retry after shared owner cancellation")
			}
		case <-time.After(time.Second):
			t.Fatal("waiter did not receive owner cancellation")
		}
		if ddb.getCalls != 1 {
			t.Fatalf("GetItem calls = %d, want 1", ddb.getCalls)
		}
		ddb.getFunc = nil
		ddb.getOutput = &dynamodb.GetItemOutput{Item: itemForKey("lv_live_after_owner_cancel")}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("retry APIKey after owner cancellation: %v", err)
		}
		if got != "lv_live_after_owner_cancel" {
			t.Fatalf("got %q want %q", got, "lv_live_after_owner_cancel")
		}
	})

	t.Run("shared deadline asks healthy waiter to retry", func(t *testing.T) {
		if !shouldRetryAPIKeyLookupAfterSharedError(context.Background(), context.DeadlineExceeded, 0) {
			t.Fatal("healthy waiter should retry after shared deadline")
		}
		if shouldRetryAPIKeyLookupAfterSharedError(context.Background(), context.DeadlineExceeded, apiKeySharedContextErrorRetryLimit) {
			t.Fatal("healthy waiter should not retry after reaching the retry limit")
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if shouldRetryAPIKeyLookupAfterSharedError(ctx, context.DeadlineExceeded, 0) {
			t.Fatal("canceled waiter should not retry after shared deadline")
		}
	})

	t.Run("APIKey waiter retries after owner cancellation", func(t *testing.T) {
		firstGetStarted := make(chan struct{})
		var callMu sync.Mutex
		getCall := 0
		ddb := &fakeDDBClient{
			getFunc: func(ctx context.Context, _ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				callMu.Lock()
				getCall++
				call := getCall
				callMu.Unlock()
				if call == 1 {
					close(firstGetStarted)
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return &dynamodb.GetItemOutput{Item: itemForKey("lv_live_waiter_retry")}, nil
			},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return time.Unix(1700000000, 0) },
		}

		ownerCtx, cancelOwner := context.WithCancel(context.Background())
		ownerErr := make(chan error, 1)
		go func() {
			_, err := p.APIKey(ownerCtx, testTeamID)
			ownerErr <- err
		}()
		<-firstGetStarted

		waiterResult := make(chan string, 1)
		waiterErr := make(chan error, 1)
		go func() {
			got, err := p.APIKey(context.Background(), testTeamID)
			if err != nil {
				waiterErr <- err
				return
			}
			waiterResult <- got
		}()

		cancelOwner()
		select {
		case err := <-ownerErr:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("owner err = %v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("owner did not observe cancellation")
		}
		select {
		case err := <-waiterErr:
			t.Fatalf("waiter should retry after owner cancellation, got err %v", err)
		case got := <-waiterResult:
			if got != "lv_live_waiter_retry" {
				t.Fatalf("waiter got %q want %q", got, "lv_live_waiter_retry")
			}
		case <-time.After(time.Second):
			t.Fatal("waiter did not retry after owner cancellation")
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want 2", ddb.getCalls)
		}
	})

	t.Run("waiter cancellation leaves owner fill active", func(t *testing.T) {
		releaseGet := make(chan struct{})
		getStarted := make(chan struct{})
		closeGetStarted := sync.OnceFunc(func() { close(getStarted) })
		ddb := &fakeDDBClient{
			getFunc: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				closeGetStarted()
				<-releaseGet
				return &dynamodb.GetItemOutput{Item: itemForKey("lv_live_owner_fill")}, nil
			},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return time.Unix(1700000000, 0) },
		}

		ownerResult := make(chan string, 1)
		ownerErr := make(chan error, 1)
		go func() {
			got, err := p.APIKey(context.Background(), testTeamID)
			if err != nil {
				ownerErr <- err
				return
			}
			ownerResult <- got
		}()
		<-getStarted

		waiterCtx, cancelWaiter := context.WithCancel(context.Background())
		waiterErr := make(chan error, 1)
		go func() {
			_, err := p.APIKey(waiterCtx, testTeamID)
			waiterErr <- err
		}()
		cancelWaiter()
		select {
		case err := <-waiterErr:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("waiter err = %v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("waiter did not observe its own cancellation")
		}

		close(releaseGet)
		select {
		case err := <-ownerErr:
			t.Fatalf("owner APIKey: %v", err)
		case got := <-ownerResult:
			if got != "lv_live_owner_fill" {
				t.Fatalf("owner got %q want %q", got, "lv_live_owner_fill")
			}
		case <-time.After(time.Second):
			t.Fatal("owner did not finish after release")
		}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("cached APIKey after owner fill: %v", err)
		}
		if got != "lv_live_owner_fill" {
			t.Fatalf("cached got %q want %q", got, "lv_live_owner_fill")
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want 2 with cache validation", ddb.getCalls)
		}
	})

	t.Run("panic releases in-flight lookup", func(t *testing.T) {
		calls := 0
		ddb := &fakeDDBClient{
			getFunc: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				calls++
				if calls == 1 {
					panic("simulated APIKey lookup panic")
				}
				return &dynamodb.GetItemOutput{Item: itemForKey("lv_live_after_panic")}, nil
			},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return time.Unix(1700000000, 0) },
		}

		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("first APIKey should panic")
				}
			}()
			_, _ = p.APIKey(context.Background(), testTeamID)
		}()

		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("second APIKey should start after panic cleanup: %v", err)
		}
		if got != "lv_live_after_panic" {
			t.Fatalf("got %q want %q", got, "lv_live_after_panic")
		}
		if calls != 2 {
			t.Fatalf("GetItem calls = %d, want 2", calls)
		}
	})
}

func TestDDBProviderAPIKeyCacheInvalidation(t *testing.T) {
	itemForKey := func(apiKey string) map[string]ddbtypes.AttributeValue {
		return map[string]ddbtypes.AttributeValue{
			attrTeamID:     &ddbtypes.AttributeValueMemberS{Value: testTeamID},
			attrQURLAPIKey: &ddbtypes.AttributeValueMemberB{Value: []byte(apiKey)},
			attrDataKeyCT:  &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
		}
	}

	t.Run("SetAPIKeyWithMetadata seeds new cached value", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return time.Unix(1700000000, 0) },
		}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("prime APIKey cache: %v", err)
		}
		if got != testOldAPIKey {
			t.Fatalf("got %q want %q", got, testOldAPIKey)
		}

		if err := p.SetAPIKeyWithMetadata(context.Background(), testTeamID, testNewAPIKey, testKeyID, testKeyPrefix, testQURLAccount, "U_ADMIN"); err != nil {
			t.Fatalf("SetAPIKeyWithMetadata: %v", err)
		}
		ddb.getFunc = func(_ context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if getItemUsesCacheValidationProjection(in) {
				return &dynamodb.GetItemOutput{Item: itemForKey(testNewAPIKey)}, nil
			}
			return &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}, nil
		}
		got, err = p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey after SetAPIKeyWithMetadata: %v", err)
		}
		if got != testNewAPIKey {
			t.Fatalf("got %q want %q", got, testNewAPIKey)
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want 2 including cache validation", ddb.getCalls)
		}
		requireCacheValidationProjection(t, ddb.getInputs[1])
	})

	t.Run("DeleteAPIKey evicts stale cached value", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return time.Unix(1700000000, 0) },
		}
		if _, err := p.APIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("prime APIKey cache: %v", err)
		}

		if err := p.DeleteAPIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("DeleteAPIKey: %v", err)
		}
		ddb.getOutput = &dynamodb.GetItemOutput{Item: nil}
		_, err := p.APIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatalf("want ErrWorkspaceNotConfigured after delete, got %v", err)
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want 2 after cache eviction", ddb.getCalls)
		}
		if !getItemConsistentRead(ddb.getInputs[1]) {
			t.Fatal("post-delete refill should use a strongly consistent read")
		}
	})

	t.Run("DeleteAPIKey forces strong refill after local invalidation", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		encryptor := &passthroughEncryptor{}
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: encryptor,
			Now:       func() time.Time { return now },
		}
		if _, err := p.APIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("prime APIKey cache: %v", err)
		}

		if err := p.DeleteAPIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("DeleteAPIKey: %v", err)
		}
		ddb.getFunc = func(_ context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if getItemConsistentRead(in) {
				return &dynamodb.GetItemOutput{Item: nil}, nil
			}
			return &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}, nil
		}

		_, err := p.APIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatalf("want ErrWorkspaceNotConfigured after delete, got %v", err)
		}
		if len(ddb.getInputs) != 2 || !getItemConsistentRead(ddb.getInputs[1]) {
			t.Fatalf("post-delete refill should use a strongly consistent read, inputs=%v", ddb.getInputs)
		}
		if encryptor.openCalls != 1 {
			t.Fatalf("Open calls = %d, want 1; stale post-delete row must not be decrypted", encryptor.openCalls)
		}
	})

	t.Run("sibling DeleteAPIKey invalidates warm cache via validation", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		encryptor := &passthroughEncryptor{}
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: encryptor,
			Now:       func() time.Time { return now },
		}
		if _, err := p.APIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("prime APIKey cache: %v", err)
		}

		ddb.getFunc = func(_ context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if getItemUsesCacheValidationProjection(in) {
				requireCacheValidationProjection(t, in)
				if !getItemConsistentRead(in) {
					t.Fatal("cache validation should use a strongly consistent read")
				}
				return &dynamodb.GetItemOutput{Item: nil}, nil
			}
			if !getItemConsistentRead(in) {
				t.Fatal("post-validation refill should use a strongly consistent read")
			}
			return &dynamodb.GetItemOutput{Item: nil}, nil
		}
		_, err := p.APIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatalf("want ErrWorkspaceNotConfigured after sibling delete, got %v", err)
		}
		if ddb.getCalls != 3 {
			t.Fatalf("GetItem calls = %d, want prime + validation + strong refill", ddb.getCalls)
		}
		if encryptor.openCalls != 1 {
			t.Fatalf("Open calls = %d, want only the prime decrypt", encryptor.openCalls)
		}
	})

	t.Run("sibling SetAPIKeyWithMetadata refreshes warm cache via validation", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		encryptor := &passthroughEncryptor{}
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: encryptor,
			Now:       func() time.Time { return now },
		}
		if _, err := p.APIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("prime APIKey cache: %v", err)
		}

		ddb.getFunc = func(_ context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if getItemUsesCacheValidationProjection(in) {
				requireCacheValidationProjection(t, in)
				if !getItemConsistentRead(in) {
					t.Fatal("cache validation should use a strongly consistent read")
				}
				return &dynamodb.GetItemOutput{Item: itemForKey(testNewAPIKey)}, nil
			}
			if !getItemConsistentRead(in) {
				t.Fatal("post-validation refill should use a strongly consistent read")
			}
			return &dynamodb.GetItemOutput{Item: itemForKey(testNewAPIKey)}, nil
		}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey after sibling rotation: %v", err)
		}
		if got != testNewAPIKey {
			t.Fatalf("got %q want rotated key %q", got, testNewAPIKey)
		}
		if ddb.getCalls != 3 {
			t.Fatalf("GetItem calls = %d, want prime + validation + strong refill", ddb.getCalls)
		}
		if encryptor.openCalls != 2 {
			t.Fatalf("Open calls = %d, want prime decrypt + rotated-key decrypt", encryptor.openCalls)
		}
	})

	t.Run("SetAPIKeyWithMetadata prevents in-flight stale read from repopulating cache", func(t *testing.T) {
		releaseGet := make(chan struct{})
		getStarted := make(chan struct{})
		ddb := &fakeDDBClient{
			getFunc: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				close(getStarted)
				<-releaseGet
				return &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}, nil
			},
		}
		now := time.Unix(1700000000, 0)
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return now },
		}

		result := make(chan string, 1)
		errc := make(chan error, 1)
		go func() {
			got, err := p.APIKey(context.Background(), testTeamID)
			if err != nil {
				errc <- err
				return
			}
			result <- got
		}()
		<-getStarted
		if err := p.SetAPIKeyWithMetadata(context.Background(), testTeamID, testNewAPIKey, testKeyID, testKeyPrefix, testQURLAccount, "U_ADMIN"); err != nil {
			t.Fatalf("SetAPIKeyWithMetadata: %v", err)
		}
		close(releaseGet)

		select {
		case err := <-errc:
			t.Fatalf("in-flight APIKey: %v", err)
		case got := <-result:
			if got != testOldAPIKey {
				t.Fatalf("in-flight call got %q want %q", got, testOldAPIKey)
			}
		}

		ddb.getFunc = func(_ context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if getItemConsistentRead(in) {
				return &dynamodb.GetItemOutput{Item: itemForKey(testNewAPIKey)}, nil
			}
			return &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}, nil
		}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey after SetAPIKeyWithMetadata: %v", err)
		}
		if got != testNewAPIKey {
			t.Fatalf("got %q want %q", got, testNewAPIKey)
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want 2 including cache validation", ddb.getCalls)
		}
	})

	t.Run("DeleteAPIKey prevents in-flight stale read from repopulating cache", func(t *testing.T) {
		releaseGet := make(chan struct{})
		getStarted := make(chan struct{})
		firstGet := true
		ddb := &fakeDDBClient{
			getFunc: func(_ context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				if firstGet && !getItemConsistentRead(in) {
					firstGet = false
					close(getStarted)
					<-releaseGet
					return &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}, nil
				}
				if getItemConsistentRead(in) {
					return &dynamodb.GetItemOutput{Item: nil}, nil
				}
				return &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}, nil
			},
		}
		now := time.Unix(1700000000, 0)
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return now },
		}

		result := make(chan string, 1)
		errc := make(chan error, 1)
		go func() {
			got, err := p.APIKey(context.Background(), testTeamID)
			if err != nil {
				errc <- err
				return
			}
			result <- got
		}()
		<-getStarted
		if err := p.DeleteAPIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("DeleteAPIKey: %v", err)
		}
		close(releaseGet)

		select {
		case err := <-errc:
			t.Fatalf("in-flight APIKey: %v", err)
		case got := <-result:
			if got != testOldAPIKey {
				t.Fatalf("in-flight call got %q want %q", got, testOldAPIKey)
			}
		}

		_, err := p.APIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatalf("want ErrWorkspaceNotConfigured after DeleteAPIKey, got %v", err)
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want 2", ddb.getCalls)
		}
		if !getItemConsistentRead(ddb.getInputs[1]) {
			t.Fatal("post-delete refill should use a strongly consistent read")
		}
	})

	t.Run("DeleteAPIKey evicts stale cache on not configured", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
			updateErr: &ddbtypes.ConditionalCheckFailedException{},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return now },
		}
		if _, err := p.APIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("prime APIKey cache: %v", err)
		}

		err := p.DeleteAPIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatalf("want ErrWorkspaceNotConfigured, got %v", err)
		}
		ddb.getFunc = func(_ context.Context, _ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: nil}, nil
		}

		_, err = p.APIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatalf("want ErrWorkspaceNotConfigured after conditional miss, got %v", err)
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want 2 after conditional miss eviction", ddb.getCalls)
		}
		if !getItemConsistentRead(ddb.getInputs[1]) {
			t.Fatal("post-conditional-miss refill should use a strongly consistent read")
		}
	})

	t.Run("cache validation error serves cached key", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		encryptor := &passthroughEncryptor{}
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: encryptor,
			Now:       func() time.Time { return now },
		}
		if _, err := p.APIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("prime APIKey cache: %v", err)
		}

		validationErr := errors.New("ddb validation down")
		ddb.getFunc = func(_ context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if getItemUsesCacheValidationProjection(in) {
				return nil, validationErr
			}
			return &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}, nil
		}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey during validation outage: %v", err)
		}
		if got != testOldAPIKey {
			t.Fatalf("got %q want cached key %q", got, testOldAPIKey)
		}
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want prime + validation", ddb.getCalls)
		}
		if encryptor.openCalls != 1 {
			t.Fatalf("Open calls = %d, want only the prime decrypt", encryptor.openCalls)
		}
	})

	t.Run("cache validation error rechecks local token before serving cached key", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		encryptor := &passthroughEncryptor{}
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: encryptor,
			Now:       func() time.Time { return now },
		}
		if _, err := p.APIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("prime APIKey cache: %v", err)
		}

		validationAttempts := 0
		validationErr := errors.New("ddb validation down")
		ddb.getFunc = func(ctx context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if getItemUsesCacheValidationProjection(in) {
				validationAttempts++
				if validationAttempts == 1 {
					if err := p.SetAPIKeyWithMetadata(ctx, testTeamID, testNewAPIKey, testKeyID, testKeyPrefix, testQURLAccount, "U_ADMIN"); err != nil {
						t.Fatalf("SetAPIKeyWithMetadata during validation: %v", err)
					}
					return nil, validationErr
				}
			}
			return &dynamodb.GetItemOutput{Item: itemForKey(testNewAPIKey)}, nil
		}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey after validation outage racing SetAPIKeyWithMetadata: %v", err)
		}
		if got != testNewAPIKey {
			t.Fatalf("got %q want new cached key %q", got, testNewAPIKey)
		}
		if validationAttempts != 2 {
			t.Fatalf("validation attempts = %d, want old-token error + new-token validation", validationAttempts)
		}
		if encryptor.openCalls != 1 {
			t.Fatalf("Open calls = %d, want only the prime decrypt", encryptor.openCalls)
		}
	})

	t.Run("cache validation context error retries before serving cached key", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		encryptor := &passthroughEncryptor{}
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: encryptor,
			Now:       func() time.Time { return now },
		}
		if _, err := p.APIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("prime APIKey cache: %v", err)
		}

		validationAttempts := 0
		ddb.getFunc = func(_ context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if getItemUsesCacheValidationProjection(in) {
				validationAttempts++
				if validationAttempts == 1 {
					return nil, context.DeadlineExceeded
				}
			}
			return &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}, nil
		}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey after transient validation context error: %v", err)
		}
		if got != testOldAPIKey {
			t.Fatalf("got %q want cached key %q", got, testOldAPIKey)
		}
		if validationAttempts != 2 {
			t.Fatalf("validation attempts = %d, want retry + success", validationAttempts)
		}
		if ddb.getCalls != 3 {
			t.Fatalf("GetItem calls = %d, want prime + two validations", ddb.getCalls)
		}
		if encryptor.openCalls != 1 {
			t.Fatalf("Open calls = %d, want only the prime decrypt", encryptor.openCalls)
		}
	})

	t.Run("SetAPIKeyWithMetadata racing cache validation prevents stale return", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		encryptor := &passthroughEncryptor{}
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: encryptor,
			Now:       func() time.Time { return now },
		}
		if _, err := p.APIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("prime APIKey cache: %v", err)
		}

		validationCalls := 0
		ddb.getFunc = func(ctx context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if getItemUsesCacheValidationProjection(in) {
				validationCalls++
				if validationCalls == 1 {
					if err := p.SetAPIKeyWithMetadata(ctx, testTeamID, testNewAPIKey, testKeyID, testKeyPrefix, testQURLAccount, "U_ADMIN"); err != nil {
						t.Fatalf("SetAPIKeyWithMetadata during validation: %v", err)
					}
					return &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}, nil
				}
				return &dynamodb.GetItemOutput{Item: itemForKey(testNewAPIKey)}, nil
			}
			return &dynamodb.GetItemOutput{Item: itemForKey(testNewAPIKey)}, nil
		}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey racing SetAPIKeyWithMetadata: %v", err)
		}
		if got != testNewAPIKey {
			t.Fatalf("got %q want %q", got, testNewAPIKey)
		}
		if validationCalls != 2 {
			t.Fatalf("validation calls = %d, want old-token validation + new-token validation", validationCalls)
		}
		if encryptor.openCalls != 1 {
			t.Fatalf("Open calls = %d, want only the prime decrypt", encryptor.openCalls)
		}
	})

	t.Run("cache validation recheck loop is bounded", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return now },
		}
		if _, err := p.APIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("prime APIKey cache: %v", err)
		}

		validatedKey := testOldAPIKey
		replacements := []string{
			"lv_live_recheck_1",
			"lv_live_recheck_2",
			"lv_live_recheck_3",
			"lv_live_recheck_4",
		}
		validationCalls := 0
		ddb.getFunc = func(ctx context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			if getItemUsesCacheValidationProjection(in) {
				currentKey := validatedKey
				if validationCalls >= len(replacements) {
					t.Fatalf("unexpected validation call %d", validationCalls+1)
				}
				nextKey := replacements[validationCalls]
				validationCalls++
				if err := p.SetAPIKeyWithMetadata(ctx, testTeamID, nextKey, testKeyID, testKeyPrefix, testQURLAccount, "U_ADMIN"); err != nil {
					t.Fatalf("SetAPIKeyWithMetadata during validation %d: %v", validationCalls, err)
				}
				validatedKey = nextKey
				return &dynamodb.GetItemOutput{Item: itemForKey(currentKey)}, nil
			}
			return &dynamodb.GetItemOutput{Item: itemForKey(validatedKey)}, nil
		}

		_, err := p.APIKey(context.Background(), testTeamID)
		if err == nil || !strings.Contains(err.Error(), "cache validation did not converge") {
			t.Fatalf("err = %v, want cache validation did not converge", err)
		}
		if validationCalls != apiKeyValidationRecheckLimit+1 {
			t.Fatalf("validation calls = %d, want %d", validationCalls, apiKeyValidationRecheckLimit+1)
		}
	})

	t.Run("blank private cache mutation guards are no-ops", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return now },
		}
		p.seedAPIKeyCache(testTeamID, testOldAPIKey, newAPIKeyCacheToken([]byte(testOldAPIKey), []byte(passthroughWrappedKey)), now)
		p.apiKeyCache().WithLock(func() {
			p.apiKeyStrongReadUntil = map[string]time.Time{
				testTeamID: now.Add(apiKeyCacheTTL),
			}
		})

		p.invalidateAPIKeyCache(" \t ", now.Add(apiKeyCacheTTL))
		p.seedAPIKeyCache("\n", testNewAPIKey, newAPIKeyCacheToken([]byte(testNewAPIKey), []byte(passthroughWrappedKey)), now)

		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("cached APIKey after blank mutation: %v", err)
		}
		if got != testOldAPIKey {
			t.Fatalf("got %q want %q", got, testOldAPIKey)
		}
		if len(p.apiKeyStrongReadUntil) != 1 || !p.apiKeyStrongReadUntil[testTeamID].Equal(now.Add(apiKeyCacheTTL)) {
			t.Fatalf("blank mutation changed strong-read markers: %#v", p.apiKeyStrongReadUntil)
		}
	})
}

func TestDDBProviderAPIKeyID(t *testing.T) {
	const (
		apiKey = "lv_live_abcd1234"
		keyID  = "key_123"
	)
	ddb := &fakeDDBClient{
		getOutput: &dynamodb.GetItemOutput{
			Item: map[string]ddbtypes.AttributeValue{
				attrTeamID:           &ddbtypes.AttributeValueMemberS{Value: testTeamID},
				attrQURLAPIKey:       &ddbtypes.AttributeValueMemberB{Value: []byte(apiKey)},
				attrDataKeyCT:        &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
				attrQURLAPIKeyID:     &ddbtypes.AttributeValueMemberS{Value: keyID},
				attrQURLAPIKeyPrefix: &ddbtypes.AttributeValueMemberS{Value: "lv_live_abcd"},
			},
		},
	}
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
	}
	gotID, err := p.APIKeyID(context.Background(), testTeamID)
	if err != nil {
		t.Fatalf("APIKeyID: %v", err)
	}
	if gotID != keyID {
		t.Fatalf("APIKeyID = %q, want %q", gotID, keyID)
	}
	if ddb.getCalls != 1 {
		t.Fatalf("GetItem calls = %d, want 1", ddb.getCalls)
	}
	if !getItemConsistentRead(ddb.getInputs[0]) {
		t.Fatal("APIKeyID must use ConsistentRead for rotation")
	}
}

func TestDDBProviderAPIKeyIDLabelsGetItemErrors(t *testing.T) {
	ddb := &fakeDDBClient{getErr: errors.New("boom")}
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
	}
	_, err := p.APIKeyID(context.Background(), testTeamID)
	if err == nil {
		t.Fatal("APIKeyID error = nil, want GetItem failure")
	}
	if got := err.Error(); !strings.Contains(got, "DDBProvider.APIKeyID: GetItem:") || strings.Contains(got, "DDBProvider.APIKey: GetItem:") {
		t.Fatalf("APIKeyID error = %q, want APIKeyID operation label", got)
	}
}

func TestDDBProviderAPIKeyIdentity(t *testing.T) {
	const (
		apiKey  = "lv_live_abcd1234"
		keyID   = "key_123"
		account = "auth0|owner-acct"
	)
	ddb := &fakeDDBClient{
		getOutput: &dynamodb.GetItemOutput{
			Item: map[string]ddbtypes.AttributeValue{
				attrTeamID:        &ddbtypes.AttributeValueMemberS{Value: testTeamID},
				attrQURLAPIKey:    &ddbtypes.AttributeValueMemberB{Value: []byte(apiKey)},
				attrDataKeyCT:     &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
				attrQURLAPIKeyID:  &ddbtypes.AttributeValueMemberS{Value: keyID},
				attrQURLAccountID: &ddbtypes.AttributeValueMemberS{Value: account},
			},
		},
	}
	p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
	gotKeyID, gotAccount, err := p.APIKeyIdentity(context.Background(), testTeamID)
	if err != nil {
		t.Fatalf("APIKeyIdentity: %v", err)
	}
	if gotKeyID != keyID {
		t.Errorf("APIKeyIdentity keyID = %q, want %q", gotKeyID, keyID)
	}
	if gotAccount != account {
		t.Errorf("APIKeyIdentity account = %q, want %q", gotAccount, account)
	}
	if ddb.getCalls != 1 {
		t.Fatalf("GetItem calls = %d, want 1 (single combined read backs key_id + account)", ddb.getCalls)
	}
	if !getItemConsistentRead(ddb.getInputs[0]) {
		t.Fatal("APIKeyIdentity must use ConsistentRead so rotation/repoint read the latest identity")
	}
}

// A configured row written before the account field (or by the sandbox/no-
// verifier path) has a key but no qurl_account_id: return account "" so
// --repoint fails closed rather than assuming same-account.
func TestDDBProviderAPIKeyIdentityLegacyRowReturnsEmptyAccount(t *testing.T) {
	ddb := &fakeDDBClient{
		getOutput: &dynamodb.GetItemOutput{
			Item: map[string]ddbtypes.AttributeValue{
				attrTeamID:       &ddbtypes.AttributeValueMemberS{Value: testTeamID},
				attrQURLAPIKey:   &ddbtypes.AttributeValueMemberB{Value: []byte("lv_live_legacy")},
				attrDataKeyCT:    &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
				attrQURLAPIKeyID: &ddbtypes.AttributeValueMemberS{Value: "key_legacy"},
			},
		},
	}
	p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
	gotKeyID, gotAccount, err := p.APIKeyIdentity(context.Background(), testTeamID)
	if err != nil {
		t.Fatalf("APIKeyIdentity: %v", err)
	}
	if gotKeyID != "key_legacy" {
		t.Errorf("APIKeyIdentity keyID = %q, want %q", gotKeyID, "key_legacy")
	}
	if gotAccount != "" {
		t.Errorf("APIKeyIdentity legacy account = %q, want empty", gotAccount)
	}
}

func TestDDBProviderAPIKeyIdentityUnconfigured(t *testing.T) {
	ddb := &fakeDDBClient{getOutput: &dynamodb.GetItemOutput{Item: nil}}
	p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
	_, _, err := p.APIKeyIdentity(context.Background(), testTeamID)
	if !errors.Is(err, ErrWorkspaceNotConfigured) {
		t.Fatalf("APIKeyIdentity err = %v, want ErrWorkspaceNotConfigured", err)
	}
}

func TestDDBProviderSetAPIKeyWithMetadataStoresQURLAccount(t *testing.T) {
	ddb := &fakeDDBClient{}
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	if err := p.SetAPIKeyWithMetadata(context.Background(), testTeamID, testNewAPIKey, testKeyID, testKeyPrefix, testQURLAccount, "U_ADMIN"); err != nil {
		t.Fatalf("SetAPIKeyWithMetadata: %v", err)
	}
	got := *ddb.updateInput.UpdateExpression
	if !strings.Contains(got, attrQURLAccountID+" = :account_id") {
		t.Errorf("UpdateExpression should store qurl_account_id, got %q", got)
	}
	if v, ok := ddb.updateInput.ExpressionAttributeValues[":account_id"].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != testQURLAccount {
		t.Errorf("qurl_account_id value wrong: %v", ddb.updateInput.ExpressionAttributeValues[":account_id"])
	}
}

// An empty qURL account (sandbox/no-verifier path) must NOT write the attribute,
// so it can never erase the provenance a prior verified mint recorded.
func TestDDBProviderSetAPIKeyWithMetadataOmitsEmptyQURLAccount(t *testing.T) {
	ddb := &fakeDDBClient{}
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	if err := p.SetAPIKeyWithMetadata(context.Background(), testTeamID, testNewAPIKey, testKeyID, testKeyPrefix, "  ", "U_ADMIN"); err != nil {
		t.Fatalf("SetAPIKeyWithMetadata: %v", err)
	}
	got := *ddb.updateInput.UpdateExpression
	if strings.Contains(got, attrQURLAccountID) {
		t.Errorf("blank qURL account must be omitted from UpdateExpression, got %q", got)
	}
	if _, ok := ddb.updateInput.ExpressionAttributeValues[":account_id"]; ok {
		t.Error("blank qURL account must not bind :account_id")
	}
}

func TestDDBProviderSetAPIKeyWithMetadataUpdatesKeyAndPreservesSlackAttrs(t *testing.T) {
	ddb := &fakeDDBClient{}
	fixedNow := time.Unix(1700000000, 0).UTC()
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return fixedNow },
	}
	err := p.SetAPIKeyWithMetadata(context.Background(), testTeamID, "lv_live_xxx", testKeyID, testKeyPrefix, testQURLAccount, "U_ADMIN")
	if err != nil {
		t.Fatalf("SetAPIKeyWithMetadata: %v", err)
	}
	if ddb.updateInput == nil {
		t.Fatal("expected UpdateItem called")
	}
	if v, ok := ddb.updateInput.Key[attrTeamID].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != testTeamID {
		t.Errorf("team_id wrong: %v", ddb.updateInput.Key[attrTeamID])
	}
	values := ddb.updateInput.ExpressionAttributeValues
	if v, ok := values[":key"].(*ddbtypes.AttributeValueMemberB); !ok || string(v.Value) != "lv_live_xxx" {
		t.Errorf("qurl_api_key wrong: %v", values[":key"])
	}
	if v, ok := values[":dk"].(*ddbtypes.AttributeValueMemberB); !ok || string(v.Value) != passthroughWrappedKey {
		t.Errorf("dk wrong: %v", values[":dk"])
	}
	if v, ok := values[":by"].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != "U_ADMIN" {
		t.Errorf("configured_by wrong: %v", values[":by"])
	}
	if v, ok := values[":key_id"].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != testKeyID {
		t.Errorf("qurl_api_key_id wrong: %v", values[":key_id"])
	}
	if v, ok := values[":key_prefix"].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != testKeyPrefix {
		t.Errorf("qurl_api_key_prefix wrong: %v", values[":key_prefix"])
	}
	wantTS := fixedNow.Format(time.RFC3339)
	if v, ok := values[":now"].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != wantTS {
		t.Errorf("timestamp wrong: got %v want %q", values[":now"], wantTS)
	}
	got := *ddb.updateInput.UpdateExpression
	if !strings.Contains(got, "configured_at = if_not_exists(configured_at, :now)") {
		t.Errorf("UpdateExpression should preserve configured_at with if_not_exists, got %q", got)
	}
	if !strings.Contains(got, attrQURLAPIKeyID+" = :key_id") || !strings.Contains(got, attrQURLAPIKeyPrefix+" = :key_prefix") {
		t.Errorf("UpdateExpression should store key metadata, got %q", got)
	}
	for _, attr := range []string{
		attrSlackBotToken,
		attrSlackBotTokenDK,
		attrSlackBotInstalledBy,
		attrSlackBotInstalledAt,
		attrSlackBotUpdatedAt,
		attrSlackBotUserID,
		attrSlackAppID,
		attrSlackEnterpriseID,
		attrSlackBotScopes,
	} {
		if strings.Contains(got, attr) {
			t.Errorf("SetAPIKeyWithMetadata UpdateExpression should not touch Slack attr %s, got %q", attr, got)
		}
	}
	if ddb.updateInput.ReturnValues != ddbtypes.ReturnValueUpdatedOld {
		t.Errorf("ReturnValues = %v, want UPDATED_OLD for rotation observability", ddb.updateInput.ReturnValues)
	}
}

func TestDDBProviderSetAPIKeyWithMetadata(t *testing.T) {
	const (
		apiKey    = "lv_live_abcd1234"
		keyID     = "key_123"
		keyPrefix = "lv_live_abcd"
	)
	ddb := &fakeDDBClient{}
	fixedNow := time.Unix(1700000000, 0).UTC()
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return fixedNow },
	}
	if err := p.SetAPIKeyWithMetadata(context.Background(), testTeamID, apiKey, keyID, keyPrefix, testQURLAccount, "U_ADMIN"); err != nil {
		t.Fatalf("SetAPIKeyWithMetadata: %v", err)
	}
	if ddb.updateInput == nil {
		t.Fatal("expected UpdateItem called")
	}
	values := ddb.updateInput.ExpressionAttributeValues
	if v, ok := values[":key_id"].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != keyID {
		t.Errorf("qurl_api_key_id wrong: %v", values[":key_id"])
	}
	if v, ok := values[":key_prefix"].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != keyPrefix {
		t.Errorf("qurl_api_key_prefix wrong: %v", values[":key_prefix"])
	}
	got := *ddb.updateInput.UpdateExpression
	if !strings.Contains(got, attrQURLAPIKeyID+" = :key_id") || !strings.Contains(got, attrQURLAPIKeyPrefix+" = :key_prefix") {
		t.Errorf("UpdateExpression should store key metadata, got %q", got)
	}
	if strings.Contains(got, " REMOVE ") {
		t.Errorf("SetAPIKeyWithMetadata should not clear metadata, got %q", got)
	}
}

func TestDDBProviderSetAPIKeyWithMetadataRequiresKeyID(t *testing.T) {
	p := &DDBProvider{Client: &fakeDDBClient{}, TableName: "ws", Encryptor: &passthroughEncryptor{}}
	err := p.SetAPIKeyWithMetadata(context.Background(), testTeamID, "lv_live_abcd1234", "", "lv_live_abcd", testQURLAccount, "U_ADMIN")
	if err == nil || !strings.Contains(err.Error(), "keyID") {
		t.Fatalf("expected keyID error, got %v", err)
	}
}

func TestDDBProviderSetAPIKeyWithMetadataRequiresKeyPrefix(t *testing.T) {
	p := &DDBProvider{Client: &fakeDDBClient{}, TableName: "ws", Encryptor: &passthroughEncryptor{}}
	err := p.SetAPIKeyWithMetadata(context.Background(), testTeamID, "lv_live_abcd1234", "key_123", "", testQURLAccount, "U_ADMIN")
	if err == nil || !strings.Contains(err.Error(), "keyPrefix") {
		t.Fatalf("expected keyPrefix error, got %v", err)
	}
}

func TestDDBProviderSetAPIKeyWithMetadataSurfacesOperationName(t *testing.T) {
	ddb := &fakeDDBClient{updateErr: errors.New("ddb transient")}
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
	}
	err := p.SetAPIKeyWithMetadata(context.Background(), testTeamID, "lv_live_abcd1234", "key_123", "lv_live_abcd", testQURLAccount, "U_ADMIN")
	if err == nil || !strings.HasPrefix(err.Error(), "DDBProvider.SetAPIKeyWithMetadata: UpdateItem:") {
		t.Fatalf("expected SetAPIKeyWithMetadata UpdateItem error, got %v", err)
	}
}

// TestDDBProviderSetAPIKeyWithMetadataPreservesConfiguredAt locks the rotation
// contract: when a row already exists, configured_at retains its
// original value (the install timestamp) while updated_at moves to
// "now". A rotation that wiped configured_at would silently destroy
// audit trail.
func TestDDBProviderSetAPIKeyWithMetadataPreservesConfiguredAt(t *testing.T) {
	ddb := &fakeDDBClient{}
	rotatedAt := time.Unix(1800000000, 0).UTC()
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return rotatedAt },
	}
	if err := p.SetAPIKeyWithMetadata(context.Background(), testTeamID, "lv_live_new", testKeyID, testKeyPrefix, testQURLAccount, "U_ADMIN2"); err != nil {
		t.Fatalf("SetAPIKeyWithMetadata: %v", err)
	}
	if ddb.updateInput == nil {
		t.Fatal("expected UpdateItem called")
	}
	if got := *ddb.updateInput.UpdateExpression; !strings.Contains(got, "configured_at = if_not_exists(configured_at, :now)") {
		t.Errorf("configured_at must preserve original on rotation via if_not_exists, got %q", got)
	}
	wantUpdated := rotatedAt.Format(time.RFC3339)
	if v, ok := ddb.updateInput.ExpressionAttributeValues[":now"].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != wantUpdated {
		t.Errorf("updated_at must move to rotation time: got %v want %q", ddb.updateInput.ExpressionAttributeValues[":now"], wantUpdated)
	}
}

// TestDDBProviderSetAPIKeyWithMetadataNilNowDoesNotPanic locks the contract that
// a bare-struct DDBProvider (no Now field set) doesn't nil-deref on
// SetAPIKeyWithMetadata. NewDDBProvider always sets Now, but tests / unusual
// constructions can produce a DDBProvider{} that previously crashed
// the moment a write path executed.
func TestDDBProviderSetAPIKeyWithMetadataNilNowDoesNotPanic(t *testing.T) {
	ddb := &fakeDDBClient{}
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		// Now deliberately unset.
	}
	if err := p.SetAPIKeyWithMetadata(context.Background(), testTeamID, "lv_live", testKeyID, testKeyPrefix, testQURLAccount, "U_x"); err != nil {
		t.Fatalf("SetAPIKeyWithMetadata with nil Now should fall through to time.Now, got err: %v", err)
	}
}

func TestDDBProviderSlackBotToken(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{
				Item: map[string]ddbtypes.AttributeValue{
					attrTeamID:          &ddbtypes.AttributeValueMemberS{Value: testTeamID},
					attrSlackBotToken:   &ddbtypes.AttributeValueMemberB{Value: []byte(testSlackBotToken)},
					attrSlackBotTokenDK: &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
				},
			},
		}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		got, err := p.SlackBotToken(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("SlackBotToken: %v", err)
		}
		if got != testSlackBotToken {
			t.Fatalf("got %q want %q", got, testSlackBotToken)
		}
	})

	t.Run("missing row", func(t *testing.T) {
		ddb := &fakeDDBClient{getOutput: &dynamodb.GetItemOutput{}}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		_, err := p.SlackBotToken(context.Background(), testTeamID)
		if !errors.Is(err, ErrSlackBotTokenNotConfigured) {
			t.Fatalf("want ErrSlackBotTokenNotConfigured, got %v", err)
		}
	})

	t.Run("nil get output", func(t *testing.T) {
		ddb := &fakeDDBClient{}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		_, err := p.SlackBotToken(context.Background(), testTeamID)
		if !errors.Is(err, ErrSlackBotTokenNotConfigured) {
			t.Fatalf("want ErrSlackBotTokenNotConfigured, got %v", err)
		}
	})

	t.Run("old row without Slack token", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
				attrTeamID: &ddbtypes.AttributeValueMemberS{Value: testTeamID},
			}},
		}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		_, err := p.SlackBotToken(context.Background(), testTeamID)
		if !errors.Is(err, ErrSlackBotTokenNotConfigured) {
			t.Fatalf("want ErrSlackBotTokenNotConfigured, got %v", err)
		}
	})
}

func TestDDBProviderSetSlackBotToken(t *testing.T) {
	ddb := &fakeDDBClient{}
	fixedNow := time.Unix(1700000000, 0).UTC()
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return fixedNow },
	}
	err := p.SetSlackBotToken(context.Background(), testTeamID, &SlackBotTokenInstall{
		BotToken:     testSlackBotToken,
		InstalledBy:  "U_INSTALLER",
		BotUserID:    "U_BOT",
		AppID:        "A_APP",
		EnterpriseID: "E_GRID",
		Scopes:       []string{"chat:write", "commands", "chat:write"},
	})
	if err != nil {
		t.Fatalf("SetSlackBotToken: %v", err)
	}
	if ddb.updateInput == nil {
		t.Fatal("expected UpdateItem called")
	}
	values := ddb.updateInput.ExpressionAttributeValues
	if v, ok := values[":token"].(*ddbtypes.AttributeValueMemberB); !ok || string(v.Value) != testSlackBotToken {
		t.Errorf("slack token wrong: %v", values[":token"])
	}
	if v, ok := values[":dk"].(*ddbtypes.AttributeValueMemberB); !ok || string(v.Value) != passthroughWrappedKey {
		t.Errorf("slack token data key wrong: %v", values[":dk"])
	}
	if v, ok := values[":scopes"].(*ddbtypes.AttributeValueMemberSS); !ok || strings.Join(v.Value, ",") != "chat:write,commands" {
		t.Errorf("scopes should be sorted/deduped: %v", values[":scopes"])
	}
	if got := *ddb.updateInput.UpdateExpression; !strings.Contains(got, "slack_bot_installed_at = if_not_exists(slack_bot_installed_at, :now)") {
		t.Errorf("UpdateExpression should preserve original Slack installed_at, got %q", got)
	}
}

func TestDDBProviderSetSlackBotTokenRemovesEmptyOptionalMetadata(t *testing.T) {
	ddb := &fakeDDBClient{}
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	err := p.SetSlackBotToken(context.Background(), testTeamID, &SlackBotTokenInstall{
		BotToken: testSlackBotToken,
	})
	if err != nil {
		t.Fatalf("SetSlackBotToken: %v", err)
	}
	got := *ddb.updateInput.UpdateExpression
	for _, attr := range []string{attrSlackBotInstalledBy, attrSlackBotUserID, attrSlackAppID, attrSlackEnterpriseID, attrSlackBotScopes} {
		if !strings.Contains(got, attr) {
			t.Fatalf("UpdateExpression should mention %s in REMOVE branch, got %q", attr, got)
		}
	}
	if _, ok := ddb.updateInput.ExpressionAttributeValues[":scopes"]; ok {
		t.Fatal("empty scopes should not write :scopes")
	}
}

func TestDDBProviderSetSlackBotTokenRejectsMalformedToken(t *testing.T) {
	ddb := &fakeDDBClient{}
	p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
	err := p.SetSlackBotToken(context.Background(), testTeamID, &SlackBotTokenInstall{
		BotToken: "xoxa-123456789012345678901234567890",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid bot token") {
		t.Fatalf("err=%v, want invalid bot token", err)
	}
	if ddb.updateInput != nil {
		t.Fatal("malformed token should not write DDB")
	}
}

func TestDDBProviderDeleteAPIKey(t *testing.T) {
	t.Run("removes qURL key columns", func(t *testing.T) {
		ddb := &fakeDDBClient{}
		now := time.Date(2026, 6, 13, 12, 34, 56, 0, time.UTC)
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return now },
		}
		if err := p.DeleteAPIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("DeleteAPIKey: %v", err)
		}
		if ddb.updateInput == nil {
			t.Fatal("expected UpdateItem called")
		}
		if v, ok := ddb.updateInput.Key[attrTeamID].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != testTeamID {
			t.Errorf("delete key wrong: %v", ddb.updateInput.Key)
		}
		if got, want := *ddb.updateInput.UpdateExpression, "SET #updated_at = :now REMOVE #qurl_api_key, #qurl_api_key_dk, #qurl_api_key_id, #qurl_api_key_prefix, #qurl_account_id, #configured_by, #configured_at"; got != want {
			t.Errorf("UpdateExpression = %q, want %q", got, want)
		}
		if got, want := *ddb.updateInput.ConditionExpression, "attribute_exists(#qurl_api_key) OR attribute_exists(#qurl_api_key_dk) OR attribute_exists(#qurl_api_key_id) OR attribute_exists(#qurl_api_key_prefix) OR attribute_exists(#configured_by) OR attribute_exists(#configured_at)"; got != want {
			t.Errorf("ConditionExpression = %q, want %q", got, want)
		}
		if v, ok := ddb.updateInput.ExpressionAttributeValues[":now"].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != "2026-06-13T12:34:56Z" {
			t.Errorf("ExpressionAttributeValues[:now] = %v, want timestamp", ddb.updateInput.ExpressionAttributeValues[":now"])
		}
		wantNames := map[string]string{
			"#qurl_api_key":        attrQURLAPIKey,
			"#qurl_api_key_dk":     attrDataKeyCT,
			"#qurl_api_key_id":     attrQURLAPIKeyID,
			"#qurl_api_key_prefix": attrQURLAPIKeyPrefix,
			"#qurl_account_id":     attrQURLAccountID,
			"#configured_by":       attrConfiguredBy,
			"#configured_at":       attrConfiguredAt,
			"#updated_at":          attrUpdatedAt,
		}
		for name, want := range wantNames {
			if got := ddb.updateInput.ExpressionAttributeNames[name]; got != want {
				t.Errorf("ExpressionAttributeNames[%q] = %q, want %q", name, got, want)
			}
		}
	})

	t.Run("preserves Slack install metadata", func(t *testing.T) {
		row := map[string]ddbtypes.AttributeValue{
			attrTeamID:           &ddbtypes.AttributeValueMemberS{Value: testTeamID},
			attrQURLAPIKey:       &ddbtypes.AttributeValueMemberB{Value: []byte(testOldAPIKey)},
			attrDataKeyCT:        &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
			attrQURLAPIKeyID:     &ddbtypes.AttributeValueMemberS{Value: testKeyID},
			attrQURLAPIKeyPrefix: &ddbtypes.AttributeValueMemberS{Value: testKeyPrefix},
			attrConfiguredBy:     &ddbtypes.AttributeValueMemberS{Value: "U_ADMIN"},
			attrConfiguredAt:     &ddbtypes.AttributeValueMemberS{Value: "2026-06-13T00:00:00Z"},
			attrSlackBotToken:    &ddbtypes.AttributeValueMemberB{Value: []byte(testSlackBotToken)},
			attrSlackBotTokenDK:  &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
		}
		ddb := &fakeDDBClient{
			updateFunc: func(_ context.Context, in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
				parts := strings.SplitN(aws.ToString(in.UpdateExpression), " REMOVE ", 2)
				if len(parts) != 2 {
					t.Fatalf("UpdateExpression %q missing REMOVE clause", aws.ToString(in.UpdateExpression))
				}
				for _, alias := range strings.Split(parts[1], ",") {
					attrName := in.ExpressionAttributeNames[strings.TrimSpace(alias)]
					delete(row, attrName)
				}
				row[attrUpdatedAt] = in.ExpressionAttributeValues[":now"]
				return &dynamodb.UpdateItemOutput{}, nil
			},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return time.Date(2026, 6, 13, 12, 34, 56, 0, time.UTC) },
		}

		if err := p.DeleteAPIKey(context.Background(), testTeamID); err != nil {
			t.Fatalf("DeleteAPIKey: %v", err)
		}
		if _, ok := row[attrQURLAPIKey]; ok {
			t.Fatal("qURL API key column survived DeleteAPIKey")
		}
		if _, ok := row[attrDataKeyCT]; ok {
			t.Fatal("qURL API key data-key column survived DeleteAPIKey")
		}
		if _, ok := row[attrQURLAPIKeyID]; ok {
			t.Fatal("qURL API key ID column survived DeleteAPIKey")
		}
		if _, ok := row[attrQURLAPIKeyPrefix]; ok {
			t.Fatal("qURL API key prefix column survived DeleteAPIKey")
		}
		if got := row[attrSlackBotToken].(*ddbtypes.AttributeValueMemberB).Value; string(got) != testSlackBotToken {
			t.Fatalf("Slack bot token = %q, want %q", got, testSlackBotToken)
		}
		if got := row[attrSlackBotTokenDK].(*ddbtypes.AttributeValueMemberB).Value; string(got) != passthroughWrappedKey {
			t.Fatalf("Slack bot token data key = %q, want %q", got, passthroughWrappedKey)
		}
	})

	t.Run("missing qURL key maps to not configured", func(t *testing.T) {
		ddb := &fakeDDBClient{updateErr: &ddbtypes.ConditionalCheckFailedException{}}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		err := p.DeleteAPIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatalf("want ErrWorkspaceNotConfigured, got %v", err)
		}
	})

	t.Run("update error is wrapped without not configured sentinel", func(t *testing.T) {
		updateErr := errors.New("ddb update down")
		ddb := &fakeDDBClient{updateErr: updateErr}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		err := p.DeleteAPIKey(context.Background(), testTeamID)
		if err == nil {
			t.Fatal("want update error, got nil")
		}
		if !errors.Is(err, updateErr) {
			t.Fatalf("want wrapped update error, got %v", err)
		}
		if errors.Is(err, ErrWorkspaceNotConfigured) {
			t.Fatalf("generic update error should not map to ErrWorkspaceNotConfigured: %v", err)
		}
		if !strings.Contains(err.Error(), "UpdateItem") {
			t.Fatalf("wrapped error should name UpdateItem, got %v", err)
		}
	})
}

func TestEnvProviderDeleteAPIKey(t *testing.T) {
	const envVar = "TEST_QURL_API_KEY"

	t.Run("missing key maps to unsupported", func(t *testing.T) {
		t.Setenv(envVar, "")
		provider := EnvProvider{EnvVar: envVar}
		if provider.SupportsDeleteAPIKey() {
			t.Fatal("EnvProvider must not advertise DeleteAPIKey support")
		}
		err := provider.DeleteAPIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceAPIKeyDeleteUnsupported) {
			t.Fatalf("want ErrWorkspaceAPIKeyDeleteUnsupported, got %v", err)
		}
	})

	t.Run("configured key maps to unsupported", func(t *testing.T) {
		t.Setenv(envVar, "lv_live_test")
		provider := EnvProvider{EnvVar: envVar}
		if provider.SupportsDeleteAPIKey() {
			t.Fatal("EnvProvider must not advertise DeleteAPIKey support")
		}
		err := provider.DeleteAPIKey(context.Background(), testTeamID)
		if !errors.Is(err, ErrWorkspaceAPIKeyDeleteUnsupported) {
			t.Fatalf("want ErrWorkspaceAPIKeyDeleteUnsupported, got %v", err)
		}
	})
}

// KMSEncryptor itself is covered by a round-trip test that exercises
// real AES-GCM under a fixed data key returned by a stub KMSClient.
// See ddb_provider_kms_test.go.
