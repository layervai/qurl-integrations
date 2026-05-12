package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/shared/auth"
)

// noopVerifier replaces oauth.NewJWKSVerifier in tests so buildOAuthConfig
// doesn't hit the real internet trying to prime example.auth0.com's JWKS.
type noopVerifier struct{}

func (noopVerifier) VerifyEmail(_ context.Context, _ string) (string, error) {
	return "", errors.New("noopVerifier: unused in env-var tests")
}

// newFakeProvider builds the minimum-viable DDBProvider buildOAuthConfig
// will accept. The test only inspects the (cfg, ok) return — no DDB or
// KMS calls are made through the returned provider.
func newFakeProvider() *auth.DDBProvider {
	return &auth.DDBProvider{}
}

const validStateSecret = "0123456789abcdef0123456789abcdef" // 32 bytes; matches minStateSecretBytes.

var oauthEnvKeys = []string{
	"AUTH0_DOMAIN", "AUTH0_CLIENT_ID", "AUTH0_CLIENT_SECRET", "AUTH0_AUDIENCE",
	"SLACK_BASE_URL", "OAUTH_STATE_SECRET", "QURL_ENDPOINT",
}

func validEnv() map[string]string {
	return map[string]string{
		"AUTH0_DOMAIN":        "example.auth0.com",
		"AUTH0_CLIENT_ID":     "client-id",
		"AUTH0_CLIENT_SECRET": "client-secret",
		"AUTH0_AUDIENCE":      "aud",
		"SLACK_BASE_URL":      "https://slack-bot.example",
		"OAUTH_STATE_SECRET":  validStateSecret,
		"QURL_ENDPOINT":       "https://api.qurl.invalid",
	}
}

// applyEnv writes every oauthEnvKeys entry — empty when absent from kvs
// — so the test doesn't depend on what was inherited from the shell.
// t.Setenv handles per-test cleanup.
func applyEnv(t *testing.T, kvs map[string]string) {
	t.Helper()
	for _, k := range oauthEnvKeys {
		t.Setenv(k, kvs[k])
	}
}

// stubJWKSVerifier swaps newJWKSVerifier for a noop so the env-var tests
// stay hermetic. Returns a t.Cleanup-restored seam.
func stubJWKSVerifier(t *testing.T) {
	t.Helper()
	prev := newJWKSVerifier
	newJWKSVerifier = func(_ context.Context, _, _ string) (oauth.IDTokenVerifier, error) {
		return noopVerifier{}, nil
	}
	t.Cleanup(func() { newJWKSVerifier = prev })
}

func TestBuildOAuthConfigHappyPath(t *testing.T) {
	stubJWKSVerifier(t)
	applyEnv(t, validEnv())
	cfg, ok := buildOAuthConfig(context.Background(), newFakeProvider())
	if !ok {
		t.Fatalf("expected ok=true with all env vars set; cfg=%+v", cfg)
	}
	if cfg.Auth0Domain != "example.auth0.com" {
		t.Errorf("Auth0Domain: got %q", cfg.Auth0Domain)
	}
	if string(cfg.OAuthStateSecret) != validStateSecret {
		t.Errorf("OAuthStateSecret not threaded through")
	}
	if cfg.IDTokenVerifier == nil {
		t.Error("IDTokenVerifier should be wired when the stubbed factory returns nil err")
	}
}

func TestBuildOAuthConfigMissingVar(t *testing.T) {
	stubJWKSVerifier(t)
	for _, missing := range oauthEnvKeys {
		t.Run("missing="+missing, func(t *testing.T) {
			env := validEnv()
			delete(env, missing)
			applyEnv(t, env)
			_, ok := buildOAuthConfig(context.Background(), newFakeProvider())
			if ok {
				t.Errorf("expected ok=false when %s is missing", missing)
			}
		})
	}
}

func TestBuildOAuthConfigShortSecret(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env["OAUTH_STATE_SECRET"] = strings.Repeat("a", 16) // half of the required minimum
	applyEnv(t, env)
	_, ok := buildOAuthConfig(context.Background(), newFakeProvider())
	if ok {
		t.Error("expected ok=false on short OAUTH_STATE_SECRET")
	}
}
