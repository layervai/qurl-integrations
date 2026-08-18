package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
	if q.Get("prompt") != "login consent" {
		t.Errorf("prompt: got %q want %q (setup re-authenticates and forces consent)", q.Get("prompt"), "login consent")
	}
	if q.Get("connection") != defaultPasswordlessConnection {
		t.Errorf("connection: got %q want %q (passwordless is the Slack login method)", q.Get("connection"), defaultPasswordlessConnection)
	}
	if q.Get("login_hint") != "" {
		t.Errorf("login_hint: got %q want empty for legacy setup", q.Get("login_hint"))
	}
	if !strings.Contains(q.Get("scope"), "qurl:write") || !strings.Contains(q.Get("scope"), "qurl:read") || !strings.Contains(q.Get("scope"), "qurl:agent") {
		t.Errorf("scope missing qurl:read/write/agent: %q", q.Get("scope"))
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

func TestStartEmailSetupPinsPasswordlessConnection(t *testing.T) {
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
	if q.Get("connection") != defaultPasswordlessConnection {
		t.Errorf("connection: got %q want %q so the tenant's enabled connections cannot route this away from passwordless", q.Get("connection"), defaultPasswordlessConnection)
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

// TestStartAlwaysReauthenticatesAndPinsPasswordless pins the login contract for
// every setup path. Both first install and the explicit key operations decide
// which qURL account a Slack workspace is bound to, and qurl-service keys
// accounts on the id_token sub — so neither may ride an ambient Auth0 session
// (from qurl-desktop, the dashboard, or a previous bot run) and neither may be
// routed to a non-passwordless connection by tenant configuration.
//
// Covers BOTH state paths: production runs the opaque StateStore path, so a
// legacy-signed-state-only test would leave the shipping path unpinned.
func TestStartAlwaysReauthenticatesAndPinsPasswordless(t *testing.T) {
	for _, mode := range []SetupMode{SetupModeReuse, SetupModeRotate, SetupModeRepoint} {
		t.Run("signed state/"+string(mode), func(t *testing.T) {
			cfg := newStartCfg()
			state, err := MintStateWithEmailMode(cfg.OAuthStateSecret, testStateTeamID, testStateUserID, "admin@example.com", mode, cfg.Now())
			if err != nil {
				t.Fatalf("MintStateWithEmailMode: %v", err)
			}
			assertStartAuthParams(t, cfg, state)
		})

		t.Run("stored state/"+string(mode), func(t *testing.T) {
			cfg := newStartCfg()
			store := newMemoryStateStore()
			cfg.StateStore = store
			state, err := MintStoredStateWithEmailMode(context.Background(), store, testStateTeamID, testStateUserID, "admin@example.com", mode, cfg.Now())
			if err != nil {
				t.Fatalf("MintStoredStateWithEmailMode: %v", err)
			}
			assertStartAuthParams(t, cfg, state)
		})
	}
}

// assertStartAuthParams drives Start and checks only the two parameters that
// decide WHO authorizes the setup, so a failure names the mode/state-path
// subtest rather than a shared helper.
//
//nolint:gocritic // hugeParam: value-passed in line with the rest of the package's posture; see Callback.
func assertStartAuthParams(t *testing.T, cfg Config, state string) {
	t.Helper()
	h := Start(cfg)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/oauth/qurl/start?state="+url.QueryEscape(state), http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d want %d (body=%s)", rec.Code, http.StatusFound, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	q := u.Query()
	if got := q.Get("prompt"); got != "login consent" {
		t.Errorf("prompt: got %q want %q", got, "login consent")
	}
	if got := q.Get("connection"); got != defaultPasswordlessConnection {
		t.Errorf("connection: got %q want %q", got, defaultPasswordlessConnection)
	}
}

// TestAuthorizeURLPromptCarriesLoginAndConsent guards the two halves of the
// prompt independently. `login` stops an ambient session from authorizing the
// bind; `consent` stops Auth0 from silently reusing a prior consent grant,
// which would let a setup re-run complete without issuing a new token. Losing
// either one is a silent failure, so assert both rather than the joined string.
func TestAuthorizeURLPromptCarriesLoginAndConsent(t *testing.T) {
	cfg := newStartCfg()
	for _, mode := range []SetupMode{SetupModeReuse, SetupModeRotate, SetupModeRepoint} {
		raw := authorizeURL(cfg, "state-handle", VerifiedState{
			TeamID: testStateTeamID,
			UserID: testStateUserID,
			Email:  "admin@example.com",
			Mode:   mode,
		})
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("mode %q: parse authorize URL: %v", mode, err)
		}
		prompt := strings.Fields(u.Query().Get("prompt"))
		for _, want := range []string{"login", "consent"} {
			if !slices.Contains(prompt, want) {
				t.Errorf("mode %q: prompt %v must include %q", mode, prompt, want)
			}
		}
	}
}

// TestAuthorizeURLConnectionOverrideWins keeps AUTH0_EMAIL_CONNECTION usable
// for a tenant whose passwordless connection is not named "email". Without
// this, pinning the default would strand any such deployment.
//
// The whitespace case is not cosmetic: an env var set to " " (a stray value in
// a task definition or .env) must fall back to the passwordless pin rather than
// send a blank connection, which Auth0 would reject.
func TestAuthorizeURLConnectionOverrideWins(t *testing.T) {
	const customConnection = "passwordless-otp"
	for _, tt := range []struct {
		name       string
		configured string
		want       string
	}{
		{name: "override wins", configured: customConnection, want: customConnection},
		{name: "empty falls back to passwordless", configured: "", want: defaultPasswordlessConnection},
		{name: "whitespace-only falls back to passwordless", configured: "   ", want: defaultPasswordlessConnection},
		{name: "override is trimmed", configured: "  " + customConnection + "  ", want: customConnection},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newStartCfg()
			cfg.Auth0EmailConnection = tt.configured
			raw := authorizeURL(cfg, "state-handle", VerifiedState{
				TeamID: testStateTeamID,
				UserID: testStateUserID,
				Email:  "admin@example.com",
				Mode:   SetupModeReuse,
			})
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parse authorize URL: %v", err)
			}
			if got := u.Query().Get("connection"); got != tt.want {
				t.Errorf("connection: got %q want %q", got, tt.want)
			}
		})
	}
}

// TestAuthorizeURLPinsConnectionWithoutEmail covers the legacy no-email state:
// the connection must still be pinned even when there is no login_hint to
// prefill, so an emailless setup cannot fall through to whichever connections
// the Auth0 app happens to enable.
func TestAuthorizeURLPinsConnectionWithoutEmail(t *testing.T) {
	cfg := newStartCfg()
	raw := authorizeURL(cfg, "state-handle", VerifiedState{
		TeamID: testStateTeamID,
		UserID: testStateUserID,
		Mode:   SetupModeReuse,
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	q := u.Query()
	if got := q.Get("connection"); got != defaultPasswordlessConnection {
		t.Errorf("connection: got %q want %q", got, defaultPasswordlessConnection)
	}
	if got := q.Get("login_hint"); got != "" {
		t.Errorf("login_hint: got %q want empty when state carries no email", got)
	}
}
