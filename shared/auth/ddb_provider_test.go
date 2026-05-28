package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const testTeamID = "T123ABCDEF"

// fakeDDBClient is a hand-rolled stub the table tests configure with
// predetermined results. Captures Put/Delete inputs for assertion.
type fakeDDBClient struct {
	getOutput *dynamodb.GetItemOutput
	getErr    error
	putInput  *dynamodb.PutItemInput
	putErr    error
	delInput  *dynamodb.DeleteItemInput
	delErr    error
}

func (f *fakeDDBClient) GetItem(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return f.getOutput, f.getErr
}
func (f *fakeDDBClient) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putInput = in
	return &dynamodb.PutItemOutput{}, f.putErr
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
}

const passthroughWrappedKey = "DK"

func (p *passthroughEncryptor) Seal(_ context.Context, plaintext, _ []byte) (ciphertext, wrappedKey []byte, err error) {
	if p.sealErr != nil {
		return nil, nil, p.sealErr
	}
	return append([]byte(nil), plaintext...), []byte(passthroughWrappedKey), nil
}
func (p *passthroughEncryptor) Open(_ context.Context, ciphertext, wrappedKey, _ []byte) ([]byte, error) {
	if p.openErr != nil {
		return nil, p.openErr
	}
	if string(wrappedKey) != passthroughWrappedKey {
		return nil, errors.New("wrong wrapped key")
	}
	return append([]byte(nil), ciphertext...), nil
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
			t.Fatalf("want ErrWorkspaceNotConfigured, got %v", err)
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
	if ddb.putInput == nil {
		t.Fatal("expected PutItem called")
	}
	item := ddb.putInput.Item
	if v, ok := item[attrTeamID].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != testTeamID {
		t.Errorf("team_id wrong: %v", item[attrTeamID])
	}
	if v, ok := item[attrQURLAPIKey].(*ddbtypes.AttributeValueMemberB); !ok || string(v.Value) != "lv_live_xxx" {
		t.Errorf("qurl_api_key wrong: %v", item[attrQURLAPIKey])
	}
	if v, ok := item[attrDataKeyCT].(*ddbtypes.AttributeValueMemberB); !ok || string(v.Value) != passthroughWrappedKey {
		t.Errorf("dk wrong: %v", item[attrDataKeyCT])
	}
	if v, ok := item[attrConfiguredBy].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != "U_ADMIN" {
		t.Errorf("configured_by wrong: %v", item[attrConfiguredBy])
	}
	wantTS := fixedNow.Format(time.RFC3339)
	if v, ok := item[attrUpdatedAt].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != wantTS {
		t.Errorf("updated_at wrong: got %v want %q", item[attrUpdatedAt], wantTS)
	}
	if v, ok := item[attrConfiguredAt].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != wantTS {
		t.Errorf("configured_at wrong: got %v want %q", item[attrConfiguredAt], wantTS)
	}
}

// TestDDBProviderSetAPIKeyFailsFastOnGetItemError locks the contract
// that a transient pre-flight GetItem failure aborts the write rather
// than degrading to "destroy configured_at on rotation". Without the
// fail-fast, a transport blip would silently wipe the original install
// timestamp.
func TestDDBProviderSetAPIKeyFailsFastOnGetItemError(t *testing.T) {
	ddb := &fakeDDBClient{getErr: errors.New("ddb transient")}
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	err := p.SetAPIKey(context.Background(), testTeamID, "lv_live_xxx", "U_ADMIN")
	if err == nil {
		t.Fatal("expected error when pre-flight GetItem fails")
	}
	if ddb.putInput != nil {
		t.Error("PutItem must NOT run when pre-flight GetItem errored")
	}
}

// TestDDBProviderSetAPIKeyPreservesConfiguredAt locks the rotation
// contract: when a row already exists, configured_at retains its
// original value (the install timestamp) while updated_at moves to
// "now". A rotation that wiped configured_at would silently destroy
// audit trail.
func TestDDBProviderSetAPIKeyPreservesConfiguredAt(t *testing.T) {
	original := "2026-01-01T00:00:00Z"
	ddb := &fakeDDBClient{
		getOutput: &dynamodb.GetItemOutput{
			Item: map[string]ddbtypes.AttributeValue{
				attrTeamID:       &ddbtypes.AttributeValueMemberS{Value: testTeamID},
				attrQURLAPIKey:   &ddbtypes.AttributeValueMemberB{Value: []byte("old-ct")},
				attrDataKeyCT:    &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
				attrConfiguredAt: &ddbtypes.AttributeValueMemberS{Value: original},
			},
		},
	}
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
	item := ddb.putInput.Item
	if v, ok := item[attrConfiguredAt].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != original {
		t.Errorf("configured_at must preserve original on rotation: got %v want %q", item[attrConfiguredAt], original)
	}
	wantUpdated := rotatedAt.Format(time.RFC3339)
	if v, ok := item[attrUpdatedAt].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != wantUpdated {
		t.Errorf("updated_at must move to rotation time: got %v want %q", item[attrUpdatedAt], wantUpdated)
	}
}

