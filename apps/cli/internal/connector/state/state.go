// Package state owns qurl's on-disk native agent state:
// where the state directory lives, the qurl-go agent state envelope opened
// inside it, and the assignment-refresh marker breadcrumb written next to it.
//
// The connector's ResolveKeyProvider picks the envelope. A fresh namespace is
// sealed to this machine's TPM 2.0 when the process can use one, and is the
// plaintext file envelope otherwise; an existing envelope keeps its provider,
// and a TPM envelope opens with no environment, so the natively supervised
// background job can serve it. LAYERV_KEY_PROVIDER overrides the choice:
// file opts out of the TPM, and the other providers follow the connector's
// environment contract (for example local-key, with the wrapping key
// inherited on LAYERV_LOCAL_KEY_FD). qurl has no flag for the provider; the
// supervisor that owns the key sets the environment. The plaintext envelope
// is opened with qurl-go's OpenFileAgentState, which pins the state
// directory, requires owner-only permissions, and validates continuity across
// every lifecycle operation.
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

// ErrAgentStateEnvelope means the state directory's envelope cannot be opened
// as selected: a sealed envelope whose provider needs variables that are not
// set, an envelope that conflicts with LAYERV_KEY_PROVIDER, a provider the
// connector does not accept, or a TPM-sealed envelope this machine's TPM can
// no longer open (cleared, replaced, or another machine's state; the wrapped
// error names the cause and the recovery, moving the directory aside). The
// remedy is the environment or the state directory, never the command line,
// so exitcode maps it to Config. A TPM that is merely not responding is
// Unavailable instead.
//
// A sealed open also wraps it around failures qurl-go classifies itself, such
// as a loose directory mode or a continuity break. exitcode therefore checks
// it last of all, after every qurl-go row: the specific cause keeps the code
// it would have had on the plaintext branch, and this sentinel is the
// fallback for what is left.
var ErrAgentStateEnvelope = errors.New("agent state envelope")

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

// SelectedKeyProvider returns LAYERV_KEY_PROVIDER as this process reads it and
// whether it selects a sealed envelope. Callers that need to name the value in
// an error take it from here rather than reading the environment a second time
// with different trimming.
//
// TODO(upstream-contract): mirrors qurl-connector pkg/agentstate
// explicitKeyProviderName (trimmed, case-folded, empty leaves the choice to
// the namespace).
func SelectedKeyProvider() (string, bool) {
	raw, name := selectedKeyProviderName()
	return raw, name != "" && name != connectoragentstate.KeyProviderFile
}

// selectedKeyProviderName returns LAYERV_KEY_PROVIDER trimmed, and its
// normalized (case-folded) name: the one place that rule is applied.
func selectedKeyProviderName() (raw, name string) {
	raw = strings.TrimSpace(os.Getenv(connectoragentstate.EnvKeyProvider))
	return raw, strings.ToLower(raw)
}

// SelectedProviderNeedsEnvironment reports whether LAYERV_KEY_PROVIDER names a
// provider whose key only its environment can supply, so a namespace sealed
// under it can only be served by the supervisor that sets that environment.
// The TPM provider does not: its key never leaves this machine's TPM.
//
// TODO(upstream-contract): relies on qurl-connector pkg/agentstate
// KeyProviderRequiresEnvironment returning true for every name it does not
// know, so a mistyped provider is still refused under native supervision.
func SelectedProviderNeedsEnvironment() (string, bool) {
	raw, name := selectedKeyProviderName()
	sealed := name != "" && name != connectoragentstate.KeyProviderFile
	return raw, sealed && connectoragentstate.KeyProviderRequiresEnvironment(name)
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
// satisfies this directly. Continuity is Store.Handoff's job, so an
// implementation here need not repeat it.
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

// Handoff returns the plaintext store itself. Store.Handoff validates
// continuity for both branches.
func (o fileStateOwner) Handoff() (qurl.AgentStateStore, error) { return o.store, nil }

// ValidateContinuity checks the retained plaintext state capability.
func (o fileStateOwner) ValidateContinuity() error { return o.store.ValidateContinuity() }

// Close releases the plaintext state capability.
func (o fileStateOwner) Close() error { return o.store.Close() }

// Open prepares dir (owner-only 0700) and opens the agent state envelope the
// connector's ResolveKeyProvider selects for it: the plaintext file, or a
// sealed envelope through the connector's SDK store. The resolver refuses a
// directory whose existing envelope conflicts with LAYERV_KEY_PROVIDER, or
// whose sealed envelope needs an environment this process lacks. The caller
// must Close every successful result.
func Open(dir string) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("state directory path is empty")
	}
	if err := EnsureDirMode(dir); err != nil {
		return nil, fmt.Errorf("prepare native agent state directory: %w", err)
	}
	provider, err := connectoragentstate.ResolveKeyProvider(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAgentStateEnvelope, err)
	}
	if provider != connectoragentstate.KeyProviderFile {
		sealed, err := connectoragentstate.NewSDKStore(dir, ConfiguredAgentID())
		if err != nil {
			return nil, fmt.Errorf("%w: initialize sealed agent state: %w", ErrAgentStateEnvelope, err)
		}
		return &Store{dir: dir, envelope: connectoragentstate.SealedAgentStateFile, owner: sealed}, nil
	}
	// The plaintext branch deliberately bypasses NewSDKStore, which would put
	// the default path through the connector's pinned namespace preparation
	// (creating its durability artifacts) where qurl-go's own capability
	// already pins it. The sealed branch inherits the connector's
	// legacy-artifact reject list (agent_id, private_key, registration_refresh,
	// etc/, ...), so no file qurl writes into this directory may take one of
	// those names. The window between the resolver's inspection and
	// OpenFileAgentState is benign: a sealed envelope that appears inside it
	// leaves a directory holding both, which the connector refuses on its next
	// open.
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
	// Validate here rather than trusting each owner to do it: the sealed owner
	// is another repository's type, and a silent upstream change would
	// otherwise cost the sealed branch a check the plaintext branch keeps. The
	// sealed store repeats the check, so a sealed handoff spends its namespace
	// and SDK continuity validations twice. They are stats on a path already
	// held open, and a handoff is a lifecycle boundary, not a hot path.
	if err := s.owner.ValidateContinuity(); err != nil {
		return nil, err
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
