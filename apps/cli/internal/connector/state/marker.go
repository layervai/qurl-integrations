package state

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// RefreshMarkerFile is the state-dir filename that records a pending "the
// persisted Hub assignment may be stale; force one authenticated assignment
// refresh on the next warm restart" request. It is written when the
// supervisor exits on its consecutive-knock-failure budget (the
// sustained-failure signal) and consumed by the warm-restart path, which asks
// the Hub for a replacement assignment.
//
// It is not an identity envelope: its presence says nothing about whether a
// native identity or device credential was ever persisted. Mode 0644 (not
// 0600) because it carries no secret — just a self-heal breadcrumb an
// operator may inspect from a host-mounted state directory. The name and JSON
// shape are shared with the standalone qURL Connector so a state directory an
// env override points at keeps its episode semantics.
const RefreshMarkerFile = "registration_refresh.json"

// errInvalidRefreshMarker distinguishes a corrupt marker shape that the
// native warm-open path may safely log and remove from a real filesystem I/O
// fault. A transient read/stat fault must not erase an Attempted=true marker
// and reopen the bounded Hub-refresh episode. Deliberately unexported with an
// exported predicate: this condition is handled (logged and cleared), never
// surfaced to a customer, so it stays outside the CLI's exported-sentinel
// exit-code contract.
var errInvalidRefreshMarker = errors.New("invalid assignment refresh marker")

// IsInvalidRefreshMarker reports whether err is the corrupt-marker condition
// a warm-open caller may fail-safe on (log, clear, and proceed), as opposed
// to a real I/O fault that must stop the self-heal decision.
func IsInvalidRefreshMarker(err error) bool {
	return errors.Is(err, errInvalidRefreshMarker)
}

// refreshMarkerFileMaxBytes caps the marker read. The marker is a tiny JSON
// object; the cap defends the warm-restart read against a corrupt or
// accidentally grown file being pulled into memory.
const refreshMarkerFileMaxBytes = 4 << 10 // 4 KiB
const refreshMarkerVersion = 1
const refreshMarkerReasonMaxBytes = 256

const markerMode os.FileMode = 0o644

// RefreshMarker is the decoded registration-refresh marker. It records why
// the refresh was requested and whether the native assignment refresh it asks
// for has already been ATTEMPTED in the current sustained-failure episode.
//
// Attempted is the anti-storm bound: startup requests a Hub assignment
// refresh at most once per failure episode. It flips true BEFORE the request
// and the marker is not cleared merely because assignment succeeds — that
// does not prove knocks work. So a refresh that succeeds while knocks still
// fail (a non-binding problem: the admission controller is down, the resource
// is deleted, UDP egress is blocked) cannot spin a fresh Hub request on every
// failure-budget cycle: the marker stays Attempted=true, and the next budget
// exit's presence-gated RequestRefresh leaves it untouched. A genuinely new
// episode re-opens refresh only because a confirmed-healthy knock clears the
// marker entirely (ClearRefreshMarker), so the next budget exit writes a
// fresh Attempted=false marker.
type RefreshMarker struct {
	// Version selects the closed marker schema.
	Version int `json:"version"`

	// Reason is a short, human-readable tag for why the refresh was
	// requested (for example the sustained-knock-failure cause).
	// Operator-facing only; the self-heal logic keys off Attempted.
	Reason string `json:"reason,omitempty"`

	// Attempted is false when the marker was freshly set by a budget exit
	// and the Hub refresh has not run yet, and true once startup has
	// consumed it to request one refresh.
	Attempted bool `json:"attempted"`

	// SetAtUnix is the wall-clock second the marker was first written, for
	// operator triage. Not load-bearing for the self-heal decision.
	SetAtUnix int64 `json:"set_at_unix,omitempty"`
}

// LoadRefreshMarker reads the registration-refresh marker from the store's
// state directory. It returns (marker, true, nil) when a well-formed marker
// is present, (zero, false, nil) when absent, and (zero, false, err) only for
// a real I/O fault or a corrupt/oversized marker.
//
// A corrupt marker is surfaced as an error rather than silently treated as
// absent: the warm-open caller logs it, clears it, and proceeds on the
// ordinary persisted-assignment path (fail-safe — a torn self-heal breadcrumb
// must not itself wedge startup), but swallowing it here would hide the
// corruption from that log entirely.
func (s *Store) LoadRefreshMarker() (RefreshMarker, bool, error) {
	var marker RefreshMarker
	var present bool
	err := s.withMarkerAccess(func(dir string) error {
		var err error
		marker, present, err = loadRefreshMarker(dir)
		return err
	})
	if err != nil {
		return RefreshMarker{}, false, err
	}
	return marker, present, nil
}