func TestDDBProviderSetAPIKeyPreservesSlackBotToken(t *testing.T) {
	ddb := &fakeDDBClient{
		getOutput: &dynamodb.GetItemOutput{
			Item: map[string]ddbtypes.AttributeValue{
				attrTeamID:              &ddbtypes.AttributeValueMemberS{Value: testTeamID},
				attrSlackBotToken:       &ddbtypes.AttributeValueMemberB{Value: []byte("xoxb-existing")},
				attrSlackBotTokenDK:     &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
				attrSlackBotInstalledAt: &ddbtypes.AttributeValueMemberS{Value: "2026-05-01T00:00:00Z"},
			},
		},
	}
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return time.Unix(1800000000, 0).UTC() },
	}
	if err := p.SetAPIKey(context.Background(), testTeamID, "lv_live_new", "U_ADMIN"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}
	item := ddb.putInput.Item
	if v, ok := item[attrSlackBotToken].(*ddbtypes.AttributeValueMemberB); !ok || string(v.Value) != "xoxb-existing" {
		t.Errorf("slack_bot_token must be preserved: got %v", item[attrSlackBotToken])
	}
	if v, ok := item[attrQURLAPIKey].(*ddbtypes.AttributeValueMemberB); !ok || string(v.Value) != "lv_live_new" {
		t.Errorf("qurl_api_key wrong: got %v", item[attrQURLAPIKey])
	}
}

func TestDDBProviderSlackBotToken(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		ddb := &fakeDDBClient{
			getOutput: &dynamodb.GetItemOutput{
				Item: map[string]ddbtypes.AttributeValue{
					attrTeamID:          &ddbtypes.AttributeValueMemberS{Value: testTeamID},
					attrSlackBotToken:   &ddbtypes.AttributeValueMemberB{Value: []byte("xoxb-installed")},
					attrSlackBotTokenDK: &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
				},
			},
		}
		p := &DDBProvider{Client: ddb, TableName: "ws", Encryptor: &passthroughEncryptor{}}
		got, err := p.SlackBotToken(context.Background(), testTeamID)
		if err != nil {
			t.Fatalf("SlackBotToken: %v", err)
		}
		if got != "xoxb-installed" {
			t.Fatalf("got %q want xoxb-installed", got)
		}
	})

	t.Run("not installed", func(t *testing.T) {
		ddb := &fakeDDBClient{getOutput: &dynamodb.GetItemOutput{}}
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
	if err := p.SetSlackBotToken(context.Background(), testTeamID, "xoxb-installed", "U_INSTALLER"); err != nil {
		t.Fatalf("SetSlackBotToken: %v", err)
	}
	item := ddb.putInput.Item
	if v, ok := item[attrSlackBotToken].(*ddbtypes.AttributeValueMemberB); !ok || string(v.Value) != "xoxb-installed" {
		t.Errorf("slack_bot_token wrong: got %v", item[attrSlackBotToken])
	}
	if v, ok := item[attrSlackBotTokenDK].(*ddbtypes.AttributeValueMemberB); !ok || string(v.Value) != passthroughWrappedKey {
		t.Errorf("slack_bot_token_dk wrong: got %v", item[attrSlackBotTokenDK])
	}
	if v, ok := item[attrSlackBotInstalledBy].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != "U_INSTALLER" {
		t.Errorf("slack_bot_installed_by wrong: got %v", item[attrSlackBotInstalledBy])
	}
	wantTS := fixedNow.Format(time.RFC3339)
	if v, ok := item[attrSlackBotInstalledAt].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != wantTS {
		t.Errorf("slack_bot_installed_at wrong: got %v want %q", item[attrSlackBotInstalledAt], wantTS)
	}
}

func TestDDBProviderSetSlackBotTokenPreservesAPIKey(t *testing.T) {
	ddb := &fakeDDBClient{
		getOutput: &dynamodb.GetItemOutput{
			Item: map[string]ddbtypes.AttributeValue{
				attrTeamID:       &ddbtypes.AttributeValueMemberS{Value: testTeamID},
				attrQURLAPIKey:   &ddbtypes.AttributeValueMemberB{Value: []byte("lv_live_existing")},
				attrDataKeyCT:    &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
				attrConfiguredAt: &ddbtypes.AttributeValueMemberS{Value: "2026-05-01T00:00:00Z"},
			},
		},
	}
	p := &DDBProvider{
		Client:    ddb,
		TableName: "ws",
		Encryptor: &passthroughEncryptor{},
		Now:       func() time.Time { return time.Unix(1800000000, 0).UTC() },
	}
	if err := p.SetSlackBotToken(context.Background(), testTeamID, "xoxb-new", "U_INSTALLER"); err != nil {
		t.Fatalf("SetSlackBotToken: %v", err)
	}
	item := ddb.putInput.Item
	if v, ok := item[attrQURLAPIKey].(*ddbtypes.AttributeValueMemberB); !ok || string(v.Value) != "lv_live_existing" {
		t.Errorf("qurl_api_key must be preserved: got %v", item[attrQURLAPIKey])
	}
	if v, ok := item[attrSlackBotToken].(*ddbtypes.AttributeValueMemberB); !ok || string(v.Value) != "xoxb-new" {
		t.Errorf("slack_bot_token wrong: got %v", item[attrSlackBotToken])
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
