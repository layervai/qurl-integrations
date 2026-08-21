package state

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/layervai/qurl-go/crid"
	qurl "github.com/layervai/qurl-go/qurl"
)

const (
	// ConnectorResourcesFile stores the native resource binding and exact
	// in-flight LST request. It is intentionally separate from qurl-go's agent
	// envelope: the SDK owns the device identity while this CLI owns the
	// customer Connector IDs it has bound to that identity.
	ConnectorResourcesFile = "connector_resources.json"
	connectorResourcesLock = ".connector_resources.lock"

	connectorResourcesVersion  = 1
	connectorResourcesMaxBytes = 1 << 20
	connectorResourcesMaxItems = 1024
	connectorResourceFileMode  = 0o600
)

var connectorIDPattern = mustCompileConnectorIDPattern()

var (
	// ErrConnectorResourceStateConflict marks an authenticated response whose
	// public identities alias a different Connector already accepted into this
	// owner's durable state. The response is never committed.
	ErrConnectorResourceStateConflict = errors.New("connector resource conflicts with durable local state")
	// ErrConnectorResourceVerification marks an authenticated response that
	// contradicts the exact request or the same Connector's durable binding.
	// Treat it as a fail-closed verification failure, never as replacement
	// authority for the previously accepted identity.
	ErrConnectorResourceVerification = errors.New("connector resource response failed durable-state verification")
)

// ConnectorResourceCommitError rejects an authenticated response that
// contradicts durable Connector state. Commit retains every accepted binding
// and attempts to atomically discard the completed request before returning
// this error, so a restart cannot exact-replay a response already known to be
// unacceptable. If that durable discard fails, the transaction remains open
// and the error directs the operator to repair state access before retrying.
type ConnectorResourceCommitError struct {
	kind             error
	detail           string
	pendingDiscarded bool
}

func (e *ConnectorResourceCommitError) Error() string {
	if e.pendingDiscarded {
		return fmt.Sprintf("%v: %s; retained durable bindings and discarded the contradictory completed request", e.kind, e.detail)
	}
	return fmt.Sprintf("%v: %s; retained durable bindings but could not discard the contradictory completed request; repair durable state access before retrying", e.kind, e.detail)
}

func (e *ConnectorResourceCommitError) Unwrap() error { return e.kind }

// ConnectorResourceBinding is the complete placement-neutral binding needed
// to configure and admit one Connector. Every field came from an authenticated
// assigned-cell LRT; no field is reconstructed from a hostname.
type ConnectorResourceBinding struct {
	ConnectorID        string `json:"connector_id"`
	ResourceID         string `json:"resource_id"`
	CRID               string `json:"crid,omitempty"`
	ConnectorRoutingID string `json:"connector_routing_id"`
	KnockResourceID    string `json:"knock_resource_id"`
}

// PendingConnectorResourceRequest is the exact logical LST that must be
// replayed after an uncertain outcome. The nonce is generated and persisted
// before the first packet is sent.
type PendingConnectorResourceRequest struct {
	ConnectorID        string `json:"connector_id"`
	RequestNonce       string `json:"request_nonce"`
	ExpectedResourceID string `json:"expected_resource_id,omitempty"`
}

type connectorResourcesState struct {
	Version  int                                        `json:"version"`
	Bindings map[string]ConnectorResourceBinding        `json:"bindings"`
	Pending  map[string]PendingConnectorResourceRequest `json:"pending"`
}

// ConnectorResourceTransaction owns the process and cross-process lock for
// one resolve exchange. Holding it through dispatch prevents two CLI
// processes sharing a state directory from racing different logical requests;
// a crash releases the OS lock while the durable pending request remains.
type ConnectorResourceTransaction struct {
	store    *Store
	state    connectorResourcesState
	request  qurl.NativeConnectorResourceRequest
	unlock   func() error
	finished bool
	closed   bool
}

