// Package oauth implements per-workspace Slack OAuth handlers
// (/oauth/qurl/start and /oauth/qurl/callback).
//
// The flow mirrors the LIVE Discord flow at
// apps/discord/src/routes/qurl-oauth.js — admin clicks a link in the
// /qurl setup ephemeral reply, /start sets a double-submit CSRF cookie
// and 302s to Auth0, Auth0 redirects to /callback with `code` + `state`,
// /callback exchanges the code for an access_token, verifies the
// id_token against Auth0's JWKS, mints a workspace-scoped qURL API key
// via POST /v1/api-keys, persists it via DDBProvider, and DMs the admin.
//
// The Slack handler is HTTP-only — no Slack-app-distribution OAuth (the
// Slack workspace install side) is handled here; the API key it stores
// is the qURL key the bot uses to call qurl-service on the workspace's
// behalf.
package oauth

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
)

// Route paths exposed by RegisterRoutes. Kept here (not callback.go /
// start.go) so a single import lists the public surface, and so the
// redirect_uri assembled in callback.go / authorizeURL stays in lockstep
// with the mux registration below.
const (
	startPath    = "/oauth/qurl/start"
	callbackPath = "/oauth/qurl/callback"
)

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
	// the `state` token threaded through Auth0. Operator-set; if empty
	// the constructor refuses.
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

	// HTTPClient is used for Auth0 token-exchange calls. Defaults to
	// &http.Client{Timeout: 15s}.
	HTTPClient *http.Client

	// Now is injected for test-time clock pinning.
	Now func() time.Time
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
	mux.HandleFunc(startPath, Start(cfg))
	mux.HandleFunc(callbackPath, Callback(cfg))
}

// authorizeURL composes the Auth0 /authorize redirect target.
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
	// qurl:write + qurl:read for the API-key mint, openid + email for
	// the id_token claim used in the success-page binding readout.
	q.Set("scope", "qurl:write qurl:read openid email")
	q.Set("redirect_uri", cfg.SlackBaseURL+callbackPath)
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String()
}
