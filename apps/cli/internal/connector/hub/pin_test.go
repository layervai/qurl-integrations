package hub

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/curve25519"
)

// usableKeyB64 derives the pin from real scalar multiplication so the full
// validation chain runs against a usable key.
func usableKeyB64(t *testing.T) (keyB64 string, key []byte) {
	t.Helper()
	scalar := bytes.Repeat([]byte{0x42}, curve25519.ScalarSize)
	public, err := curve25519.X25519(scalar, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(public), public
}

func TestDecodeServerPublicKeyB64AcceptsUsableKey(t *testing.T) {
	keyB64, want := usableKeyB64(t)
	got, err := DecodeServerPublicKeyB64(keyB64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded key = %x, want %x", got, want)
	}
}

func TestDecodeServerPublicKeyB64Rejections(t *testing.T) {
	keyB64, _ := usableKeyB64(t)
	noncanonicalKey := make([]byte, curve25519.PointSize)
	for i := range noncanonicalKey {
		noncanonicalKey[i] = 0xff
	}
	noncanonicalKey[0] = 0xed
	noncanonicalKey[len(noncanonicalKey)-1] = 0x7f
	highBitKey := make([]byte, curve25519.PointSize)
	highBitKey[len(highBitKey)-1] = 0x80
	// 0xfb's leading six bits are 111110 = index 62, so the standard alphabet
	// spells this key "+wAA…" and the URL-safe alphabet "-wAA…" — a guaranteed
	// alphabet difference for the strict-decode case below.
	urlAlphabetKey := base64.URLEncoding.EncodeToString(append([]byte{0xfb}, make([]byte, curve25519.PointSize-1)...))

	tests := []struct {
		name, key, wantErr string
	}{
		// "" is canonical base64 of zero bytes, so it falls through to the
		// length check — Bootstrap rejects empty values before validation
		// anyway.
		{name: "empty", key: "", wantErr: "32-byte X25519 public key"},
		{name: "malformed base64", key: "%%%not-base64%%%", wantErr: "canonical padded standard base64"},
		{name: "unpadded base64", key: strings.TrimRight(keyB64, "="), wantErr: "canonical padded standard base64"},
		{name: "url-safe alphabet", key: urlAlphabetKey, wantErr: "canonical padded standard base64"},
		{name: "wrong length", key: base64.StdEncoding.EncodeToString(make([]byte, 31)), wantErr: "32-byte X25519"},
		{name: "field prime spelling", key: base64.StdEncoding.EncodeToString(noncanonicalKey), wantErr: "canonical X25519"},
		{name: "high bit set", key: base64.StdEncoding.EncodeToString(highBitKey), wantErr: "canonical X25519"},
		{name: "low order zero key", key: base64.StdEncoding.EncodeToString(make([]byte, curve25519.PointSize)), wantErr: "usable X25519"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeServerPublicKeyB64(tt.key)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("DecodeServerPublicKeyB64(%q) error = %v, want containing %q", tt.key, err, tt.wantErr)
			}
			// Every rejection must read as a sentence after an origin prefix
			// (an env var name or build-input name), matching how Bootstrap
			// and a release-side pin verifier report it.
			if !strings.HasPrefix(err.Error(), "must ") {
				t.Fatalf("rejection %q is not prefixable by the candidate's origin", err)
			}
		})
	}
}

func TestFingerprintSHA256Hex(t *testing.T) {
	// SHA-256 of 32 zero bytes, independently computable; pins the raw-key
	// (not base64-string) hashing convention the fingerprint file uses.
	got := FingerprintSHA256Hex(make([]byte, 32))
	want := "66687aadf862bd776c8fc18b8e9f8e20089714856ee233b3902a591d0d5f2925"
	if got != want {
		t.Fatalf("FingerprintSHA256Hex(zero key) = %q, want %q", got, want)
	}
}

func TestParseFingerprintFile(t *testing.T) {
	fingerprint := strings.Repeat("ab", 32)
	tests := []struct {
		name, contents, want, wantErr string
	}{
		{name: "bare fingerprint", contents: fingerprint + "\n", want: fingerprint},
		{name: "no trailing newline", contents: fingerprint, want: fingerprint},
		{name: "provenance comments and blanks", contents: "# provisioning ceremony record: <link>\n\n" + fingerprint + "\n", want: fingerprint},
		{name: "empty file", contents: "", wantErr: "no fingerprint line"},
		{name: "comments only", contents: "# pending\n", wantErr: "no fingerprint line"},
		{name: "uppercase hex", contents: strings.ToUpper(fingerprint), wantErr: "lowercase hex SHA-256"},
		{name: "short line", contents: fingerprint[:63], wantErr: "lowercase hex SHA-256"},
		{name: "full key instead of hash", contents: strings.Repeat("A", 43) + "=", wantErr: "lowercase hex SHA-256"},
		{name: "two fingerprints", contents: fingerprint + "\n" + fingerprint + "\n", wantErr: "more than one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseFingerprintFile([]byte(tt.contents))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseFingerprintFile error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ParseFingerprintFile = (%q, %v), want (%q, nil)", got, err, tt.want)
			}
		})
	}
}
