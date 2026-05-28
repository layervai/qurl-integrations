package slackinstall

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
)

const testStateSecret = "0123456789abcdef0123456789abcdef"

const (
	testClientID        = "111.222"
	testClientSecret    = "secret"
	testScopeCSV        = "commands,views:write"
	testWorkspaceID     = "T_WORKSPACE"
	testWorkspaceToken  = "xoxb-123456789012345678901234567890"
	testAuthCode        = "abc123"
	testAccessTokenKey  = "access_token"
	testSlackInstallURL = CallbackPath + "?code=" + testAuthCode + "&state="
)

type fakeTokenStore struct {
	workspaceID string
	install     auth.SlackBotTokenInstall
	err         error
}

func (f *fakeTokenStore) SetSlackBotToken(_ context.Context, workspaceID string, install *auth.SlackBotTokenInstall) error {
	f.workspaceID = workspaceID
	if install != nil {
		f.install = *install
	}
	return f.err
}

func testConfig(store *fakeTokenStore) Config {
	return Config{
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
		SlackBaseURL: "https://slack-bot.example",
		StateSecret:  []byte(testStateSecret),
		BotScopes:    []string{botScopeCommands, botScopeViewsWrite},
		TokenStore:   store,
		Now:          func() time.Time { return time.Unix(1800000000, 0).UTC() },
	}
}

func testStateHTTPCookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     stateCookieName,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

func TestConfigValidateRequiresGuidedInstallScopes(t *testing.T) {
	cfg := testConfig(&fakeTokenStore{})
	cfg.BotScopes = []string{botScopeCommands}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), botScopeViewsWrite) {
		t.Fatalf("Validate error = %v, want missing views:write", err)
	}
	cfg.BotScopes = []string{botScopeViewsWrite}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), botScopeCommands) {
		t.Fatalf("Validate error = %v, want missing commands", err)
	}
}

func TestInstallRedirectsToSlackAuthorizeWithStateCookie(t *testing.T) {
	store := &fakeTokenStore{}
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, InstallPath, http.NoBody)
	cfg := testConfig(store)
	Install(&cfg).ServeHTTP(w, req)

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
	if q.Get("client_id") != testClientID {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("scope") != testScopeCSV {
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

func TestInstallRejectsNonGET(t *testing.T) {
	cfg := testConfig(&fakeTokenStore{})
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, InstallPath, http.NoBody)
	Install(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if w.Header().Get("Allow") != "GET" {
		t.Fatalf("Allow = %q, want GET", w.Header().Get("Allow"))
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
			"ok":               true,
			testAccessTokenKey: testWorkspaceToken,
			"scope":            testScopeCSV,
			"bot_user_id":      "U_BOT",
			"app_id":           "A_APP",
			"team": map[string]string{
				"id":   testWorkspaceID,
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
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie(state))
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", w.Code, w.Body.String())
	}
	if gotForm.Get("client_id") != testClientID || gotForm.Get("client_secret") != testClientSecret || gotForm.Get("code") != testAuthCode {
		t.Fatalf("unexpected Slack exchange form: %v", gotForm)
	}
	if gotForm.Get("redirect_uri") != "https://slack-bot.example/oauth/slack/callback" {
		t.Fatalf("redirect_uri = %q", gotForm.Get("redirect_uri"))
	}
	if store.workspaceID != testWorkspaceID {
		t.Fatalf("workspaceID = %q", store.workspaceID)
	}
	if store.install.BotToken != testWorkspaceToken ||
		store.install.InstalledBy != "U_INSTALLER" ||
		store.install.BotUserID != "U_BOT" ||
		store.install.AppID != "A_APP" ||
		store.install.EnterpriseID != "E_GRID" {
		t.Fatalf("stored install mismatch: %+v", store.install)
	}
	if strings.Join(store.install.Scopes, ",") != testScopeCSV {
		t.Fatalf("scopes = %v", store.install.Scopes)
	}
	if strings.Contains(w.Body.String(), testWorkspaceToken) {
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
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie("different"))
	cfg := testConfig(store)
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if store.workspaceID != "" {
		t.Fatalf("store should not be called on CSRF mismatch, got %q", store.workspaceID)
	}
}

func TestCallbackRejectsExpiredState(t *testing.T) {
	store := &fakeTokenStore{}
	createdAt := time.Unix(1800000000, 0).UTC().Add(-stateTTL - time.Second)
	state, err := mintState([]byte(testStateSecret), createdAt)
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	cfg := testConfig(store)
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie(state))
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if store.workspaceID != "" {
		t.Fatalf("store should not be called on expired state, got %q", store.workspaceID)
	}
}

func TestCallbackRejectsWrongStateFlow(t *testing.T) {
	store := &fakeTokenStore{}
	payload, err := json.Marshal(statePayload{
		Flow:          "qurl-oauth",
		Nonce:         "nonce",
		CreatedAtUnix: time.Unix(1800000000, 0).UTC().Unix(),
	})
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	state := body + "." + signState([]byte(testStateSecret), body)
	cfg := testConfig(store)
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie(state))
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if store.workspaceID != "" {
		t.Fatalf("store should not be called on wrong state flow, got %q", store.workspaceID)
	}
}

