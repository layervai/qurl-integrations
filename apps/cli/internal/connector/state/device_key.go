package state

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"path/filepath"
	"strings"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	qurl "github.com/layervai/qurl-go/qurl"
)

// NoDeviceKey is one fixed word that says why ReadDeviceStaticPrivateKey
// returned no key. The empty value means that it returned a key.
//
// The word is the only thing a caller may show about a failed read, and only
// in a diagnostic. The errors behind it can name paths on this machine, so
// they are never returned.
type NoDeviceKey string

const (
	// NoDeviceKeyNoState means the directory holds no device state.
	NoDeviceKeyNoState NoDeviceKey = "no_state"
	// NoDeviceKeyPlatform means this operating system has no read-only way
	// to open the state that is supported and tested.
	NoDeviceKeyPlatform NoDeviceKey = "platform"
	// NoDeviceKeySupervision means the directory belongs to another
	// supervision mode than the one this command runs with.
	NoDeviceKeySupervision NoDeviceKey = "supervision"
	// NoDeviceKeyStorage means the state is not kept in one of the two key
	// storages this function reads: the plaintext file and the TPM.
	NoDeviceKeyStorage NoDeviceKey = "key_storage"
	// NoDeviceKeyUnreadable means the state could not be opened or loaded.
	NoDeviceKeyUnreadable NoDeviceKey = "unreadable"
	// NoDeviceKeyNotRegistered means the state is of a device whose
	// registration did not finish.
	NoDeviceKeyNotRegistered NoDeviceKey = "not_registered"
	// NoDeviceKeyInvalid means the state holds no usable key pair.
	NoDeviceKeyInvalid NoDeviceKey = "invalid_key"
	// NoDeviceKeyAgentID means the state is of another device than the one
	// QURL_CONNECTOR_AGENT_ID names.
	NoDeviceKeyAgentID NoDeviceKey = "agent_id"
)

// deviceStaticPrivateKeySize is the length of an X25519 private key.
const deviceStaticPrivateKeySize = 32

// ReadDeviceStaticPrivateKey returns the static private key of the device
// whose state is in dir: 32 bytes of X25519 key. supervision is the
// supervision mode this command runs with.
//
// It only reads. It creates no directory and no file, changes no permission,
// takes no lock, renews nothing, and sends nothing. So it is safe on a
// machine that holds no state, and next to a running daemon.
//
// It reads the key from two key storages only: the plaintext state file, and
// state sealed to this machine's TPM. A read from the TPM is one unseal. It
// asks the user nothing and uses no network. Every other key storage needs
// values from its environment, and some of them call a service of another
// company to open the state. A command that only wants to ask for a link
// must not do that, so such a directory gives no key.
//
// A failure is never an error of the command that asked. The result is then
// no key, and one fixed word that says why. The caller goes on without the
// key.
//
// The caller owns the returned key and wipes it after use.
//
// ctx is passed to the read unchanged, and the caller must not give it a
// shorter time limit than the rest of the command has. After a TPM call is
// given up because its context ended, later TPM calls in the same process
// fail at once for some time. A short limit here would then break a later
// step of the same command that opens the state.
//
// TODO(upstream-contract): the checks of the loaded state below are the ones
// qurl-go makes before it hands a device key to a caller: the device is
// registered, the private key is padded standard base64 of 32 bytes, and the
// public key in the state belongs to it. qurl-go makes them for a runtime
// binding and not for a read-only reader. If qurl-go or qurl-connector gets a
// call that returns the device key from a read-only reader, use it and delete
// these checks.
func ReadDeviceStaticPrivateKey(ctx context.Context, dir string, supervision RuntimeSupervision) ([]byte, NoDeviceKey) {
	dir = strings.TrimSpace(dir)
	// First of all: a directory with no state file is never opened, and the
	// TPM is never asked about it.
	if dir == "" || !EnvelopePresent(dir) {
		return nil, NoDeviceKeyNoState
	}
	if !deviceKeyReadSupported {
		return nil, NoDeviceKeyPlatform
	}
	// The check every command makes before it opens device state. It reads
	// the environment and one small file.
	if err := RequireRuntimeSupervision(dir, supervision); err != nil {
		return nil, NoDeviceKeySupervision
	}
	reader, why := openDeviceStateReader(dir)
	if why != "" {
		return nil, why
	}
	loaded, err := reader.LoadAgentState(ctx)
	closeErr := reader.Close()
	if loaded != nil {
		// The state also holds the credential this device uses for the qURL
		// API. Nothing but the key is taken from it.
		defer func() { *loaded = qurl.AgentState{} }()
	}
	if err != nil || closeErr != nil || loaded == nil {
		// A close that fails says the directory was replaced while it was
		// read. What was read is then not used.
		return nil, NoDeviceKeyUnreadable
	}
	return deviceKeyFromState(loaded, ConfiguredAgentID())
}

// openDeviceStateReader opens the state in dir with a reader that has no way
// to write. It opens the plaintext file and state sealed to the TPM, and
// nothing else.
func openDeviceStateReader(dir string) (qurl.AgentStateReader, NoDeviceKey) {
	// A provider that takes its key from the environment is refused before
	// the directory is looked at, whatever the directory holds.
	if _, needsEnvironment := selectedProviderNeedsEnvironment(); needsEnvironment {
		return nil, NoDeviceKeyStorage
	}
	provider, err := ResolveKeyProvider(dir)
	if err != nil {
		// This includes a directory that holds both state files, and state
		// sealed by a provider that needs its environment.
		return nil, NoDeviceKeyStorage
	}
	var reader qurl.AgentStateReader
	switch provider {
	case connectoragentstate.KeyProviderFile:
		// The plaintext file is opened with qurl-go's own check of the
		// directory, as Open does for the same file.
		reader, err = qurl.OpenFileAgentStateReadOnly(filepath.Join(dir, AgentStateFile))
	case connectoragentstate.KeyProviderTPM:
		reader, err = connectoragentstate.OpenSDKStateReader(dir, ConfiguredAgentID())
	default:
		return nil, NoDeviceKeyStorage
	}
	if err != nil || reader == nil {
		return nil, NoDeviceKeyUnreadable
	}
	return reader, ""
}

// deviceKeyFromState checks loaded and returns the device key it holds.
// pinnedAgentID is the device this process was told to be, or empty when it
// was told nothing.
func deviceKeyFromState(loaded *qurl.AgentState, pinnedAgentID string) ([]byte, NoDeviceKey) {
	if loaded.RegisteredAt == nil || loaded.RegisteredAt.IsZero() {
		// The key of a device that never finished its registration is not a
		// registered device's key.
		return nil, NoDeviceKeyNotRegistered
	}
	if pinnedAgentID != "" && loaded.AgentID != pinnedAgentID {
		return nil, NoDeviceKeyAgentID
	}
	// Strict, padded standard base64: the only form qurl-go writes.
	key, err := base64.StdEncoding.Strict().DecodeString(loaded.PrivateKeyB64)
	if err != nil || len(key) != deviceStaticPrivateKeySize || allZero(key) {
		clear(key)
		return nil, NoDeviceKeyInvalid
	}
	private, err := ecdh.X25519().NewPrivateKey(key)
	if err != nil || base64.StdEncoding.EncodeToString(private.PublicKey().Bytes()) != loaded.PublicKeyB64 {
		clear(key)
		return nil, NoDeviceKeyInvalid
	}
	return key, ""
}

// allZero reports whether b holds only zero bytes, as a wiped key does.
func allZero(b []byte) bool {
	var seen byte
	for _, v := range b {
		seen |= v
	}
	return seen == 0
}
