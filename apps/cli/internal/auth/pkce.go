// Package auth provides authentication helpers for the QURL CLI,
// including the OAuth 2.0 Authorization Code flow with PKCE (RFC 7636).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultDomain is the Auth0 tenant domain for QURL.
	DefaultDomain = "auth.layerv.ai"

	// DefaultAudience is the API audience identifier.
	DefaultAudience = "https://api.layerv.ai"

	maxResponseBytes = 1 << 20 // 1 MiB
	formContentType  = "application/x-www-form-urlencoded"

	callbackPath    = "/callback"
	codeVerifierLen = 32 // 32 random bytes → 43 base64url chars
	stateLen        = 32

	serverShutdownTimeout = 2 * time.Second
)

// DefaultClientID is the Auth0 SPA client ID compiled into the CLI.
// It is a var (not const) so release builds can inject it at link time:
//
//	go build -ldflags "-X github.com/layervai/qurl-integrations/apps/cli/internal/auth.DefaultClientID=<id>"
//
// Override at runtime via the QURL_AUTH0_CLIENT_ID environment variable.
var DefaultClientID = "" //nolint:gochecknoglobals // intentional: ldflags injection point

// TokenResponse contains a successful OAuth token response.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// OAuthError represents an error from the OAuth authorization server.
type OAuthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

// Error returns the error message.
func (e *OAuthError) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	return e.Code
}

// PKCEConfig holds Auth0 configuration for the Authorization Code + PKCE flow.
type PKCEConfig struct {
	Domain   string   // Auth0 tenant domain (e.g., "auth.layerv.ai").
	ClientID string   // Auth0 SPA application client ID.
	Audience string   // API audience identifier.
	Scopes   []string // OAuth scopes to request.
	BaseURL  string   // Override base URL for testing (default: "https://{Domain}").
}

// PKCEFlow implements the OAuth 2.0 Authorization Code flow with PKCE (RFC 7636).
type PKCEFlow struct {
	config     PKCEConfig
	httpClient *http.Client
}

// PKCEFlowOption configures a PKCEFlow instance.
type PKCEFlowOption func(*PKCEFlow)

// WithHTTPClient sets a custom HTTP client for the PKCE flow.
func WithHTTPClient(c *http.Client) PKCEFlowOption {
	return func(p *PKCEFlow) { p.httpClient = c }
}

// NewPKCEFlow creates a new Authorization Code + PKCE flow handler.
func NewPKCEFlow(cfg *PKCEConfig, opts ...PKCEFlowOption) *PKCEFlow {
	p := &PKCEFlow{
		config:     *cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *PKCEFlow) authBaseURL() string {
	if p.config.BaseURL != "" {
		return strings.TrimRight(p.config.BaseURL, "/")
	}
	return "https://" + p.config.Domain
}

// callbackResult carries the authorization code or error from the callback handler.
type callbackResult struct {
	code string
	err  error
}

// LoginSession represents an in-progress PKCE login. It holds the authorization
// URL to open in the browser and provides WaitForToken to complete the exchange.
type LoginSession struct {
	// AuthURL is the authorization URL to open in the browser.
	AuthURL string

	// RedirectURI is the 127.0.0.1 callback URL (useful for testing).
	RedirectURI string

	codeCh       chan callbackResult
	server       *http.Server
	codeVerifier string
	state        string
	flow         *PKCEFlow
}

// StartLogin starts a local callback server, generates PKCE parameters,
// and returns a LoginSession with the authorization URL to open in the browser.
func (p *PKCEFlow) StartLogin(ctx context.Context) (*LoginSession, error) {
	verifier, err := generateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("generate code verifier: %w", err)
	}
	challenge := generateCodeChallenge(verifier)

	state, err := generateState()
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	lc := net.ListenConfig{}
	// Bind to 127.0.0.1 explicitly (not "localhost") so the listener and
	// redirect URI use the same address on dual-stack hosts where "localhost"
	// may resolve to ::1 while Auth0's callback targets 127.0.0.1.
	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, errors.New("unexpected listener address type")
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", addr.Port, callbackPath)
	codeCh := make(chan callbackResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, callbackHandler(state, codeCh))

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()

	authURL := p.buildAuthURL(redirectURI, state, challenge)

	return &LoginSession{
		AuthURL:      authURL,
		RedirectURI:  redirectURI,
		codeCh:       codeCh,
		server:       server,
		codeVerifier: verifier,
		state:        state,
		flow:         p,
	}, nil
}

