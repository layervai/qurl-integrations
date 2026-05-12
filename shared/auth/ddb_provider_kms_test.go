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
	_, _, err := enc.Seal(context.Background(), []byte("pt"), []byte("workspace-id"))
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

// TestKMSEncryptorRejectsEmptyAAD locks the fail-fast on AAD-required:
// the workspace_id binding is what blocks cross-workspace ciphertext
// swap, and a Seal/Open call without it is a misuse to surface loudly.
func TestKMSEncryptorRejectsEmptyAAD(t *testing.T) {
	enc := &KMSEncryptor{
		Client: &fakeKMS{dataKey: make([]byte, 32), wrappedBlob: []byte("wrapped")},
		KeyID:  testKMSKeyARN,
	}
	if _, _, err := enc.Seal(context.Background(), []byte("pt"), nil); err == nil {
		t.Error("Seal with nil aad must reject")
	}
	if _, err := enc.Open(context.Background(), []byte("ciphertext-long-enough"), []byte("wrapped"), nil); err == nil {
		t.Error("Open with nil aad must reject")
	}
}

// TestKMSEncryptorOpenRejectsTruncatedCiphertext exercises the short-
// ciphertext guard. A ciphertext shorter than the GCM nonce can only
// arise from a corrupted DDB row; we surface it before attempting
// AES-GCM open so the error message is useful for incident response.
func TestKMSEncryptorOpenRejectsTruncatedCiphertext(t *testing.T) {
	enc := &KMSEncryptor{
		Client: &fakeKMS{dataKey: make([]byte, 32), wrappedBlob: []byte("wrapped")},
		KeyID:  testKMSKeyARN,
	}
	// gcmNonceSize is 12; 5 bytes is unambiguously too short.
	if _, err := enc.Open(context.Background(), []byte("short"), []byte("wrapped"), nil); err == nil {
		t.Fatal("expected truncated-ciphertext error")
	}
}

// TestKMSEncryptorEncryptionContextWorkspaceID locks the CloudTrail-
// visible attribute name. A renamed key would silently break attribution
// across versions; pinning the wire-side name catches that.
func TestKMSEncryptorEncryptionContextWorkspaceID(t *testing.T) {
	captured := &capturingKMS{fakeKMS: fakeKMS{dataKey: make([]byte, 32), wrappedBlob: []byte("wrapped")}}
	enc := &KMSEncryptor{Client: captured, KeyID: testKMSKeyARN}
	if _, _, err := enc.Seal(context.Background(), []byte("pt"), []byte("T123ABCDEF")); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got := captured.lastContext["workspace_id"]; got != "T123ABCDEF" {
		t.Errorf("EncryptionContext[workspace_id]: got %q want %q", got, "T123ABCDEF")
	}
	if _, ok := captured.lastContext["aad"]; ok {
		t.Error("legacy EncryptionContext[aad] should no longer be set")
	}
}

type capturingKMS struct {
	fakeKMS
	lastContext map[string]string
}

func (c *capturingKMS) GenerateDataKey(ctx context.Context, in *kms.GenerateDataKeyInput, opts ...func(*kms.Options)) (*kms.GenerateDataKeyOutput, error) {
	c.lastContext = in.EncryptionContext
	return c.fakeKMS.GenerateDataKey(ctx, in, opts...)
}
