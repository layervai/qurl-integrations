// Package auth provides authentication helpers for qURL integrations.
package auth

import (
	"context"
	"fmt"
	"os"
)

// Provider resolves API credentials for a given workspace.
type Provider interface {
	// APIKey returns the qURL API key for the given workspace ID.
	APIKey(ctx context.Context, workspaceID string) (string, error)
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
