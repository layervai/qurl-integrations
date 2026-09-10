package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
)

const (
	// RuntimeModeFile is the supervision policy marker of one native state
	// namespace. It is written once, when the namespace is created for an
	// external supervisor, and never rewritten; its absence is native mode.
	RuntimeModeFile = "runtime_mode.json"

	// EnvRuntimeSupervision selects the supervision mode. The persistent
	// --supervision flag and the daemon_supervision config key resolve through
	// the CLI's ordinary settings precedence.
	EnvRuntimeSupervision = "QURL_DAEMON_SUPERVISION"

	runtimeModeSchemaVersion = 1
	runtimeModeMaxBytes      = 4 << 10
)

// RuntimeSupervision names the process that owns the share daemon's
// lifecycle for a state namespace.
type RuntimeSupervision string

const (
	// RuntimeSupervisionNative is the CLI-managed lifecycle: publish, start,
	// and restart install the per-user background job. It is the default and
	// is represented by an absent policy marker.
	RuntimeSupervisionNative RuntimeSupervision = "native"
	// RuntimeSupervisionExternal delegates the daemon process to another
	// supervisor, which runs `qurl daemon run --supervision external` itself.
	// Lifecycle commands then only reload a running daemon and never install
	// or replace a background job.
	RuntimeSupervisionExternal RuntimeSupervision = "external"
)

// DefaultRuntimeSupervision is the mode a namespace has when nothing selects
// one.
const DefaultRuntimeSupervision = RuntimeSupervisionNative

// ErrRuntimeSupervision is the identity of a command whose supervision
// setting does not match the namespace it addresses. The remedy is the
// --supervision setting, so it is configuration in the exit-code sense.
var ErrRuntimeSupervision = errors.New("runtime supervision")

// RuntimeSupervisionValues lists every accepted mode, default first.
func RuntimeSupervisionValues() []string {
	return []string{string(RuntimeSupervisionNative), string(RuntimeSupervisionExternal)}
}

// ParseRuntimeSupervision validates an operator-supplied mode. Only the
// canonical spellings are accepted: the mode decides whether a command may
// install a background job, so it is never normalized silently.
func ParseRuntimeSupervision(value string) (RuntimeSupervision, error) {
	switch mode := RuntimeSupervision(value); mode {
	case RuntimeSupervisionNative, RuntimeSupervisionExternal:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid daemon supervision %q: must be %s", value, strings.Join(RuntimeSupervisionValues(), " or "))
	}
}

type runtimeModeState struct {
	SchemaVersion int                `json:"schema_version"`
	Supervision   RuntimeSupervision `json:"supervision"`
}

// ReadRuntimeSupervision reports the namespace's supervision policy. An absent
// directory or marker is native. It never creates or repairs anything.
func ReadRuntimeSupervision(dir string) (RuntimeSupervision, error) {
	present, err := loadRuntimeMode(dir)
	if err != nil {
		return "", err
	}
	if !present {
		return RuntimeSupervisionNative, nil
	}
	return RuntimeSupervisionExternal, nil
}

// RequireRuntimeSupervision rejects a namespace whose policy does not match
// the caller's lifecycle contract.
func RequireRuntimeSupervision(dir string, expected RuntimeSupervision) error {
	if _, err := ParseRuntimeSupervision(string(expected)); err != nil {
		return err
	}
	actual, err := ReadRuntimeSupervision(dir)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("%w is %q, not %q; run this command with --supervision %s", ErrRuntimeSupervision, actual, expected, actual)
	}
	return nil
}

// EstablishExternalRuntimeMode commits the external policy for dir, creating
// the owner-only directory when needed. It is idempotent once the marker
// exists and refuses a namespace that already holds native lifecycle state,
// so a natively managed namespace can never be relabelled underneath its
// background job.
func EstablishExternalRuntimeMode(ctx context.Context, dir string) (retErr error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("state directory path is empty")
	}
	if err := EnsureDirMode(dir); err != nil {
		return fmt.Errorf("prepare external runtime namespace: %w", err)
	}
	unlock, err := acquireConnectorResourcesLock(ctx, dir)
	if err != nil {
		return fmt.Errorf("lock runtime supervision policy: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, unlock()) }()
	present, err := loadRuntimeMode(dir)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	// TODO(upstream-contract): Keep this freshness guard aligned with every
	// durable agent-state envelope introduced by qurl-go and qurl-connector.
	for _, name := range []string{AgentStateFile, connectoragentstate.SealedAgentStateFile, LocalSharesFile, ConnectorResourcesFile} {
		_, err := os.Lstat(filepath.Join(dir, name))
		switch {
		case err == nil:
			return fmt.Errorf("state directory is not a fresh external namespace: %s already exists", name)
		case errors.Is(err, os.ErrNotExist):
		default:
			return fmt.Errorf("inspect external namespace: %w", err)
		}
	}
	data, err := json.Marshal(runtimeModeState{SchemaVersion: runtimeModeSchemaVersion, Supervision: RuntimeSupervisionExternal})
	if err != nil {
		return fmt.Errorf("encode runtime supervision policy: %w", err)
	}
	if err := replaceConnectorResources(dir, filepath.Join(dir, RuntimeModeFile), data); err != nil {
		return fmt.Errorf("commit runtime supervision policy: %w", err)
	}
	return nil
}

// loadRuntimeMode reports whether dir carries a valid external policy marker.
// The marker is read under the Connector resource journal's fail-closed file
// contract: owner-only, non-symlink, bounded, strict JSON.
func loadRuntimeMode(dir string) (bool, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false, errors.New("state directory path is empty")
	}
	path := filepath.Join(dir, RuntimeModeFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect runtime supervision policy: %w", err)
	}
	if err := validateRuntimeModeInfo(path, info); err != nil {
		return false, fmt.Errorf("runtime supervision policy: %w", err)
	}
	file, err := openConnectorResourceState(path)
	if err != nil {
		return false, fmt.Errorf("open runtime supervision policy: %w", err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("inspect opened runtime supervision policy: %w", err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("reinspect runtime supervision policy: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, opened) || !os.SameFile(opened, current) {
		return false, errors.New("runtime supervision policy changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, runtimeModeMaxBytes+1))
	if err != nil {
		return false, fmt.Errorf("read runtime supervision policy: %w", err)
	}
	if len(data) > runtimeModeMaxBytes {
		return false, fmt.Errorf("runtime supervision policy exceeds %d bytes", runtimeModeMaxBytes)
	}
	if err := decodeRuntimeMode(data); err != nil {
		return false, fmt.Errorf("invalid runtime supervision policy: %w", err)
	}
	return true, nil
}

func validateRuntimeModeInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("must be a non-symlink regular file")
	}
	if err := validateConnectorResourceFile(path, info); err != nil {
		return err
	}
	if info.Size() <= 0 || info.Size() > runtimeModeMaxBytes {
		return fmt.Errorf("must hold between 1 and %d bytes", runtimeModeMaxBytes)
	}
	return nil
}

func decodeRuntimeMode(data []byte) error {
	if err := rejectDuplicateResourceFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state runtimeModeState
	if err := decoder.Decode(&state); err != nil {
		return err
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return err
	}
	if state.SchemaVersion != runtimeModeSchemaVersion || state.Supervision != RuntimeSupervisionExternal {
		return fmt.Errorf("requires schema %d with external supervision", runtimeModeSchemaVersion)
	}
	return nil
}
