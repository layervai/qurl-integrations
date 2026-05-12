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

const (
	testTeamID        = "T123ABCDEF"
	testUserID        = "U_ADMIN1"
	testAuth0ClientID = "client-id"
	testKeyID         = "k_1"
	testKeyPrefix     = "lv_live_abcd"
	testAPIKey        = "lv_live_abcd1234"
	testAdminEmail    = "admin@example.com"
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

// fakeSlackClient captures PostDirectMessage calls.
type fakeSlackClient struct {
	mu        sync.Mutex
	gotUser   string
	gotText   string
	sendErr   error
	postedCh  chan struct{}
	closeOnce sync.Once
}

func (f *fakeSlackClient) PostDirectMessage(_ context.Context, userID, text string) error {
	f.mu.Lock()
	f.gotUser = userID
	f.gotText = text
	f.mu.Unlock()
	if f.postedCh != nil {
		// sync.Once guards against a future test that triggers two
		// PostDirectMessage calls — close-of-closed-channel panics.
		f.closeOnce.Do(func() { close(f.postedCh) })
	}
	return f.sendErr
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
		})
	}))
	t.Cleanup(auth0.Close)
	store := &fakeWorkspaceStore{}
	minter := &fakeMinter{apiKey: testAPIKey, keyID: testKeyID, keyPrefix: testKeyPrefix}

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
		OAuthStateSecret:  testSecret,
		Provider:          store,
		IDTokenVerifier:   &fakeIDTokenVerifier{email: testAdminEmail},
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

func mintTestState(t *testing.T, cfg *Config) string {
	t.Helper()
	state, err := MintState(cfg.OAuthStateSecret, testTeamID, testUserID, cfg.Now())
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	return state
}

func callbackRequest(state string) *http.Request {
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/qurl/callback?code=abc&state="+url.QueryEscape(state), http.NoBody)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: state, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	return req
}

func TestCallbackHappyPath(t *testing.T) {
	cfg, _, store, minter := newCallbackCfg(t)

	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "qURL Connected") {
		t.Errorf("success body missing headline: %s", body)
	}
	// Lock auto-escape: KeyPrefix and Email must render verbatim (html/
	// template is the load-bearing XSS defense; a refactor to text/template
	// would silently drop the protection).
	if !strings.Contains(body, testKeyPrefix) {
		t.Errorf("success body missing key prefix: %s", body)
	}
	if !strings.Contains(body, testAdminEmail) {
		t.Errorf("success body missing email: %s", body)
	}
	// Defense-in-depth headers are required on the success page.
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("X-Frame-Options: got %q want DENY", rec.Header().Get("X-Frame-Options"))
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("CSP missing default-src 'none': %q", rec.Header().Get("Content-Security-Policy"))
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("Referrer-Policy: got %q want no-referrer", rec.Header().Get("Referrer-Policy"))
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options: got %q want nosniff", rec.Header().Get("X-Content-Type-Options"))
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs == nil {
		t.Fatal("SetAPIKey not called")
	}
	if store.setArgs.WorkspaceID != testTeamID {
		t.Errorf("workspaceID: got %q", store.setArgs.WorkspaceID)
	}
	if store.setArgs.APIKey != testAPIKey {
		t.Errorf("apiKey: got %q", store.setArgs.APIKey)
	}
	// configuredBy must come from the verified state's userID — never
	// from an unsigned query parameter.
	if store.setArgs.ConfiguredBy != testUserID {
		t.Errorf("configuredBy: got %q want %q (must be recovered from signed state)", store.setArgs.ConfiguredBy, testUserID)
	}
	if minter.revoked {
		t.Error("happy path should not revoke")
	}
}

