// Package state owns the CLI qURL Connector's on-disk native agent state:
// where the state directory lives, the qurl-go file-backed agent state
// envelope opened inside it, and the assignment-refresh marker breadcrumb
// written next to it.
//
// Only the plaintext file provider is supported: qurl-go's OpenFileAgentState
// pins the state directory, requires owner-only permissions, and validates
// continuity across every lifecycle operation. Cloud key-management providers
// for a sealed envelope are deliberately not part of this port; deployments
// that need one run the standalone qURL Connector.
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
	// AgentStateFile is the qurl-go-owned plaintext credential envelope
	// inside the state directory. The name is shared with the standalone
	// qURL Connector so an explicitly pointed-at state volume keeps working.
	AgentStateFile = "agent_state.json"

	// EnvStateDirPrimary is the preferred state-directory override. It uses
	// the QURL_CONNECTOR_* prefix the rest of the Connector env surface
	// shares.
	EnvStateDirPrimary = "QURL_CONNECTOR_STATE_DIR"
	// EnvStateDir is the legacy state-directory override, honored for
	// compatibility with existing volume mounts at lower precedence than
	// EnvStateDirPrimary.
	EnvStateDir = "LAYERV_AGENT_STATE_DIR"
	// EnvAgentID optionally pins the stable agent identity. qurl-go
	// generates and persists a UUID when it is empty.
	EnvAgentID = "LAYERV_AGENT_ID"

	// xdgStateSubdir is the per-application directory appended to the XDG
	// state base (or ~/.local/state) for the default user path. It is
	// deliberately distinct from the standalone Connector's default so the
	// two tools never mutate one identity by accident; pointing both at one
	// directory remains an explicit env-override decision.
	xdgStateSubdir = "qurl/connector"

	dirMode os.FileMode = 0o700
)

// ResolveDir resolves the native-agent state directory. Resolution order,
// most specific first:
//
//  1. explicit override argument (a future --state-dir flag)
//  2. QURL_CONNECTOR_STATE_DIR
//  3. LAYERV_AGENT_STATE_DIR (legacy, compatibility)
//  4. $XDG_STATE_HOME/qurl/connector, else ~/.local/state/qurl/connector
//
// Unlike the standalone Connector there is no root-owned system default: the
// CLI is a user tool, and the service-install deployment shape that default
// serves is not part of this port. When no override is set and no home
// directory is available, ResolveDir fails with a clear error naming the
// override instead of silently writing under the working directory.
func ResolveDir(override string) (string, error) {
	if dir := absCleanDir(override); dir != "" {
		return dir, nil
	}
	if dir := absCleanDir(os.Getenv(EnvStateDirPrimary)); dir != "" {
		return dir, nil
	}
	if dir := absCleanDir(os.Getenv(EnvStateDir)); dir != "" {
		return dir, nil
	}
	if xdg := xdgStateDir(); xdg != "" {
		return xdg, nil
	}
	return "", fmt.Errorf("no usable state directory: no home directory is available; set %s", EnvStateDirPrimary)
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

// xdgStateDir returns the XDG user state directory for the Connector, or ""
// when neither an absolute XDG_STATE_HOME nor a home directory is available.
func xdgStateDir() string {
	if base := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); base != "" && filepath.IsAbs(base) {
		return filepath.Join(base, xdgStateSubdir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if home = strings.TrimSpace(home); home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", xdgStateSubdir)
}

// EnsureDirMode makes dir exist as an owner-only 0700 directory before
// qurl-go's pinned-state layer validates it.
//
// qurl-go's OpenFileAgentState requires the state directory to be exactly
// 0700 and fails closed otherwise. It creates a missing directory at 0700,
// but it deliberately does not loosen or tighten one that already exists — so
// a directory a user created by hand at 0755 would make the very first run
// die on a mode check it never had a chance to satisfy. EnsureDirMode closes
// that gap by satisfying the 0700 requirement up front, without weakening it:
// os.MkdirAll honors the umask on the components it creates and leaves an
// existing directory's mode untouched, so an explicit Chmod pins the final
// directory to exactly 0700. qurl-go still performs its full no-follow and
// ownership validation afterward, so this cannot turn a symlink or
// foreign-owned path into an accepted namespace.
func EnsureDirMode(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("state directory path is empty")
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create state directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return fmt.Errorf("restrict state directory %s to owner-only %#o: %w", dir, dirMode, err)
	}
	return nil
}

// ConfiguredAgentID returns the optional stable identity supplied by the
// operator. qurl-go generates and persists a UUID when it is empty.
func ConfiguredAgentID() string {
	return strings.TrimSpace(os.Getenv(EnvAgentID))
}

// Store owns the qurl-go file-backed agent state envelope for the process
// lifetime plus the refresh-marker breadcrumb beside it. Call Handoff at each
// SDK lifecycle boundary and retain the Store until every returned client and
// runtime binding has finished; Close releases the pinned state directory.
//
// The mutex keeps Close from releasing the pinned directory while a marker
// mutation or handoff validation is in flight (a supervisor's healthy-knock
// callback clears the marker concurrently with shutdown), so safety does not
// rely only on call order.
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
