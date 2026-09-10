package internal

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
)

type testOAuthStateStore struct {
	mu      sync.Mutex
	items   map[string]oauth.StoredState
	started map[string]bool
}

func newTestOAuthSetupConfig() oauth.SetupConfig {
	return oauth.SetupConfig{
		SlackBaseURL: "https://slack-bot.example",
		StateStore: &testOAuthStateStore{
			items:   map[string]oauth.StoredState{},
			started: map[string]bool{},
		},
	}
}

func (s *testOAuthStateStore) PutState(_ context.Context, handle string, state oauth.StoredState) error { //nolint:gocritic // test fake mirrors the StateStore value signature.
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[handle]; exists {
		return errors.New("duplicate OAuth state handle")
	}
	s.items[handle] = state
	return nil
}

func (s *testOAuthStateStore) StartState(_ context.Context, handle string, now time.Time) (oauth.VerifiedState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.items[handle]
	if !ok || !now.Before(state.ExpiresAt) {
		return oauth.VerifiedState{}, errors.New("OAuth state unavailable")
	}
	s.started[handle] = true
	return state.VerifiedState, nil
}

func (s *testOAuthStateStore) ConsumeState(_ context.Context, handle string, now time.Time) (oauth.VerifiedState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.items[handle]
	if !ok || !s.started[handle] || !now.Before(state.ExpiresAt) {
		return oauth.VerifiedState{}, errors.New("OAuth state unavailable")
	}
	delete(s.items, handle)
	return state.VerifiedState, nil
}