// TestSuccessPageHTMLEscapesInterpolations is the explicit lock for the
// html/template auto-escape contract — the load-bearing XSS defense
// for the success page. A refactor that swapped html/template for
// text/template (or for a string concat) would let TeamID/KeyPrefix/
// Email render raw. We render a synthetic payload with a <script> tag
// and assert it appears escaped.
func TestSuccessPageHTMLEscapesInterpolations(t *testing.T) {
	rec := httptest.NewRecorder()
	renderSuccess(rec, "T<script>alert(1)</script>", "lv_<b>", "user@<i>x</i>.com")
	body := rec.Body.String()
	if strings.Contains(body, "<script>") {
		t.Errorf("raw <script> rendered — auto-escape regressed:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected escaped <script> in body:\n%s", body)
	}
	if strings.Contains(body, "<b>") || strings.Contains(body, "<i>") {
		t.Errorf("HTML tags leaked through:\n%s", body)
	}
}

func TestCallbackIgnoresAdminUserQueryParam(t *testing.T) {
	// Regression: configuredBy used to be read from ?admin_user=…
	// which let an attacker pick the DM target. Now the value is
	// strictly recovered from the signed state payload.
	cfg, _, store, _ := newCallbackCfg(t)
	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/qurl/callback?code=abc&admin_user=U_ATTACKER&state="+url.QueryEscape(state), http.NoBody)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: state, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs.ConfiguredBy == "U_ATTACKER" {
		t.Error("configuredBy must NOT come from admin_user query param")
	}
	if store.setArgs.ConfiguredBy != testUserID {
		t.Errorf("configuredBy: got %q want %q", store.setArgs.ConfiguredBy, testUserID)
	}
}

func TestCallbackRejectsCSRFMismatch(t *testing.T) {
	cfg, store := newCallbackCfgStore(t)
	state := mintTestState(t, &cfg)

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
	// On reject, the cookie should be cleared so a refresh isn't stuck
	// looping on the same mismatch.
	var clearedCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName && c.MaxAge < 0 {
			clearedCookie = c
			break
		}
	}
	if clearedCookie == nil {
		t.Error("expected state cookie to be cleared on CSRF reject")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs != nil {
		t.Error("SetAPIKey should NOT have been called on CSRF reject")
	}
}

func TestCallbackRejectsMissingCookie(t *testing.T) {
	cfg := newCallbackCfgOnly(t)
	state := mintTestState(t, &cfg)
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
	state, _ := MintState(cfg.OAuthStateSecret, testTeamID, testUserID, oldNow)
	cfg.Now = func() time.Time { return oldNow.Add(10 * time.Minute) }

	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestCallbackMintFailureDoesNotRevoke locks the contract: when the
// qurl-service mint itself fails, there's no keyID to revoke, so the
// RevokeAPIKey path MUST NOT fire. A refactor that moved the revoke
// spawn earlier would silently regress.
func TestCallbackMintFailureDoesNotRevoke(t *testing.T) {
	cfg, _, minter := newCallbackCfgStoreMinter(t)
	minter.mintErr = errors.New("qurl-service down")
	state := mintTestState(t, &cfg)

	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d want 502 (mint failure → 502)", rec.Code)
	}
	// Give any spurious revoke goroutine a window to fire.
	time.Sleep(50 * time.Millisecond)
	minter.revokeMu.Lock()
	defer minter.revokeMu.Unlock()
	if minter.revoked {
		t.Error("RevokeAPIKey must NOT be called when mint itself failed (no keyID exists)")
	}
}

