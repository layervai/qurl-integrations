package auth

import (
	"errors"
	"fmt"
	"testing"
)

func TestSlackGridFallbackAllowed(t *testing.T) {
	for _, tc := range []struct {
		name, team, org string
		err             error
		want            bool
	}{
		{"missing workspace token", "T1", "E1", ErrSlackBotTokenNotConfigured, true},
		{"wrapped missing token", "T1", "E1", fmt.Errorf("lookup: %w", ErrSlackBotTokenNotConfigured), true},
		{"successful workspace lookup", "T1", "E1", nil, false},
		{"storage failure", "T1", "E1", errors.New("storage unavailable"), false},
		{"Slack rejection", "T1", "E1", errors.New("invalid_auth"), false},
		{"no org install", "T1", "", ErrSlackBotTokenNotConfigured, false},
		{"same owner", "T1", "T1", ErrSlackBotTokenNotConfigured, false},
		{"missing workspace with org", "", "E1", ErrSlackBotTokenNotConfigured, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SlackGridFallbackAllowed(tc.team, tc.org, tc.err); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
