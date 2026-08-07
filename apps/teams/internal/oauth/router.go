package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
)

// OAuth route paths and handler defaults for the Teams setup flow.
const (
	StartPath           = "/oauth/qurl/start"
	callbackPath        = "/oauth/qurl/callback"
	oauthHandlerTimeout = 60 * time.Second
)

func apiKeyScopes() []string {
	return []string{"qurl:read", "qurl:write"}
}

func callbackURL(baseURL string) string {
	if u, err := url.JoinPath(baseURL, callbackPath); err == nil {
		return u
	}
	return strings.TrimRight(baseURL, "/") + callbackPath
}

// SetupConfig contains the Teams-side configuration for minting setup links.
type SetupConfig struct {
	StateSecret  []byte
	TeamsBaseURL string
}

// SetupURL returns the Teams setup entrypoint URL for the supplied signed state.
func (s SetupConfig) SetupURL(state string) string {
	u, err := url.JoinPath(s.TeamsBaseURL, StartPath)
	if err != nil {
		u = strings.TrimRight(s.TeamsBaseURL, "/") + StartPath
	}
	return u + "?state=" + url.QueryEscape(state)
}

// WorkspaceAPIKeyMint describes a provisioned qURL workspace API key.
type WorkspaceAPIKeyMint struct {
	APIKey        string
	KeyID         string
	KeyPrefix     string
	BindingBacked bool
}

// QURLAPIKeyMinter provisions, validates, and revokes workspace-scoped qURL API keys.
type QURLAPIKeyMinter interface {
	ValidateAPIKey(ctx context.Context, apiKey string) error
	MintWorkspaceAPIKey(ctx context.Context, accessToken, tenantID string) (WorkspaceAPIKeyMint, error)
	MintWorkspaceReplacementAPIKey(ctx context.Context, accessToken, tenantID, oldKeyID string) (WorkspaceAPIKeyMint, error)
	RevokeAPIKey(ctx context.Context, accessToken, keyID string) error
	APIKeyRevoked(ctx context.Context, accessToken, keyID string) (bool, error)
}

// IDTokenVerifier verifies Auth0 id_tokens and extracts stable caller identity claims.
type IDTokenVerifier interface {
	VerifyEmail(ctx context.Context, idToken string) (email string, err error)
	VerifySub(ctx context.Context, idToken string) (sub string, err error)
}

// WorkspaceMapping records the Teams tenant owner bound during setup.
type WorkspaceMapping struct {
	TenantID  string
	OwnerID   string
	CreatedAt time.Time
}

// AdminStore persists the owner binding created during Teams setup.
type AdminStore interface {
	BindWorkspace(ctx context.Context, m *WorkspaceMapping, seedAdmin string) error
}

// BindConflictCode classifies owner-binding conflicts during Teams setup.
type BindConflictCode string

// BindConflictCode values returned by the Teams OAuth bind flow.
const (
	// BindConflictAlreadyBoundToCaller reports that the caller already owns the tenant binding.
	BindConflictAlreadyBoundToCaller BindConflictCode = "workspace_already_bound_to_caller"
	// BindConflictAlreadyBound reports that a different caller already owns the tenant binding.
	BindConflictAlreadyBound BindConflictCode = "workspace_already_bound"
)

// WorkspaceStore reads and writes the Teams tenant's stored qURL API key state.
type WorkspaceStore interface {
	APIKey(ctx context.Context, workspaceID string) (string, error)
	APIKeyID(ctx context.Context, workspaceID string) (keyID string, err error)
	APIKeyIdentity(ctx context.Context, workspaceID string) (keyID, qurlAccountID string, err error)
	SetAPIKeyWithMetadata(ctx context.Context, workspaceID, apiKey, keyID, keyPrefix, qurlAccountID, configuredBy string) error
	DeleteAPIKey(ctx context.Context, workspaceID string) error
}

var _ WorkspaceStore = (*auth.DDBProvider)(nil)

// Config wires the Teams OAuth handlers to Auth0, qurl-service, and local storage.
type Config struct {
	Auth0Domain                   string
	Auth0ClientID                 string
	Auth0ClientSecret             string
	Auth0Audience                 string
	Auth0EmailConnection          string
	TeamsBaseURL                  string
	SetupBindingReplayWindowHours int
	APIKeyMintReplayWindowHours   int
	OAuthStateSecret              []byte
	Provider                      WorkspaceStore
	IDTokenVerifier               IDTokenVerifier
	Minter                        QURLAPIKeyMinter
	AdminStore                    AdminStore
	BindClassifyError             func(err error) BindConflictCode
	HTTPClient                    *http.Client
	Now                           func() time.Time
}

func (c *Config) now() func() time.Time {
	if c.Now != nil {
		return c.Now
	}
	return time.Now
}

func replayWindowHoursOrDefault(configuredHours, defaultHours int) int {
	if configuredHours > 0 {
		return configuredHours
	}
	return defaultHours
}

// RegisterRoutes installs the Teams OAuth start and callback handlers.
func RegisterRoutes(mux *http.ServeMux, cfg *Config) {
	if err := cfg.Validate(); err != nil {
		panic("oauth.RegisterRoutes: " + err.Error())
	}
	mux.Handle(StartPath, http.TimeoutHandler(Start(cfg), oauthHandlerTimeout, "oauth/start timed out"))
	mux.Handle(callbackPath, http.TimeoutHandler(Callback(cfg), oauthHandlerTimeout, "oauth/callback timed out"))
}

// Validate checks Config for internally inconsistent wiring.
func (c *Config) Validate() error {
	if c.AdminStore != nil && c.BindClassifyError == nil {
		return errors.New("AdminStore wired without BindClassifyError")
	}
	if c.SetupBindingReplayWindowHours < 0 {
		return errors.New("SetupBindingReplayWindowHours must be zero or positive")
	}
	if c.APIKeyMintReplayWindowHours < 0 {
		return errors.New("APIKeyMintReplayWindowHours must be zero or positive")
	}
	return nil
}

func authorizeURL(cfg *Config, state string, verified VerifiedState) string {
	u := url.URL{
		Scheme: "https",
		Host:   cfg.Auth0Domain,
		Path:   "/authorize",
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", cfg.Auth0ClientID)
	q.Set("audience", cfg.Auth0Audience)
	q.Set("scope", strings.Join(apiKeyScopes(), " ")+" openid email")
	q.Set("redirect_uri", callbackURL(cfg.TeamsBaseURL))
	q.Set("state", state)
	q.Set("prompt", "consent")
	if verified.Email != "" {
		if connection := strings.TrimSpace(cfg.Auth0EmailConnection); connection != "" {
			q.Set("connection", connection)
		}
		q.Set("login_hint", verified.Email)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func teamsWorkspaceID(tenantID string) string {
	return "teams:" + strings.TrimSpace(tenantID)
}
