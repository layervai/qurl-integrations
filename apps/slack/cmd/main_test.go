package main

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
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
const defaultSlackBotScopesCSV = "commands,chat:write"

var oauthEnvKeys = []string{
	"AUTH0_DOMAIN", "AUTH0_CLIENT_ID", "AUTH0_CLIENT_SECRET", "AUTH0_AUDIENCE",
	"SLACK_BASE_URL", "OAUTH_STATE_SECRET", "QURL_ENDPOINT",
}

var slackInstallEnvKeys = []string{
	envSlackClientID, envSlackClientSecret, "SLACK_BASE_URL",
	envSlackInstallStateSecret, "OAUTH_STATE_SECRET", envSlackBotScopes,
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

func validSlackInstallEnv() map[string]string {
	return map[string]string{
		envSlackClientID:           "111.222",
		envSlackClientSecret:       "slack-secret",
		"SLACK_BASE_URL":           "https://slack-bot.example",
		envSlackInstallStateSecret: validStateSecret,
		"OAUTH_STATE_SECRET":       "",
		envSlackBotScopes:          "",
	}
}

func TestValidateSlackBotToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{name: "unset"},
		{name: "bot token", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxb-")+10)},
		{name: "rotating bot token", token: "xoxe.xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxe.xoxb-"))},
		{name: "rotating refresh token", token: "xoxe-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxe-")), wantErr: true},
		{name: "user token", token: "xoxp-test-token", wantErr: true},
		{name: "app token", token: "xapp-test-token", wantErr: true},
		{name: "placeholder bot token", token: "xoxb-", wantErr: true},
		{name: "minimum typo-guard length bot token", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxb-"))},
		{name: "one below typo-guard length", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxb-")-1), wantErr: true},
		{name: "token with whitespace", token: "xoxb-test-token\r", wantErr: true},
		{name: "token with non-ascii", token: "xoxb-test-tokené", wantErr: true},
		{name: "token with underscore", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxb-")) + "_ok"},
		{name: "token with dot", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMin-len("xoxb-")) + ".ok"},
		{name: "long bot token", token: "xoxb-" + strings.Repeat("a", 250)},
		{name: "maximum typo-guard length bot token", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMax-len("xoxb-"))},
		{name: "one above typo-guard length", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMax-len("xoxb-")+1), wantErr: true},
		{name: "token too long", token: "xoxb-" + strings.Repeat("a", auth.SlackBotTokenTypoGuardMax-len("xoxb-")+100), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := auth.ValidateSlackBotTokenShape(tc.token)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateSlackBotTokenShape(%q) err=%v, wantErr=%v", tc.token, err, tc.wantErr)
			}
		})
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
	t.Setenv("AUTH0_EMAIL_CONNECTION", kvs["AUTH0_EMAIL_CONNECTION"])
}

