package slackinstall

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
)

const testStateSecret = "0123456789abcdef0123456789abcdef"

type fakeTokenStore struct {
	workspaceID string
	install     auth.SlackBotTokenInstall
	err         error
}

func (f *fakeTokenStore) SetSlackBotToken(_ context.Context, workspaceID string, install auth.SlackBotTokenInstall) error {
	f.workspaceID = workspaceID
	f.install = install
	return f.err
}

func testConfig(store *fakeTokenStore) Config {
	return Config{
		ClientID:     "111.222",
		ClientSecret: "secret",
		SlackBaseURL: "https://slack-bot.example",
		StateSecret:  []byte(testStateSecret),
		BotScopes:    []string{"commands", "views:write"},
		TokenStore:   store,
		Now:          func() time.Time { return time.Unix(1800000000, 0).UTC() },
	}
}

func TestConfigValidateRequiresGuidedInstallScopes(t *testing.T) {
	cfg := testConfig(&fakeTokenStore{})
	cfg.BotScopes = []string{"commands"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "views:write") {
		t.Fatalf("Validate error = %v, want missing views:write", err)
	}
	cfg.BotScopes = []string{"views:write"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "commands") {
		t.Fatalf("Validate error = %v, want missing commands", err)
	}
}

func TestInstallRedirectsToSlackAuthorizeWithStateCookie(t *testing.T) {
	store := &fakeTokenStore{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, InstallPath, http.NoBody)
	Install(testConfig(store)).ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Scheme != "https" || loc.Host != "slack.com" || loc.Path != "/oauth/v2/authorize" {
		t.Fatalf("Location = %s, want Slack authorize URL", loc.String())
	}
	q := loc.Query()
	if q.Get("client_id") != "111.222" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("scope") != "commands,views:write" {
		t.Errorf("scope = %q", q.Get("scope"))
	}
	if q.Get("redirect_uri") != "https://slack-bot.example/oauth/slack/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	state := q.Get("state")
	if state == "" {
		t.Fatal("state missing from redirect")
	}
	if err := verifyState([]byte(testStateSecret), state, time.Unix(1800000000, 0).UTC()); err != nil {
		t.Fatalf("redirect state did not verify: %v", err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != stateCookieName || cookies[0].Value != state {
		t.Fatalf("state cookie mismatch: %#v", cookies)
	}
	if !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("state cookie flags not hardened: %#v", cookies[0])
	}
}

func TestCallbackStoresWorkspaceBotToken(t *testing.T) {
	store := &fakeTokenStore{}
	state, err := mintState([]byte(testStateSecret), time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}

	var gotForm url.Values
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("Slack exchange method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		gotForm = r.Form
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":           true,
			"access_token": "xoxb-workspace-token",
			"scope":        "commands,views:write",
			"bot_user_id":  "U_BOT",
			"app_id":       "A_APP",
			"team": map[string]string{
				"id":   "T_WORKSPACE",
				"name": "Customer",
			},
			"enterprise": map[string]string{
				"id": "E_GRID",
			},
			"authed_user": map[string]string{
				"id": "U_INSTALLER",
			},
		})
	}))
	defer slack.Close()

	cfg := testConfig(store)
	cfg.OAuthAccessURL = slack.URL
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, CallbackPath+"?code=abc123&state="+url.QueryEscape(state), http.NoBody)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: state})
	Callback(cfg).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	if gotForm.Get("client_id") != "111.222" || gotForm.Get("client_secret") != "secret" || gotForm.Get("code") != "abc123" {
		t.Fatalf("unexpected Slack exchange form: %v", gotForm)
	}
	if gotForm.Get("redirect_uri") != "https://slack-bot.example/oauth/slack/callback" {
		t.Fatalf("redirect_uri = %q", gotForm.Get("redirect_uri"))
	}
	if store.workspaceID != "T_WORKSPACE" {
		t.Fatalf("workspaceID = %q", store.workspaceID)
	}
	if store.install.BotToken != "xoxb-workspace-token" ||
		store.install.InstalledBy != "U_INSTALLER" ||
		store.install.BotUserID != "U_BOT" ||
		store.install.AppID != "A_APP" ||
		store.install.EnterpriseID != "E_GRID" {
		t.Fatalf("stored install mismatch: %+v", store.install)
	}
	if strings.Join(store.install.Scopes, ",") != "commands,views:write" {
		t.Fatalf("scopes = %v", store.install.Scopes)
	}
	if strings.Contains(w.Body.String(), "xoxb-workspace-token") {
		t.Fatal("success page leaked bot token")
	}
}

func TestCallbackRejectsStateCookieMismatch(t *testing.T) {
	store := &fakeTokenStore{}
	state, err := mintState([]byte(testStateSecret), time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, CallbackPath+"?code=abc123&state="+url.QueryEscape(state), http.NoBody)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "different"})
	Callback(testConfig(store)).ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if store.workspaceID != "" {
		t.Fatalf("store should not be called on CSRF mismatch, got %q", store.workspaceID)
	}
}

func TestCallbackSurfacesSlackOAuthError(t *testing.T) {
	store := &fakeTokenStore{}
	state, err := mintState([]byte(testStateSecret), time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "bad_redirect_uri",
		})
	}))
	defer slack.Close()

	cfg := testConfig(store)
	cfg.OAuthAccessURL = slack.URL
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, CallbackPath+"?code=abc123&state="+url.QueryEscape(state), http.NoBody)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: state})
	Callback(cfg).ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if store.workspaceID != "" {
		t.Fatalf("store should not be called after Slack OAuth error, got %q", store.workspaceID)
	}
}
