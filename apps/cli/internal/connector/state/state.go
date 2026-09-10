// Package state owns qurl's on-disk native agent state:
// where the state directory lives, the qurl-go agent state envelope opened
// inside it, and the assignment-refresh marker breadcrumb written next to it.
//
// The plaintext file envelope is the default: qurl-go's OpenFileAgentState
// pins the state directory, requires owner-only permissions, and validates
// continuity across every lifecycle operation. Sealed envelopes follow the
// connector's environment contract: LAYERV_KEY_PROVIDER names a key provider
// (for example local-key, with the wrapping key inherited on
// LAYERV_LOCAL_KEY_FD) and the connector's SDK store seals the same qurl-go
// state under it. qurl has no flag for the provider; the supervisor that owns
// the key sets the environment.
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	qurl "github.com/layervai/qurl-go/qurl"
)

const (
	// AgentStateFile is the qurl-go-owned plaintext credential envelope inside
	// the state directory.
	AgentStateFile = "agent_state.json"

	// EnvStateDirPrimary is the preferred state-directory override. It uses
	// the QURL_CONNECTOR_* prefix the rest of the Connector env surface
	// shares.
	EnvStateDirPrimary = "QURL_CONNECTOR_STATE_DIR"
	// EnvAgentID optionally pins the stable agent identity. qurl-go
	// generates and persists a UUID when it is empty.
	EnvAgentID = "QURL_CONNECTOR_AGENT_ID"

	// stateSubdir is appended to the platform user-state base. The v2 namespace
	// is intentionally new: prerelease v2.0.3 state did not retain the
	// authenticated enrollment kind and cannot authorize native session
	// operations safely. No decoder, inference, or destructive migration is
	// permitted for that incomplete state.
	stateSubdir = "qurl/connector-v2"
)

// ErrNoDefaultStateDir means no explicit state override or absolute platform
// user-state directory exists. Read-only remote commands treat this as an
// absent local share namespace; commands that create local state surface it.
var ErrNoDefaultStateDir = errors.New("no default qurl sharing state directory")

// ResolveDir resolves the native-agent state directory. Resolution order,
// most specific first:
//
//  1. explicit override argument (a future --state-dir flag)
//  2. QURL_CONNECTOR_STATE_DIR
//  3. the platform user-state directory below qurl/connector-v2
//
// There is no root-owned system default: qurl is a per-user tool. When no
// override or platform user-state directory is available, ResolveDir fails
// with a clear error instead of writing under the working directory.
func ResolveDir(override string) (string, error) {
	if dir := absCleanDir(override); dir != "" {
		return dir, nil
	}
	if dir := absCleanDir(os.Getenv(EnvStateDirPrimary)); dir != "" {
		return dir, nil
	}
	if platform := defaultStateDir(); platform != "" {
		return platform, nil
	}
	return "", fmt.Errorf("%w: set %s", ErrNoDefaultStateDir, EnvStateDirPrimary)
}

// absCleanDir trims raw and returns its absolute, cleaned form, or "" when
// raw is blank.
func absCleanDir(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if abs, err := filepath.Abs(raw); err == nil {
		return abs
	}
	return filepath.Clean(raw)
}

// ConfiguredAgentID returns the optional stable identity supplied by the
// operator. qurl-go generates and persists a UUID when it is empty; a sealed
// envelope is additionally pinned to it.
func ConfiguredAgentID() string {
	return strings.TrimSpace(os.Getenv(EnvAgentID))
}

// errStoreNotOpen reports use of a nil or closed Store; it wraps
// qurl.ErrAgentStateContinuity so callers fail closed on errors.Is.
var errStoreNotOpen = fmt.Errorf("%w: Connector state store is not open", qurl.ErrAgentStateContinuity)

// sealedProviderSelected reports whether LAYERV_KEY_PROVIDER names a key
// provider other than the plaintext file default. The connector validates the
// name and the provider's own environment when the sealed store opens.
//
// TODO(upstream-contract): mirrors qurl-connector pkg/agentstate
// selectedKeyProviderName (trimmed, case-folded, empty means file). If the
// connector adds a provider that still writes the plaintext envelope, route
// it here too or the plaintext guard in Open is skipped for it.
func sealedProviderSelected() bool {
	name := strings.ToLower(strings.TrimSpace(os.Getenv(connectoragentstate.EnvKeyProvider)))
	return name != "" && name != connectoragentstate.KeyProviderFile
}

// Store owns the qurl-go agent state envelope for the process lifetime: the
// plaintext file store by default, or the connector's SDK store around the
// sealed envelope when LAYERV_KEY_PROVIDER selects a key provider. Call
// Handoff at each SDK lifecycle boundary and retain the Store until every
// returned client and runtime binding has finished; Close releases the pinned
// state directory. The mutex keeps Close from racing a handoff or continuity
// validation.
type Store struct {
	mu       sync.RWMutex
	dir      string
	envelope string
	owner    stateOwner
}

// stateOwner is the process-lifetime owner of one qurl-go agent state
// envelope. Handoff returns qurl-go's exact concrete store so its setup-lock
// and operation-lease contracts stay active; *connectoragentstate.SDKStore
// satisfies this directly.
type stateOwner interface {
	Handoff() (qurl.AgentStateStore, error)
	ValidateContinuity() error
	Close() error
}

