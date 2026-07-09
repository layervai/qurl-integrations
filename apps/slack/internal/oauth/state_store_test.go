package oauth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryStateStore struct {
	mu                 sync.Mutex
	items              map[string]StoredState
	started            map[string]bool
	consumed           map[string]bool
	startHadDeadline   bool
	consumeHadDeadline bool
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{
		items:    map[string]StoredState{},
		started:  map[string]bool{},
		consumed: map[string]bool{},
	}
}

func (s *memoryStateStore) PutState(_ context.Context, handle string, state StoredState) error { //nolint:gocritic // test fake mirrors StateStore's value signature.
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[handle]; exists {
		return errors.New("duplicate handle")
	}
	s.items[handle] = state
	return nil
}

func (s *memoryStateStore) StartState(ctx context.Context, handle string, now time.Time) (VerifiedState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, s.startHadDeadline = ctx.Deadline()
	state, ok := s.items[handle]
	if !ok || s.consumed[handle] || !now.Before(state.ExpiresAt) {
		return VerifiedState{}, errStateExpired
	}
	s.started[handle] = true
	return state.VerifiedState, nil
}

func (s *memoryStateStore) ConsumeState(ctx context.Context, handle string, now time.Time) (VerifiedState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, s.consumeHadDeadline = ctx.Deadline()
	state, ok := s.items[handle]
	if !ok || !s.started[handle] || s.consumed[handle] || !now.Before(state.ExpiresAt) {
		return VerifiedState{}, errStateExpired
	}
	s.consumed[handle] = true
	delete(s.items, handle)
	return state.VerifiedState, nil
}

func TestMintStoredStateProducesOpaqueHandle(t *testing.T) {
	now := time.Unix(1700000000, 0)
	store := newMemoryStateStore()
	handle, err := MintStoredStateWithEmailMode(context.Background(), store, testStateTeamID, testStateUserID, "Admin+Setup@Example.COM", SetupModeRotate, now)
	if err != nil {
		t.Fatalf("MintStoredStateWithEmailMode: %v", err)
	}
	if len(handle) != stateHandleEncodedLen {
		t.Fatalf("opaque handle len = %d, want %d-char base64url state handle", len(handle), stateHandleEncodedLen)
	}
	for _, leaked := range []string{testStateTeamID, testStateUserID, testNormalizedSetupEmail, "rotate"} {
		if handle == leaked || len(leaked) > 4 && strings.Contains(handle, leaked) {
			t.Fatalf("opaque handle leaked payload %q in %q", leaked, handle)
		}
	}
	got, err := store.StartState(context.Background(), handle, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("StartState: %v", err)
	}
	if got.Email != testNormalizedSetupEmail || got.Mode != SetupModeRotate {
		t.Fatalf("stored payload mismatch: %+v", got)
	}
	assertStateHasNonceAndVerifier(t, got.Nonce, got.CodeVerifier)
}

func TestMintStoredStateExpiresAfterOneHour(t *testing.T) {
	now := time.Unix(1700000000, 0)
	store := newMemoryStateStore()
	handle, err := MintStoredStateWithEmailMode(context.Background(), store, testStateTeamID, testStateUserID, "", SetupModeReuse, now)
	if err != nil {
		t.Fatalf("MintStoredStateWithEmailMode: %v", err)
	}
	store.mu.Lock()
	state := store.items[handle]
	store.mu.Unlock()
	if got := state.ExpiresAt.Sub(now); got != time.Hour {
		t.Fatalf("stored state lifetime = %s, want 1h", got)
	}
}
