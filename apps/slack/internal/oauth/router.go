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
// The Slack workspace install side is handled by apps/slack/internal/slackinstall's
// /oauth/slack/install routes. This package owns the qURL account connection
// only: the API key it stores is the qURL key the bot uses to call qurl-service
// on the workspace's behalf.
package oauth

import (
	"context"
	"errors"
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
// and returns the claims the callback consumes.
//
// Two split methods rather than one combined return so the existing
// success-page email path (best-effort, suppress-on-error) and the
// new BindWorkspace OwnerID path (mandatory, fail-the-bind-on-error)
// can branch independently.
//
//   - VerifyEmail returns the email claim. Returns ("", err) on
//     verify-failure or ("", nil) when the claim is missing /
//     email_verified is false — the success page renders without the
//     email line in either case.
//
//   - VerifySub returns the Auth0 `sub` claim used as the workspace
//     OwnerID in BindWorkspace. Returns ("", err) on verify-failure;
//     callers MUST surface the error (an empty sub can't legitimately
//     bind a workspace).
type IDTokenVerifier interface {
	VerifyEmail(ctx context.Context, idToken string) (email string, err error)
	VerifySub(ctx context.Context, idToken string) (sub string, err error)
}

// WorkspaceMapping is the value BindWorkspace persists. Re-declared
// here (vs importing slackdata) so the oauth package's only inbound
// dependency stays the shared/auth package — slackdata depends on
// oauth's interfaces in cmd/main.go but the reverse would create a
// cycle.
//
// Fields mirror slackdata.WorkspaceMapping exactly; the drift fence
// lives in cmd/main_test.go's TestAdminStoreAdapterMappingShapesMatch.
type WorkspaceMapping struct {
	TeamID    string
	OwnerID   string
	CreatedAt time.Time
}

// AdminStore is the slice of slackdata.Store the callback hits to
// persist the workspace_mappings row that seeds the installer as the
// first admin. Optional — when nil (sandbox / no-DDB deploy) the
// callback skips the bind with a slog.Warn so the API-key surface
// stays functional.
type AdminStore interface {
	BindWorkspace(ctx context.Context, m *WorkspaceMapping, seedAdmin string) error
}

// BindConflictCode names the slackdata.Error.Code values BindWorkspace
// surfaces on its 409 paths. The callback branches on these via
// [Config.BindClassifyError] so this package doesn't have to import
// slackdata.
//
// Values intentionally mirror slackdata.ErrCodeWorkspace* constants
// verbatim — drift either side and the classifier wiring in
// cmd/main.go silently routes the wrong 409 to the success-page
// rebind-refusal branch.
type BindConflictCode string

const (
	// BindConflictAlreadyBoundToCaller mirrors
	// slackdata.ErrCodeWorkspaceAlreadyBoundToCaller. The same Slack
	// user re-running /qurl setup against a workspace they already
	// admin — idempotent success on the callback side.
	BindConflictAlreadyBoundToCaller BindConflictCode = "workspace_already_bound_to_caller"
	// BindConflictAlreadyBound mirrors slackdata.ErrCodeWorkspaceAlreadyBound.
	// A different admin holds the workspace — the callback renders a
	// rebind-refused page rather than silently overwriting.
	BindConflictAlreadyBound BindConflictCode = "workspace_already_bound"
	// BindConflictUnverified mirrors slackdata.ErrCodeWorkspaceBindUnverified.
	// Bind is held but the disambiguation read failed — treated as
	// rebind-refused, with operator-actionable copy.
	BindConflictUnverified BindConflictCode = "workspace_bind_unverified"
)

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

	// AdminStore persists the workspace_mappings row that seeds the
	// installer as the workspace's first admin. The callback calls
	// BindWorkspace after the qurl-service key mint + DDB persist
	// succeed, so /qurl-admin admin verbs work immediately without a
	// second /qurl-admin admin claim step.
	//
	// Nil disables the bind (sandbox / no-DDB deploy) — the callback
	// emits a slog.Warn and continues with the existing API-key
	// surface. Production cmd/main.go wires a *slackdata.Store.
	AdminStore AdminStore

	// BindClassifyError classifies a BindWorkspace error into a
	// 409 rebind-conflict code (when the error came from the
	// already-bound branch) or "" when the error is a transport /
	// validation failure that the callback should treat as a
	// generic bind failure (500).
	//
	// Wired in cmd/main.go to a small classifier that errors.As's
	// the *slackdata.Error and returns its Code field when
	// StatusCode == 409. Nil falls back to "always treat as
	// generic bind failure".
	//
	// COUPLING: callers that set AdminStore MUST also set
	// BindClassifyError. Otherwise every bind conflict — including
	// idempotent same-caller re-entries — falls through to the
	// default 500 arm in handleBindError, downgrading rebind-refused
	// to a generic failure for the user. cmd/main.go wires both
	// together; future callers should mirror that pairing.
	BindClassifyError func(err error) BindConflictCode

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
// Panics if Config fails its Validate() check. RegisterRoutes is a
// boot-time call so panic is the right escalation — a misconfigured
// Config can't possibly serve a coherent /oauth/qurl/callback, and
// proceeding would surface as silent rebind-refused → 500 on every
// install attempt instead of a clear startup failure.
//
//nolint:gocritic // hugeParam: Config is value-passed at startup once; pointer churn here isn't worth the API surface friction.
func RegisterRoutes(mux *http.ServeMux, cfg Config) {
	if err := cfg.Validate(); err != nil {
		panic("oauth.RegisterRoutes: " + err.Error())
	}
	mux.Handle(StartPath, http.TimeoutHandler(
		Start(cfg), oauthHandlerTimeout, "oauth/start timed out"))
	mux.Handle(callbackPath, http.TimeoutHandler(
		Callback(cfg), oauthHandlerTimeout, "oauth/callback timed out"))
}

// Validate checks the cross-field invariants that the callback's
// branching depends on. Returns nil when the Config is safe to wire.
// Callers should run this before RegisterRoutes (which calls it
// internally and panics on failure).
//
//nolint:gocritic // hugeParam: see Callback — Config is value-passed.
func (c Config) Validate() error {
	// AdminStore wired without BindClassifyError would route every
	// bind error — including the idempotent same-caller case — to
	// handleBindError's default 500 arm. Same-caller rotation
	// surfaces as a generic failure instead of "key rotated, admin
	// set unchanged." Callers MUST pair the two.
	if c.AdminStore != nil && c.BindClassifyError == nil {
		return errors.New("AdminStore wired without BindClassifyError — same-caller idempotent re-entries would silently surface as 500")
	}
	return nil
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
