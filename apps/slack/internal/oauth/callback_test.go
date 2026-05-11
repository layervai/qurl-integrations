package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// testTeamID + testAuth0ClientID are reused across cases to keep
// fixture values comparable and to avoid the goconst linter trip.
const (
	testTeamID        = "T123ABCDEF"
	testAuth0ClientID = "client-id"
)

// fakeWorkspaceStore captures SetAPIKey calls.
type fakeWorkspaceStore struct {
	mu      sync.Mutex
	setArgs *struct {
		WorkspaceID, APIKey, ConfiguredBy string
	}
	setErr error
}

func (f *fakeWorkspaceStore) SetAPIKey(_ context.Context, ws, key, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setArgs = &struct{ WorkspaceID, APIKey, ConfiguredBy string }{ws, key, by}
	return f.setErr
}
func (f *fakeWorkspaceStore) DeleteAPIKey(_ context.Context, _ string) error { return nil }

// fakeMinter implements QURLAPIKeyMinter.
type fakeMinter struct {
	apiKey, keyID, keyPrefix string
	mintErr                  error
	revoked                  bool
	revokeMu                 sync.Mutex
}

func (f *fakeMinter) MintAPIKey(_ context.Context, _, _ string, _ []string) (apiKey, keyID, keyPrefix string, err error) {
	return f.apiKey, f.keyID, f.keyPrefix, f.mintErr
}
func (f *fakeMinter) RevokeAPIKey(_ context.Context, _, _ string) error {
	f.revokeMu.Lock()
	defer f.revokeMu.Unlock()
	f.revoked = true
	return nil
}

// fakeIDTokenVerifier always returns the configured email or err.
type fakeIDTokenVerifier struct {
	email string
	err   error
}

func (f *fakeIDTokenVerifier) VerifyEmail(_ context.Context, _ string) (string, error) {
	return f.email, f.err
}

// Narrowed helpers that pick only what each test actually asserts on —
// keeps dogsled happy without losing the multi-return flexibility of
// the shared builder.
func newCallbackCfgOnly(t *testing.T) Config {
	t.Helper()
	cfg, _, _, _ := newCallbackCfg(t) //nolint:dogsled // wrapper deliberately discards unused dependencies.
	return cfg
}

func newCallbackCfgStore(t *testing.T) (Config, *fakeWorkspaceStore) {
	t.Helper()
	cfg, _, store, _ := newCallbackCfg(t)
	return cfg, store
}

func newCallbackCfgStoreMinter(t *testing.T) (Config, *fakeWorkspaceStore, *fakeMinter) {
	t.Helper()
	cfg, _, store, minter := newCallbackCfg(t)
	return cfg, store, minter
}

func newCallbackCfg(t *testing.T) (Config, *httptest.Server, *fakeWorkspaceStore, *fakeMinter) {
	t.Helper()
	// Stub Auth0 token endpoint.
	auth0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" {
			http.Error(w, "wrong grant", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": "auth0-access",
			"id_token":     "auth0-id-token",
			"token_type":   "Bearer",
		})
	}))
	t.Cleanup(auth0.Close)
	store := &fakeWorkspaceStore{}
	minter := &fakeMinter{apiKey: "lv_live_abcd1234", keyID: "k_1", keyPrefix: "lv_live_abcd"}

	// Re-point HTTPClient at the stub Auth0 by rewriting the request host
	// via a custom Transport. The simplest path: a Transport that
	// redirects every outbound to auth0.URL.
	stubTransport := &rewriteTransport{target: auth0.URL}
	cfg := Config{
		Auth0Domain:       strings.TrimPrefix(auth0.URL, "http://"),
		Auth0ClientID:     testAuth0ClientID,
		Auth0ClientSecret: "client-secret",
		Auth0Audience:     "aud",
		SlackBaseURL:      "https://slack-bot.example",
		OAuthStateSecret:  []byte("test-secret"),
		Provider:          store,
		IDTokenVerifier:   &fakeIDTokenVerifier{email: "admin@example.com"},
		Minter:            minter,
		HTTPClient:        &http.Client{Transport: stubTransport, Timeout: 5 * time.Second},
		Now:               func() time.Time { return time.Unix(1700000000, 0) },
	}
	return cfg, auth0, store, minter
}