func TestCallbackRejectsMissingQueryParams(t *testing.T) {
	for _, rawQuery := range []string{"code=" + testAuthCode, "state=" + testAuthCode, ""} {
		t.Run(rawQuery, func(t *testing.T) {
			store := &fakeTokenStore{}
			cfg := testConfig(store)
			w := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, CallbackPath+"?"+rawQuery, http.NoBody)
			Callback(&cfg).ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			if store.workspaceID != "" {
				t.Fatalf("store should not be called on missing params, got %q", store.workspaceID)
			}
		})
	}
}

func TestCallbackRejectsNonGET(t *testing.T) {
	cfg := testConfig(&fakeTokenStore{})
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, CallbackPath, http.NoBody)
	Callback(&cfg).ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if w.Header().Get("Allow") != "GET" {
		t.Fatalf("Allow = %q, want GET", w.Header().Get("Allow"))
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
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie(state))
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if store.workspaceID != "" {
		t.Fatalf("store should not be called after Slack OAuth error, got %q", store.workspaceID)
	}
}

func TestCallbackRejectsSlackResponseMissingTeamID(t *testing.T) {
	store := &fakeTokenStore{}
	state, err := mintState([]byte(testStateSecret), time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":               true,
			testAccessTokenKey: testWorkspaceToken,
		})
	}))
	defer slack.Close()

	cfg := testConfig(store)
	cfg.OAuthAccessURL = slack.URL
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie(state))
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if store.workspaceID != "" {
		t.Fatalf("store should not be called without team id, got %q", store.workspaceID)
	}
}

func TestCallbackRejectsEnterpriseGridOrgInstall(t *testing.T) {
	store := &fakeTokenStore{}
	state, err := mintState([]byte(testStateSecret), time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                    true,
			testAccessTokenKey:      testWorkspaceToken,
			"scope":                 testScopeCSV,
			"is_enterprise_install": true,
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
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie(state))
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if store.workspaceID != "" {
		t.Fatalf("store should not be called for org-level Enterprise Grid install, got %q", store.workspaceID)
	}
	if !strings.Contains(w.Body.String(), "Enterprise Grid") {
		t.Fatalf("body = %q, want Enterprise Grid guidance", w.Body.String())
	}
}

func TestCallbackRejectsSlackResponseMissingAuthedUserID(t *testing.T) {
	store := &fakeTokenStore{}
	state, err := mintState([]byte(testStateSecret), time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":               true,
			testAccessTokenKey: testWorkspaceToken,
			"scope":            testScopeCSV,
			"team": map[string]string{
				"id": testWorkspaceID,
			},
		})
	}))
	defer slack.Close()

	cfg := testConfig(store)
	cfg.OAuthAccessURL = slack.URL
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie(state))
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if store.workspaceID != "" {
		t.Fatalf("store should not be called without authed user id, got %q", store.workspaceID)
	}
}

