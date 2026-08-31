// Package auth resolves the qURL API credential for the CLI.
//
// The account API key is a bootstrap or explicit-recovery credential. Current
// CLI commands never store it; steady-state commands use the registered device
// identity. It is never accepted as a command-line flag (argv leaks into shell
// history and process lists). Bootstrap resolves it from these sources:
//
//  1. The QURL_API_KEY_FILE environment variable — private-file hermetic mode.
//  2. The QURL_API_KEY environment variable — inline hermetic mode.
//
// Neither source is written to persistent account-key storage.
package auth

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// EnvAPIKey is the environment variable holding the qURL API key.
	// #nosec G101 -- the NAME of the variable, not a secret.
	EnvAPIKey = "QURL_API_KEY"
	// EnvAPIKeyFile names one exact owner-only file holding the qURL API key.
	// #nosec G101 -- the NAME of the variable, not a secret.
	EnvAPIKeyFile = "QURL_API_KEY_FILE"
)

// Sentinel errors; each maps to the authentication exit code.
var (
	// ErrNoCredential reports that no API key is configured anywhere.
	ErrNoCredential = errors.New("cli: no qURL API key configured")
	// ErrInvalidKey reports a configured value that cannot be a qURL API key.
	ErrInvalidKey = errors.New("cli: the configured value does not look like a qURL API key")
	// ErrCredentialConflict rejects ambiguous inline and file authority.
	ErrCredentialConflict = errors.New("cli: QURL_API_KEY and QURL_API_KEY_FILE cannot both be set")
	// ErrDeviceAccountConflict rejects reuse of one durable device state
	// directory across different qURL accounts.
	ErrDeviceAccountConflict = errors.New("cli: registered device state belongs to a different qURL account")
)

// Source names where a resolved credential came from.
type Source string

// Credential sources, in precedence order.
const (
	SourceEnvironment     Source = "environment"
	SourceEnvironmentFile Source = "environment file"
)

// Resolve returns the API key using the credential precedence contract.
// lookup provides environment access (usually os.LookupEnv).
func Resolve(lookup func(string) (string, bool)) (string, Source, error) {
	if lookup != nil {
		inline, inlineSet := lookup(EnvAPIKey)
		path, fileSet := lookup(EnvAPIKeyFile)
		inline = strings.TrimSpace(inline)
		// Empty environment variables are common in shared CI templates. Treat
		// an empty file variable as unset, just as an empty inline variable is
		// unset. A non-empty path remains byte-strict in the file reader below.
		if fileSet && strings.TrimSpace(path) == "" {
			fileSet = false
		}
		if inlineSet && inline != "" && fileSet {
			return "", "", ErrCredentialConflict
		}
		if fileSet {
			key, err := readAPIKeyEnvironmentFile(path)
			if err != nil {
				return "", "", err
			}
			return key, SourceEnvironmentFile, nil
		}
		if inlineSet && inline != "" {
			return inline, SourceEnvironment, nil
		}
	}
	return "", "", ErrNoCredential
}

// The pinned qURL API key wire format.
//
// TODO(upstream-contract): mirrors qurl-service's single canonical key
// construction — internal/domain/apikey.go (APIKeyLivePrefix/APIKeyTestPrefix,
// APIKeySecretBytes=32, APIKeySecretLength=51) and generateAPIKeySecret in
// internal/service/apikey_service.go: prefix + 32 random bytes as unpadded
// URL-safe base-64, i.e. 8 + 43 = 51 characters over [A-Za-z0-9_-] after the
// prefix. The prefix and charset are pinned exactly; the length is a FLOOR
// (today's mint, 43 after the prefix) rather than an exact match, so a
// server-side move to longer keys cannot brick already-shipped CLIs while a
// truncated paste is still caught. If the service ever changes the prefix,
// charset, or shortens the mint, update these in lockstep.
const (
	keyPrefixLive = "lv_live_"
	keyPrefixTest = "lv_test_"
	// keySecretLength is today's minted character count after the prefix,
	// enforced as a minimum.
	keySecretLength = 43
	// keyLength is today's full wire length of a qURL API key.
	keyLength = len(keyPrefixLive) + keySecretLength
)

// ValidateKeyShape checks that key has the shape of a qURL API key before it
// is sent anywhere: a live or test prefix followed by at least 43 characters
// of unpadded URL-safe base-64 alphabet. The service remains the authority on
// whether the key is real; this gate only stops obvious mistakes (a pasted
// fragment, the wrong secret entirely) from going on the wire.
func ValidateKeyShape(key string) error {
	rest, ok := strings.CutPrefix(key, keyPrefixLive)
	if !ok {
		rest, ok = strings.CutPrefix(key, keyPrefixTest)
	}
	if !ok {
		return fmt.Errorf("%w: it should start with lv_live_ or lv_test_", ErrInvalidKey)
	}
	if len(rest) < keySecretLength {
		return fmt.Errorf("%w: a full key is at least %d characters, this value is %d", ErrInvalidKey, keyLength, len(key))
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if !isAlpha && !isDigit && c != '-' && c != '_' {
			return fmt.Errorf("%w: it contains unexpected characters", ErrInvalidKey)
		}
	}
	return nil
}
