// Package auth resolves the qURL API credential for the CLI.
//
// The API key is the only durable customer credential. It is never accepted
// as a command-line flag (argv leaks into shell history and process lists);
// it comes from exactly two places, in this order:
//
//  1. The QURL_API_KEY environment variable — hermetic mode. When set, the
//     credential store is bypassed entirely: nothing is read from or written
//     to disk, which is what CI jobs and containers want.
//  2. The credential store. This build ships the plain-file fallback store;
//     an OS-secure store backend lands in a later step behind the same
//     CredentialStore interface.
package auth

import (
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
type CredentialStore interface {
	// Save stores the key, replacing any previous one.
	Save(key string) error
	// Load returns the stored key, or an error wrapping ErrNoCredential
	// when nothing is stored.
	Load() (string, error)
	// Delete removes the stored key. Deleting when nothing is stored is not
	// an error.
	Delete() error
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

// ValidateKeyShape checks that key is plausibly a qURL API key before it is
// sent anywhere. The check is deliberately shallow — prefix plus a sane
// remainder — because the service is the authority on real keys.
// TODO(upstream-contract): qurl-service mints 51-character keys today
// (lv_live_/lv_test_ + 43); the length floor here stays loose so a service-side
// length change does not brick the CLI.
func ValidateKeyShape(key string) error {
	rest, ok := strings.CutPrefix(key, "lv_live_")
	if !ok {
		rest, ok = strings.CutPrefix(key, "lv_test_")
	}
	if !ok {
		return fmt.Errorf("%w: it should start with lv_live_ or lv_test_", ErrInvalidKey)
	}
	if len(rest) < 16 {
		return fmt.Errorf("%w: it is too short", ErrInvalidKey)
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !isAlpha && (c < '0' || c > '9') {
			return fmt.Errorf("%w: it contains unexpected characters", ErrInvalidKey)
		}
	}
	return nil
}

// fileStore is the plain-file fallback CredentialStore: one key in one file
// with owner-only permissions. It exists so the CLI works everywhere before
// the OS-secure backend lands, and remains the fallback after.
type fileStore struct {
	path string
}

// NewFileStore returns the file-backed credential store rooted at dir
// (normally the CLI config directory). The key is stored in a separate file,
// never in config.yaml.
func NewFileStore(dir string) CredentialStore {
	return &fileStore{path: filepath.Join(dir, "credential")}
}

// Name identifies the backend in user-facing messages.
func (s *fileStore) Name() string { return "file" }

// Save writes the key with owner-only permissions.
func (s *fileStore) Save(key string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create credential dir: %w", err)
	}
	return os.WriteFile(s.path, []byte(key+"\n"), 0o600)
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
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("%w: the credential file is empty", ErrNoCredential)
	}
	return key, nil
}

// Delete removes the stored key; deleting nothing is a no-op.
func (s *fileStore) Delete() error {
	err := os.Remove(s.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove credential file: %w", err)
	}
	return nil
}
