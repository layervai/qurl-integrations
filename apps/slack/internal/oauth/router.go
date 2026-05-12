// Package oauth implements per-workspace Slack OAuth handlers
// (/oauth/qurl/start and /oauth/qurl/callback).
//
// The flow mirrors the LIVE Discord flow at
// apps/discord/src/routes/qurl-oauth.js — admin runs /qurl setup, the
// slash-command handler (signature-verified by Slack) mints a signed
// state token and replies with a link to /start. /start verifies the
// state token, sets a double-submit CSRF cookie and 302s to Auth0.
// Auth0 redirects to /callback with `code` + `state`. /callback exchanges
// the code for an access_token, verifies the id_token against Auth0's
// JWKS, mints a workspace-scoped qURL API key via POST /v1/api-keys,
// persists it via DDBProvider, and DMs the admin.
//
// The Slack handler is HTTP-only — no Slack-app-distribution OAuth (the
// Slack workspace install side) is handled here; the API key it stores
// is the qURL key the bot uses to call qurl-service on the workspace's
// behalf.
package oauth

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
)

// Route paths exposed by RegisterRoutes. StartPath is exported so the
// /qurl setup slash-command handler in package internal builds the
// setup-link URL from the same constant the mux registers — drift
// between the two would silently 404 every setup attempt.
const (
	StartPath    = "/oauth/qurl/start"
	callbackPath = "/oauth/qurl/callback"
)

// oauthHandlerTimeout caps the per-request budget for the OAuth surface.
// The Slack bot's parent http.Server runs with WriteTimeout=15s, which
// is appropriate for the /slack/* surface (slash-command handlers ack
// in <1s) but too tight for /oauth/qurl/callback whose worst-case sum
// of upstream calls (Auth0 token exchange + qurl-service mint + KMS
// GenerateDataKey + DDB PutItem) approaches 30s. http.TimeoutHandler
// gives the OAuth routes a per-handler ceiling without bumping the
// server-wide write timeout (which would mask hung /slack/* requests).
const oauthHandlerTimeout = 60 * time.Second

// apiKeyScopes is the qurl-service scope set the callback requests for
// the workspace API key. Returned fresh on each call so an in-package
// caller can't mutate the slice and silently change every future mint.
// authorizeURL also weaves "openid email" in for the id_token email
// claim consumed by the success page.
func apiKeyScopes() []string {
	return []string{"qurl:read", "qurl:write"}
}

