package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeSlackBotTokenStore struct {
	workspaceID string
	botToken    string
	installedBy string
	err         error
}

func (f *fakeSlackBotTokenStore) SetSlackBotToken(_ context.Context, workspaceID, botToken, installedBy string) error {
	f.workspaceID = workspaceID
	f.botToken = botToken
	f.installedBy = installedBy
	return f.err
}

func TestSlackInstallStartRedirectsToSlackOAuth(t *testing.T) {
	secret := []byte(validStateSecret)
	now := time.Unix(1800000000, 0).UTC()
	cfg := slackInstallConfig{
		ClientID:     "C123",
		ClientSecret: "secret",
		SlackBaseURL: "https://slack-bot.example",
		StateSecret:  secret,
		Store:        &fakeSlackBotTokenStore{},
		Now:          func() time.Time { return now },
	}
	req := httptest.NewRequest(http.MethodGet, slackInstallPath, nil)
	w := httptest.NewRecorder()

	slackInstallStart(cfg)(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location parse: %v", err)
	}
	if u.String() == "" || u.Host != "slack.com" || u.Path != "/oauth/v2/authorize" {
		t.Fatalf("Location = %q, want Slack authorize URL", loc)
	}
	q := u.Query()
	if q.Get("client_id") != "C123" {
		t.Fatalf("client_id = %q, want C123", q.Get("client_id"))
	}
	if q.Get("scope") != "commands,views:write" {
		t.Fatalf("scope = %q, want commands,views:write", q.Get("scope"))
	}
	if q.Get("redirect_uri") != "https://slack-bot.example/slack/oauth/callback" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if err := verifySlackInstallState(secret, q.Get("state"), now); err != nil {
		t.Fatalf("state did not verify: %v", err)
	}
	if got := w.Result().Cookies()[0].Name; got != slackInstallStateCookieName {
		t.Fatalf("cookie name = %q, want %q", got, slackInstallStateCookieName)
	}
}

func TestBuildSlackInstallConfigFromEnv(t *testing.T) {
	t.Setenv("SLACK_CLIENT_ID", "C123")
	t.Setenv("SLACK_CLIENT_SECRET", "secret")
	t.Setenv("SLACK_BASE_URL", "https://slack-bot.example")
	t.Setenv("SLACK_INSTALL_STATE_SECRET", validStateSecret)
	t.Setenv("OAUTH_STATE_SECRET", "")
	t.Setenv("SLACK_INSTALL_BOT_SCOPES", "commands,views:write,chat:write")

	cfg, ok, err := buildSlackInstallConfig(newFakeProvider())

	if err != nil {
		t.Fatalf("buildSlackInstallConfig: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if cfg.ClientID != "C123" || cfg.ClientSecret != "secret" || cfg.SlackBaseURL != "https://slack-bot.example" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.BotScopes, []string{"commands", "views:write", "chat:write"}) {
		t.Fatalf("BotScopes = %v", cfg.BotScopes)
	}
}

func TestBuildSlackInstallConfigDisabledWhenNoSlackClientCredentials(t *testing.T) {
	t.Setenv("SLACK_CLIENT_ID", "")
	t.Setenv("SLACK_CLIENT_SECRET", "")
	t.Setenv("SLACK_BASE_URL", "https://slack-bot.example")
	t.Setenv("SLACK_INSTALL_STATE_SECRET", validStateSecret)

	_, ok, err := buildSlackInstallConfig(newFakeProvider())

	if err != nil {
		t.Fatalf("buildSlackInstallConfig: %v", err)
	}
	if ok {
		t.Fatal("ok = true, want false")
	}
}

func TestSlackInstallCallbackStoresWorkspaceBotToken(t *testing.T) {
	secret := []byte(validStateSecret)
	now := time.Unix(1800000000, 0).UTC()
	state, err := mintSlackInstallState(secret, now)
	if err != nil {
		t.Fatalf("mint state: %v", err)
	}
	store := &fakeSlackBotTokenStore{}
	var gotForm url.Values
	cfg := slackInstallConfig{
		ClientID:     "C123",
		ClientSecret: "client-secret",
		SlackBaseURL: "https://slack-bot.example",
		StateSecret:  secret,
		Store:        store,
		Now:          func() time.Time { return now },
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			gotForm, _ = url.ParseQuery(string(body))
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{
					"ok": true,
					"access_token": "xoxb-installed",
					"token_type": "bot",
					"bot_user_id": "B123",
					"app_id": "A123",
					"team": {"id": "T123", "name": "Acme"},
					"authed_user": {"id": "UINSTALL"}
				}`)),
			}, nil
		})},
	}
	req := httptest.NewRequest(http.MethodGet, slackInstallCallbackPath+"?code=code123&state="+url.QueryEscape(state), nil)
	req.AddCookie(&http.Cookie{Name: slackInstallStateCookieName, Value: state})
	w := httptest.NewRecorder()

	slackInstallCallback(cfg)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q, want 200", w.Code, w.Body.String())
	}
	if gotForm.Get("client_id") != "C123" || gotForm.Get("client_secret") != "client-secret" || gotForm.Get("code") != "code123" {
		t.Fatalf("oauth form = %v", gotForm)
	}
	if gotForm.Get("redirect_uri") != "https://slack-bot.example/slack/oauth/callback" {
		t.Fatalf("redirect_uri = %q", gotForm.Get("redirect_uri"))
	}
	if store.workspaceID != "T123" || store.botToken != "xoxb-installed" || store.installedBy != "UINSTALL" {
		t.Fatalf("stored = workspace:%q token:%q by:%q", store.workspaceID, store.botToken, store.installedBy)
	}
}

func TestSlackInstallCallbackRejectsStateCookieMismatch(t *testing.T) {
	secret := []byte(validStateSecret)
	now := time.Unix(1800000000, 0).UTC()
	state, err := mintSlackInstallState(secret, now)
	if err != nil {
		t.Fatalf("mint state: %v", err)
	}
	cfg := slackInstallConfig{
		ClientID:     "C123",
		ClientSecret: "secret",
		SlackBaseURL: "https://slack-bot.example",
		StateSecret:  secret,
		Store:        &fakeSlackBotTokenStore{},
		Now:          func() time.Time { return now },
	}
	req := httptest.NewRequest(http.MethodGet, slackInstallCallbackPath+"?code=code123&state="+url.QueryEscape(state), nil)
	req.AddCookie(&http.Cookie{Name: slackInstallStateCookieName, Value: "different"})
	w := httptest.NewRecorder()

	slackInstallCallback(cfg)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
