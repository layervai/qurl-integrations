// Package slackinstall implements the Slack app OAuth installation flow.
//
// This is distinct from apps/slack/internal/oauth, which connects an already
// installed Slack workspace to a qURL account through Auth0. This package
// captures the Slack-issued bot token for each customer workspace so Web API
// calls such as views.open can be made with the token that belongs to the
// workspace that invoked the slash command.
package slackinstall

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
)

const (
	InstallPath  = "/oauth/slack/install"
	CallbackPath = "/oauth/slack/callback"

	defaultSlackOAuthAccessURL = "https://slack.com/api/oauth.v2.access"
	slackAuthorizeURL          = "https://slack.com/oauth/v2/authorize"
	stateCookieName            = "__Host-qurl-slack-install-state"
	stateTTL                   = 10 * time.Minute
	stateMinSecretBytes        = 32
	slackOAuthTimeout          = 10 * time.Second
	persistTimeout             = 15 * time.Second
	slackOAuthBodyLimit        = 8 << 10
)

var successTemplate = template.Must(template.New("slack-install-success").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>qURL Slack Installed</title>
<meta name="robots" content="noindex">
<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:480px;margin:4rem auto;padding:0 1rem;color:#111}
.card{border:1px solid #d1d5db;border-radius:12px;padding:2rem;background:#f9fafb}
h1{margin:0 0 .5rem;font-size:1.5rem}
code{background:#e5e7eb;padding:.1rem .3rem;border-radius:4px;font-size:.875em}
.ok{color:#059669;font-weight:600}
</style>
</head>
<body>
<div class="card">
<h1><span class="ok">&#10003;</span> qURL Slack app installed</h1>
<p>Guided tunnel setup is enabled for this workspace. Return to Slack and run <code>/qurl tunnel install</code>.</p>
<p>Workspace: <code>{{.TeamID}}</code></p>
</div>
</body>
</html>`))

type Config struct {
	ClientID       string
	ClientSecret   string
	SlackBaseURL   string
	StateSecret    []byte
	BotScopes      []string
	TokenStore     TokenStore
	HTTPClient     *http.Client
	Now            func() time.Time
	OAuthAccessURL string
}

type TokenStore interface {
	SetSlackBotToken(ctx context.Context, workspaceID string, install auth.SlackBotTokenInstall) error
}

func DefaultBotScopes() []string {
	return []string{"commands", "views:write"}
}

func RegisterRoutes(mux *http.ServeMux, cfg Config) {
	if err := cfg.Validate(); err != nil {
		panic("slackinstall.RegisterRoutes: " + err.Error())
	}
	mux.HandleFunc(InstallPath, Install(cfg))
	mux.HandleFunc(CallbackPath, Callback(cfg))
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.ClientID) == "" {
		return errors.New("ClientID is required")
	}
	if strings.TrimSpace(c.ClientSecret) == "" {
		return errors.New("ClientSecret is required")
	}
	if strings.TrimSpace(c.SlackBaseURL) == "" {
		return errors.New("SlackBaseURL is required")
	}
	if !strings.HasPrefix(c.SlackBaseURL, "https://") {
		return fmt.Errorf("SlackBaseURL must be https:// (got %q)", c.SlackBaseURL)
	}
	if len(c.StateSecret) < stateMinSecretBytes {
		return fmt.Errorf("StateSecret must be at least %d bytes", stateMinSecretBytes)
	}
	if c.TokenStore == nil {
		return errors.New("TokenStore is required")
	}
	scopes := normalizeScopes(c.BotScopes)
	if len(scopes) == 0 {
		return errors.New("at least one bot scope is required")
	}
	if missing := missingRequiredScopes(scopes); len(missing) > 0 {
		return fmt.Errorf("bot scopes missing required scope(s): %s", strings.Join(missing, ","))
	}
	return nil
}

func missingRequiredScopes(scopes []string) []string {
	have := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		have[scope] = struct{}{}
	}
	var missing []string
	for _, scope := range DefaultBotScopes() {
		if _, ok := have[scope]; !ok {
			missing = append(missing, scope)
		}
	}
	return missing
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Config) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{
		Timeout: slackOAuthTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (c Config) oauthAccessURL() string {
	if strings.TrimSpace(c.OAuthAccessURL) != "" {
		return strings.TrimSpace(c.OAuthAccessURL)
	}
	return defaultSlackOAuthAccessURL
}

func Install(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		state, err := mintState(cfg.StateSecret, cfg.now())
		if err != nil {
			slog.Error("slack install state mint failed", "error", err)
			http.Error(w, "could not start Slack install", http.StatusInternalServerError)
			return
		}
		setStateCookie(w, state, cfg.now())
		http.Redirect(w, r, authorizeURL(cfg, state), http.StatusFound)
	}
}

func Callback(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			slog.Warn("Slack install callback returned error", "error", errParam)
			clearStateCookie(w)
			http.Error(w, "Slack install was not completed", http.StatusBadRequest)
			return
		}
		code := strings.TrimSpace(r.URL.Query().Get("code"))
		state := strings.TrimSpace(r.URL.Query().Get("state"))
		if code == "" || state == "" {
			clearStateCookie(w)
			http.Error(w, "missing Slack install code or state", http.StatusBadRequest)
			return
		}
		if !verifyCookieState(r, state) {
			clearStateCookie(w)
			http.Error(w, "Slack install must be completed in the same browser", http.StatusBadRequest)
			return
		}
		if err := verifyState(cfg.StateSecret, state, cfg.now()); err != nil {
			slog.Warn("Slack install rejected invalid state", "error", err)
			clearStateCookie(w)
			http.Error(w, "invalid or expired Slack install link", http.StatusBadRequest)
			return
		}

		resp, err := exchangeCode(r.Context(), cfg, code)
		if err != nil {
			slog.Error("Slack install token exchange failed", "error", err)
			clearStateCookie(w)
			http.Error(w, "Slack install token exchange failed", http.StatusBadGateway)
			return
		}
		teamID := strings.TrimSpace(resp.Team.ID)
		if teamID == "" {
			slog.Error("Slack install token exchange missing team id", "app_id", resp.AppID)
			clearStateCookie(w)
			http.Error(w, "Slack workspace install did not include a workspace id", http.StatusBadGateway)
			return
		}
		enterpriseID := strings.TrimSpace(resp.Enterprise.ID)
		installedBy := strings.TrimSpace(resp.AuthedUser.ID)
		persistCtx, persistCancel := context.WithTimeout(context.Background(), persistTimeout)
		defer persistCancel()
		if err := cfg.TokenStore.SetSlackBotToken(persistCtx, teamID, auth.SlackBotTokenInstall{
			BotToken:     resp.AccessToken,
			InstalledBy:  installedBy,
			BotUserID:    resp.BotUserID,
			AppID:        resp.AppID,
			EnterpriseID: enterpriseID,
			Scopes:       splitSlackScopes(resp.Scope),
		}); err != nil {
			slog.Error("Slack install token persist failed",
				"error", err, "team_id", teamID, "installed_by", installedBy)
			clearStateCookie(w)
			http.Error(w, "Slack install could not be stored", http.StatusInternalServerError)
			return
		}

		clearStateCookie(w)
		slog.Info("Slack app install stored workspace bot token",
			"team_id", teamID, "installed_by", installedBy,
			"bot_user_id", resp.BotUserID, "app_id", resp.AppID,
			"enterprise_id", enterpriseID)
		renderSuccess(w, teamID)
	}
}

func authorizeURL(cfg Config, state string) string {
	u, _ := url.Parse(slackAuthorizeURL)
	q := u.Query()
	q.Set("client_id", strings.TrimSpace(cfg.ClientID))
	q.Set("scope", strings.Join(normalizeScopes(cfg.BotScopes), ","))
	q.Set("redirect_uri", callbackURL(cfg.SlackBaseURL))
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String()
}

func callbackURL(baseURL string) string {
	u, err := url.JoinPath(strings.TrimRight(baseURL, "/"), CallbackPath)
	if err != nil {
		return strings.TrimRight(baseURL, "/") + CallbackPath
	}
	return u
}

type statePayload struct {
	Nonce         string `json:"nonce"`
	CreatedAtUnix int64  `json:"created_at_unix"`
}

func mintState(secret []byte, now time.Time) (string, error) {
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload, err := json.Marshal(statePayload{
		Nonce:         base64.RawURLEncoding.EncodeToString(nonce),
		CreatedAtUnix: now.UTC().Unix(),
	})
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	sig := signState(secret, body)
	return body + "." + sig, nil
}

func verifyState(secret []byte, token string, now time.Time) error {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return errors.New("malformed state")
	}
	wantSig := signState(secret, parts[0])
	if !hmac.Equal([]byte(parts[1]), []byte(wantSig)) {
		return errors.New("state signature mismatch")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("state payload decode: %w", err)
	}
	var payload statePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("state payload JSON: %w", err)
	}
	createdAt := time.Unix(payload.CreatedAtUnix, 0)
	if now.Before(createdAt.Add(-1 * time.Minute)) {
		return errors.New("state timestamp is in the future")
	}
	if now.Sub(createdAt) > stateTTL {
		return errors.New("state expired")
	}
	if payload.Nonce == "" {
		return errors.New("state nonce is empty")
	}
	return nil
}

func signState(secret []byte, body string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func setStateCookie(w http.ResponseWriter, state string, now time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		Expires:  now.Add(stateTTL),
		MaxAge:   int(stateTTL.Seconds()),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func verifyCookieState(r *http.Request, state string) bool {
	cookie, err := r.Cookie(stateCookieName)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(cookie.Value), []byte(state))
}

type oauthAccessResponse struct {
	OK          bool   `json:"ok"`
	Error       string `json:"error"`
	AccessToken string `json:"access_token"`
	Scope       string `json:"scope"`
	BotUserID   string `json:"bot_user_id"`
	AppID       string `json:"app_id"`
	Team        struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
	Enterprise struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"enterprise"`
	AuthedUser struct {
		ID string `json:"id"`
	} `json:"authed_user"`
}

func exchangeCode(ctx context.Context, cfg Config, code string) (*oauthAccessResponse, error) {
	form := url.Values{}
	form.Set("client_id", strings.TrimSpace(cfg.ClientID))
	form.Set("client_secret", strings.TrimSpace(cfg.ClientSecret))
	form.Set("code", code)
	form.Set("redirect_uri", callbackURL(cfg.SlackBaseURL))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.oauthAccessURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := cfg.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, slackOAuthBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(raw) > slackOAuthBodyLimit {
		return nil, fmt.Errorf("response exceeded %d bytes", slackOAuthBodyLimit)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, responseSnippet(raw))
	}
	var out oauthAccessResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if !out.OK {
		code := strings.TrimSpace(out.Error)
		if code == "" {
			code = "not_ok"
		}
		return nil, fmt.Errorf("Slack oauth.v2.access: %s", code)
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return nil, errors.New("Slack oauth.v2.access returned empty access_token")
	}
	return &out, nil
}

func responseSnippet(raw []byte) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 200 {
		raw = append(raw[:197], '.', '.', '.')
	}
	return strings.ToValidUTF8(string(raw), "\uFFFD")
}

func normalizeScopes(scopes []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		for _, part := range strings.Split(scope, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			out = append(out, part)
		}
	}
	return out
}

func splitSlackScopes(scope string) []string {
	return normalizeScopes(strings.FieldsFunc(scope, func(r rune) bool {
		return r == ',' || r == ' '
	}))
}

func renderSuccess(w http.ResponseWriter, teamID string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := successTemplate.Execute(w, struct{ TeamID string }{TeamID: teamID}); err != nil {
		slog.Warn("Slack install success page write failed", "error", err)
	}
}
