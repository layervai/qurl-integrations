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
		if err == nil {
			t.Fatal("want error, got nil")
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