// RequestRefresh records that the Connector should force one Hub assignment
// refresh on its next warm restart, because knocks have failed for long
// enough to trip the supervisor's consecutive-failure budget.
//
// It is deliberately EPISODE-IDEMPOTENT, the second half of the anti-storm
// bound: if a marker already exists it is left unchanged — in particular an
// existing Attempted=true marker is NOT reset to Attempted=false. A Connector
// whose forced refresh already ran and did not fix knocks keeps tripping the
// budget on every restart, but each subsequent budget exit is a no-op here
// rather than re-arming another refresh. Only a confirmed-healthy knock
// (ClearRefreshMarker) ends the episode.
//
// Marker creation failures are returned so the caller can log them; they are
// not fatal — a Connector that cannot write the breadcrumb simply loses the
// self-heal on that restart.
func (s *Store) RequestRefresh(reason string) error {
	return s.withMarkerAccess(func(dir string) error {
		return requestRefresh(dir, reason)
	})
}

// MarkRefreshAttempted flips the marker's Attempted bit true, preserving
// Reason/SetAtUnix. The warm-restart path calls this BEFORE it runs the
// forced refresh, so a crash or refresh failure mid-attempt still leaves
// Attempted=true and the next restart does not re-arm another refresh (the
// anti-storm bound). An absent marker is a no-op; a write fault is returned
// for logging.
func (s *Store) MarkRefreshAttempted() error {
	return s.withMarkerAccess(markRefreshAttempted)
}

// ClearRefreshMarker removes the registration-refresh marker, ending the
// current self-heal episode. It is called on a confirmed-healthy knock so
// steady-state restarts stay on the efficient persisted-assignment path. It
// is deliberately NOT called when an assignment refresh succeeds: a Hub
// response proves only assignment, not that assigned-cell knocks work.
// Absence is success — the marker not existing is exactly the desired
// post-condition. A non-ENOENT remove fault is returned for logging.
func (s *Store) ClearRefreshMarker() error {
	return s.withMarkerAccess(clearRefreshMarker)
}

