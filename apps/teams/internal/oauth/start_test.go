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

type noopAdminStore struct{}

func (noopAdminStore) BindWorkspace(context.Context, *WorkspaceMapping, string) error { return nil }

func newStartCfg() Config {
	return Config{
		Auth0Domain:      "example.auth0.com",
		Auth0ClientID:    testAuth0ClientID,
		Auth0Audience:    testAuth0Audience,
		TeamsBaseURL:     "https://teams-bot.example",
		OAuthStateSecret: testSecret,
		Now:              func() time.Time { return time.Unix(1700000000, 0) },
	}
}

func TestStartHappyPath(t *testing.T) {
	cfg := newStartCfg()
	state, err := MintState(cfg.OAuthStateSecret, testTenantID, testUserID, cfg.Now())
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	h := Start(&cfg)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, StartPath+"?state="+url.QueryEscape(state), http.NoBody)
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
	if u.Host != "example.auth0.com" || u.Path != "/authorize" {
		t.Fatalf("Location host/path wrong: %s", loc)
	}
	q := u.Query()
	if q.Get("client_id") != testAuth0ClientID || q.Get("audience") != testAuth0Audience {
		t.Fatalf("unexpected auth params: %v", q)
	}
	if q.Get("redirect_uri") != "https://teams-bot.example/oauth/qurl/callback" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("state") != state || q.Get("prompt") != "consent" {
		t.Fatalf("unexpected redirect params: %v", q)
	}
	if !strings.Contains(q.Get("scope"), "qurl:read") || !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("scope missing expected values: %q", q.Get("scope"))
	}
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
	if stateCookie.Value != state || stateCookie.Path != cookiePath || !stateCookie.HttpOnly || !stateCookie.Secure {
		t.Fatalf("unexpected cookie: %+v", stateCookie)
	}
}

func TestStartEmailSetupUsesLoginHintAndOptionalConnection(t *testing.T) {
	cfg := newStartCfg()
	state, err := MintStateWithEmail(cfg.OAuthStateSecret, testTenantID, testUserID, "Admin@Example.COM", cfg.Now())
	if err != nil {
		t.Fatalf("MintStateWithEmail: %v", err)
	}
	h := Start(&cfg)
	req := httptest.NewRequest(http.MethodGet, StartPath+"?state="+url.QueryEscape(state), http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if got := u.Query().Get("login_hint"); got != testAdminEmail {
		t.Fatalf("login_hint = %q, want %q", got, testAdminEmail)
	}
	if got := u.Query().Get("connection"); got != "" {
		t.Fatalf("connection = %q, want empty", got)
	}

	cfg.Auth0EmailConnection = "Username-Password-Authentication"
	h = Start(&cfg)
	rec = httptest.NewRecorder()
	h(rec, req)
	u, err = url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if got := u.Query().Get("connection"); got != "Username-Password-Authentication" {
		t.Fatalf("connection = %q", got)
	}
}

func TestStartRejectsBadRequests(t *testing.T) {
	cfg := newStartCfg()
	cases := []struct {
		name    string
		method  string
		target  string
		heading string
	}{
		{name: "missing state", method: http.MethodGet, target: StartPath, heading: "Setup link is incomplete"},
		{name: "wrong method", method: http.MethodPost, target: StartPath, heading: "Use the Teams setup link"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Start(&cfg)(rec, httptest.NewRequest(tc.method, tc.target, http.NoBody))
			assertOAuthErrorPage(t, rec, tc.heading)
		})
	}
	state, err := MintState(cfg.OAuthStateSecret, testTenantID, testUserID, cfg.Now())
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	tampered := state[:len(state)-1] + "A"
	rec := httptest.NewRecorder()
	Start(&cfg)(rec, httptest.NewRequest(http.MethodGet, StartPath+"?state="+url.QueryEscape(tampered), http.NoBody))
	assertOAuthErrorPage(t, rec, "Setup link is invalid or expired")
}

func TestStartRejectsShortSecret(t *testing.T) {
	cfg := newStartCfg()
	cfg.OAuthStateSecret = []byte("short")
	rec := httptest.NewRecorder()
	Start(&cfg)(rec, httptest.NewRequest(http.MethodGet, StartPath+"?state=anything", http.NoBody))
	assertOAuthErrorPage(t, rec, "qURL setup is unavailable")
}

func TestOAuthHelperFunctions(t *testing.T) {
	if got := callbackURL("https://teams-bot.example/"); got != "https://teams-bot.example/oauth/qurl/callback" {
		t.Fatalf("callbackURL = %q", got)
	}
	setupURL := (SetupConfig{TeamsBaseURL: "https://teams-bot.example/"}).SetupURL("state value")
	if !strings.HasPrefix(setupURL, "https://teams-bot.example/oauth/qurl/start?state=") {
		t.Fatalf("SetupURL = %q", setupURL)
	}
	if got := teamsWorkspaceID(" tenant-123 "); got != "teams:tenant-123" {
		t.Fatalf("teamsWorkspaceID = %q", got)
	}
	if got := replayWindowHoursOrDefault(0, 24); got != 24 {
		t.Fatalf("replayWindowHoursOrDefault default = %d", got)
	}
	if got := replayWindowHoursOrDefault(12, 24); got != 12 {
		t.Fatalf("replayWindowHoursOrDefault configured = %d", got)
	}
	cfg := newStartCfg()
	cfg.AdminStore = noopAdminStore{}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() unexpectedly succeeded without BindClassifyError")
	}
	cfg.BindClassifyError = func(error) BindConflictCode { return "" }
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestAuthorizeURLAndAPIKeyScopesAgree(t *testing.T) {
	cfg := newStartCfg()
	authURL := authorizeURL(&cfg, "state", VerifiedState{Email: testAdminEmail})
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	scope := u.Query().Get("scope")
	for _, want := range apiKeyScopes() {
		if !strings.Contains(scope, want) {
			t.Fatalf("scope %q missing %q", scope, want)
		}
	}
}
