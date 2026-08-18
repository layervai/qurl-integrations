package auth

import (
	"errors"
	"fmt"

	keyring "github.com/zalando/go-keyring"
)

// The OS keyring coordinates for the CLI's credential: one item, named so a
// customer browsing Keychain Access / Credential Manager / Seahorse can tell
// what it is. Changing either value orphans previously stored keys, so treat
// them as frozen.
const (
	keyringService = "qURL CLI"
	keyringUser    = "api-key"
)

// keyringFuncs is the seam between the store and the OS keyring library.
// Production uses go-keyring's package functions (wincred on Windows, the
// macOS Keychain via /usr/bin/security, the freedesktop Secret Service over
// D-Bus — gnome-keyring — on Linux); unit tests inject fakes so they never
// touch a developer's real keyring.
type keyringFuncs struct {
	set func(service, user, password string) error
	get func(service, user string) (string, error)
	del func(service, user string) error
}

func osKeyringFuncs() keyringFuncs {
	return keyringFuncs{set: keyring.Set, get: keyring.Get, del: keyring.Delete}
}

// keyringStore is the OS-keyring CredentialStore.
//
// Error contract (what Chain keys on): Load returns an error wrapping
// ErrNoCredential when the keyring works but holds no key, and any other
// error when the keyring itself is unavailable (no D-Bus session, locked or
// absent keychain, unsupported platform). go-keyring's ErrNotFound is the
// only "works but empty" signal it defines; everything else is treated as
// unavailability.
type keyringStore struct {
	fns     keyringFuncs
	service string
}

// NewKeyringStore returns the OS-keyring credential store.
func NewKeyringStore() CredentialStore {
	return &keyringStore{fns: osKeyringFuncs(), service: keyringService}
}

// Name identifies the backend in user-facing messages.
func (s *keyringStore) Name() string { return "OS keyring" }

// Save stores the key in the OS keyring. Any error means the keyring did not
// take it (Chain then falls back to the file store).
func (s *keyringStore) Save(key string) error {
	if err := s.fns.set(s.service, keyringUser, key); err != nil {
		return fmt.Errorf("store key in OS keyring: %w", err)
	}
	return nil
}

// Load reads the stored key, distinguishing "no key stored" (ErrNoCredential)
// from "keyring unavailable" (any other error) per the CredentialStore
// contract.
func (s *keyringStore) Load() (string, error) {
	key, err := s.fns.get(s.service, keyringUser)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", fmt.Errorf("%w: run `qurl login` or set %s", ErrNoCredential, EnvAPIKey)
		}
		return "", fmt.Errorf("read key from OS keyring: %w", err)
	}
	return key, nil
}

// Delete removes the stored key. Any failure is reported so callers can
// classify it: Chain.RemoveAll swallows it only when the keyring is
// unreachable (a keyring this process cannot reach also could not have
// served the credential in this environment) and propagates it when a
// reachable keyring genuinely failed to delete.
func (s *keyringStore) Delete() (bool, error) {
	if err := s.fns.del(s.service, keyringUser); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("remove key from OS keyring: %w", err)
	}
	return true, nil
}