func TestCallbackRevokesOnPersistFailure(t *testing.T) {
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	store.setErr = errors.New("ddb down")
	state := mintTestState(t, &cfg)

	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
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

// TestCallbackRejectsNonGET locks the method-allow contract: a POST
// (or any non-GET) to /oauth/qurl/callback returns 405 with an Allow
// header — Auth0 redirects with GET so any non-GET hit is a
// misconfiguration or probe.
func TestCallbackRejectsNonGET(t *testing.T) {
	cfg := newCallbackCfgOnly(t)
	h := Callback(cfg)
	req := httptest.NewRequest(http.MethodPost, "/oauth/qurl/callback", http.NoBody)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("got %d want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET" {
		t.Errorf("Allow header: got %q want GET", got)
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

// TestCallbackRendersSuccessWhenVerifierFails locks the documented
// non-fatal contract: a JWKS / id_token verify failure suppresses the
// email line on the success page but never blocks the key mint or
// the success render.
func TestCallbackRendersSuccessWhenVerifierFails(t *testing.T) {
	cfg := newCallbackCfgOnly(t)
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{err: errors.New("jwks fetch failed")}
	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), testAdminEmail) {
		t.Error("verifier-error path should NOT render email")
	}
	if !strings.Contains(rec.Body.String(), "qURL Connected") {
		t.Errorf("expected success body even when verifier errored: %s", rec.Body.String())
	}
}

// TestCallbackAuth0TokenFailure exercises the non-200 branch in
// exchangeAuth0Code by swapping in an Auth0 stub that always 500s.
func TestCallbackAuth0TokenFailure(t *testing.T) {
	cfg := newCallbackCfgOnly(t)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	cfg.HTTPClient = &http.Client{
		Transport: &rewriteTransport{target: failing.URL},
		Timeout:   2 * time.Second,
	}
	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("got %d want 502 (auth0 5xx surfaces as 502)", rec.Code)
	}
}

// TestCallbackAuth0EmptyAccessToken locks the "empty access_token →
// failure" guard in exchangeAuth0Code. An Auth0 200 with no
// access_token (or no id_token) is a misconfiguration the callback
// must not silently proceed past.
func TestCallbackAuth0EmptyAccessToken(t *testing.T) {
	cfg := newCallbackCfgOnly(t)
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": "only"})
	}))
	t.Cleanup(empty.Close)
	cfg.HTTPClient = &http.Client{
		Transport: &rewriteTransport{target: empty.URL},
		Timeout:   2 * time.Second,
	}
	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("got %d want 502", rec.Code)
	}
}

// countingTracker satisfies AsyncTracker by tracking how many fn
// invocations passed through it. Used to assert that fire-and-forget
// goroutines route through handler.wg (i.e. are drained by SIGTERM).
type countingTracker struct {
	mu   sync.Mutex
	wg   sync.WaitGroup
	used int
}

func (c *countingTracker) Go(fn func()) {
	c.mu.Lock()
	c.used++
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		fn()
	}()
}

// TestCallbackAsyncTrackerRoutesGoroutines locks the SIGTERM-safety
// contract: when an AsyncTracker is wired, the callback's fire-and-
// forget DM (and the revoke path) flow through it instead of plain
// `go`. Without this, a SIGTERM during a callback could cut the
// orphan-key revoke off mid-call.
func TestCallbackAsyncTrackerRoutesGoroutines(t *testing.T) {
	cfg := newCallbackCfgOnly(t)
	cfg.SlackClient = &fakeSlackClient{}
	tracker := &countingTracker{}
	cfg.AsyncTracker = tracker

	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	tracker.wg.Wait()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.used == 0 {
		t.Error("AsyncTracker.Go must be called for the success-path DM goroutine")
	}
}

// TestCallbackDMsConfiguredUser asserts that the success-path DM uses
// the userID recovered from the verified state — and only that userID.
func TestCallbackDMsConfiguredUser(t *testing.T) {
	cfg := newCallbackCfgOnly(t)
	posted := make(chan struct{})
	slackClient := &fakeSlackClient{postedCh: posted}
	cfg.SlackClient = slackClient
	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	select {
	case <-posted:
	case <-time.After(time.Second):
		t.Fatal("DM goroutine never fired")
	}
	slackClient.mu.Lock()
	defer slackClient.mu.Unlock()
	if slackClient.gotUser != testUserID {
		t.Errorf("DM user: got %q want %q", slackClient.gotUser, testUserID)
	}
}
