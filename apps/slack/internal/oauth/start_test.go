package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newStartCfg() Config {
	return Config{
		Auth0Domain:      "example.auth0.com",
		Auth0ClientID:    testAuth0ClientID,
		Auth0Audience:    "https://api.qurl.invalid",
		SlackBaseURL:     "https://slack-bot.example",
		OAuthStateSecret: testSecret,
		Now:              func() time.Time { return time.Unix(1700000000, 0) },
	}
}

func TestStartHappyPath(t *testing.T) {
	cfg := newStartCfg()
	state, err := MintState(cfg.OAuthStateSecret, testStateTeamID, testStateUserID, cfg.Now())
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	h := Start(cfg)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/oauth/qurl/start?state="+url.QueryEscape(state), http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d want %d (body=%s)", rec.Code, http.StatusFound, rec.Body.String())
	}

	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatal("Location header missing")
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if u.Host != "example.auth0.com" || u.Path != "/authorize" {
		t.Errorf("Location host/path wrong: %s", loc)
	}
	q := u.Query()
	if q.Get("response_type") != "code" {
		t.Errorf("response_type: %q", q.Get("response_type"))
	}
	if q.Get("client_id") != testAuth0ClientID {
		t.Errorf("client_id: %q", q.Get("client_id"))
	}
	if q.Get("audience") != "https://api.qurl.invalid" {
		t.Errorf("audience: %q", q.Get("audience"))
	}
	if q.Get("prompt") != "consent" {
		t.Errorf("prompt: got %q want %q (setup re-entry forces consent)", q.Get("prompt"), "consent")
	}
	if q.Get("connection") != "" {
		t.Errorf("connection: got %q want empty for legacy setup", q.Get("connection"))
	}
	if q.Get("login_hint") != "" {
		t.Errorf("login_hint: got %q want empty for legacy setup", q.Get("login_hint"))
	}
	if !strings.Contains(q.Get("scope"), "qurl:write") || !strings.Contains(q.Get("scope"), "qurl:read") {
		t.Errorf("scope missing qurl:write/read: %q", q.Get("scope"))
	}
	if !strings.Contains(q.Get("scope"), "openid") || !strings.Contains(q.Get("scope"), "email") {
		t.Errorf("scope missing openid/email: %q", q.Get("scope"))
	}
	if q.Get("redirect_uri") != "https://slack-bot.example/oauth/qurl/callback" {
		t.Errorf("redirect_uri: %q", q.Get("redirect_uri"))
	}
	if q.Get("state") != state {
		t.Errorf("state: got %q want %q (must pass through the signed state)", q.Get("state"), state)
	}

	// Cookie set with the same state, HttpOnly + Lax.
	var stateCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			stateCookie = c
			break
		}
	}
	if stateCookie == nil {
		t.Fatal("state cookie not set")
	}
	if stateCookie.Value != state {
		t.Errorf("cookie != state: %q vs %q", stateCookie.Value, state)
	}
	if !stateCookie.HttpOnly {
		t.Error("cookie must be HttpOnly")
	}
	if stateCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite: got %v want Lax", stateCookie.SameSite)
	}
	if !stateCookie.Secure {
		t.Error("cookie must be Secure")
	}
	if stateCookie.Path != "/oauth/qurl" {
		t.Errorf("cookie path: got %q want %q (tightened from /oauth)", stateCookie.Path, "/oauth/qurl")
	}
}

func TestStartEmailSetupUsesLoginHintWithoutForcingConnection(t *testing.T) {
	cfg := newStartCfg()
	state, err := MintStateWithEmail(cfg.OAuthStateSecret, testStateTeamID, testStateUserID, "Admin@Example.COM", cfg.Now())
	if err != nil {
		t.Fatalf("MintStateWithEmail: %v", err)
	}
	h := Start(cfg)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/oauth/qurl/start?state="+url.QueryEscape(state), http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d want %d (body=%s)", rec.Code, http.StatusFound, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	q := u.Query()
	if q.Get("connection") != "" {
		t.Errorf("connection: got %q want empty so Auth0 app enabled connections choose the login method", q.Get("connection"))
	}
	if q.Get("login_hint") != "admin@example.com" {
		t.Errorf("login_hint: got %q want normalized email", q.Get("login_hint"))
	}
	if q.Get("state") != state {
		t.Errorf("state: got %q want %q", q.Get("state"), state)
	}
}