// callbackURL composes the Auth0 redirect_uri. SlackBaseURL is tolerated
// with or without a trailing slash via url.JoinPath. Falling back to
// plain concat keeps drift between this and SetupURL impossible.
// buildOAuthConfig already validates the input is https://-prefixed and
// trailing-slash-stripped, so the error branch is unreachable in
// practice; the slog.Warn fires only if a future caller bypasses the
// validation.
func callbackURL(slackBaseURL string) string {
	if u, err := url.JoinPath(slackBaseURL, callbackPath); err == nil {
		return u
	}
	// Log only the host so an accidental "https://user:pass@..."
	// configuration doesn't surface credentials in operator logs.
	host := slackBaseURL
	if u, err := url.Parse(slackBaseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	//nolint:gosec // G706: slog escapes control bytes in attribute values, same posture as the request-path slog sites.
	slog.Warn("callbackURL: url.JoinPath failed — falling back to concat",
		"slack_base_host", host)
	return slackBaseURL + callbackPath
}

// SetupConfig is the slice of runtime configuration the /qurl setup
// slash-command handler needs to mint a state token and build the link
// to /start. Carrying its own struct (vs accepting Config) keeps the
// slash-command surface decoupled from the OAuth-handler surface — a
// future addition like SetupLinkTTL only changes one signature.
type SetupConfig struct {
	StateSecret  []byte
	SlackBaseURL string
}

// SetupURL builds the /qurl setup link from the supplied state token.
// The path is owned by package oauth (StartPath) so handlers in other
// packages don't drift on it. SlackBaseURL is tolerated with or without
// a trailing slash — url.JoinPath collapses the duplicate.
func (s SetupConfig) SetupURL(state string) string {
	u, err := url.JoinPath(s.SlackBaseURL, StartPath)
	if err != nil {
		slog.Warn("SetupURL: url.JoinPath failed — falling back to concat",
			"error", err, "slack_base_url", s.SlackBaseURL)
		u = s.SlackBaseURL + StartPath
	}
	return u + "?state=" + url.QueryEscape(state)
}

// SlackClient is the slice of slack-API surface the callback uses to DM
// the admin after a successful key mint. Interface so tests don't need
// a live Slack token.
type SlackClient interface {
	PostDirectMessage(ctx context.Context, userID, text string) error
}

// QURLAPIKeyMinter is the slice of qurl-service the callback hits to
// mint the workspace-scoped key. Interface for the same testability
// reason as SlackClient.
type QURLAPIKeyMinter interface {
	MintAPIKey(ctx context.Context, accessToken, name string, scopes []string) (apiKey, keyID, keyPrefix string, err error)
	RevokeAPIKey(ctx context.Context, accessToken, keyID string) error
}

// AsyncTracker lets the OAuth callback spawn its fire-and-forget
// goroutines (DM admin, revoke orphan key) under a parent's waitgroup
// so a SIGTERM mid-callback waits for them to drain instead of
// cutting them off mid-call. Wired in production from
// internal.Handler; falls back to plain `go` when nil.
//
// The contract: Go must call fn in a goroutine and complete its bookkeeping
// (waitgroup decrement) when fn returns. Callers may not assume fn runs
// synchronously.
type AsyncTracker interface {
	Go(fn func())
}

// IDTokenVerifier verifies an Auth0 id_token JWT against Auth0's JWKS
// and returns the email claim (the only field we actually consume).
//
// Returns ("", nil) on verify-failure: the success page renders without
// the email line per the Discord pattern (failure-to-verify is non-fatal;
// we never fall back to an unverified decode).
type IDTokenVerifier interface {
	VerifyEmail(ctx context.Context, idToken string) (email string, err error)
}

// Config holds the cross-handler runtime config.
type Config struct {
	// Auth0Domain is the tenant FQDN (no scheme), e.g. "layerv.us.auth0.com".
	Auth0Domain string
	// Auth0ClientID + Auth0ClientSecret are the application credentials
	// for the Slack bot's Auth0 application. The secret only leaves
	// memory in the form-urlencoded token-exchange POST.
	Auth0ClientID     string
	Auth0ClientSecret string
	// Auth0Audience is the resource-server identifier in Auth0 (the
	// qurl-service API), pasted into the `audience` param so the
	// returned access_token actually carries the qurl:* scopes.
	Auth0Audience string

	// SlackBaseURL is the public origin of the Slack bot (e.g.
	// https://slack-bot.example.com). The redirect_uri threaded to
	// Auth0 is SlackBaseURL + "/oauth/qurl/callback".
	SlackBaseURL string

	// OAuthStateSecret is the HMAC-SHA256 key used to mint and verify
	// the `state` token threaded through Auth0. Operator-set; the
	// constructor refuses anything shorter than stateMinSecret.
	OAuthStateSecret []byte

	// Provider is the DDB-backed key store. The callback handler calls
	// SetAPIKey on it after a successful mint.
	Provider WorkspaceStore

	// IDTokenVerifier validates Auth0 id_tokens against JWKS. Tests
	// inject a noop verifier; production wires a JWKSVerifier.
	IDTokenVerifier IDTokenVerifier

	// Minter calls qurl-service /v1/api-keys. Tests inject a fake;
	// production wires HTTPAPIKeyMinter.
	Minter QURLAPIKeyMinter

	// SlackClient sends the success-confirmation DM. Tests can inject
	// a noop. Nil disables the DM (the success page still renders).
	SlackClient SlackClient

	// AsyncTracker scopes the fire-and-forget DM + orphan-key-revoke
	// goroutines under a parent's waitgroup so SIGTERM during a
	// callback waits for them to drain. Nil falls back to plain `go`
	// — fine for tests, leaves a small orphan-key window during
	// production shutdown.
	AsyncTracker AsyncTracker

	// HTTPClient is used for Auth0 token-exchange calls. Defaults to
	// &http.Client{Timeout: 15s}.
	HTTPClient *http.Client

	// Now is injected for test-time clock pinning. Nil → time.Now.
	Now func() time.Time
}

// now returns Now() or time.Now if unset. Centralizing here so handlers
// don't each carry a `now := cfg.Now; if now == nil { now = time.Now }`
// preamble.
//
//nolint:gocritic // hugeParam: Config is value-passed across the package for the API-surface reasons documented in Callback; same posture here.
func (c Config) now() func() time.Time {
	if c.Now != nil {
		return c.Now
	}
	return time.Now
}

// WorkspaceStore is the write-path the callback hits after a successful
// mint. Implemented by *auth.DDBProvider.
type WorkspaceStore interface {
	SetAPIKey(ctx context.Context, workspaceID, apiKey, configuredBy string) error
	DeleteAPIKey(ctx context.Context, workspaceID string) error
}

// Ensure auth.DDBProvider satisfies the WorkspaceStore interface at
// compile time so a refactor on the auth side that drops one of these
// methods breaks here, not at runtime in the callback handler.
var _ WorkspaceStore = (*auth.DDBProvider)(nil)

// RegisterRoutes wires /oauth/qurl/start and /oauth/qurl/callback onto
// the supplied mux. Routes are registered with HandleFunc so the caller
// stays in control of the mux's prefix discipline (the production mux
// in cmd/main.go is the same http.ServeMux that already serves /health
// and /slack/* — no prefix).
//
//nolint:gocritic // hugeParam: Config is value-passed at startup once; pointer churn here isn't worth the API surface friction.
func RegisterRoutes(mux *http.ServeMux, cfg Config) {
	mux.Handle(StartPath, http.TimeoutHandler(
		Start(cfg), oauthHandlerTimeout, "oauth/start timed out"))
	mux.Handle(callbackPath, http.TimeoutHandler(
		Callback(cfg), oauthHandlerTimeout, "oauth/callback timed out"))
}

// authorizeURL composes the Auth0 /authorize redirect target.
//
// prompt=consent matches the Discord rotation contract: even though the
// signed-state-token round-trip already enforces same-user origin
// binding, an admin re-running /qurl setup to rotate keys would
// otherwise hit Auth0's silent-consent shortcut and skip the user-facing
// confirmation. Forcing consent keeps the surface predictable: every
// /qurl setup ends in a fresh Auth0 prompt → new key → new DDB row.
//
//nolint:gocritic // hugeParam: value-passed in line with the rest of the package's posture; see Callback.
func authorizeURL(cfg Config, state string) string {
	u := url.URL{
		Scheme: "https",
		Host:   cfg.Auth0Domain,
		Path:   "/authorize",
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", cfg.Auth0ClientID)
	q.Set("audience", cfg.Auth0Audience)
	// Scope set is symmetric with the Discord flow (qurl-oauth.js):
	// APIKeyScopes for the qurl-service mint, openid + email for the
	// id_token claim used in the success-page binding readout.
	q.Set("scope", strings.Join(apiKeyScopes(), " ")+" openid email")
	q.Set("redirect_uri", callbackURL(cfg.SlackBaseURL))
	q.Set("state", state)
	q.Set("prompt", "consent")
	u.RawQuery = q.Encode()
	return u.String()
}
