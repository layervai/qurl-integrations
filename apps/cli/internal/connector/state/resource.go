package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/layervai/qurl-go/crid"
	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/resourceidentity"
)

const (
	// ConnectorResourcesFile stores the native resource binding and exact
	// in-flight LST request. It is intentionally separate from qurl-go's agent
	// envelope: the SDK owns the device identity while this CLI owns the
	// customer Connector IDs it has bound to that identity.
	ConnectorResourcesFile = "connector_resources.json"
	connectorResourcesLock = ".connector_resources.lock"

	connectorResourcesVersion = 2
	// connectorResourcesMaxBytes bounds the whole state file. Bindings and
	// pending both filled to connectorResourcesMaxItems with maximal entries
	// marshal to ~3.5 MiB (measured by
	// TestConnectorResourceStateRoundTripsMaxItemsUnderByteCap); the 8 MiB cap
	// keeps ~2.3x headroom above that worst case.
	connectorResourcesMaxBytes = 8 << 20
	// connectorResourcesMaxItems is the hard per-map bound a load or write
	// refuses. It must never be the cap a publish hits before the local share
	// registry's LocalSharesMaxItems: every desired share holds one live
	// binding here, so bindings must fit the whole registry plus the retired
	// memory below with headroom (pinned at compile time under this block).
	// 4096 is 2x the registry cap.
	connectorResourcesMaxItems = 4096
	// connectorResourcesMaxRetired bounds the retired memory. A retired
	// Connector keeps its accepted binding (the default Connector ID chain
	// derives its successor from that identity), so each one occupies a
	// bindings slot for as long as it is remembered. Without a bound, deleted
	// shares from earlier runs would eventually crowd out live ones: the cap
	// was hit at 683 live shares with 340 retired leftovers. Retirement
	// evicts beyond this bound; see pruneRetired for what forgetting costs.
	connectorResourcesMaxRetired = 1024
	// connectorResourcesMaxPending bounds the exact-replay memory. A pending
	// request outlives a process only while its outcome is uncertain, and a
	// request whose commit failed locally is replayed by the next publish of
	// the same Connector ID; a batch that never republishes those IDs leaves
	// them behind forever. BeginConnectorResource evicts beyond this bound;
	// see prunePending for what forgetting costs.
	connectorResourcesMaxPending = 1024
	connectorResourceFileMode    = 0o600
)