// BeginConnectorResource serializes resource setup, loads the strict cache,
// and either reuses its exact pending request or atomically persists a fresh
// one. A warm request always carries the cached public resource identity as a
// continuity assertion.
func (s *Store) BeginConnectorResource(ctx context.Context, connectorID string) (_ *ConnectorResourceTransaction, retErr error) {
	if s == nil {
		return nil, fmt.Errorf("%w: Connector state store is not open", qurl.ErrAgentStateContinuity)
	}
	if err := validateConnectorID(connectorID); err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, errors.New("begin Connector resource transaction: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Transfer this read lock to the returned transaction and hold it through
	// UDP dispatch, commit/discard, and unlock. That prevents Store.Close from
	// releasing pinned state underneath the exchange. The cross-process lock
	// below deliberately has the same lifetime: shortening either lock would
	// let two callers race different durable nonces. The CLI runs one Connector
	// route per process, so writer starvation is bounded by the caller's resolve
	// context rather than multiplying across routes.
	s.mu.RLock()
	defer func() {
		if retErr != nil {
			s.mu.RUnlock()
		}
	}()
	if err := s.validateContinuityLocked(); err != nil {
		return nil, err
	}
	unlock, err := acquireConnectorResourcesLock(ctx, s.dir)
	if err != nil {
		return nil, fmt.Errorf("lock Connector resource state: %w", err)
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, unlock())
		}
	}()
	if err := s.validateContinuityLocked(); err != nil {
		return nil, err
	}

	current, err := loadConnectorResources(s.dir)
	if err != nil {
		return nil, err
	}
	if err := s.validateContinuityLocked(); err != nil {
		return nil, err
	}
	pending, ok := current.Pending[connectorID]
	if !ok {
		expected := ""
		if binding, exists := current.Bindings[connectorID]; exists {
			expected = binding.ResourceID
		}
		request, err := qurl.NewNativeConnectorResourceRequest(connectorID, expected)
		if err != nil {
			return nil, fmt.Errorf("prepare native Connector resource request: %w", err)
		}
		pending = PendingConnectorResourceRequest{
			ConnectorID: connectorID, RequestNonce: request.RequestNonce,
			ExpectedResourceID: expected,
		}
		current.Pending[connectorID] = pending
		if err := writeConnectorResources(s.dir, current); err != nil {
			return nil, fmt.Errorf("persist native Connector resource request before dispatch: %w", err)
		}
		if err := s.validateContinuityLocked(); err != nil {
			return nil, fmt.Errorf("validate state continuity after persisting native Connector resource request: %w", err)
		}
	}

	return &ConnectorResourceTransaction{
		store: s,
		state: current,
		request: qurl.NativeConnectorResourceRequest{
			ConnectorID:        pending.ConnectorID,
			RequestNonce:       pending.RequestNonce,
			ExpectedResourceID: pending.ExpectedResourceID,
		},
		unlock: unlock,
	}, nil
}

// Request returns a copy of the exact durable LST request.
func (t *ConnectorResourceTransaction) Request() *qurl.NativeConnectorResourceRequest {
	if t == nil || t.closed || t.finished {
		return nil
	}
	request := t.request
	return &request
}

