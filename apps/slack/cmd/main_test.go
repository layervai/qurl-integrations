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

func (noopVerifier) VerifySub(_ context.Context, _ string) (string, error) {
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
	cfg, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
			_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
			if err != nil {
				t.Errorf("expected nil error on missing var; got %v", err)
			}
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
	_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if ok {
		t.Error("expected ok=false on short OAUTH_STATE_SECRET")
	}
	if !errors.Is(err, errOAuthStateSecretTooShort) {
		t.Errorf("expected errOAuthStateSecretTooShort, got %v", err)
	}
}

// TestBuildOAuthConfigSecretLengthBoundary pins both sides of the
// StateMinSecret floor — one byte less rejects, exactly StateMinSecret
// accepts. A future bump of the constant on one side without the other
// would be caught here.
func TestBuildOAuthConfigSecretLengthBoundary(t *testing.T) {
	stubJWKSVerifier(t)
	t.Run("just_under", func(t *testing.T) {
		env := validEnv()
		env["OAUTH_STATE_SECRET"] = strings.Repeat("a", oauth.StateMinSecret-1)
		applyEnv(t, env)
		_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
		if ok || !errors.Is(err, errOAuthStateSecretTooShort) {
			t.Errorf("ok=%v err=%v — want ok=false + errOAuthStateSecretTooShort at StateMinSecret-1 bytes", ok, err)
		}
	})
	t.Run("exactly_at", func(t *testing.T) {
		env := validEnv()
		env["OAUTH_STATE_SECRET"] = strings.Repeat("a", oauth.StateMinSecret)
		applyEnv(t, env)
		_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
		if !ok || err != nil {
			t.Errorf("ok=%v err=%v — want ok=true at exactly StateMinSecret bytes", ok, err)
		}
	})
}

// TestBuildOAuthConfigRejectsEmptyHostSlackBaseURL locks the contract
// that a parse-valid but host-less URL (e.g. "https://") is rejected.
// Without this, the resulting redirect_uri would be
// "https:///oauth/qurl/callback" — silently broken.
func TestBuildOAuthConfigRejectsEmptyHostSlackBaseURL(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env["SLACK_BASE_URL"] = "https://"
	applyEnv(t, env)
	_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if ok {
		t.Error("expected ok=false on empty-host SLACK_BASE_URL")
	}
	if err == nil {
		t.Error("expected error on empty-host SLACK_BASE_URL")
	}
}

// TestBuildOAuthConfigRejectsNonHTTPSSlackBaseURL locks the Secure-cookie
// contract: a Set-Cookie: Secure is dropped silently by browsers over
// http://, which would break the double-submit check with a misleading
// "setup must be completed in the same browser" error. Fail-fast at
// config load.
func TestBuildOAuthConfigRejectsNonHTTPSSlackBaseURL(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env["SLACK_BASE_URL"] = "http://slack-bot.example"
	applyEnv(t, env)
	_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if ok {
		t.Error("expected ok=false on http:// SLACK_BASE_URL")
	}
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Errorf("expected https:// error, got %v", err)
	}
}

// TestBuildOAuthConfigNormalizesURLEnvVars asserts SLACK_BASE_URL,
// AUTH0_DOMAIN, and QURL_ENDPOINT are normalized at config-load:
//   - trailing slashes are stripped (Auth0 rejects redirect_uri mismatches)
//   - AUTH0_DOMAIN's accidental scheme prefix is stripped (jwks.go
//     composes "https://" + domain, so "https://example.auth0.com" would
//     otherwise yield "https://https://example.auth0.com/...")
func TestBuildOAuthConfigNormalizesURLEnvVars(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env["SLACK_BASE_URL"] = "https://slack-bot.example/"
	env["AUTH0_DOMAIN"] = "https://example.auth0.com/"
	env["QURL_ENDPOINT"] = "https://api.qurl.invalid/"
	applyEnv(t, env)
	cfg, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true with trailing slashes and scheme prefix — config should normalize them")
	}
	if cfg.SlackBaseURL != "https://slack-bot.example" {
		t.Errorf("SlackBaseURL not trimmed: got %q", cfg.SlackBaseURL)
	}
	if cfg.Auth0Domain != "example.auth0.com" {
		t.Errorf("Auth0Domain not normalized (expect scheme + trailing slash stripped): got %q", cfg.Auth0Domain)
	}
}
