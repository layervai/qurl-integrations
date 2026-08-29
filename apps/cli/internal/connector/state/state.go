// Package state owns qurl's on-disk native agent state:
// where the state directory lives, the qurl-go file-backed agent state
// envelope opened inside it, and the assignment-refresh marker breadcrumb
// written next to it.
//
// Only the plaintext file provider is supported: qurl-go's OpenFileAgentState
// pins the state directory, requires owner-only permissions, and validates
// continuity across every lifecycle operation.
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
// operator. qurl-go generates and persists a UUID when it is empty.
func ConfiguredAgentID() string {
	return strings.TrimSpace(os.Getenv(EnvAgentID))
}

// Store owns the qurl-go file-backed agent state envelope for the process
// lifetime. Call Handoff at each SDK lifecycle boundary and retain the Store
// until every returned client and runtime binding has finished; Close releases
// the pinned state directory. The mutex keeps Close from racing a handoff or
// continuity validation.
type Store struct {
	mu   sync.RWMutex
	dir  string
	file *qurl.FileAgentStateStore
}

// Open prepares dir (owner-only 0700) and opens the plaintext agent state
// envelope inside it. The caller must Close every successful result.
func Open(dir string) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("state directory path is empty")
	}
	if err := EnsureDirMode(dir); err != nil {
		return nil, fmt.Errorf("prepare native agent state directory: %w", err)
	}
	file, err := qurl.OpenFileAgentState(filepath.Join(dir, AgentStateFile))
	if err != nil {
		return nil, fmt.Errorf("initialize plaintext agent state: %w", err)
	}
	return &Store{dir: dir, file: file}, nil
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
		return nil, fmt.Errorf("%w: Connector state store is not open", qurl.ErrAgentStateContinuity)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.validateContinuityLocked(); err != nil {
		return nil, err
	}
	return s.file, nil
}

// AgentStatePresent reports whether the pinned state directory contains an
// agent-state entry. It deliberately answers only the narrow existence
// question needed to distinguish a real assignment-refresh episode from an
// orphaned non-secret marker. Any entry type (including a corrupt file or a
// symlink) counts as present so qurl-go remains authoritative for validating
// the credential state and fails closed on anything other than true absence.
func (s *Store) AgentStatePresent() (bool, error) {
	if s == nil {
		return false, fmt.Errorf("%w: Connector state store is not open", qurl.ErrAgentStateContinuity)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.validateContinuityLocked(); err != nil {
		return false, err
	}
	_, err := os.Lstat(filepath.Join(s.dir, AgentStateFile))
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
		return fmt.Errorf("%w: Connector state store is not open", qurl.ErrAgentStateContinuity)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.validateContinuityLocked()
}

func (s *Store) validateContinuityLocked() error {
	if s.file == nil {
		return fmt.Errorf("%w: Connector state store is not open", qurl.ErrAgentStateContinuity)
	}
	return s.file.ValidateContinuity()
}

// Close releases qurl-go's pinned state-directory capability. Idempotent.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	file := s.file
	s.file = nil
	return file.Close()
}