// The resource-state map cap must hold a live binding for every registry row
// (LocalSharesMaxItems) plus the full retired memory, or a bounded retired set
// could still crowd out a live share; the pending memory shares the hard cap
// but never occupies a bindings slot. Each expression must be non-negative,
// which pins both relations at compile time. Mirrors the daemon package's
// LocalSharesMaxItems == MaxGroupRoutes pin, one hop further down.
const (
	_ = uint(connectorResourcesMaxItems - LocalSharesMaxItems - connectorResourcesMaxRetired)
	_ = uint(connectorResourcesMaxItems - connectorResourcesMaxPending)
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
	// ErrConnectorResourceRetired prevents a deliberately deleted Connector ID
	// from being sent again as an implicit resource-reclamation request. The
	// refusal is a bounded local memory (connectorResourcesMaxRetired), not a
	// service-side tombstone: a forgotten retirement falls back to the
	// service's own behavior for a deleted Connector ID, which is to mint a
	// fresh resource under it. See pruneRetired for what else forgetting costs.
	// TODO(upstream-contract): that fallback assumes the qURL service keeps
	// permitting an ordinarily deleted Connector ID to be published again.
	ErrConnectorResourceRetired = errors.New("connector resource was deliberately retired")
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
	CRID               string `json:"crid"`
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
	Retired  map[string]bool                            `json:"retired"`
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
	if current.Retired[connectorID] {
		return nil, fmt.Errorf("%w: Connector ID %q", ErrConnectorResourceRetired, connectorID)
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
		current.prunePending(connectorID)
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

// ConnectorResourceBinding returns the durable binding and retirement state
// for one exact Connector ID. It never searches by a remote-supplied alias.
func (s *Store) ConnectorResourceBinding(ctx context.Context, connectorID string) (_ ConnectorResourceBinding, retired, found bool, retErr error) {
	if s == nil {
		return ConnectorResourceBinding{}, false, false, fmt.Errorf("%w: Connector state store is not open", qurl.ErrAgentStateContinuity)
	}
	if err := validateConnectorID(connectorID); err != nil {
		return ConnectorResourceBinding{}, false, false, err
	}
	if ctx == nil {
		return ConnectorResourceBinding{}, false, false, errors.New("read Connector resource binding: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return ConnectorResourceBinding{}, false, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.validateContinuityLocked(); err != nil {
		return ConnectorResourceBinding{}, false, false, err
	}
	unlock, err := acquireConnectorResourcesLock(ctx, s.dir)
	if err != nil {
		return ConnectorResourceBinding{}, false, false, fmt.Errorf("lock Connector resource state: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, unlock()) }()
	current, err := loadConnectorResources(s.dir)
	if err != nil {
		return ConnectorResourceBinding{}, false, false, err
	}
	if err := s.validateContinuityLocked(); err != nil {
		return ConnectorResourceBinding{}, false, false, err
	}
	binding, found := current.Bindings[connectorID]
	return binding, current.Retired[connectorID], found, nil
}

// RetireConnectorResource records a completed user-authorized deletion by any
// exact local public identity. The accepted binding remains available only to
// derive a new default Connector ID; BeginConnectorResource refuses its reuse.
// The retired memory is bounded: once connectorResourcesMaxRetired deletions
// are remembered, each new one evicts the oldest-in-file entry together with
// its binding so deleted shares never crowd out live ones (see pruneRetired).
func (s *Store) RetireConnectorResource(ctx context.Context, id string) (retired bool, retErr error) {
	if s == nil {
		return false, fmt.Errorf("%w: Connector state store is not open", qurl.ErrAgentStateContinuity)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false, errors.New("retire Connector resource: identity is empty")
	}
	if ctx == nil {
		return false, errors.New("retire Connector resource: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.validateContinuityLocked(); err != nil {
		return false, err
	}
	unlock, err := acquireConnectorResourcesLock(ctx, s.dir)
	if err != nil {
		return false, fmt.Errorf("lock Connector resource state: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, unlock()) }()
	current, err := loadConnectorResources(s.dir)
	if err != nil {
		return false, err
	}
	if err := s.validateContinuityLocked(); err != nil {
		return false, err
	}
	connectorID := ""
	for candidateID, binding := range current.Bindings {
		if id == candidateID || id == binding.ResourceID || id == binding.CRID {
			if connectorID != "" && connectorID != candidateID {
				return false, errors.New("retire Connector resource: public identity is ambiguous in durable state")
			}
			connectorID = candidateID
		}
	}
	if connectorID == "" {
		return false, nil
	}
	if current.Retired[connectorID] {
		return true, nil
	}
	current.Retired[connectorID] = true
	delete(current.Pending, connectorID)
	current.pruneRetired(connectorID)
	if err := writeConnectorResources(s.dir, current); err != nil {
		return false, fmt.Errorf("retire Connector resource state: %w", err)
	}
	if err := s.validateContinuityLocked(); err != nil {
		return false, fmt.Errorf("validate state continuity after retiring Connector resource: %w", err)
	}
	return true, nil
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
		if committed.CRID != existing.CRID {
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
		Retired:  make(map[string]bool),
	}
}

// LocalPublishIDDomain separates every local Connector ID derivation from any
// other use of the same inputs. A target's default ID derives from the native
// agent identity and canonical origin (in the publish command); each
// replacement derives here, from the retired binding it succeeds.
const LocalPublishIDDomain = "qurl-cli-local-publish-v1"

// ReplacementConnectorID derives the next stable default Connector ID from a
// locally accepted binding that an authorized delete retired. It lives beside
// the retired memory because that memory must recognize the links of a chain
// the publish command can still walk (see pruneRetired). It does not turn an
// unexplained authority conflict into replacement permission.
func ReplacementConnectorID(connectorID, resourceID string) (string, error) {
	connectorID = strings.TrimSpace(connectorID)
	resourceID = strings.TrimSpace(resourceID)
	if connectorID == "" || resourceID == "" {
		return "", errors.New("cannot derive a replacement Connector ID without the retired Connector and resource identities")
	}
	digest := sha256.Sum256([]byte(LocalPublishIDDomain + "\x00replacement\x00" + connectorID + "\x00" + resourceID))
	suffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:10]))
	return "local-" + suffix, nil
}

// pruneRetired forgets retired Connectors beyond connectorResourcesMaxRetired,
// removing each evicted entry from both retired and bindings so the slot it
// held is free for a live share again. keep is the Connector retired by the
// current call and is never evicted, so a retirement always sticks.
//
// What forgetting costs. A forgotten explicit ID simply stops being refused
// locally; the next publish of it mints a fresh resource, which is what the
// service does for any deleted Connector ID. Default IDs chain: the publish
// command finds a target's current default by walking retired bindings from
// the origin's root, deriving each successor with ReplacementConnectorID, and
// stops at the first ID that is absent or live. Forgetting a link of a chain
// that still ends in a live share would stop that walk early and mint a new
// resource under the forgotten ID while the live share stayed desired-on,
// listed only under its own CRID. So links of live-tailed chains are evicted
// last: first every other retirement in the file's key order (a chain whose
// tail was itself deleted restarts harmlessly wherever it is cut, and the
// links beyond the cut become ordinary evictable leftovers), and only when
// every remembered retirement leads to a live share does eviction cut one.
// Reaching that takes one default target deleted connectorResourcesMaxRetired
// times and then republished; the fork it causes is the accepted cost of a
// bounded memory.
// TODO(upstream-contract): the explicit-ID fallback assumes the qURL service
// keeps permitting an ordinarily deleted Connector ID to be published again.
func (s *connectorResourcesState) pruneRetired(keep string) {
	excess := len(s.Retired) - connectorResourcesMaxRetired
	if excess <= 0 {
		return
	}
	liveTailed := s.retiredWithLiveTail()
	evictable := make(map[string]bool, len(s.Retired))
	for id := range s.Retired {
		if !liveTailed[id] {
			evictable[id] = true
		}
	}
	for _, id := range evictionOrder(evictable, excess, keep) {
		s.forgetRetired(id)
	}
	if excess = len(s.Retired) - connectorResourcesMaxRetired; excess > 0 {
		for _, id := range evictionOrder(s.Retired, excess, keep) {
			s.forgetRetired(id)
		}
	}
}

func (s *connectorResourcesState) forgetRetired(id string) {
	delete(s.Retired, id)
	delete(s.Bindings, id)
}

// retiredWithLiveTail reports every retired Connector whose chain of
// replacements still ends in a live binding, memoizing each link so the pass
// stays linear in the size of the retired memory.
func (s *connectorResourcesState) retiredWithLiveTail() map[string]bool {
	memo := make(map[string]bool, len(s.Retired))
	result := make(map[string]bool, len(s.Retired))
	for id := range s.Retired {
		if s.chainEndsLive(id, memo) {
			result[id] = true
		}
	}
	return result
}

// chainEndsLive follows a retired Connector's replacements until the chain
// reaches a live binding (true) or leaves the remembered state (false). The
// path bound guards against a derivation cycle, which cannot occur without a
// hash collision but must not hang a delete if it ever did.
func (s *connectorResourcesState) chainEndsLive(id string, memo map[string]bool) bool {
	var path []string
	settle := func(ends bool) bool {
		for _, link := range path {
			memo[link] = ends
		}
		return ends
	}
	for current := id; ; {
		if ends, seen := memo[current]; seen {
			return settle(ends)
		}
		binding, bound := s.Bindings[current]
		if !bound {
			return settle(false)
		}
		if !s.Retired[current] {
			return settle(true)
		}
		if path = append(path, current); len(path) > len(s.Retired) {
			return settle(false)
		}
		successor, err := ReplacementConnectorID(current, binding.ResourceID)
		if err != nil {
			return settle(false)
		}
		current = successor
	}
}

// prunePending forgets exact-replay requests beyond
// connectorResourcesMaxPending. keep is the request the current transaction
// is about to dispatch and is never evicted, so a fresh request is always
// durable before its first packet.
//
// A forgotten request costs replay exactness, never correctness: the next
// publish of that Connector ID persists a fresh nonce before dispatch, and the
// service resolves a Connector ID to its one active resource, so an exchange
// whose response was lost converges on the same identity through the fresh
// request (reported as found-existing). A warm request is regenerated from
// the durable binding's identity assertion, exactly as it was first built.
// TODO(upstream-contract): that convergence assumes the qURL service keeps
// resolving a fresh request for an existing Connector ID to the active
// resource instead of minting another.
func (s *connectorResourcesState) prunePending(keep string) {
	for _, id := range evictionOrder(s.Pending, len(s.Pending)-connectorResourcesMaxPending, keep) {
		delete(s.Pending, id)
	}
}

// evictionOrder returns up to excess keys of entries to forget, never keep.
// v2 state records no timestamps, so the only durable order it carries is the
// file's own: json.Marshal writes map keys sorted, so the entries that appear
// first in the file are forgotten first. For sequentially named batches, the
// kind of workload that fills these memories, that is oldest-first.
func evictionOrder[V any](entries map[string]V, excess int, keep string) []string {
	if excess <= 0 {
		return nil
	}
	evicted := make([]string, 0, excess)
	for _, key := range slices.Sorted(maps.Keys(entries)) {
		if len(evicted) == excess {
			break
		}
		if key == keep {
			continue
		}
		evicted = append(evicted, key)
	}
	return evicted
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
	if err := validateConnectorResourceFile(path, info); err != nil {
		return connectorResourcesState{}, err
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
	if state.Bindings == nil || state.Pending == nil || state.Retired == nil {
		return errors.New("bindings, pending, and retired maps are required")
	}
	if len(state.Bindings) > connectorResourcesMaxItems || len(state.Pending) > connectorResourcesMaxItems || len(state.Retired) > connectorResourcesMaxItems {
		return fmt.Errorf("bindings, pending, and retired are limited to %d entries each", connectorResourcesMaxItems)
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
	return validateRetiredConnectorResources(state)
}

func validateRetiredConnectorResources(state connectorResourcesState) error {
	for key, retired := range state.Retired {
		if !retired {
			return fmt.Errorf("retired Connector %q must have value true", key)
		}
		if _, exists := state.Bindings[key]; !exists {
			return fmt.Errorf("retired Connector %q has no accepted binding", key)
		}
		if _, pending := state.Pending[key]; pending {
			return fmt.Errorf("retired Connector %q still has a pending request", key)
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
	if strings.TrimSpace(binding.CRID) == "" {
		return errors.New("crid is required")
	}
	matched, err := crid.KeyMatches(binding.CRID, der)
	if err != nil || !matched {
		return errors.New("crid does not match the public resource identity")
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

// TODO(upstream-contract): replace this copy when qurl-connector exports its
// canonical Connector ID validator.
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
	return resourceidentity.ValidateResourceID(value)
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
		if err := validateConnectorResourceFile(path, info); err != nil {
			return err
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
	tmp, err := createConnectorResourceTemp(tmpPath)
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
	if err := commitConnectorResourceRename(tmpPath, path); err != nil {
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
		case jsonFieldVersion, "bindings", "pending", "retired":
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