func TestCallbackRejectsMissingRequiredSlackScopes(t *testing.T) {
	store := &fakeTokenStore{}
	state, err := mintState([]byte(testStateSecret), time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":               true,
			testAccessTokenKey: testWorkspaceToken,
			"scope":            botScopeCommands,
			"team": map[string]string{
				"id": testWorkspaceID,
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
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie(state))
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if store.workspaceID != "" {
		t.Fatalf("store should not be called without required scopes, got %q", store.workspaceID)
	}
	if !strings.Contains(w.Body.String(), "required bot scopes") {
		t.Fatalf("body = %q, want required-scope guidance", w.Body.String())
	}
}

func TestCallbackRejectsMalformedSlackBotToken(t *testing.T) {
	store := &fakeTokenStore{}
	state, err := mintState([]byte(testStateSecret), time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":               true,
			testAccessTokenKey: "xoxa-123456789012345678901234567890",
			"team": map[string]string{
				"id": testWorkspaceID,
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
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie(state))
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if store.workspaceID != "" {
		t.Fatalf("store should not be called with malformed token, got %q", store.workspaceID)
	}
}

func TestCallbackSurfacesTokenStoreFailure(t *testing.T) {
	store := &fakeTokenStore{err: errors.New("ddb down")}
	state, err := mintState([]byte(testStateSecret), time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":               true,
			testAccessTokenKey: testWorkspaceToken,
			"scope":            testScopeCSV,
			"team": map[string]string{
				"id": testWorkspaceID,
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
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie(state))
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if store.workspaceID != testWorkspaceID {
		t.Fatalf("store should be called before surfacing persist failure, got %q", store.workspaceID)
	}
}

func TestCallbackRejectsOversizedSlackOAuthResponse(t *testing.T) {
	store := &fakeTokenStore{}
	state, err := mintState([]byte(testStateSecret), time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, strings.Repeat("x", slackOAuthBodyLimit+1))
	}))
	defer slack.Close()

	cfg := testConfig(store)
	cfg.OAuthAccessURL = slack.URL
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie(state))
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if store.workspaceID != "" {
		t.Fatalf("store should not be called on oversized Slack response, got %q", store.workspaceID)
	}
}

func TestCallbackDoesNotLeakSlackHTTPErrorBody(t *testing.T) {
	store := &fakeTokenStore{}
	state, err := mintState([]byte(testStateSecret), time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"access_token":"xoxb-should-not-leak"}`))
	}))
	defer slack.Close()

	cfg := testConfig(store)
	cfg.OAuthAccessURL = slack.URL
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testSlackInstallURL+url.QueryEscape(state), http.NoBody)
	req.AddCookie(testStateHTTPCookie(state))
	Callback(&cfg).ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if strings.Contains(w.Body.String(), "xoxb-should-not-leak") {
		t.Fatal("callback response leaked Slack OAuth error body")
	}
}

func TestSafeSlackOAuthErrorCode(t *testing.T) {
	for _, code := range []string{
		"access_denied",
		"bad_redirect_uri",
		"invalid_client_id",
		"invalid_redirect_uri",
		"invalid_scope",
		"invalid_state",
		"invalid_team_for_non_distributed_app",
	} {
		if got := safeSlackOAuthErrorCode(" " + code + " "); got != code {
			t.Fatalf("allowlisted error = %q, want %q", got, code)
		}
	}
	if got := safeSlackOAuthErrorCode("bad\nvalue"); got != slackOAuthErrorUnknown {
		t.Fatalf("unexpected error = %q, want %s", got, slackOAuthErrorUnknown)
	}
	if got := safeSlackOAuthErrorCode("not_authed"); got != slackOAuthErrorUnknown {
		t.Fatalf("unexpected Slack API error = %q, want %s", got, slackOAuthErrorUnknown)
	}
}