// Commit records the authenticated complete binding and clears the exact
// pending request in one atomic file replacement. A response that contradicts
// durable state is terminal for that exact request: Commit preserves accepted
// bindings, atomically clears the pending request, and returns a typed error so
// the caller fails closed without entering an exact-replay loop after restart.
// A failed durable clear leaves the transaction open and reports the required
// state-access repair instead of claiming the request was discarded.
func (t *ConnectorResourceTransaction) Commit(binding *ConnectorResourceBinding) error {
	if err := t.validateOpen(); err != nil {
		return err
	}
	if binding == nil {
		return t.rejectCommit(ErrConnectorResourceVerification, "authenticated response has no binding")
	}
	if binding.ConnectorID != t.request.ConnectorID {
		return t.rejectCommit(ErrConnectorResourceVerification, "response Connector ID does not match the durable request")
	}
	if t.request.ExpectedResourceID != "" && binding.ResourceID != t.request.ExpectedResourceID {
		return t.rejectCommit(ErrConnectorResourceVerification, "response identity does not match the continuity assertion")
	}
	if err := validateBinding(binding); err != nil {
		return t.rejectCommit(ErrConnectorResourceVerification, fmt.Sprintf("response binding is invalid: %v", err))
	}
	committed := *binding
	if existing, ok := t.state.Bindings[binding.ConnectorID]; ok {
		if existing.ConnectorRoutingID != committed.ConnectorRoutingID || existing.KnockResourceID != committed.KnockResourceID {
			return t.rejectCommit(ErrConnectorResourceVerification, "authenticated response changed the cached routing or knock binding")
		}
		switch {
		case existing.CRID != "" && committed.CRID == "":
			// CRID is optional in an authenticated response. Preserve a
			// previously key-verified value when a warm response omits it.
			committed.CRID = existing.CRID
		case existing.CRID != "" && committed.CRID != existing.CRID:
			return t.rejectCommit(ErrConnectorResourceVerification, "authenticated response changed the cached CRID")
		}
	}
	for existingID, existing := range t.state.Bindings {
		if existingID == committed.ConnectorID {
			continue
		}
		switch {
		case existing.ResourceID == committed.ResourceID:
			return t.rejectCommit(ErrConnectorResourceStateConflict, fmt.Sprintf("resource identity is already bound to Connector %q", existingID))
		case existing.ConnectorRoutingID == committed.ConnectorRoutingID:
			return t.rejectCommit(ErrConnectorResourceStateConflict, fmt.Sprintf("routing identity is already bound to Connector %q", existingID))
		}
	}
	t.state.Bindings[committed.ConnectorID] = committed
	delete(t.state.Pending, binding.ConnectorID)
	if err := writeConnectorResources(t.store.dir, t.state); err != nil {
		return fmt.Errorf("commit Connector resource state: %w", err)
	}
	if err := t.store.validateContinuityLocked(); err != nil {
		return fmt.Errorf("validate state continuity after committing Connector resource state: %w", err)
	}
	t.finished = true
	return nil
}

func (t *ConnectorResourceTransaction) rejectCommit(kind error, detail string) error {
	contradiction := &ConnectorResourceCommitError{kind: kind, detail: detail}
	delete(t.state.Pending, t.request.ConnectorID)
	if err := writeConnectorResources(t.store.dir, t.state); err != nil {
		return errors.Join(contradiction, fmt.Errorf("discard contradictory Connector resource request: %w", err))
	}
	contradiction.pendingDiscarded = true
	t.finished = true
	if err := t.store.validateContinuityLocked(); err != nil {
		return errors.Join(contradiction, fmt.Errorf("validate state continuity after discarding contradictory Connector resource request: %w", err))
	}
	return contradiction
}

// ClearPending removes a request after an authenticated terminal denial that
// proves no mutation occurred. Existing continuity state is retained.
func (t *ConnectorResourceTransaction) ClearPending() error {
	if err := t.validateOpen(); err != nil {
		return err
	}
	delete(t.state.Pending, t.request.ConnectorID)
	if err := writeConnectorResources(t.store.dir, t.state); err != nil {
		return fmt.Errorf("clear terminal Connector resource request: %w", err)
	}
	if err := t.store.validateContinuityLocked(); err != nil {
		return fmt.Errorf("validate state continuity after clearing terminal Connector resource request: %w", err)
	}
	t.finished = true
	return nil
}

func (t *ConnectorResourceTransaction) validateOpen() error {
	if t == nil || t.closed || t.finished || t.store == nil || t.unlock == nil {
		return errors.New("connector resource transaction is closed")
	}
	return t.store.validateContinuityLocked()
}

