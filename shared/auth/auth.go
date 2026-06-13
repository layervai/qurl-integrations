// Package auth provides authentication helpers for qURL integrations.
package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Provider resolves API credentials for a given workspace.
type Provider interface {
	// APIKey returns the qURL API key for the given workspace ID.
	APIKey(ctx context.Context, workspaceID string) (string, error)
	// DeleteAPIKey removes the qURL API key for the given workspace ID.
	DeleteAPIKey(ctx context.Context, workspaceID string) error
}

// EnvProvider reads the API key from an environment variable.
// Suitable for single-workspace deployments and development.
type EnvProvider struct {
	EnvVar string
}

// APIKey returns the API key from the configured environment variable.
func (p EnvProvider) APIKey(_ context.Context, _ string) (string, error) {
	key := os.Getenv(p.EnvVar)
	if key == "" {
		return "", fmt.Errorf("env var %s not set", p.EnvVar)
	}
	return key, nil
}

// DeleteAPIKey cannot mutate an environment-backed API key.
func (p EnvProvider) DeleteAPIKey(_ context.Context, _ string) error {
	if os.Getenv(p.EnvVar) == "" {
		return ErrWorkspaceNotConfigured
	}
	return errors.New("EnvProvider.DeleteAPIKey: deleting environment-backed API keys is unsupported")
}
