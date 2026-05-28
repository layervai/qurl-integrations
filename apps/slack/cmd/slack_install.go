package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
)

const (
	slackInstallPath         = "/slack/install"
	slackInstallCallbackPath = "/slack/oauth/callback"

	slackOAuthAuthorizeURL = "https://slack.com/oauth/v2/authorize"
	slackOAuthAccessURL    = "https://slack.com/api/oauth.v2.access"

	slackInstallStateCookieName = "qurl_slack_install_state"
	slackInstallStateMaxAge     = 10 * time.Minute
	slackInstallStateNonceBytes = 16
	slackInstallStatePartCount  = 3
	slackInstallHTTPTimeout     = 10 * time.Second
	slackInstallBodyLimit       = 16 << 10
	slackInstallDrainCap        = 32 << 10
)

var defaultSlackInstallBotScopes = []string{"commands", "views:write"}

func buildSlackInstallConfig(provider *auth.DDBProvider) (slackInstallConfig, bool, error) {
	clientID := strings.TrimSpace(os.Getenv("SLACK_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("SLACK_CLIENT_SECRET"))
	if clientID == "" && clientSecret == "" {
		slog.Info("Slack install OAuth disabled", "reason", "slack_client_credentials_unset")
		return slackInstallConfig{}, false, nil
	}
	baseURL := strings.TrimRight(os.Getenv("SLACK_BASE_URL"), "/")
	stateSecret := os.Getenv("SLACK_INSTALL_STATE_SECRET")
	if stateSecret == "" {
		stateSecret = os.Getenv("OAUTH_STATE_SECRET")
	}
	var missing []string
	if clientID == "" {
		missing = append(missing, "SLACK_CLIENT_ID")
	}
	if clientSecret == "" {
		missing = append(missing, "SLACK_CLIENT_SECRET")
	}
	if baseURL == "" {
		missing = append(missing, "SLACK_BASE_URL")
	}
	if stateSecret == "" {
		missing = append(missing, "SLACK_INSTALL_STATE_SECRET or OAUTH_STATE_SECRET")
	}
	if len(missing) > 0 {
		return slackInstallConfig{}, false, fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	if err := validateSlackInstallBaseURL(baseURL); err != nil {
		return slackInstallConfig{}, false, err
	}
	if len(stateSecret) < minStateSecretBytes {
		return slackInstallConfig{}, false, fmt.Errorf("Slack install state secret is shorter than %d bytes", minStateSecretBytes)
	}
	if provider == nil {
		return slackInstallConfig{}, false, errors.New("workspace state provider is required")
	}
	return slackInstallConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		SlackBaseURL: baseURL,
		StateSecret:  []byte(stateSecret),
		BotScopes:    readSlackInstallBotScopes(),
		Store:        provider,
	}, true, nil
}

func validateSlackInstallBaseURL(baseURL string) error {
	if !strings.HasPrefix(baseURL, "https://") {
		return fmt.Errorf("SLACK_BASE_URL must be https:// (got %q)", baseURL)
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("SLACK_BASE_URL must be a bare https:// origin with no path/query/userinfo (got %q)", baseURL)
	}
	return nil
}

func readSlackInstallBotScopes() []string {
	raw := strings.TrimSpace(os.Getenv("SLACK_INSTALL_BOT_SCOPES"))
	if raw == "" {
		return defaultSlackInstallBotScopes
	}
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
}

type slackBotTokenStore interface {
	SetSlackBotToken(ctx context.Context, workspaceID, botToken, installedBy string) error
}

type slackInstallConfig struct {
	ClientID     string
	ClientSecret string
	SlackBaseURL string
	StateSecret  []byte
	BotScopes    []string
	Store        slackBotTokenStore
	HTTPClient   *http.Client
	Now          func() time.Time
}

type slackOAuthAccessResponse struct {
	OK          bool   `json:"ok"`
	Error       string `json:"error"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	BotUserID   string `json:"bot_user_id"`
	Scope       string `json:"scope"`
	AppID       string `json:"app_id"`
	Team        struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
	AuthedUser struct {
		ID string `json:"id"`
	} `json:"authed_user"`
}

func registerSlackInstallRoutes(mux *http.ServeMux, cfg slackInstallConfig) {
	mux.Handle(slackInstallPath, http.TimeoutHandler(
		slackInstallStart(cfg), slackInstallHTTPTimeout, "slack/install timed out"))
	mux.Handle(slackInstallCallbackPath, http.TimeoutHandler(
		slackInstallCallback(cfg), slackInstallHTTPTimeout, "slack/oauth callback timed out"))
}

func slackInstallStart(cfg slackInstallConfig) http.HandlerFunc {
	now := cfg.now()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := cfg.validate(); err != nil {
			slog.Error("slack/install refused: config invalid", "error", err)
			http.Error(w, "Slack install is not configured", http.StatusServiceUnavailable)
			return
		}
		state, err := mintSlackInstallState(cfg.StateSecret, now())
		if err != nil {
			slog.Error("slack/install state mint failed", "error", err)
			http.Error(w, "could not start Slack install", http.StatusInternalServerError)
			return
		}
		setSlackInstallStateCookie(w, state)
		http.Redirect(w, r, slackAuthorizeURL(cfg, state), http.StatusFound)
	}
}

func slackInstallCallback(cfg slackInstallConfig) http.HandlerFunc {
	now := cfg.now()
	httpClient := cfg.httpClient()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := cfg.validate(); err != nil {
			slog.Error("slack/oauth callback refused: config invalid", "error", err)
			http.Error(w, "Slack install is not configured", http.StatusServiceUnavailable)
			return
		}
		q := r.URL.Query()
		if slackErr := q.Get("error"); slackErr != "" {
			clearSlackInstallStateCookie(w)
			slog.Warn("slack/oauth callback rejected by Slack", "error", slackErr)
			http.Error(w, "Slack install was not approved", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		state := q.Get("state")
		if code == "" || state == "" {
			http.Error(w, "missing code or state", http.StatusBadRequest)
			return
		}
		cookieState := readSlackInstallStateCookie(r)
		if cookieState == "" || !hmac.Equal([]byte(cookieState), []byte(state)) {
			clearSlackInstallStateCookie(w)
			http.Error(w, "Slack install must be completed in the same browser", http.StatusBadRequest)
			return
		}
		if err := verifySlackInstallState(cfg.StateSecret, state, now()); err != nil {
			clearSlackInstallStateCookie(w)
			slog.Warn("slack/oauth callback rejected invalid state", "error", err)
			http.Error(w, "invalid or expired Slack install link", http.StatusBadRequest)
			return
		}
		clearSlackInstallStateCookie(w)

		install, err := exchangeSlackOAuthCode(r.Context(), httpClient, cfg, code)
		if err != nil {
			slog.Error("slack/oauth exchange failed", "error", err)
			http.Error(w, "could not install qURL in Slack", http.StatusBadGateway)
			return
		}
		storeCtx, cancel := context.WithTimeout(r.Context(), slackInstallHTTPTimeout)
		defer cancel()
		if err := cfg.Store.SetSlackBotToken(storeCtx, install.Team.ID, install.AccessToken, install.AuthedUser.ID); err != nil {
			slog.Error("slack/oauth token persist failed", "error", err, "team_id", install.Team.ID)
			http.Error(w, "Slack installed but token storage failed", http.StatusInternalServerError)
			return
		}
		slog.Info("Slack app installed", "team_id", install.Team.ID, "bot_user_id", install.BotUserID, "app_id", install.AppID)
		renderSlackInstallSuccess(w, install.Team.ID)
	}
}

func (c slackInstallConfig) validate() error {
	switch {
	case strings.TrimSpace(c.ClientID) == "":
		return errors.New("SLACK_CLIENT_ID is required")
	case strings.TrimSpace(c.ClientSecret) == "":
		return errors.New("SLACK_CLIENT_SECRET is required")
	case strings.TrimSpace(c.SlackBaseURL) == "":
		return errors.New("SLACK_BASE_URL is required")
	case len(c.StateSecret) < minStateSecretBytes:
		return fmt.Errorf("Slack install state secret must be at least %d bytes", minStateSecretBytes)
	case c.Store == nil:
		return errors.New("Slack bot token store is required")
	}
	return nil
}

func (c slackInstallConfig) now() func() time.Time {
	if c.Now != nil {
		return c.Now
	}
	return time.Now
}

func (c slackInstallConfig) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{
		Timeout: slackInstallHTTPTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func slackAuthorizeURL(cfg slackInstallConfig, state string) string {
	u, _ := url.Parse(slackOAuthAuthorizeURL)
	q := u.Query()
	q.Set("client_id", cfg.ClientID)
	q.Set("scope", strings.Join(normalizeSlackInstallScopes(cfg.BotScopes), ","))
	q.Set("redirect_uri", slackInstallCallbackURL(cfg.SlackBaseURL))
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String()
}

func normalizeSlackInstallScopes(scopes []string) []string {
	if len(scopes) == 0 {
		scopes = defaultSlackInstallBotScopes
	}
	out := make([]string, 0, len(scopes))
	seen := map[string]struct{}{}
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	if len(out) == 0 {
		return append([]string(nil), defaultSlackInstallBotScopes...)
	}
	return out
}

func slackInstallCallbackURL(baseURL string) string {
	u, err := url.JoinPath(strings.TrimRight(baseURL, "/"), slackInstallCallbackPath)
	if err == nil {
		return u
	}
	return strings.TrimRight(baseURL, "/") + slackInstallCallbackPath
}

func exchangeSlackOAuthCode(ctx context.Context, httpClient *http.Client, cfg slackInstallConfig, code string) (*slackOAuthAccessResponse, error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("client_secret", cfg.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", slackInstallCallbackURL(cfg.SlackBaseURL))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackOAuthAccessURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer func() {
		_, _ = io.CopyN(io.Discard, resp.Body, slackInstallDrainCap)
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, slackInstallBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > slackInstallBodyLimit {
		return nil, fmt.Errorf("slack oauth response exceeded %d bytes", slackInstallBodyLimit)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slack oauth returned HTTP %d", resp.StatusCode)
	}
	var out slackOAuthAccessResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if !out.OK {
		if out.Error == "" {
			out.Error = "not_ok"
		}
		return nil, fmt.Errorf("slack oauth: %s", printableLogSnippet(out.Error))
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return nil, errors.New("slack oauth returned empty access_token")
	}
	if strings.TrimSpace(out.Team.ID) == "" {
		return nil, errors.New("slack oauth returned empty team.id")
	}
	if out.TokenType != "" && out.TokenType != "bot" {
		return nil, fmt.Errorf("slack oauth returned token_type %q, want bot", out.TokenType)
	}
	return &out, nil
}

func mintSlackInstallState(secret []byte, now time.Time) (string, error) {
	if len(secret) < minStateSecretBytes {
		return "", errors.New("slack install state secret too short")
	}
	nonceBytes := make([]byte, slackInstallStateNonceBytes)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("read nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	ts := fmt.Sprintf("%d", now.Unix())
	signed := nonce + "|" + ts
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signed))
	raw := signed + "|" + hex.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(raw)), nil
}

func verifySlackInstallState(secret []byte, encoded string, now time.Time) error {
	if len(secret) < minStateSecretBytes {
		return errors.New("state secret too short")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return errors.New("state malformed")
	}
	parts := bytes.Split(raw, []byte("|"))
	if len(parts) != slackInstallStatePartCount {
		return errors.New("state malformed")
	}
	signed := string(parts[0]) + "|" + string(parts[1])
	gotSig, err := hex.DecodeString(string(parts[2]))
	if err != nil {
		return errors.New("state malformed")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signed))
	if !hmac.Equal(gotSig, mac.Sum(nil)) {
		return errors.New("state HMAC mismatch")
	}
	ts, err := strconv.ParseInt(string(parts[1]), 10, 64)
	if err != nil {
		return errors.New("state malformed")
	}
	mintedAt := time.Unix(ts, 0)
	if now.Sub(mintedAt) > slackInstallStateMaxAge {
		return errors.New("state expired")
	}
	if mintedAt.After(now.Add(30 * time.Second)) {
		return errors.New("state timestamp in future")
	}
	return nil
}

func setSlackInstallStateCookie(w http.ResponseWriter, state string) {
	http.SetCookie(w, &http.Cookie{
		Name:     slackInstallStateCookieName,
		Value:    state,
		Path:     slackInstallCallbackPath,
		MaxAge:   int(slackInstallStateMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func readSlackInstallStateCookie(r *http.Request) string {
	c, err := r.Cookie(slackInstallStateCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func clearSlackInstallStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     slackInstallStateCookieName,
		Value:    "",
		Path:     slackInstallCallbackPath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func renderSlackInstallSuccess(w http.ResponseWriter, teamID string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := slackInstallSuccessTemplate.Execute(w, struct{ TeamID string }{TeamID: teamID}); err != nil {
		slog.Warn("slack/install success page write failed", "error", err)
	}
}

var slackInstallSuccessTemplate = template.Must(template.New("slack-install-success").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>qURL installed</title>
<style>body{font-family:system-ui,-apple-system,Segoe UI,sans-serif;margin:3rem;line-height:1.5;color:#17202a}code{background:#f3f4f6;padding:.1rem .25rem;border-radius:.25rem}</style></head>
<body><h1>qURL is installed in Slack</h1><p>Slack workspace <code>{{.TeamID}}</code> can now run <code>/qurl setup</code> to connect a qURL account.</p></body></html>`))
