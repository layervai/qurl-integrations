package matchedcohort

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	qurl "github.com/layervai/qurl-go/qurl"
)

var (
	// errStateNotFound is the store's exact absent-version classification.
	errStateNotFound = errors.New("matched cohort: durable state not found")
	// errStateConflict reports an exact CAS, receipt, or immutable-byte mismatch.
	errStateConflict = errors.New("matched cohort: durable state conflict")
	// errStateAmbiguous reports a mutation that cannot be classified by readback.
	errStateAmbiguous = errors.New("matched cohort: durable state outcome ambiguous")
)

// Blob is one exact committed secret version. OperationID makes a lost commit
// response replayable without accepting a coincidentally byte-equal sibling.
type Blob struct {
	Key             string
	VersionID       string
	PreviousVersion string
	OperationID     string
	SHA256          string
	Body            []byte
}

// BlobCandidate is one exact compare-and-swap request.
type BlobCandidate struct {
	Key             string
	ExpectedVersion string
	OperationID     string
	SHA256          string
	Body            []byte
}

// BlobAuthority is implemented by the orchestration-owned DDB/Secrets Manager
// adapter. Commit must be a compare-and-swap. Load must return one immutable
// version and verify the backing secret VersionId before exposing Body.
type BlobAuthority interface {
	Load(context.Context, string) (Blob, error)
	Commit(context.Context, BlobCandidate) (Blob, error)
}

// DurableAgentStateStore persists every qurl-go state transition through the
// orchestration authority before SaveAgentState returns. It holds no AWS
// client. A restarted process reconstructs it from the same BlobAuthority/key.
type DurableAgentStateStore struct {
	mu        sync.Mutex
	authority BlobAuthority
	key       string
	current   Blob
	loaded    bool
}

// NewDurableAgentStateStore binds a qurl-go state store to one secret key.
func NewDurableAgentStateStore(authority BlobAuthority, key string) (*DurableAgentStateStore, error) {
	if authority == nil || !validText(key) {
		return nil, fmt.Errorf("%w: agent state authority", errInvalidAuthority)
	}
	return &DurableAgentStateStore{authority: authority, key: key}, nil
}

// LoadAgentState implements qurl.AgentStateStore with a fresh caller-owned value.
func (s *DurableAgentStateStore) LoadAgentState(ctx context.Context) (*qurl.AgentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	blob, err := s.authority.Load(ctx, s.key)
	if errors.Is(err, errStateNotFound) {
		s.current = Blob{Key: s.key}
		s.loaded = true
		return nil, qurl.ErrAgentStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load durable agent state: %w", err)
	}
	state, err := decodeAgentStateBlob(blob)
	if err != nil {
		return nil, err
	}
	s.current = cloneBlob(blob)
	s.loaded = true
	return state, nil
}

// SaveAgentState durably commits a snapshot before returning to qurl-go.
func (s *DurableAgentStateStore) SaveAgentState(ctx context.Context, state *qurl.AgentState) error {
	if state == nil {
		return errors.New("save durable agent state: nil state")
	}
	raw, err := CanonicalJSON(state)
	if err != nil {
		return fmt.Errorf("save durable agent state: %w", err)
	}
	var decoded qurl.AgentState
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("save durable agent state: invalid SDK state: %w", err)
	}
	digest := Digest(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		current, loadErr := s.authority.Load(ctx, s.key)
		switch {
		case loadErr == nil:
			if _, decodeErr := decodeAgentStateBlob(current); decodeErr != nil {
				return decodeErr
			}
			s.current = cloneBlob(current)
		case errors.Is(loadErr, errStateNotFound):
			s.current = Blob{Key: s.key}
		default:
			return fmt.Errorf("load durable state before save: %w", loadErr)
		}
		s.loaded = true
	}
	operationID := Digest([]byte("layerv/matched-cohort-agent-state/v1\x00" + s.key + "\x00" + s.current.VersionID + "\x00" + digest))
	candidate := BlobCandidate{Key: s.key, ExpectedVersion: s.current.VersionID, OperationID: operationID, SHA256: digest, Body: raw}
	committed, err := s.authority.Commit(ctx, candidate)
	if err != nil {
		observed, loadErr := s.authority.Load(ctx, s.key)
		if loadErr != nil || !sameCommittedBlob(observed, candidate) {
			return fmt.Errorf("%w: save durable agent state", errStateAmbiguous)
		}
		committed = observed
	}
	if !sameCommittedBlob(committed, candidate) {
		return fmt.Errorf("%w: agent state commit receipt", errStateConflict)
	}
	s.current = cloneBlob(committed)
	return nil
}

// Reference returns the exact committed secret version and digest.
func (s *DurableAgentStateStore) Reference(ctx context.Context) (StateReference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded || s.current.VersionID == "" {
		blob, err := s.authority.Load(ctx, s.key)
		if err != nil {
			return StateReference{}, err
		}
		if _, err := decodeAgentStateBlob(blob); err != nil {
			return StateReference{}, err
		}
		s.current = cloneBlob(blob)
		s.loaded = true
	}
	return StateReference{Key: s.key, VersionID: s.current.VersionID, SHA256: s.current.SHA256}, nil
}

func decodeAgentStateBlob(blob Blob) (*qurl.AgentState, error) { //nolint:gocritic // Blob is an immutable receipt value.
	if !validText(blob.Key) || !validText(blob.VersionID) || !hex64Pattern.MatchString(blob.SHA256) || Digest(blob.Body) != blob.SHA256 {
		return nil, fmt.Errorf("%w: invalid agent state blob receipt", errStateConflict)
	}
	var state qurl.AgentState
	if err := json.Unmarshal(blob.Body, &state); err != nil {
		return nil, fmt.Errorf("%w: decode agent state", errStateConflict)
	}
	canonical, err := CanonicalJSON(&state)
	if err != nil || !bytes.Equal(canonical, blob.Body) {
		return nil, fmt.Errorf("%w: noncanonical agent state", errStateConflict)
	}
	return &state, nil
}

func sameCommittedBlob(blob Blob, candidate BlobCandidate) bool { //nolint:gocritic // Exact comparison is value-oriented.
	return blob.Key == candidate.Key && validText(blob.VersionID) && blob.PreviousVersion == candidate.ExpectedVersion &&
		blob.OperationID == candidate.OperationID && blob.SHA256 == candidate.SHA256 && bytes.Equal(blob.Body, candidate.Body)
}

func cloneBlob(blob Blob) Blob { //nolint:gocritic // Clone deliberately receives an immutable value.
	blob.Body = bytes.Clone(blob.Body)
	return blob
}
