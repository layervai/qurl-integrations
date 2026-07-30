package oauth

import (
	"context"
	"errors"
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
	verified, err := VerifyState(cfg.OAuthStateSecret, state, cfg.Now())
	if err != nil {
		t.Fatalf("VerifyState: %v", err)
	}
	if q.Get("nonce") != verified.Nonce {
		t.Errorf("nonce: got %q want signed state nonce %q", q.Get("nonce"), verified.Nonce)
	}
	if q.Get("code_challenge") != "" {
		t.Errorf("legacy state must not add a PKCE challenge, got %q", q.Get("code_challenge"))
	}
	if q.Get("code_challenge_method") != "" {
		t.Errorf("legacy state must not add a PKCE challenge method, got %q", q.Get("code_challenge_method"))
	}
	if q.Get("code_verifier") != "" {
		t.Errorf("code_verifier must not be sent to /authorize, got %q", q.Get("code_verifier"))
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

func TestStartUsesStoredOpaqueState(t *testing.T) {
	cfg := newStartCfg()
	store := newMemoryStateStore()
	cfg.StateStore = store
	state, err := MintStoredStateWithEmailMode(context.Background(), store, testStateTeamID, testStateUserID, "Admin@Example.COM", SetupModeReuse, cfg.Now())
	if err != nil {
		t.Fatalf("MintStoredStateWithEmailMode: %v", err)
	}
	h := Start(cfg)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/oauth/qurl/start?state="+url.QueryEscape(state), http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d want %d (body=%s)", rec.Code, http.StatusFound, rec.Body.String())
	}
	store.mu.Lock()
	startHadDeadline := store.startHadDeadline
	store.mu.Unlock()
	if !startHadDeadline {
		t.Fatal("StartState must receive an explicit deadline")
	}
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	q := u.Query()
	if q.Get("state") != state {
		t.Errorf("state: got %q want opaque handle %q", q.Get("state"), state)
	}
	verified, err := store.StartState(context.Background(), state, cfg.Now())
	if err != nil {
		t.Fatalf("StartState after handler: %v", err)
	}
	if q.Get("nonce") != verified.Nonce {
		t.Errorf("nonce: got %q want stored nonce %q", q.Get("nonce"), verified.Nonce)
	}
	if q.Get("code_challenge") != pkceCodeChallenge(verified.CodeVerifier) {
		t.Errorf("code_challenge: got %q want S256 challenge from stored verifier", q.Get("code_challenge"))
	}
	if q.Get("login_hint") != "admin@example.com" {
		t.Errorf("login_hint: got %q want normalized setup email", q.Get("login_hint"))
	}
	if strings.Contains(state, "admin@example.com") || strings.Contains(state, verified.CodeVerifier) {
		t.Fatalf("front-channel state leaked payload: state=%q verifier=%q", state, verified.CodeVerifier)
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
	assertOAuthErrorPage(t, rec, "Setup link is incomplete")
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
	assertOAuthErrorPage(t, rec, "Setup link is incomplete")
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
	assertOAuthErrorPage(t, rec, "Setup link is invalid or expired")
}

func TestStartRejectsExpiredState(t *testing.T) {
	cfg := newStartCfg()
	old := cfg.Now()
	state, _ := MintState(cfg.OAuthStateSecret, testStateTeamID, testStateUserID, old)
	cfg.Now = func() time.Time { return old.Add(stateMaxAge + time.Second) }
	h := Start(cfg)
	req := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start?state="+url.QueryEscape(state), http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d want 400", rec.Code)
	}
	assertOAuthErrorPage(t, rec, "Setup link is invalid or expired")
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
	assertOAuthErrorPage(t, rec, "Use the Slack setup link")
	if got := rec.Header().Get("Allow"); got != "GET" {
		t.Errorf("Allow header: got %q want GET", got)
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
	assertOAuthErrorPage(t, rec, "qURL setup is unavailable")
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
	assertOAuthErrorPage(t, rec, "qURL setup is unavailable")
}

func TestStartDoesNotFallbackToLegacyStateOnStoreAvailabilityError(t *testing.T) {
	cfg := newStartCfg()
	state, err := MintState(cfg.OAuthStateSecret, testStateTeamID, testStateUserID, cfg.Now())
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	cfg.StateStore = &unavailableStateStore{err: errors.New("ddb throttled")}
	h := Start(cfg)
	req := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start?state="+url.QueryEscape(state), http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	assertOAuthErrorPage(t, rec, "qURL setup is temporarily unavailable")
}
