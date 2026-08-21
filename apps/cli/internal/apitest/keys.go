// Package apitest is the mock qURL API used by the CLI's contract tests.
//
// It speaks the platform's response envelope and problem+json shapes, can be
// scripted with failure sequences (429 with Retry-After, dark 503), records
// every request for header assertions, and — because it owns real resource
// keys — can serve either consistent CRIDs or deliberately mismatched ones
// to exercise the fail-closed verification path.
package apitest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"testing"
)

// CRID version bytes from the frozen registry: full 32-byte digest forms for
// production and test environments.
const (
	VersionProduction = byte(0x01)
	VersionTest       = byte(0x81)
)

// ResourceKey is one generated resource keypair as the platform would serve
// it: the DER SubjectPublicKeyInfo bytes, the base64url public resource
// identifier, and the CRID derived for the test environment.
type ResourceKey struct {
	DER        []byte
	ResourceID string
	CRID       string
}

// GenerateResourceKey mints a fresh P-256 resource key. The derived CRID
// uses the test-environment version byte, matching what a sandbox serves.
func GenerateResourceKey(t *testing.T) *ResourceKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate resource key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("encode resource key: %v", err)
	}
	return resourceKeyFromDER(t, der)
}

// fixtureDERHex is a fixed, publicly known P-256 public key (a test fixture,
// not a credential) so golden files can carry a stable resource id and CRID.
const fixtureDERHex = "3059301306072a8648ce3d020106082a8648ce3d03010703420004" +
	"d79e56d5e95ed2ad002bf60566742acdf8492f1a8b807686dc1e6b5f33cef3b5" +
	"9996761ae535a5adb20ed82e802ef2f14fbcda1569bd7dcf0f0a496ad6fe1aae"

// FixedResourceKey returns the deterministic fixture key golden tests use.
func FixedResourceKey(t *testing.T) *ResourceKey {
	t.Helper()
	der, err := hex.DecodeString(fixtureDERHex)
	if err != nil {
		t.Fatalf("decode fixture key: %v", err)
	}
	return resourceKeyFromDER(t, der)
}

func resourceKeyFromDER(t *testing.T, der []byte) *ResourceKey {
	t.Helper()
	return &ResourceKey{
		DER:        der,
		ResourceID: base64.RawURLEncoding.EncodeToString(der),
		CRID:       DeriveCRID(t, der, VersionTest),
	}
}

// The frozen derivation constants, duplicated from the public CRID contract
// (the crid package deliberately exports no encoder). TestDeriveCRIDMatchesSDK
// pins this twin against crid.KeyMatches so it cannot drift from the SDK.
const (
	domainSeparationPrefix = "NHP-QURL-CRID-V1"
	domainSeparator        = byte(0x00)
	cridAlphabet           = "abcdefghijklmnopqrstuvwxyz234567"
)

// DeriveCRID derives the CRID for a resource public key (DER
// SubjectPublicKeyInfo bytes) under the given version byte, full-digest
// form: version || SHA-256(domain-separated key) || CRC32C, base32 lowercase
// unpadded.
func DeriveCRID(t *testing.T, derSPKI []byte, version byte) string {
	t.Helper()
	message := make([]byte, 0, len(domainSeparationPrefix)+1+len(derSPKI))
	message = append(message, domainSeparationPrefix...)
	message = append(message, domainSeparator)
	message = append(message, derSPKI...)
	digest := sha256.Sum256(message)

	payload := make([]byte, 0, 1+len(digest)+4)
	payload = append(payload, version)
	payload = append(payload, digest[:]...)

	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], crc32.Checksum(payload, crc32.MakeTable(crc32.Castagnoli)))
	payload = append(payload, checksum[:]...)

	return base32.NewEncoding(cridAlphabet).WithPadding(base32.NoPadding).EncodeToString(payload)
}