// withMarkerAccess brackets one marker operation with continuity validation
// of the retained state capability, so a marker mutation cannot proceed (or
// silently succeed) across a replaced state directory.
func (s *Store) withMarkerAccess(fn func(dir string) error) error {
	if s == nil {
		return errors.New("qURL Connector state store is not open")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.validateContinuityLocked(); err != nil {
		return err
	}
	if err := fn(s.dir); err != nil {
		return err
	}
	return s.validateContinuityLocked()
}

func loadRefreshMarker(dir string) (RefreshMarker, bool, error) {
	path := filepath.Join(dir, RefreshMarkerFile)
	entry, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RefreshMarker{}, false, nil
		}
		return RefreshMarker{}, false, fmt.Errorf("stat registration refresh marker %s: %w", path, err)
	}
	if entry.Mode()&os.ModeSymlink != 0 || !entry.Mode().IsRegular() {
		return RefreshMarker{}, false, fmt.Errorf("%w: %s must be a non-symlink regular file", errInvalidRefreshMarker, path)
	}
	if entry.Size() <= 0 || entry.Size() > refreshMarkerFileMaxBytes {
		return RefreshMarker{}, false, fmt.Errorf("%w: %s is %d bytes, exceeds %d-byte cap", errInvalidRefreshMarker, path, entry.Size(), refreshMarkerFileMaxBytes)
	}
	file, err := os.Open(path) //nolint:gosec // G304: the path is the store's own pinned state directory plus a fixed name.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RefreshMarker{}, false, nil
		}
		return RefreshMarker{}, false, fmt.Errorf("open registration refresh marker %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	// Re-validate through the opened descriptor: the no-follow Lstat above and
	// this fstat must agree the descriptor is a regular file, so a swap between
	// the two calls fails closed instead of being read.
	info, err := file.Stat()
	if err != nil {
		return RefreshMarker{}, false, fmt.Errorf("stat opened registration refresh marker %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return RefreshMarker{}, false, fmt.Errorf("%w: %s must be a non-symlink regular file", errInvalidRefreshMarker, path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, refreshMarkerFileMaxBytes+1))
	if err != nil {
		return RefreshMarker{}, false, fmt.Errorf("read registration refresh marker %s: %w", path, err)
	}
	if len(raw) > refreshMarkerFileMaxBytes {
		return RefreshMarker{}, false, fmt.Errorf("%w: %s exceeds %d-byte cap", errInvalidRefreshMarker, path, refreshMarkerFileMaxBytes)
	}
	m, err := decodeRefreshMarker(raw)
	if err != nil {
		return RefreshMarker{}, false, fmt.Errorf("%w: decode %s: %w", errInvalidRefreshMarker, path, err)
	}
	return m, true, nil
}

func decodeRefreshMarker(raw []byte) (RefreshMarker, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return RefreshMarker{}, err
	}
	if token != json.Delim('{') {
		return RefreshMarker{}, errors.New("registration refresh marker must be a JSON object")
	}
	var marker RefreshMarker
	seen := make(map[string]struct{}, 4)
	for decoder.More() {
		if err := decodeRefreshMarkerField(decoder, &marker, seen); err != nil {
			return RefreshMarker{}, err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return RefreshMarker{}, err
	}
	if closing != json.Delim('}') {
		return RefreshMarker{}, errors.New("registration refresh marker object did not close")
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return RefreshMarker{}, err
	}
	if err := validateDecodedRefreshMarker(marker, seen); err != nil {
		return RefreshMarker{}, err
	}
	return marker, nil
}

func decodeRefreshMarkerField(decoder *json.Decoder, marker *RefreshMarker, seen map[string]struct{}) error {
	keyToken, err := decoder.Token()
	if err != nil {
		return err
	}
	key, ok := keyToken.(string)
	if !ok {
		return errors.New("registration refresh marker key is not a string")
	}
	if _, duplicate := seen[key]; duplicate {
		return fmt.Errorf("duplicate registration refresh marker field %q", key)
	}
	seen[key] = struct{}{}
	switch key {
	case "version":
		return decoder.Decode(&marker.Version)
	case "reason":
		return decoder.Decode(&marker.Reason)
	case "attempted":
		return decoder.Decode(&marker.Attempted)
	case "set_at_unix":
		return decoder.Decode(&marker.SetAtUnix)
	default:
		return fmt.Errorf("unknown registration refresh marker field %q", key)
	}
}

func validateDecodedRefreshMarker(marker RefreshMarker, seen map[string]struct{}) error {
	for _, required := range []string{"version", "attempted", "set_at_unix"} {
		if _, ok := seen[required]; !ok {
			return fmt.Errorf("registration refresh marker is missing required field %q", required)
		}
	}
	if marker.Version != refreshMarkerVersion {
		return fmt.Errorf("unsupported registration refresh marker version %d", marker.Version)
	}
	if marker.SetAtUnix <= 0 {
		return errors.New("registration refresh marker set_at_unix must be positive")
	}
	if invalidRefreshMarkerReason(marker.Reason) ||
		strings.TrimSpace(marker.Reason) != marker.Reason {
		return fmt.Errorf("registration refresh marker reason must be canonical UTF-8 without control characters and at most %d bytes", refreshMarkerReasonMaxBytes)
	}
	return nil
}

func rejectTrailingJSON(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

// invalidRefreshMarkerReason reports whether reason carries bytes that are
// hostile to durable storage: invalid UTF-8, over the byte cap, or any
// control character. It is the shared core of the read and write validation;
// each caller layers its own canonical-form check (the read path additionally
// rejects untrimmed whitespace) and supplies its own error text.
func invalidRefreshMarkerReason(reason string) bool {
	return !utf8.ValidString(reason) ||
		len(reason) > refreshMarkerReasonMaxBytes ||
		strings.IndexFunc(reason, unicode.IsControl) >= 0
}

func requestRefresh(dir, reason string) error {
	// Presence-gated, NOT decode-gated: if a marker file exists at all —
	// well-formed, corrupt, or momentarily unreadable — an episode is already
	// open (or being cleaned up by the consume side), so arming is a no-op
	// and the file is left untouched. Keying off no-follow Lstat presence
	// rather than a successful decode is deliberate: a transient stat/read
	// fault on an existing Attempted=true marker must NOT fall through to an
	// overwrite that resets Attempted=false and re-arms a Hub refresh the
	// anti-storm bound was meant to prevent. A genuinely corrupt marker is
	// the warm-open side's job to log and clear; the set side never rewrites
	// it. Lstat (not Stat) means a dangling marker symlink also counts as
	// present and is left alone.
	path := filepath.Join(dir, RefreshMarkerFile)
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// A non-ENOENT lstat fault means the marker cannot be confirmed
		// absent. Fail safe: do NOT overwrite (an existing marker might be
		// Attempted=true). Surface the fault so the caller logs it; losing
		// this one arming is harmless (a later budget exit retries).
		return fmt.Errorf("lstat registration refresh marker %s: %w", path, err)
	}
	// No marker file present → arm a fresh, unattempted episode.
	reason = strings.TrimSpace(reason)
	if invalidRefreshMarkerReason(reason) {
		return fmt.Errorf("registration refresh reason must be valid UTF-8 without control characters and at most %d bytes", refreshMarkerReasonMaxBytes)
	}
	m := RefreshMarker{
		Version:   refreshMarkerVersion,
		Reason:    reason,
		Attempted: false,
		SetAtUnix: time.Now().Unix(),
	}
	return writeRefreshMarker(dir, m)
}

func markRefreshAttempted(dir string) error {
	m, present, err := loadRefreshMarker(dir)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if m.Attempted {
		return nil
	}
	m.Attempted = true
	return writeRefreshMarker(dir, m)
}

func clearRefreshMarker(dir string) error {
	path := filepath.Join(dir, RefreshMarkerFile)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove registration refresh marker %s: %w", path, err)
	}
	return syncDir(dir)
}

// writeRefreshMarker atomically persists m under dir: temp file + fsync +
// rename + directory sync, so a crash mid-write leaves either the prior
// marker or the new one — never a torn file the warm-restart read would
// reject. Mode 0644 (non-secret breadcrumb).
func writeRefreshMarker(dir string, m RefreshMarker) (retErr error) {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode registration refresh marker: %w", err)
	}
	path := filepath.Join(dir, RefreshMarkerFile)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("registration refresh marker %s must be a non-symlink regular file", path)
		}
		if goruntime.GOOS != "windows" && info.Mode().Perm() != markerMode {
			return fmt.Errorf("registration refresh marker %s has mode %04o, want %04o", path, info.Mode().Perm(), markerMode)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect registration refresh marker %s before write: %w", path, err)
	}

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Errorf("generate registration refresh marker temporary name: %w", err)
	}
	tmpName := "." + RefreshMarkerFile + ".tmp-" + hex.EncodeToString(suffix)
	tmpPath := filepath.Join(dir, tmpName)
	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, markerMode) //nolint:gosec // G302,G304: 0644 is the documented non-secret marker mode inside the store's own state directory.
	if err != nil {
		return fmt.Errorf("create registration refresh marker temporary file: %w", err)
	}
	// Windows refuses to rename a file with an open handle (a sharing
	// violation Unix does not have), so the commit order is strictly
	// write → sync → close → rename; the deferred cleanup only closes on
	// an early error and removes the leftover temporary.
	committed := false
	closed := false
	defer func() {
		if !closed {
			retErr = errors.Join(retErr, tmp.Close())
		}
		if committed {
			return
		}
		removeErr := os.Remove(tmpPath)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		retErr = errors.Join(retErr, removeErr, syncDir(dir))
	}()
	if err := tmp.Chmod(markerMode); err != nil {
		return fmt.Errorf("set registration refresh marker temporary permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write registration refresh marker temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync registration refresh marker temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close registration refresh marker temporary file: %w", err)
	}
	closed = true
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("commit registration refresh marker rename: %w", err)
	}
	committed = true
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("registration refresh marker rename committed but directory sync failed: %w", err)
	}
	return nil
}

// syncDir fsyncs the directory so a committed rename or remove is durable.
// Windows has no meaningful directory fsync; skip it there (the rename itself
// is still atomic at the filesystem level).
func syncDir(dir string) error {
	if goruntime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir) //nolint:gosec // G304: the store's own state directory.
	if err != nil {
		return fmt.Errorf("open state directory for sync: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}