// rewriteTransport rewrites every request to point at `target`. Used so
// the callback's hard-coded "https://<Auth0Domain>/oauth/token" lookup
// hits the httptest server instead.
type rewriteTransport struct {
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(t.target)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	req.Host = u.Host
	return http.DefaultTransport.RoundTrip(req)
}

func TestCallbackHappyPath(t *testing.T) {
	cfg, _, store, minter := newCallbackCfg(t)

	// Mint a valid state token + matching cookie.
	state, err := mintState(cfg.OAuthStateSecret, testTeamID, cfg.Now())
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}

	h := Callback(cfg)
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/qurl/callback?code=abc&state="+url.QueryEscape(state), http.NoBody)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: state, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "qURL connected") {
		t.Errorf("success body missing headline: %s", rec.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs == nil {
		t.Fatal("SetAPIKey not called")
	}
	if store.setArgs.WorkspaceID != testTeamID {
		t.Errorf("workspaceID: got %q", store.setArgs.WorkspaceID)
	}
	if store.setArgs.APIKey != "lv_live_abcd1234" {
		t.Errorf("apiKey: got %q", store.setArgs.APIKey)
	}
	if minter.revoked {
		t.Error("happy path should not revoke")
	}
}

func TestCallbackRejectsCSRFMismatch(t *testing.T) {
	cfg, store := newCallbackCfgStore(t)
	state, _ := mintState(cfg.OAuthStateSecret, testTeamID, cfg.Now())

	h := Callback(cfg)
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/qurl/callback?code=abc&state="+url.QueryEscape(state), http.NoBody)
	// Cookie carries DIFFERENT state value — replay/leak scenario.
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "tampered", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400", rec.Code)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs != nil {
		t.Error("SetAPIKey should NOT have been called on CSRF reject")
	}
}

func TestCallbackRejectsMissingCookie(t *testing.T) {
	cfg := newCallbackCfgOnly(t)
	state, _ := mintState(cfg.OAuthStateSecret, testTeamID, cfg.Now())
	h := Callback(cfg)
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/qurl/callback?code=abc&state="+url.QueryEscape(state), http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400", rec.Code)
	}
}

func TestCallbackRejectsExpiredState(t *testing.T) {
	cfg := newCallbackCfgOnly(t)
	// Mint state at T0; verify at T0+10min — past stateMaxAge (5min).
	oldNow := cfg.Now()
	state, _ := mintState(cfg.OAuthStateSecret, testTeamID, oldNow)
	cfg.Now = func() time.Time { return oldNow.Add(10 * time.Minute) }

	h := Callback(cfg)
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/qurl/callback?code=abc&state="+url.QueryEscape(state), http.NoBody)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: state, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestCallbackRevokesOnPersistFailure(t *testing.T) {
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	store.setErr = errors.New("ddb down")
	state, _ := mintState(cfg.OAuthStateSecret, testTeamID, cfg.Now())

	h := Callback(cfg)
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/qurl/callback?code=abc&state="+url.QueryEscape(state), http.NoBody)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: state, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d want 500", rec.Code)
	}
	// Revoke is fire-and-forget in a goroutine — give it a moment.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		minter.revokeMu.Lock()
		revoked := minter.revoked
		minter.revokeMu.Unlock()
		if revoked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("expected RevokeAPIKey to be called after persist failure")
}

func TestCallbackRejectsMissingCode(t *testing.T) {
	cfg := newCallbackCfgOnly(t)
	h := Callback(cfg)
	req := httptest.NewRequest(http.MethodGet, "/oauth/qurl/callback?state=x", http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d want 400", rec.Code)
	}
}

func TestCallbackHandlesAuth0Error(t *testing.T) {
	cfg := newCallbackCfgOnly(t)
	h := Callback(cfg)
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/qurl/callback?error=access_denied&error_description=user+declined", http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d want 400", rec.Code)
	}
}
