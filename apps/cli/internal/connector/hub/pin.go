// Package hub resolves and validates the NHP Hub trust bootstrap for the
// CLI's qURL Connector: which Hub endpoint to talk to, and which pinned
// server public key proves it is the real one.
//
// Trust posture: this build ships DARK. The production pin variable below is
// empty in source and in every dev/CI build, so with no environment override
// the bootstrap fails closed instead of trusting DNS without a pinned server
// identity. Custom and test deployments supply the all-or-none
// QURL_CONNECTOR_HUB_HOST / QURL_CONNECTOR_HUB_PORT /
// QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64 triple.
//
// Pin flip procedure (reference): once the production Hub trust root is
// provisioned, the release workflow injects the pin at build time via
// -ldflags "-X <this package>.defaultServerPublicKeyB64=<base64 key>",
// gated on a committed SHA-256 fingerprint of the raw key
// (FingerprintSHA256Hex is the spelling contract) and verified by a
// release-side check that decodes the candidate with exactly this package's
// DecodeServerPublicKeyB64 before any artifact is published. A key the
// release pipeline accepts is exactly a key this package will accept, and a
// key this package would reject at startup can never be published. Source
// and unflagged builds stay dark; TestDefaultPinRemainsUnprovisionedInSource
// fences that.
package hub

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// DecodeServerPublicKeyB64 decodes a candidate Hub server public key and
// returns its raw bytes. A pinned trust root must have one byte spelling, so
// the input must be the canonical padded standard-base64 encoding of a
// canonical, usable X25519 public key; anything else is rejected. Error
// messages are phrased so callers can prefix the candidate's origin (an env
// var name, a repository-variable name) and read a sentence.
func DecodeServerPublicKeyB64(keyB64 string) ([]byte, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(keyB64)
	if err != nil || base64.StdEncoding.EncodeToString(key) != keyB64 {
		return nil, errors.New("must be canonical padded standard base64")
	}
	if len(key) != curve25519.PointSize {
		return nil, fmt.Errorf("must encode a %d-byte X25519 public key", curve25519.PointSize)
	}
	if !canonicalPublicKey(key) {
		return nil, errors.New("must encode a canonical X25519 public key")
	}
	if _, err := curve25519.X25519(make([]byte, curve25519.ScalarSize), key); err != nil {
		return nil, fmt.Errorf("must encode a usable X25519 public key: %w", err)
	}
	return key, nil
}

func canonicalPublicKey(key []byte) bool {
	// X25519's field prime is 2^255-19, encoded little-endian as
	// ed ff ... ff 7f. RFC 7748 permits non-canonical inputs for generic
	// interoperability, but a pinned identity must have one byte spelling.
	for i := len(key) - 1; i >= 0; i-- {
		primeByte := byte(0xff)
		switch i {
		case 0:
			primeByte = 0xed
		case curve25519.PointSize - 1:
			primeByte = 0x7f
		}
		if key[i] != primeByte {
			return key[i] < primeByte
		}
	}
	return false
}

// FingerprintSHA256Hex returns the lowercase hex SHA-256 of the raw key
// bytes — the spelling used by the committed release fingerprint file and by
// operator-facing pin output.
func FingerprintSHA256Hex(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

var fingerprintLine = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseFingerprintFile parses the committed release fingerprint file: exactly
// one lowercase hex SHA-256 line, plus optional blank lines and `#` comments
// (for recording the key's provenance next to the value). Anything else —
// including zero or multiple fingerprint lines, uppercase hex, or a full key
// pasted where its hash belongs — is an error, so a malformed flip commit
// fails loudly instead of being skipped as "no pin expected".
func ParseFingerprintFile(contents []byte) (string, error) {
	fingerprint := ""
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if !fingerprintLine.MatchString(text) {
			return "", fmt.Errorf("line %d: expected a lowercase hex SHA-256 of the raw 32-byte key, got %q", line, text)
		}
		if fingerprint != "" {
			return "", fmt.Errorf("line %d: more than one fingerprint line", line)
		}
		fingerprint = text
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if fingerprint == "" {
		return "", errors.New("no fingerprint line found")
	}
	return fingerprint, nil
}
