package hub

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
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

func TestReleaseHubPinEnvironment(t *testing.T) {
	keyB64, fingerprint, skip, err := releaseHubPinInputs(
		os.Getenv("QURL_RELEASE_HUB_PUBLIC_KEY_B64"),
		os.Getenv("QURL_RELEASE_HUB_PUBLIC_KEY_SHA256"),
		os.Getenv("QURL_REQUIRE_RELEASE_HUB_PIN") == "1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if skip {
		t.Skip("release Hub pin inputs are not present")
	}
	key, err := DecodeServerPublicKeyB64(keyB64)
	if err != nil {
		t.Fatalf("release Hub public key %v", err)
	}
	if got := FingerprintSHA256Hex(key); got != fingerprint {
		t.Fatalf("release Hub public-key fingerprint = %q, want configured fingerprint", got)
	}
}

func releaseHubPinInputs(keyB64, fingerprint string, required bool) (normalizedKeyB64, normalizedFingerprint string, skip bool, err error) {
	if keyB64 != strings.TrimSpace(keyB64) || fingerprint != strings.TrimSpace(fingerprint) {
		return "", "", false, errors.New("release Hub public key and fingerprint must not have surrounding whitespace")
	}
	if keyB64 == "" && fingerprint == "" && !required {
		return "", "", true, nil
	}
	if keyB64 == "" || fingerprint == "" {
		return "", "", false, errors.New("release Hub public key and fingerprint must both be non-empty")
	}
	return keyB64, fingerprint, false, nil
}

func TestReleaseHubPinInputs(t *testing.T) {
	tests := []struct {
		name, key, fingerprint string
		required, wantSkip     bool
		wantErr                bool
	}{
		{name: "dark optional", wantSkip: true},
		{name: "dark required", required: true, wantErr: true},
		{name: "key only optional", key: "key", wantErr: true},
		{name: "key only required", key: "key", required: true, wantErr: true},
		{name: "fingerprint only optional", fingerprint: "fingerprint", wantErr: true},
		{name: "fingerprint only required", fingerprint: "fingerprint", required: true, wantErr: true},
		{name: "configured optional", key: "key", fingerprint: "fingerprint"},
		{name: "configured required", key: "key", fingerprint: "fingerprint", required: true},
		{name: "key leading whitespace", key: " key", fingerprint: "fingerprint", wantErr: true},
		{name: "key trailing whitespace", key: "key ", fingerprint: "fingerprint", wantErr: true},
		{name: "fingerprint leading whitespace", key: "key", fingerprint: " fingerprint", wantErr: true},
		{name: "fingerprint trailing whitespace", key: "key", fingerprint: "fingerprint\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, fingerprint, skip, err := releaseHubPinInputs(tt.key, tt.fingerprint, tt.required)
			if (err != nil) != tt.wantErr || skip != tt.wantSkip {
				t.Fatalf("releaseHubPinInputs() = (%q, %q, %t, %v), want skip=%t err=%t", key, fingerprint, skip, err, tt.wantSkip, tt.wantErr)
			}
			if err == nil && !skip && (key != "key" || fingerprint != "fingerprint") {
				t.Fatalf("releaseHubPinInputs() normalized values = (%q, %q), want (key, fingerprint)", key, fingerprint)
			}
		})
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