func applySlackInstallEnv(t *testing.T, kvs map[string]string) {
	t.Helper()
	for _, k := range slackInstallEnvKeys {
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
	if cfg.Auth0EmailConnection != "" {
		t.Errorf("Auth0EmailConnection: got %q want empty by default", cfg.Auth0EmailConnection)
	}
	if string(cfg.OAuthStateSecret) != validStateSecret {
		t.Errorf("OAuthStateSecret not threaded through")
	}
	if cfg.IDTokenVerifier == nil {
		t.Error("IDTokenVerifier should be wired when the stubbed factory returns nil err")
	}
}

func TestBuildOAuthConfigEmailConnectionOverride(t *testing.T) {
	stubJWKSVerifier(t)
	env := validEnv()
	env["AUTH0_EMAIL_CONNECTION"] = "Username-Password-Authentication"
	applyEnv(t, env)
	cfg, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true with all required env vars set")
	}
	if cfg.Auth0EmailConnection != "Username-Password-Authentication" {
		t.Errorf("Auth0EmailConnection: got %q want override", cfg.Auth0EmailConnection)
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

// TestBuildOAuthConfigFailsFastOnJWKSWhenAdminStoreWired fences the
// mismatched-degradation guard: when the JWKS prime fails AND AdminStore
// is wired, every callback would reject the install (no sub → no
// OwnerID → no bind → 500). Catching that at boot beats catching it
// after the first user hits /qurl setup. The sandbox path (no
// AdminStore) is the inverse — falls through to a warn + nil
// verifier so the API-key surface keeps working.
func TestBuildOAuthConfigFailsFastOnJWKSWhenAdminStoreWired(t *testing.T) {
	prev := newJWKSVerifier
	newJWKSVerifier = func(_ context.Context, _, _ string) (oauth.IDTokenVerifier, error) {
		return nil, errors.New("simulated JWKS prime failure")
	}
	t.Cleanup(func() { newJWKSVerifier = prev })

	applyEnv(t, validEnv())

	t.Run("admin store wired — must fail-fast", func(t *testing.T) {
		_, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, &fakeAdminStore{})
		if ok {
			t.Error("expected ok=false when JWKS prime fails with AdminStore wired (every callback would 500)")
		}
		if err == nil || !strings.Contains(err.Error(), "JWKS") {
			t.Errorf("expected fail-fast error mentioning JWKS, got %v", err)
		}
	})

	t.Run("admin store nil — must warn-and-continue", func(t *testing.T) {
		cfg, ok, err := buildOAuthConfig(context.Background(), newFakeProvider(), nil, nil)
		if err != nil {
			t.Fatalf("expected nil err on sandbox path, got %v", err)
		}
		if !ok {
			t.Fatal("expected ok=true on sandbox path (no AdminStore → bind is skipped, verifier is best-effort)")
		}
		if cfg.IDTokenVerifier != nil {
			t.Error("expected verifier=nil when prime failed; callback gates on this to skip claim extraction")
		}
	})
}

// fakeAdminStore is the cmd-side stand-in for oauth.AdminStore in
// tests that only need a non-nil to flip the JWKS fail-fast branch.
type fakeAdminStore struct{}

func (*fakeAdminStore) BindWorkspace(_ context.Context, _ *oauth.WorkspaceMapping, _ string) error {
	return nil
}

// TestClassifyBindErrorMapping locks the slackdata.Error.Code →
// oauth.BindConflictCode mapping that wires the callback's switch
// arm to slackdata's 409 surface. The reflect-shape fence covers
// the struct; this covers the code-string mapping which is its own
// drift surface — rename slackdata.ErrCodeWorkspaceAlreadyBound and
// the classifier silently falls through to the empty-string "generic
// failure" arm, downgrading rebind-refused to a 500.
func TestClassifyBindErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want oauth.BindConflictCode
	}{
		{
			"already bound to caller (idempotent re-entry)",
			&slackdata.Error{StatusCode: http.StatusConflict, Code: slackdata.ErrCodeWorkspaceAlreadyBoundToCaller},
			oauth.BindConflictAlreadyBoundToCaller,
		},
		{
			"already bound to different admin (rebind-refused)",
			&slackdata.Error{StatusCode: http.StatusConflict, Code: slackdata.ErrCodeWorkspaceAlreadyBound},
			oauth.BindConflictAlreadyBound,
		},
		{
			"bind held but disambig read failed (unverified)",
			&slackdata.Error{StatusCode: http.StatusConflict, Code: slackdata.ErrCodeWorkspaceBindUnverified},
			oauth.BindConflictUnverified,
		},
		{
			"non-409 *slackdata.Error → empty (generic failure)",
			&slackdata.Error{StatusCode: http.StatusServiceUnavailable, Code: "ddb_error"},
			"",
		},
		{
			"409 with unknown Code → empty (default arm)",
			&slackdata.Error{StatusCode: http.StatusConflict, Code: "future_unmapped_code"},
			"",
		},
		{
			"non-*slackdata.Error → empty",
			errors.New("plain string error"),
			"",
		},
		{"nil → empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyBindError(c.err); got != c.want {
				t.Errorf("classifyBindError(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// TestAdminStoreAdapterForwardsAllFields exercises the production
// adminStoreAdapter against a captor that satisfies slackdataBinder,
// with a non-zero CreatedAt. The reflect-shape test fences the struct
// field set; this fences the adapter's translation line so a future
// regression that drops one of TeamID / OwnerID / CreatedAt from the
// copy fails here rather than slipping through unnoticed because the
// callback passes zero values today.
func TestAdminStoreAdapterForwardsAllFields(t *testing.T) {
	captured := &capturingSlackdataStore{}
	adapter := &adminStoreAdapter{store: captured}
	want := oauth.WorkspaceMapping{
		TeamID:    "T_capture",
		OwnerID:   "auth0|capture-owner",
		CreatedAt: mustParseTime(t, "2026-05-20T12:34:56Z"),
	}
	if err := adapter.BindWorkspace(context.Background(), &want, "U_seed"); err != nil {
		t.Fatalf("BindWorkspace: %v", err)
	}
	if captured.gotMapping == nil {
		t.Fatal("adapter did not forward to the wrapped store")
	}
	if captured.gotMapping.TeamID != want.TeamID ||
		captured.gotMapping.OwnerID != want.OwnerID ||
		!captured.gotMapping.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("forwarded mapping mismatch:\nwant TeamID=%q OwnerID=%q CreatedAt=%v\ngot  TeamID=%q OwnerID=%q CreatedAt=%v",
			want.TeamID, want.OwnerID, want.CreatedAt,
			captured.gotMapping.TeamID, captured.gotMapping.OwnerID, captured.gotMapping.CreatedAt)
	}
	if captured.gotSeedAdmin != "U_seed" {
		t.Errorf("seedAdmin: got %q want %q", captured.gotSeedAdmin, "U_seed")
	}
}

// capturingSlackdataStore satisfies slackdataBinder so the production
// adminStoreAdapter can be exercised without standing up a real
// slackdata.Store.
type capturingSlackdataStore struct {
	gotMapping   *slackdata.WorkspaceMapping
	gotSeedAdmin string
}

func (c *capturingSlackdataStore) BindWorkspace(_ context.Context, m *slackdata.WorkspaceMapping, seedAdmin string) error {
	c.gotMapping = m
	c.gotSeedAdmin = seedAdmin
	return nil
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// TestAdminStoreAdapterMappingShapesMatch fences the field-for-field
// equivalence of oauth.WorkspaceMapping and slackdata.WorkspaceMapping.
// The adminStoreAdapter copies between the two by named field; a new
// field added to one and not the other would silently drop on the
// adapter's copy. Reflect-walk the field sets so the build breaks
// when they drift.
func TestAdminStoreAdapterMappingShapesMatch(t *testing.T) {
	oauthFields := structFieldSet(reflect.TypeOf(oauth.WorkspaceMapping{}))
	storeFields := structFieldSet(reflect.TypeOf(slackdata.WorkspaceMapping{}))
	if !reflect.DeepEqual(oauthFields, storeFields) {
		t.Errorf("oauth.WorkspaceMapping vs slackdata.WorkspaceMapping fields differ — adminStoreAdapter copy would silently drop the diff\noauth:     %v\nslackdata: %v", oauthFields, storeFields)
	}
}

// structFieldSet returns the {name → full type string} map for a
// struct type. Used to compare field shapes across packages without
// requiring identical declaration order. Type string (not Kind) so
// a future drift like `OwnerID OwnerID` (named-string) vs
// `OwnerID string` fails the test here rather than at the adapter
// build line — the test owns the contract end-to-end.
func structFieldSet(t reflect.Type) map[string]string {
	out := make(map[string]string, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		out[f.Name] = f.Type.String()
	}
	return out
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

func TestBuildSlackInstallConfigHappyPath(t *testing.T) {
	env := validSlackInstallEnv()
	applySlackInstallEnv(t, env)
	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true with Slack install env set")
	}
	if cfg.ClientID != "111.222" || cfg.ClientSecret != "slack-secret" {
		t.Fatalf("Slack client config not threaded through: %+v", cfg)
	}
	if cfg.SlackBaseURL != "https://slack-bot.example" {
		t.Fatalf("SlackBaseURL = %q", cfg.SlackBaseURL)
	}
	if string(cfg.StateSecret) != validStateSecret {
		t.Fatalf("StateSecret not threaded through")
	}
	if strings.Join(cfg.BotScopes, ",") != defaultSlackBotScopesCSV {
		t.Fatalf("default bot scopes = %v", cfg.BotScopes)
	}
	if cfg.TokenStore == nil {
		t.Fatal("TokenStore should be wired")
	}
}

func TestBuildSlackInstallConfigFallsBackToOAuthStateSecret(t *testing.T) {
	env := validSlackInstallEnv()
	env[envSlackInstallStateSecret] = ""
	env["OAUTH_STATE_SECRET"] = validStateSecret
	applySlackInstallEnv(t, env)
	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want fallback to OAUTH_STATE_SECRET", ok, err)
	}
	if string(cfg.StateSecret) != validStateSecret {
		t.Fatalf("StateSecret not sourced from OAUTH_STATE_SECRET")
	}
}

func TestBuildSlackInstallConfigMissingVar(t *testing.T) {
	for _, missing := range []string{envSlackClientID, envSlackClientSecret, "SLACK_BASE_URL", envSlackInstallStateSecret} {
		t.Run("missing="+missing, func(t *testing.T) {
			env := validSlackInstallEnv()
			delete(env, missing)
			if missing == envSlackInstallStateSecret {
				env["OAUTH_STATE_SECRET"] = ""
			}
			applySlackInstallEnv(t, env)
			_, ok, err := buildSlackInstallConfig(newFakeProvider())
			if err != nil {
				t.Fatalf("expected nil error on missing var, got %v", err)
			}
			if ok {
				t.Fatalf("expected ok=false when %s is missing", missing)
			}
		})
	}
}

func TestBuildSlackInstallConfigCustomScopes(t *testing.T) {
	env := validSlackInstallEnv()
	env[envSlackBotScopes] = "commands,channels:read"
	applySlackInstallEnv(t, env)
	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if strings.Join(cfg.BotScopes, ",") != defaultSlackBotScopesCSV+",channels:read" {
		t.Fatalf("custom scopes = %v", cfg.BotScopes)
	}
}

func TestBuildSlackInstallConfigUnionsRequiredScopesIntoLegacyOverride(t *testing.T) {
	env := validSlackInstallEnv()
	env[envSlackBotScopes] = "commands"
	applySlackInstallEnv(t, env)
	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want legacy override to keep required defaults", ok, err)
	}
	if strings.Join(cfg.BotScopes, ",") != defaultSlackBotScopesCSV {
		t.Fatalf("scopes = %v, want required defaults unioned into legacy override", cfg.BotScopes)
	}
}

// A stale `views:write` in a SLACK_BOT_SCOPES override is stripped at config
// load (see slackinstall.DropUnsupportedScopes), so the install flow keeps
// working off the valid scopes instead of breaking every install with
// invalid_scope or aborting startup. Mixed case confirms the wiring uses the
// case-insensitive helper; the drop decision itself is unit-tested in the
// slackinstall package.
func TestBuildSlackInstallConfigStripsViewsWriteOverride(t *testing.T) {
	env := validSlackInstallEnv()
	env[envSlackBotScopes] = "commands,Views:Write"
	applySlackInstallEnv(t, env)
	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want config load to succeed", ok, err)
	}
	if strings.Join(cfg.BotScopes, ",") != defaultSlackBotScopesCSV {
		t.Fatalf("scopes = %v, want views:write stripped", cfg.BotScopes)
	}
}

// If a SLACK_BOT_SCOPES override strips to nothing (only views:write), config
// load keeps the required defaults rather than aborting startup.
func TestBuildSlackInstallConfigUsesDefaultsWhenOverrideStripsToEmpty(t *testing.T) {
	env := validSlackInstallEnv()
	env[envSlackBotScopes] = "views:write"
	applySlackInstallEnv(t, env)
	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v, want config load to succeed with defaults", ok, err)
	}
	if strings.Join(cfg.BotScopes, ",") != defaultSlackBotScopesCSV {
		t.Fatalf("scopes = %v, want required defaults", cfg.BotScopes)
	}
}

func TestSlackInstallConfigRejectsBadBaseURL(t *testing.T) {
	env := validSlackInstallEnv()
	env["SLACK_BASE_URL"] = "http://slack-bot.example"
	applySlackInstallEnv(t, env)
	_, ok, err := buildSlackInstallConfig(newFakeProvider())
	if ok {
		t.Fatal("ok=true, want bad Slack install base URL to fail at config build")
	}
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("err=%v, want https error", err)
	}
}
