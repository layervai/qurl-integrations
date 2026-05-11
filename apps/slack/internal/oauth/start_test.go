package oauth

import (
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
		OAuthStateSecret: []byte("test-secret"),
		Now:              func() time.Time { return time.Unix(1700000000, 0) },
	}
}

func TestStartHappyPath(t *testing.T) {
	cfg := newStartCfg()
	h := Start(cfg)
	req := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start?team=T123ABCDEF", http.NoBody) //nolint:noctx // test convention matches handler_test.go.
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
	if !strings.Contains(q.Get("scope"), "qurl:write") || !strings.Contains(q.Get("scope"), "qurl:read") {
		t.Errorf("scope missing qurl:write/read: %q", q.Get("scope"))
	}
	if !strings.Contains(q.Get("scope"), "openid") || !strings.Contains(q.Get("scope"), "email") {
		t.Errorf("scope missing openid/email: %q", q.Get("scope"))
	}
	if q.Get("redirect_uri") != "https://slack-bot.example/oauth/qurl/callback" {
		t.Errorf("redirect_uri: %q", q.Get("redirect_uri"))
	}
	if q.Get("state") == "" {
		t.Error("state missing")
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
	if stateCookie.Value != q.Get("state") {
		t.Errorf("cookie != query state: %q vs %q", stateCookie.Value, q.Get("state"))
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
}

func TestStartRejectsBadTeam(t *testing.T) {
	for _, in := range []string{"", "foo", "t123abcdef", "T123", "X12345678"} {
		cfg := newStartCfg()
		h := Start(cfg)
		req := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start?team="+url.QueryEscape(in), http.NoBody) //nolint:noctx // test convention.
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("team=%q: got %d want 400", in, rec.Code)
		}
	}
}

func TestStartRejectsWrongMethod(t *testing.T) {
	cfg := newStartCfg()
	h := Start(cfg)
	req := httptest.NewRequest(http.MethodPost, "/oauth/qurl/start?team=T123ABCDEF", http.NoBody) //nolint:noctx // test convention.
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
	req := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start?team=T123ABCDEF", http.NoBody) //nolint:noctx // test convention matches handler_test.go.
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d want 503", rec.Code)
	}
}
