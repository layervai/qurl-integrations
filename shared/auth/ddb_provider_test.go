package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	testTeamID        = "T123ABCDEF"
	testSlackBotToken = "xoxb-123456789012345678901234567890"
	testOldAPIKey     = "lv_live_old"
	testNewAPIKey     = "lv_live_new"
)

// fakeDDBClient is a hand-rolled stub the table tests configure with
// predetermined results. Captures Put/Delete inputs for assertion.
type fakeDDBClient struct {
	getOutput    *dynamodb.GetItemOutput
	getFunc      func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error)
	getErr       error
	getCalls     int
	getInputs    []*dynamodb.GetItemInput
	putInput     *dynamodb.PutItemInput
	putErr       error
	updateInput  *dynamodb.UpdateItemInput
	updateOutput *dynamodb.UpdateItemOutput
	updateErr    error
	delInput     *dynamodb.DeleteItemInput
	delErr       error
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
func (f *fakeDDBClient) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.updateInput = in
	if f.updateOutput != nil {
		return f.updateOutput, f.updateErr
	}
	return &dynamodb.UpdateItemOutput{}, f.updateErr
}
func (f *fakeDDBClient) DeleteItem(_ context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.delInput = in
	return &dynamodb.DeleteItemOutput{}, f.delErr
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

	t.Run("hit skips DDB and decrypt", func(t *testing.T) {
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
		if ddb.getCalls != 1 {
			t.Fatalf("GetItem calls = %d, want 1", ddb.getCalls)
		}
		if encryptor.openCalls != 1 {
			t.Fatalf("Open calls = %d, want 1", encryptor.openCalls)
		}
	})

	t.Run("cache hit ignores canceled context", func(t *testing.T) {
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
		if err != nil {
			t.Fatalf("cached APIKey with canceled context: %v", err)
		}
		if got != "lv_live_cached_after_cancel" {
			t.Fatalf("cached got %q want %q", got, "lv_live_cached_after_cancel")
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

		ddb.getOutput = &dynamodb.GetItemOutput{Item: itemForKey(testNewAPIKey)}
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
		if ddb.getCalls != 2 {
			t.Fatalf("GetItem calls = %d, want 2", ddb.getCalls)
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
		p.apiKeyCache = map[string]*cachedAPIKey{
			testTeamID: {
				apiKey:    testOldAPIKey,
				expiresAt: now.Add(-time.Second),
			},
		}

		if _, err := p.APIKey(context.Background(), testTeamID); err == nil {
			t.Fatal("want DDB error, got nil")
		}
		if _, ok := p.apiKeyCache[testTeamID]; ok {
			t.Fatal("expired cache entry should be deleted before the refill")
		}
	})

	t.Run("sweeps expired entries during lookup", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{Item: itemForKey(testNewAPIKey)},
		}
		p := &DDBProvider{
			Client:    ddb,
			TableName: "ws",
			Encryptor: &passthroughEncryptor{},
			Now:       func() time.Time { return now },
		}
		p.apiKeyCache = map[string]*cachedAPIKey{
			"T_expired": {
				apiKey:    testOldAPIKey,
				expiresAt: now.Add(-time.Second),
			},
			"T_fresh": {
				apiKey:    testOldAPIKey,
				expiresAt: now.Add(time.Minute),
			},
		}
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
		if _, ok := p.apiKeyCache["T_expired"]; ok {
			t.Fatal("expired cache entry was not swept")
		}
		if _, ok := p.apiKeyCache["T_fresh"]; !ok {
			t.Fatal("fresh cache entry was swept")
		}
		if _, ok := p.apiKeyStrongReadUntil["T_expired"]; ok {
			t.Fatal("expired strong-read marker was not swept")
		}
		if _, ok := p.apiKeyStrongReadUntil["T_fresh"]; !ok {
			t.Fatal("fresh strong-read marker was swept")
		}
		if ddb.getCalls != 1 {
			t.Fatalf("GetItem calls = %d, want 1", ddb.getCalls)
		}
	})

	t.Run("concurrent miss shares one DDB and decrypt fill", func(t *testing.T) {
		releaseGet := make(chan struct{})
		getStarted := make(chan struct{})
		ddb := &fakeDDBClient{
			getFunc: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				close(getStarted)
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
		if ddb.getCalls != 1 {
			t.Fatalf("GetItem calls = %d, want 1", ddb.getCalls)
		}
		if encryptor.openCalls != 1 {
			t.Fatalf("Open calls = %d, want 1", encryptor.openCalls)
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
			<-waiter.call.done
			waiterErr <- waiter.call.err
		}()

		_, err := p.fetchAndFinishAPIKeyLookup(context.Background(), testTeamID, owner.call, now, owner.generation, false)
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
		if _, ok := p.apiKeyCache[testTeamID]; ok {
			t.Fatal("owner error should not populate the cache")
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
			<-waiter.call.done
			waiterErr <- waiter.call.err
		}()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := p.fetchAndFinishAPIKeyLookup(ctx, testTeamID, owner.call, now, owner.generation, false)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("owner err = %v, want context.Canceled", err)
		}
		select {
		case err := <-waiterErr:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("waiter err = %v, want context.Canceled", err)
			}
			if !shouldRetryAPIKeyLookupAfterSharedError(context.Background(), err) {
				t.Fatal("healthy waiter should retry after shared owner cancellation")
			}
		case <-time.After(time.Second):
			t.Fatal("waiter did not receive owner cancellation")
		}
		if ddb.getCalls != 1 {
			t.Fatalf("GetItem calls = %d, want 1", ddb.getCalls)
		}
		if _, ok := p.apiKeyCache[testTeamID]; ok {
			t.Fatal("owner cancellation should not populate the cache")
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

		time.Sleep(10 * time.Millisecond)
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
		ddb := &fakeDDBClient{
			getFunc: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				close(getStarted)
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
		if ddb.getCalls != 1 {
			t.Fatalf("GetItem calls = %d, want 1", ddb.getCalls)
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

	t.Run("SetAPIKey seeds new cached value", func(t *testing.T) {
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

		if err := p.SetAPIKey(context.Background(), testTeamID, testNewAPIKey, "U_ADMIN"); err != nil {
			t.Fatalf("SetAPIKey: %v", err)
		}
		ddb.getOutput = &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}
		got, err = p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey after SetAPIKey: %v", err)
		}
		if got != testNewAPIKey {
			t.Fatalf("got %q want %q", got, testNewAPIKey)
		}
		if ddb.getCalls != 1 {
			t.Fatalf("GetItem calls = %d, want 1 because SetAPIKey seeds the cache", ddb.getCalls)
		}
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

	t.Run("SetAPIKey prevents in-flight stale read from repopulating cache", func(t *testing.T) {
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
		if err := p.SetAPIKey(context.Background(), testTeamID, testNewAPIKey, "U_ADMIN"); err != nil {
			t.Fatalf("SetAPIKey: %v", err)
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

		ddb.getFunc = nil
		ddb.getOutput = &dynamodb.GetItemOutput{Item: itemForKey(testOldAPIKey)}
		got, err := p.APIKey(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("APIKey after SetAPIKey: %v", err)
		}
		if got != testNewAPIKey {
			t.Fatalf("got %q want %q", got, testNewAPIKey)
		}
		if ddb.getCalls != 1 {
			t.Fatalf("GetItem calls = %d, want 1 because SetAPIKey seeds the cache", ddb.getCalls)
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
		if _, ok := p.apiKeyCache[testTeamID]; ok {
			t.Fatal("in-flight stale read should not populate the cache after DeleteAPIKey")
		}
	})
}

func TestDDBProviderSetAPIKey(t *testing.T) {
	ddb := &fakeDDBClient{}
	fixedNow := time.Unix(1700000000, 0).UTC()
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return fixedNow },
	}
	err := p.SetAPIKey(context.Background(), testTeamID, "lv_live_xxx", "U_ADMIN")
	if err != nil {
		t.Fatalf("SetAPIKey: %v", err)
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
	wantTS := fixedNow.Format(time.RFC3339)
	if v, ok := values[":now"].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != wantTS {
		t.Errorf("timestamp wrong: got %v want %q", values[":now"], wantTS)
	}
	got := *ddb.updateInput.UpdateExpression
	if !strings.Contains(got, "configured_at = if_not_exists(configured_at, :now)") {
		t.Errorf("UpdateExpression should preserve configured_at with if_not_exists, got %q", got)
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
			t.Errorf("SetAPIKey UpdateExpression should not touch Slack attr %s, got %q", attr, got)
		}
	}
	if ddb.updateInput.ReturnValues != ddbtypes.ReturnValueUpdatedOld {
		t.Errorf("ReturnValues = %v, want UPDATED_OLD for rotation observability", ddb.updateInput.ReturnValues)
	}
}

func TestDDBProviderSetAPIKeySurfacesUpdateError(t *testing.T) {
	ddb := &fakeDDBClient{updateErr: errors.New("ddb transient")}
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	err := p.SetAPIKey(context.Background(), testTeamID, "lv_live_xxx", "U_ADMIN")
	if err == nil || !strings.Contains(err.Error(), "UpdateItem") {
		t.Fatalf("expected UpdateItem error, got %v", err)
	}
	if ddb.putInput != nil {
		t.Error("PutItem must NOT run on the SetAPIKey path")
	}
}

// TestDDBProviderSetAPIKeyPreservesConfiguredAt locks the rotation
// contract: when a row already exists, configured_at retains its
// original value (the install timestamp) while updated_at moves to
// "now". A rotation that wiped configured_at would silently destroy
// audit trail.
func TestDDBProviderSetAPIKeyPreservesConfiguredAt(t *testing.T) {
	ddb := &fakeDDBClient{}
	rotatedAt := time.Unix(1800000000, 0).UTC()
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return rotatedAt },
	}
	if err := p.SetAPIKey(context.Background(), testTeamID, "lv_live_new", "U_ADMIN2"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
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

// TestDDBProviderSetAPIKeyNilNowDoesNotPanic locks the contract that
// a bare-struct DDBProvider (no Now field set) doesn't nil-deref on
// SetAPIKey. NewDDBProvider always sets Now, but tests / unusual
// constructions can produce a DDBProvider{} that previously crashed
// the moment a write path executed.
func TestDDBProviderSetAPIKeyNilNowDoesNotPanic(t *testing.T) {
	ddb := &fakeDDBClient{}
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		// Now deliberately unset.
	}
	if err := p.SetAPIKey(context.Background(), testTeamID, "lv_live", "U_x"); err != nil {
		t.Fatalf("SetAPIKey with nil Now should fall through to time.Now, got err: %v", err)
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
	ddb := &fakeDDBClient{}
	p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
	if err := p.DeleteAPIKey(context.Background(), testTeamID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if ddb.delInput == nil {
		t.Fatal("expected DeleteItem called")
	}
	if v, ok := ddb.delInput.Key[attrTeamID].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != testTeamID {
		t.Errorf("delete key wrong: %v", ddb.delInput.Key)
	}
}

// KMSEncryptor itself is covered by a round-trip test that exercises
// real AES-GCM under a fixed data key returned by a stub KMSClient.
// See ddb_provider_kms_test.go.