// WaitForToken waits for the user to complete authentication in the browser,
// then exchanges the authorization code for an access token.
func (s *LoginSession) WaitForToken(ctx context.Context) (*TokenResponse, error) {
	defer s.Close()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-s.codeCh:
		if result.err != nil {
			return nil, result.err
		}
		// Use a fresh 30s context for the token exchange so a slow browser flow
		// (ctx nearly expired) doesn't time out the network call itself. Derive
		// from ctx (not Background) so Ctrl-C still cancels the exchange.
		exchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return s.flow.exchangeCode(exchCtx, result.code, s.codeVerifier, s.RedirectURI)
	}
}

// Close shuts down the callback server.
func (s *LoginSession) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
	defer cancel()
	_ = s.server.Shutdown(ctx)
}

// callbackHandler returns an HTTP handler that processes the OAuth callback.
// All sends to codeCh are non-blocking so that a second request (e.g. browser
// refresh, port probe) never stalls the handler goroutine — the first result
// wins and subsequent requests receive an HTTP response without hanging.
//
// The success page is only shown when the result is actually delivered to
// codeCh; duplicate or probe requests get a neutral "already completed" page.
func callbackHandler(expectedState string, codeCh chan<- callbackResult) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(expectedState)) != 1 {
			select {
			case codeCh <- callbackResult{err: errors.New("state mismatch — possible CSRF attack")}:
			default:
			}
			http.Error(w, "State mismatch", http.StatusBadRequest)
			return
		}

		if errCode := q.Get("error"); errCode != "" {
			select {
			case codeCh <- callbackResult{err: &OAuthError{Code: errCode, Description: q.Get("error_description")}}:
			default:
			}
			http.Error(w, "Authentication failed: "+errCode, http.StatusBadRequest)
			return
		}

		code := q.Get("code")
		if code == "" {
			select {
			case codeCh <- callbackResult{err: errors.New("no authorization code in callback")}:
			default:
			}
			http.Error(w, "Missing code", http.StatusBadRequest)
			return
		}

		var delivered bool
		select {
		case codeCh <- callbackResult{code: code}:
			delivered = true
		default:
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if delivered {
			_, _ = fmt.Fprint(w, successHTML)
		} else {
			_, _ = fmt.Fprint(w, alreadyDoneHTML)
		}
	}
}

func (p *PKCEFlow) buildAuthURL(redirectURI, state, codeChallenge string) string {
	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.config.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(p.config.Scopes, " ")},
		"audience":              {p.config.Audience},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return p.authBaseURL() + "/authorize?" + params.Encode()
}

func (p *PKCEFlow) exchangeCode(ctx context.Context, code, codeVerifier, redirectURI string) (*TokenResponse, error) {
	endpoint := p.authBaseURL() + "/oauth/token"

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {p.config.ClientID},
		"code":          {code},
		"code_verifier": {codeVerifier},
		"redirect_uri":  {redirectURI},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", formContentType)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var oauthErr OAuthError
		if jsonErr := json.Unmarshal(body, &oauthErr); jsonErr == nil && oauthErr.Code != "" {
			return nil, &oauthErr
		}
		return nil, fmt.Errorf("token exchange failed (status %d): %s", resp.StatusCode, string(body))
	}

	var token TokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	if token.AccessToken == "" {
		return nil, errors.New("token response missing access_token")
	}
	return &token, nil
}

// --- PKCE utilities ---

func generateCodeVerifier() (string, error) {
	b := make([]byte, codeVerifierLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func generateCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func generateState() (string, error) {
	b := make([]byte, stateLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const successHTML = `<!DOCTYPE html>
<html><head><title>QURL CLI</title></head>
<body style="font-family:system-ui,sans-serif;display:flex;justify-content:center;align-items:center;height:100vh;margin:0;background:#0a0a0a;color:#fafafa;">
<div style="text-align:center;">
<h1>Authentication successful</h1>
<p>You can close this window and return to the terminal.</p>
</div>
</body></html>`

// alreadyDoneHTML is served when a second request arrives at the callback
// endpoint (e.g. browser refresh) after the authorization code has already
// been delivered. It avoids incorrectly showing "Authentication successful"
// for a request that was a no-op.
const alreadyDoneHTML = `<!DOCTYPE html>
<html><head><title>QURL CLI</title></head>
<body style="font-family:system-ui,sans-serif;display:flex;justify-content:center;align-items:center;height:100vh;margin:0;background:#0a0a0a;color:#fafafa;">
<div style="text-align:center;">
<h1>Already authenticated</h1>
<p>You can close this window and return to the terminal.</p>
</div>
</body></html>`
