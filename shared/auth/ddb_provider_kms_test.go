package auth

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

const testKMSKeyARN = "arn:fake"

// fakeKMS implements KMSClient with a fixed 32-byte data key, so the
// AES-GCM round-trip in KMSEncryptor.Seal/Open exercises real crypto
// without hitting AWS.
type fakeKMS struct {
	dataKey     []byte // 32 bytes (AES-256)
	wrappedBlob []byte // ciphertext returned to caller
	genErr      error
	decErr      error
}

var _ KMSClient = (*fakeKMS)(nil)

func (f *fakeKMS) GenerateDataKey(_ context.Context, _ *kms.GenerateDataKeyInput, _ ...func(*kms.Options)) (*kms.GenerateDataKeyOutput, error) {
	if f.genErr != nil {
		return nil, f.genErr
	}
	return &kms.GenerateDataKeyOutput{
		Plaintext:      append([]byte(nil), f.dataKey...),
		CiphertextBlob: append([]byte(nil), f.wrappedBlob...),
		KeyId:          aws.String(testKMSKeyARN),
	}, nil
}

func (f *fakeKMS) Decrypt(_ context.Context, _ *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	if f.decErr != nil {
		return nil, f.decErr
	}
	return &kms.DecryptOutput{
		Plaintext: append([]byte(nil), f.dataKey...),
		KeyId:     aws.String(testKMSKeyARN),
	}, nil
}

func TestKMSEncryptorRoundTrip(t *testing.T) {
	dataKey := make([]byte, 32)
	for i := range dataKey {
		dataKey[i] = byte(i)
	}
	enc := &KMSEncryptor{
		Client: &fakeKMS{dataKey: dataKey, wrappedBlob: []byte("wrapped")},
		KeyID:  testKMSKeyARN,
	}
	pt := []byte("lv_live_secret")
	aad := []byte("T123ABCDEF")

	ct, wrapped, err := enc.Seal(context.Background(), pt, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !bytes.Equal(wrapped, []byte("wrapped")) {
		t.Errorf("wrapped key not threaded through: %q", wrapped)
	}
	if bytes.Equal(ct, pt) {
		t.Error("ciphertext equals plaintext — encryption clearly not running")
	}

	got, err := enc.Open(context.Background(), ct, wrapped, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Errorf("round-trip mismatch: got %q want %q", got, pt)
	}
}

func TestKMSEncryptorOpenAADMismatch(t *testing.T) {
	dataKey := make([]byte, 32)
	enc := &KMSEncryptor{
		Client: &fakeKMS{dataKey: dataKey, wrappedBlob: []byte("wrapped")},
		KeyID:  testKMSKeyARN,
	}
	ct, wrapped, err := enc.Seal(context.Background(), []byte("pt"), []byte("aad-A"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := enc.Open(context.Background(), ct, wrapped, []byte("aad-B")); err == nil {
		t.Fatal("expected Open with wrong AAD to fail")
	}
}

func TestKMSEncryptorSealGenErrorPropagates(t *testing.T) {
	enc := &KMSEncryptor{
		Client: &fakeKMS{genErr: errors.New("KMS access denied")},
		KeyID:  testKMSKeyARN,
	}
	_, _, err := enc.Seal(context.Background(), []byte("pt"), nil)
	if err == nil {
		t.Fatal("want error, got nil")
	}
}