func TestStartEmailSetupUsesConfiguredConnection(t *testing.T) {
	cfg := newStartCfg()
	cfg.Auth0EmailConnection = "Username-Password-Authentication"
	state, err := MintStateWithEmail(cfg.OAuthStateSecret, testStateTeamID, testStateUserID, "admin@example.com", cfg.Now())
	if err != nil {
		t.Fatalf("MintStateWithEmail: %v", err)
	}
	h := Start(cfg)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/oauth/qurl/start?state="+url.QueryEscape(state), http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d want %d (body=%s)", rec.Code, http.StatusFound, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if got := u.Query().Get("connection"); got != "Username-Password-Authentication" {
		t.Errorf("connection: got %q want configured connection", got)
	}
}

// TestAuthorizeURLAndAPIKeyScopesAgree locks the contract that the
// scopes requested at /authorize match the scopes carried by the
// downstream qurl-service mint. A drift here would surface as an
// Auth0-issued access_token with the wrong scopes, the mint succeeding
// but the resulting key carrying scopes the workspace bot never
// expected.
func TestAuthorizeURLAndAPIKeyScopesAgree(t *testing.T) {
	cfg := newStartCfg()
	authURL := authorizeURL(cfg, "irrelevant", VerifiedState{})
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	authScope := u.Query().Get("scope")
	for _, want := range apiKeyScopes() {
		if !strings.Contains(authScope, want) {
			t.Errorf("authorize scope %q missing %q from apiKeyScopes()", authScope, want)
		}
	}
}

// TestClearStateCookieScopedToOAuthPath locks the contract that the
// cleared cookie carries the same Path as the set cookie. A mismatch
// would leave the browser holding a stale cookie under the original
// path (clear-only-applies-when-path-matches).
func TestClearStateCookieScopedToOAuthPath(t *testing.T) {
	rec := httptest.NewRecorder()
	clearStateCookie(rec)
	var got *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			got = c
			break
		}
	}
	if got == nil {
		t.Fatal("clearStateCookie did not set a cookie")
	}
	if got.Path != "/oauth/qurl" {
		t.Errorf("cleared cookie Path: got %q want %q", got.Path, "/oauth/qurl")
	}
	if got.MaxAge >= 0 {
		t.Errorf("cleared cookie MaxAge must be negative, got %d", got.MaxAge)
	}
}

func TestStartRejectsMissingState(t *testing.T) {
	cfg := newStartCfg()
	h := Start(cfg)
	req := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start", http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d want 400", rec.Code)
	}
}

func TestStartRejectsRawTeamQuery(t *testing.T) {
	// The unsigned `?team=` form used to mint state on the server side;
	// after the origin-binding refactor it has no special meaning. The
	// request fails as "missing state".
	cfg := newStartCfg()
	h := Start(cfg)
	req := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start?team=T123ABCDEF", http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d want 400", rec.Code)
	}
}

func TestStartRejectsTamperedState(t *testing.T) {
	cfg := newStartCfg()
	state, _ := MintState(cfg.OAuthStateSecret, testStateTeamID, testStateUserID, cfg.Now())
	// Flip a byte in the encoded state to invalidate the HMAC.
	tampered := state[:len(state)-1] + "A"
	if tampered == state {
		tampered = "A" + state[1:]
	}
	h := Start(cfg)
	req := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start?state="+url.QueryEscape(tampered), http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d want 400", rec.Code)
	}
}

func TestStartRejectsExpiredState(t *testing.T) {
	cfg := newStartCfg()
	old := cfg.Now()
	state, _ := MintState(cfg.OAuthStateSecret, testStateTeamID, testStateUserID, old)
	cfg.Now = func() time.Time { return old.Add(10 * time.Minute) }
	h := Start(cfg)
	req := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start?state="+url.QueryEscape(state), http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d want 400", rec.Code)
	}
}

func TestStartRejectsWrongMethod(t *testing.T) {
	cfg := newStartCfg()
	h := Start(cfg)
	req := httptest.NewRequest(http.MethodPost, "/oauth/qurl/start?state=anything", http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("got %d want 405", rec.Code)
	}
}

func TestStartRefusesWithoutSecret(t *testing.T) {
	cfg := newStartCfg()
	cfg.OAuthStateSecret = nil
	h := Start(cfg)
	req := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start?state=anything", http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d want 503", rec.Code)
	}
}

func TestStartRefusesWithShortSecret(t *testing.T) {
	cfg := newStartCfg()
	cfg.OAuthStateSecret = []byte("too-short")
	h := Start(cfg)
	req := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start?state=anything", http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d want 503", rec.Code)
	}
}
