package auth

import (
	"errors"
)

// Backend names one credential storage backend in machine-stable form (the
// values appear in `-o json` output; prose renderings map them to words).
type Backend string

// The two storage backends.
const (
	BackendKeyring Backend = "keyring"
	BackendFile    Backend = "file"
)

// Chain is the CLI's credential storage: the OS keyring first, with the
// mode-0600 file store as fallback. The fallback is consulted only when the
// keyring reports itself unavailable (no D-Bus session, locked or absent
// keychain, unsupported platform) — a keyring that works but holds no key is
// an authoritative "nothing stored", so a stale fallback file cannot shadow
// it. Every read served from the file fallback triggers onFileRead once per
// Chain (i.e. once per command invocation), which is how commands warn that
// the key sits in a plain file.
//
// Chain implements CredentialStore, so auth.Resolve's hermetic contract is
// unchanged: with QURL_API_KEY set, none of this is ever touched.
type Chain struct {
	keyring    CredentialStore
	file       CredentialStore
	onFileRead func()
	warned     bool
}

// NewStore builds the production storage chain rooted at dir (the CLI config
// directory). onFileRead may be nil.
func NewStore(dir string, onFileRead func()) *Chain {
	return NewChain(NewKeyringStore(), NewFileStore(dir), onFileRead)
}

// NewChain assembles a chain from explicit backends; tests inject fakes.
func NewChain(keyring, file CredentialStore, onFileRead func()) *Chain {
	return &Chain{keyring: keyring, file: file, onFileRead: onFileRead}
}

// Name identifies the chain in user-facing messages.
func (c *Chain) Name() string { return "credential store" }

// Save stores the key, preferring the OS keyring. See SaveTo.
func (c *Chain) Save(key string) error {
	_, err := c.SaveTo(key)
	return err
}

// SaveTo stores the key and reports which backend took it. The keyring is
// tried first; any keyring failure falls back to the 0600 file. On a
// successful keyring save the fallback file is removed so a previously
// fallen-back key cannot linger on disk (and diverge from the keyring) after
// the keyring becomes available again.
func (c *Chain) SaveTo(key string) (Backend, error) {
	keyringErr := c.keyring.Save(key)
	if keyringErr == nil {
		if _, err := c.file.Delete(); err != nil {
			return BackendKeyring, err
		}
		return BackendKeyring, nil
	}
	if err := c.file.Save(key); err != nil {
		return "", errors.Join(keyringErr, err)
	}
	return BackendFile, nil
}

// Load returns the stored key. See LoadFrom.
func (c *Chain) Load() (string, error) {
	key, _, err := c.LoadFrom()
	return key, err
}

// LoadFrom returns the stored key and the backend that served it, applying
// the fallback contract described on Chain.
func (c *Chain) LoadFrom() (string, Backend, error) {
	key, err := c.keyring.Load()
	if err == nil {
		return key, BackendKeyring, nil
	}
	if errors.Is(err, ErrNoCredential) {
		// The keyring works and is empty: authoritative.
		return "", "", err
	}
	// The keyring is unavailable; the file fallback is in effect.
	key, ferr := c.file.Load()
	if ferr != nil {
		return "", "", ferr
	}
	if c.onFileRead != nil && !c.warned {
		c.warned = true
		c.onFileRead()
	}
	return key, BackendFile, nil
}

// Delete removes the key from every backend. See RemoveAll.
func (c *Chain) Delete() (bool, error) {
	removed, err := c.RemoveAll()
	return len(removed) > 0, err
}

// RemoveAll removes the credential from every backend that holds one and
// reports which ones did. An unavailable keyring counts as "not holding":
// a keyring this process cannot reach also could not have served the
// credential in this environment, and treating it as an error would make
// logout fail on every machine that never had a keyring at all. The file
// backend is always cleared regardless of what the keyring said.
func (c *Chain) RemoveAll() ([]Backend, error) {
	var removed []Backend
	if ok, err := c.keyring.Delete(); err == nil && ok {
		removed = append(removed, BackendKeyring)
	}
	ok, err := c.file.Delete()
	if ok {
		removed = append(removed, BackendFile)
	}
	return removed, err
}