// fileStateOwner adapts the plaintext file store, which is its own
// qurl.AgentStateStore, to the stateOwner contract.
type fileStateOwner struct {
	store *qurl.FileAgentStateStore
}

// Handoff validates the retained state capability and returns the plaintext
// store itself.
func (o fileStateOwner) Handoff() (qurl.AgentStateStore, error) {
	if err := o.store.ValidateContinuity(); err != nil {
		return nil, err
	}
	return o.store, nil
}

// ValidateContinuity checks the retained plaintext state capability.
func (o fileStateOwner) ValidateContinuity() error { return o.store.ValidateContinuity() }

// Close releases the plaintext state capability.
func (o fileStateOwner) Close() error { return o.store.Close() }

// Open prepares dir (owner-only 0700) and opens the agent state envelope
// inside it: the plaintext file unless LAYERV_KEY_PROVIDER selects a key
// provider, in which case the connector's SDK store opens the sealed envelope
// and refuses a directory that already holds the plaintext one. The caller
// must Close every successful result.
func Open(dir string) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("state directory path is empty")
	}
	if err := EnsureDirMode(dir); err != nil {
		return nil, fmt.Errorf("prepare native agent state directory: %w", err)
	}
	if sealedProviderSelected() {
		sealed, err := connectoragentstate.NewSDKStore(dir, ConfiguredAgentID())
		if err != nil {
			return nil, fmt.Errorf("initialize sealed agent state: %w", err)
		}
		return &Store{dir: dir, envelope: connectoragentstate.SealedAgentStateFile, owner: sealed}, nil
	}
	// A sealed envelope means another owner (for example qURL Desktop, which
	// supplies a wrapping key over an inherited descriptor) established this
	// namespace. Writing a plaintext envelope beside it would make the
	// connector refuse the directory outright, so fail closed here instead.
	//
	// TODO(upstream-contract): this is the file-provider clause of the
	// connector's validateSDKStoreLayoutInNamespace. It is deliberately not
	// ValidateSDKStoreLayout, which would put the default plaintext path
	// through the connector's pinned namespace preparation (creating its
	// durability artifacts) where qurl-go's own capability already pins it.
	// The sealed branch also inherits the connector's legacy-artifact reject
	// list (agent_id, private_key, registration_refresh, etc/, ...), so no
	// file qurl writes into this directory may take one of those names.
	if _, err := os.Lstat(filepath.Join(dir, connectoragentstate.SealedAgentStateFile)); err == nil {
		return nil, fmt.Errorf("state directory holds a sealed agent state envelope (%s); set %s and %s to open it, or use a different state directory",
			connectoragentstate.SealedAgentStateFile, connectoragentstate.EnvKeyProvider, connectoragentstate.EnvLocalKeyFD)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect native agent state directory: %w", err)
	}
	file, err := qurl.OpenFileAgentState(filepath.Join(dir, AgentStateFile))
	if err != nil {
		return nil, fmt.Errorf("initialize plaintext agent state: %w", err)
	}
	return &Store{dir: dir, envelope: AgentStateFile, owner: fileStateOwner{store: file}}, nil
}

// Dir returns the resolved state directory this store was opened in.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Handoff validates the retained state capability and returns the concrete
// qurl-go store. qurl-go must receive that exact dynamic value so its
// setup-lock and operation-lease contracts remain active; wrapping it would
// hide the store's package-private capabilities.
func (s *Store) Handoff() (qurl.AgentStateStore, error) {
	if s == nil {
		return nil, errStoreNotOpen
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.owner == nil {
		return nil, errStoreNotOpen
	}
	return s.owner.Handoff()
}

// AgentStatePresent reports whether the pinned state directory contains the
// opened envelope's agent-state entry. It deliberately answers only the narrow
// existence question needed to distinguish a real assignment-refresh episode
// from an orphaned non-secret marker. Any entry type (including a corrupt file
// or a symlink) counts as present so qurl-go remains authoritative for
// validating the credential state and fails closed on anything other than
// true absence.
func (s *Store) AgentStatePresent() (bool, error) {
	if s == nil {
		return false, errStoreNotOpen
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.validateContinuityLocked(); err != nil {
		return false, err
	}
	_, err := os.Lstat(filepath.Join(s.dir, s.envelope))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect Connector agent state: %w", err)
	}
	if err := s.validateContinuityLocked(); err != nil {
		return false, err
	}
	return true, nil
}

// ValidateContinuity proves qurl-go still resolves the configured state path
// to its retained directory capability.
func (s *Store) ValidateContinuity() error {
	if s == nil {
		return errStoreNotOpen
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.validateContinuityLocked()
}

func (s *Store) validateContinuityLocked() error {
	if s.owner == nil {
		return errStoreNotOpen
	}
	return s.owner.ValidateContinuity()
}

// Close releases qurl-go's pinned state-directory capability. Idempotent.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == nil {
		return nil
	}
	owner := s.owner
	s.owner = nil
	return owner.Close()
}