// Close releases the cross-process lock and the Store continuity lease. It
// never changes pending state: callers preserve uncertain and retryable
// requests simply by closing without Commit or ClearPending.
func (t *ConnectorResourceTransaction) Close() error {
	if t == nil || t.closed {
		return nil
	}
	t.closed = true
	unlock := t.unlock
	t.unlock = nil
	store := t.store
	t.store = nil
	err := unlock()
	store.mu.RUnlock()
	return err
}

func emptyConnectorResourcesState() connectorResourcesState {
	return connectorResourcesState{
		Version:  connectorResourcesVersion,
		Bindings: make(map[string]ConnectorResourceBinding),
		Pending:  make(map[string]PendingConnectorResourceRequest),
	}
}

func loadConnectorResources(dir string) (connectorResourcesState, error) {
	path := filepath.Join(dir, ConnectorResourcesFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyConnectorResourcesState(), nil
	}
	if err != nil {
		return connectorResourcesState{}, fmt.Errorf("inspect Connector resource state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return connectorResourcesState{}, errors.New("connector resource state must be a non-symlink regular file")
	}
	if !connectorResourceOwnerOK(info) {
		return connectorResourcesState{}, errors.New("connector resource state must be owned by the current user")
	}
	if info.Mode().Perm() != connectorResourceFileMode {
		return connectorResourcesState{}, fmt.Errorf("connector resource state has mode %04o, want %04o", info.Mode().Perm(), connectorResourceFileMode)
	}
	if info.Size() > connectorResourcesMaxBytes {
		return connectorResourcesState{}, fmt.Errorf("connector resource state exceeds %d bytes", connectorResourcesMaxBytes)
	}
	file, err := openConnectorResourceState(path)
	if err != nil {
		return connectorResourcesState{}, fmt.Errorf("open Connector resource state: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return connectorResourcesState{}, fmt.Errorf("inspect opened Connector resource state: %w", err)
	}
	currentInfo, err := os.Lstat(path)
	if err != nil || currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, openedInfo) || !os.SameFile(openedInfo, currentInfo) {
		return connectorResourcesState{}, errors.New("connector resource state changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, connectorResourcesMaxBytes+1))
	if err != nil {
		return connectorResourcesState{}, fmt.Errorf("read Connector resource state: %w", err)
	}
	if len(data) > connectorResourcesMaxBytes {
		return connectorResourcesState{}, fmt.Errorf("connector resource state exceeds %d bytes", connectorResourcesMaxBytes)
	}
	state, err := decodeConnectorResources(data)
	if err != nil {
		return connectorResourcesState{}, fmt.Errorf("invalid Connector resource state: %w", err)
	}
	return state, nil
}

func decodeConnectorResources(data []byte) (connectorResourcesState, error) {
	// RFC 8259 state must be valid UTF-8 before encoding/json sees it. The
	// standard decoder deliberately replaces malformed bytes with U+FFFD,
	// which could otherwise normalize an opaque knock identity into a different
	// accepted value instead of failing closed.
	if !utf8.Valid(data) {
		return connectorResourcesState{}, errors.New("connector resource state is not valid UTF-8")
	}
	// Inspect the original token stream before typed decoding so duplicate keys,
	// excessive nesting, and case-insensitive struct-field aliases cannot be
	// collapsed into accepted continuity state.
	if err := rejectDuplicateResourceFields(data); err != nil {
		return connectorResourcesState{}, err
	}
	// encoding/json also replaces unpaired UTF-16 surrogate escapes with U+FFFD.
	// Reject those spellings while accepting a correctly paired JSON escape.
	if err := rejectUnpairedJSONSurrogates(data); err != nil {
		return connectorResourcesState{}, err
	}
	if err := rejectNonCanonicalResourceFields(data); err != nil {
		return connectorResourcesState{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state connectorResourcesState
	if err := decoder.Decode(&state); err != nil {
		return connectorResourcesState{}, err
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return connectorResourcesState{}, err
	}
	if err := validateConnectorResourcesState(state); err != nil {
		return connectorResourcesState{}, err
	}
	return state, nil
}

func validateConnectorResourcesState(state connectorResourcesState) error {
	if state.Version != connectorResourcesVersion {
		return fmt.Errorf("unsupported version %d", state.Version)
	}
	if state.Bindings == nil || state.Pending == nil {
		return errors.New("bindings and pending maps are required")
	}
	if len(state.Bindings) > connectorResourcesMaxItems || len(state.Pending) > connectorResourcesMaxItems {
		return fmt.Errorf("bindings and pending are limited to %d entries each", connectorResourcesMaxItems)
	}
	resourceOwners := make(map[string]string, len(state.Bindings))
	routingOwners := make(map[string]string, len(state.Bindings))
	for key, binding := range state.Bindings {
		if key != binding.ConnectorID {
			return fmt.Errorf("binding key %q does not match Connector ID %q", key, binding.ConnectorID)
		}
		if err := validateBinding(&binding); err != nil {
			return fmt.Errorf("binding %q: %w", key, err)
		}
		if owner, exists := resourceOwners[binding.ResourceID]; exists {
			return fmt.Errorf("bindings %q and %q share one resource identity", owner, key)
		}
		if owner, exists := routingOwners[binding.ConnectorRoutingID]; exists {
			return fmt.Errorf("bindings %q and %q share one routing identity", owner, key)
		}
		resourceOwners[binding.ResourceID] = key
		routingOwners[binding.ConnectorRoutingID] = key
	}
	for key, pending := range state.Pending {
		if key != pending.ConnectorID {
			return fmt.Errorf("pending key %q does not match Connector ID %q", key, pending.ConnectorID)
		}
		if err := validatePending(pending); err != nil {
			return fmt.Errorf("pending %q: %w", key, err)
		}
		binding, exists := state.Bindings[key]
		switch {
		case exists && pending.ExpectedResourceID != binding.ResourceID:
			return fmt.Errorf("pending %q does not assert its cached resource identity", key)
		case !exists && pending.ExpectedResourceID != "":
			return fmt.Errorf("pending %q asserts an identity without a cached binding", key)
		}
	}
	return nil
}

func validateBinding(binding *ConnectorResourceBinding) error {
	if binding == nil {
		return errors.New("binding is nil")
	}
	if err := validateConnectorID(binding.ConnectorID); err != nil {
		return err
	}
	der, err := validateResourceID(binding.ResourceID)
	if err != nil {
		return err
	}
	if !validRoutingID(binding.ConnectorRoutingID) {
		return errors.New("connector routing ID is not canonical")
	}
	if !validKnockResourceID(binding.KnockResourceID) {
		return errors.New("knock resource ID is not canonical")
	}
	if binding.CRID != "" {
		matched, err := crid.KeyMatches(binding.CRID, der)
		if err != nil || !matched {
			return errors.New("crid does not match the public resource identity")
		}
	}
	return nil
}

func validatePending(pending PendingConnectorResourceRequest) error {
	if err := validateConnectorID(pending.ConnectorID); err != nil {
		return err
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(pending.RequestNonce)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != pending.RequestNonce {
		return errors.New("request nonce must be canonical unpadded base64url of 32 bytes")
	}
	if pending.ExpectedResourceID != "" {
		if _, err := validateResourceID(pending.ExpectedResourceID); err != nil {
			return fmt.Errorf("expected resource identity: %w", err)
		}
	}
	return nil
}

func validateConnectorID(id string) error {
	if !connectorIDPattern(id) {
		return fmt.Errorf("connector ID %q must be 3-64 lowercase letters, digits, or hyphens; start with a letter and end with a letter or digit", id)
	}
	return nil
}

func mustCompileConnectorIDPattern() func(string) bool {
	return func(value string) bool {
		if len(value) < 3 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
			return false
		}
		last := value[len(value)-1]
		lastIsLetter := last >= 'a' && last <= 'z'
		lastIsDigit := last >= '0' && last <= '9'
		if !lastIsLetter && !lastIsDigit {
			return false
		}
		for _, c := range []byte(value) {
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
				continue
			}
			return false
		}
		return true
	}
}

func validateResourceID(value string) ([]byte, error) {
	der, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(der) != value {
		return nil, errors.New("resource identity is not canonical unpadded base64url")
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, errors.New("resource identity is not a DER SPKI public key")
	}
	publicKey, ok := parsed.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() {
		return nil, errors.New("resource identity is not a P-256 public key")
	}
	if canonical, err := x509.MarshalPKIXPublicKey(publicKey); err != nil || !bytes.Equal(canonical, der) {
		return nil, errors.New("resource identity DER is not canonical")
	}
	return der, nil
}

func validRoutingID(value string) bool {
	if !strings.HasPrefix(value, "c-") || len(value) != 54 {
		return false
	}
	raw := value[2:]
	decoded, err := base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding).DecodeString(raw)
	return err == nil && len(decoded) == sha256.Size &&
		base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding).EncodeToString(decoded) == raw
}

func validKnockResourceID(value string) bool {
	return value != "" && len(value) <= 64 && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func writeConnectorResources(dir string, state connectorResourcesState) error {
	data, err := encodeConnectorResources(state)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ConnectorResourcesFile)
	if err := validateConnectorResourcesWriteTarget(path); err != nil {
		return err
	}
	return replaceConnectorResources(dir, path, data)
}

func encodeConnectorResources(state connectorResourcesState) ([]byte, error) {
	if err := validateConnectorResourcesState(state); err != nil {
		return nil, fmt.Errorf("refuse invalid Connector resource state: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("encode Connector resource state: %w", err)
	}
	if len(data) > connectorResourcesMaxBytes {
		return nil, fmt.Errorf("connector resource state exceeds %d bytes", connectorResourcesMaxBytes)
	}
	return data, nil
}

func validateConnectorResourcesWriteTarget(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("connector resource state must be a non-symlink regular file")
		}
		if info.Mode().Perm() != connectorResourceFileMode {
			return fmt.Errorf("connector resource state has mode %04o, want %04o", info.Mode().Perm(), connectorResourceFileMode)
		}
		if !connectorResourceOwnerOK(info) {
			return errors.New("connector resource state must be owned by the current user")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Connector resource state before write: %w", err)
	}
	return nil
}

func replaceConnectorResources(dir, path string, data []byte) (retErr error) {
	suffix := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, suffix); err != nil {
		return fmt.Errorf("generate Connector resource state temporary name: %w", err)
	}
	tmpPath := filepath.Join(dir, "."+ConnectorResourcesFile+".tmp-"+hex.EncodeToString(suffix))
	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, connectorResourceFileMode) //nolint:gosec // G302,G304: fixed 0600 in the pinned owner-only state dir.
	if err != nil {
		return fmt.Errorf("create Connector resource state temporary file: %w", err)
	}
	committed, closed := false, false
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
	if err := tmp.Chmod(connectorResourceFileMode); err != nil {
		return fmt.Errorf("set Connector resource state temporary permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write Connector resource state temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync Connector resource state temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close Connector resource state temporary file: %w", err)
	}
	closed = true
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("commit Connector resource state rename: %w", err)
	}
	committed = true
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("connector resource state rename committed but directory sync failed: %w", err)
	}
	return nil
}

func rejectDuplicateResourceFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanUniqueJSONValue(decoder, 0); err != nil {
		return err
	}
	return rejectTrailingJSON(decoder)
}

const connectorResourcesMaxJSONDepth = 8

func rejectUnpairedJSONSurrogates(data []byte) error {
	// rejectDuplicateResourceFields has already established syntactically valid
	// JSON, so this pass only has to preserve string/escape boundaries and can
	// focus on the Unicode scalar-value rule encoding/json intentionally relaxes.
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		for i++; i < len(data) && data[i] != '"'; i++ {
			if data[i] != '\\' {
				continue
			}
			i++
			if i >= len(data) || data[i] != 'u' {
				continue
			}
			var err error
			i, err = validateJSONUnicodeEscape(data, i)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func validateJSONUnicodeEscape(data []byte, marker int) (int, error) {
	value, ok := decodeJSONHexQuad(data[marker+1:])
	if !ok {
		return marker, errors.New("invalid JSON Unicode escape")
	}
	last := marker + 4
	if value >= 0xdc00 && value <= 0xdfff {
		return marker, errors.New("unpaired low surrogate in JSON string")
	}
	if value < 0xd800 || value > 0xdbff {
		return last, nil
	}
	if last+6 >= len(data) || data[last+1] != '\\' || data[last+2] != 'u' {
		return marker, errors.New("unpaired high surrogate in JSON string")
	}
	low, validLow := decodeJSONHexQuad(data[last+3:])
	if !validLow || low < 0xdc00 || low > 0xdfff {
		return marker, errors.New("unpaired high surrogate in JSON string")
	}
	return last + 6, nil
}

func decodeJSONHexQuad(data []byte) (uint16, bool) {
	if len(data) < 4 {
		return 0, false
	}
	var value uint16
	for _, digit := range data[:4] {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func scanUniqueJSONValue(decoder *json.Decoder, depth int) error {
	if depth > connectorResourcesMaxJSONDepth {
		return fmt.Errorf("JSON nesting exceeds %d levels", connectorResourcesMaxJSONDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		return scanUniqueJSONObject(decoder, depth)
	case '[':
		return scanUniqueJSONArray(decoder, depth)
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func scanUniqueJSONObject(decoder *json.Decoder, depth int) error {
	seen := map[string]struct{}{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("json object key is not a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate JSON field %q", key)
		}
		seen[key] = struct{}{}
		if err := scanUniqueJSONValue(decoder, depth+1); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return errors.New("json object did not close")
	}
	return nil
}

func scanUniqueJSONArray(decoder *json.Decoder, depth int) error {
	for decoder.More() {
		if err := scanUniqueJSONValue(decoder, depth+1); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim(']') {
		return errors.New("json array did not close")
	}
	return nil
}

func rejectNonCanonicalResourceFields(data []byte) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	for key := range envelope {
		switch key {
		case jsonFieldVersion, "bindings", "pending":
		default:
			return fmt.Errorf("unknown field %q", key)
		}
	}
	if err := rejectNonCanonicalResourceMap(envelope["bindings"], "bindings", map[string]bool{
		"connector_id": true, "resource_id": true, "crid": true,
		"connector_routing_id": true, "knock_resource_id": true,
	}, "crid"); err != nil {
		return err
	}
	return rejectNonCanonicalResourceMap(envelope["pending"], "pending", map[string]bool{
		"connector_id": true, "request_nonce": true, "expected_resource_id": true,
	}, "expected_resource_id")
}

func rejectNonCanonicalResourceMap(raw json.RawMessage, field string, allowed map[string]bool, optional string) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil // The typed validator reports a missing/null required map.
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	for entryKey, entryRaw := range entries {
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(entryRaw, &entry); err != nil {
			return fmt.Errorf("%s[%q]: %w", field, entryKey, err)
		}
		for key := range entry {
			if !allowed[key] {
				return fmt.Errorf("%s[%q]: unknown field %q", field, entryKey, key)
			}
		}
		if value, present := entry[optional]; present {
			trimmed := bytes.TrimSpace(value)
			switch {
			case bytes.Equal(trimmed, []byte("null")):
				return fmt.Errorf("%s[%q]: %s must be absent rather than null", field, entryKey, optional)
			case bytes.Equal(trimmed, []byte(`""`)):
				return fmt.Errorf("%s[%q]: %s must be absent rather than empty", field, entryKey, optional)
			}
		}
	}
	return nil
}
