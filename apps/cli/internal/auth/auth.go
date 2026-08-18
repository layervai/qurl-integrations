// Package auth resolves the qURL API credential for the CLI.
//
// The API key is the only durable customer credential. It is never accepted
// as a command-line flag (argv leaks into shell history and process lists);
// it comes from exactly two places, in this order:
//
//  1. The QURL_API_KEY environment variable — hermetic mode. When set, the
//     credential store is bypassed entirely: nothing is read from or written
//     to disk, which is what CI jobs and containers want.
//  2. The credential store: the OS keyring first, with a mode-0600 file
//     fallback used only where the keyring is unavailable. See Chain.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvAPIKey is the environment variable holding the qURL API key.
// #nosec G101 -- the NAME of the variable, not a secret.
const EnvAPIKey = "QURL_API_KEY"

// Sentinel errors; each maps to the authentication exit code.
var (
	// ErrNoCredential reports that no API key is configured anywhere.
	ErrNoCredential = errors.New("cli: no qURL API key configured")
	// ErrInvalidKey reports a configured value that cannot be a qURL API key.
	ErrInvalidKey = errors.New("cli: the configured value does not look like a qURL API key")
)

// Source names where a resolved credential came from.
type Source string

// Credential sources, in precedence order.
const (
	SourceEnvironment Source = "environment"
	SourceStore       Source = "credential store"
)

// CredentialStore persists the API key between runs. Implementations must
// never write the key anywhere a config file could pick it up.
//
// Load error contract: an error wrapping ErrNoCredential means the backend
// works but holds nothing; any other error means the backend itself is
// unavailable or broken. Chain relies on that distinction to decide when the
// file fallback may be consulted.
type CredentialStore interface {
	// Save stores the key, replacing any previous one.
	Save(key string) error
	// Load returns the stored key, or an error wrapping ErrNoCredential
	// when nothing is stored.
	Load() (string, error)
	// Delete removes the stored key, reporting whether one was removed.
	// Deleting when nothing is stored is not an error.
	Delete() (removed bool, err error)
	// Name describes the backend for user-facing messages (e.g. "file").
	Name() string
}

// Resolve returns the API key using the credential precedence contract.
// lookup provides environment access (usually os.LookupEnv); store may be nil
// when no on-disk store is available.
func Resolve(lookup func(string) (string, bool), store CredentialStore) (string, Source, error) {
	if lookup != nil {
		if v, ok := lookup(EnvAPIKey); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), SourceEnvironment, nil
		}
	}
	if store == nil {
		return "", "", ErrNoCredential
	}
	key, err := store.Load()
	if err != nil {
		return "", "", err
	}
	return key, SourceStore, nil
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

// The file fallback's on-disk contract is qurl-go's FileCredentials, so a key
// saved by `qurl login` on a machine WITHOUT an OS keyring is the same
// credential the SDK (and the Connector installer) read. That cross-tool
// sharing is a property of the keyring-less fallback only: on keyring
// machines the credential lives in the OS keyring, which file-only readers
// never consult (and the chain removes the file precisely so the two can't
// diverge) — SDK-based tools there take QURL_API_KEY instead.
//
// TODO(upstream-contract): mirrors qurl-go v0.5.3 qurl/client.go —
// UserIssuerStatePath = ".config/qurl/token" (the per-user credential file
// resolveCredentials reads) and the credentialState JSON document
// {"bearer_token": ...} its FileCredentials provider decodes. The SDK's
// readPrivateStateFile (qurl/private_state.go) refuses symlinks, non-regular
// files, file modes wider than 0600, and group/other-writable parent
// directories, which is why Save pins 0600/0700 below. If the SDK moves the
// path or the document shape, update these in lockstep.
const (
	// credentialFileName is the file inside the CLI config directory; with
	// the default directory (~/.config/qurl) the full path is exactly the
	// SDK's UserIssuerStatePath.
	credentialFileName = "token"
)

// credentialFile is qurl-go's credentialState document. Only bearer_token is
// written by the CLI; authorization is read-tolerated so a file written by
// another LayerV tool is diagnosed rather than misparsed.
type credentialFile struct {
	Authorization string `json:"authorization,omitempty"`
	BearerToken   string `json:"bearer_token,omitempty"`
}

// fileStore is the plain-file fallback CredentialStore: one key in one file
// with owner-only permissions, in the exact place and shape qurl-go's
// FileCredentials reads. It is used only where the OS keyring is unavailable.
type fileStore struct {
	path string
}

// NewFileStore returns the file-backed credential store rooted at dir
// (normally the CLI config directory). The key is stored in a separate file,
// never in config.yaml.
func NewFileStore(dir string) CredentialStore {
	return &fileStore{path: filepath.Join(dir, credentialFileName)}
}

// Name identifies the backend in user-facing messages.
func (s *fileStore) Name() string { return "file" }

// Save writes the key as the SDK's credential document with owner-only
// permissions (the SDK refuses anything wider on read).
func (s *fileStore) Save(key string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create credential dir: %w", err)
	}
	doc, err := json.Marshal(credentialFile{BearerToken: key})
	if err != nil {
		return fmt.Errorf("encode credential file: %w", err)
	}
	return os.WriteFile(s.path, append(doc, '\n'), 0o600)
}

// Load reads the stored key; a missing or empty file is ErrNoCredential.
func (s *fileStore) Load() (string, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: run `qurl login` or set %s", ErrNoCredential, EnvAPIKey)
		}
		return "", fmt.Errorf("read credential file: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("%w: the credential file is empty", ErrNoCredential)
	}
	var doc credentialFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("%w: the credential file at %s is not in the expected format — run `qurl login` to rewrite it", ErrInvalidKey, s.path)
	}
	key := strings.TrimSpace(doc.BearerToken)
	if key == "" {
		if strings.TrimSpace(doc.Authorization) != "" {
			// Another LayerV tool stored a raw authorization header here; the
			// CLI manages API keys only. Refuse rather than misuse it.
			return "", fmt.Errorf("%w: the credential file at %s does not hold a qURL API key — run `qurl login` to replace it", ErrInvalidKey, s.path)
		}
		return "", fmt.Errorf("%w: the credential file holds no key", ErrNoCredential)
	}
	return key, nil
}

// Delete removes the stored key; deleting nothing is a no-op.
func (s *fileStore) Delete() (bool, error) {
	err := os.Remove(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("remove credential file: %w", err)
	}
	return true, nil
}
