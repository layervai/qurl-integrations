package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/observability"
)

type memoryStateStore struct {
	mu                 sync.Mutex
	items              map[string]StoredState
	started            map[string]bool
	consumed           map[string]bool
	putHadDeadline     bool
	startHadDeadline   bool
	consumeHadDeadline bool
}

type unavailableStateStore struct {
	err error
}

func (s *unavailableStateStore) PutState(context.Context, string, StoredState) error { return s.err }

func (s *unavailableStateStore) StartState(context.Context, string, time.Time) (VerifiedState, error) {
	return VerifiedState{}, s.err
}

func (s *unavailableStateStore) ConsumeState(context.Context, string, time.Time) (VerifiedState, error) {
	return VerifiedState{}, s.err
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{
		items:    map[string]StoredState{},
		started:  map[string]bool{},
		consumed: map[string]bool{},
	}
}

func (s *memoryStateStore) PutState(ctx context.Context, handle string, state StoredState) error { //nolint:gocritic // test fake mirrors StateStore's value signature.
	s.mu.Lock()
	defer s.mu.Unlock()
	_, s.putHadDeadline = ctx.Deadline()
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
	store.mu.Lock()
	putHadDeadline := store.putHadDeadline
	store.mu.Unlock()
	if !putHadDeadline {
		t.Fatal("PutState must receive an explicit deadline")
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

func TestMintStoredStatePreservesFiveMinuteExpiry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	store := newMemoryStateStore()
	handle, err := MintStoredStateWithEmailMode(context.Background(), store, testStateTeamID, testStateUserID, "", SetupModeReuse, now)
	if err != nil {
		t.Fatalf("MintStoredStateWithEmailMode: %v", err)
	}
	store.mu.Lock()
	state := store.items[handle]
	store.mu.Unlock()
	if got := state.ExpiresAt.Sub(now); got != 5*time.Minute {
		t.Fatalf("stored state lifetime = %s, want 5m", got)
	}
}

func TestLoadStateWithLegacyFallbackPropagatesAvailabilityError(t *testing.T) {
	now := time.Unix(1700000000, 0)
	legacyState, err := MintState(testSecret, testStateTeamID, testStateUserID, now)
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	storeErr := errors.New("ddb throttled")
	if _, err := loadStateWithLegacyFallback(testSecret, legacyState, now, func() (VerifiedState, error) {
		return VerifiedState{}, storeErr
	}); !errors.Is(err, storeErr) {
		t.Fatalf("loadStateWithLegacyFallback error = %v, want store error", err)
	}
}

func TestStatePutTelemetryHasNoStatePayload(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		err          error
	}{
		{name: "success", result: "success"},
		{name: "error", result: "error", err: errors.New("private-state-error")},
		{name: "timeout", result: "timeout", err: fmt.Errorf("wrapped: %w", context.DeadlineExceeded)},
		{name: "canceled", result: "canceled", err: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(observability.NewRedactingJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			handle, err := MintStoredStateWithEmailMode(context.Background(), &unavailableStateStore{err: tc.err}, "T_PRIVATE", "U_PRIVATE", "private@example.invalid", SetupModeReuse, time.Now())
			if !errors.Is(err, tc.err) {
				t.Fatalf("returned error changed: %v", err)
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if len(record) != 5 || record["msg"] != "oauth state put" || record["result"] != tc.result {
				t.Fatalf("unexpected telemetry fields: %v", record)
			}
			elapsed, ok := record["duration_ms"].(float64)
			if !ok || elapsed < 0 {
				t.Fatalf("invalid elapsed value: %v", record["duration_ms"])
			}
			for _, secret := range []string{"T_PRIVATE", "U_PRIVATE", "private@example.invalid", "private-state-error", handle} {
				if secret != "" && strings.Contains(logs.String(), secret) {
					t.Fatal("telemetry contains state or error data")
				}
			}
		})
	}
}
