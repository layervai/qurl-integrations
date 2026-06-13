package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/auth"
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

func captureDefaultSlogJSON(t *testing.T) func() []map[string]any {
	t.Helper()
	// Mutates process-global slog state; adding t.Parallel anywhere in this
	// package requires replacing this helper with non-global log capture.
	var buf lockedLogBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		t.Helper()
		var records []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("unmarshal log line %q: %v", line, err)
			}
			records = append(records, rec)
		}
		return records
	}
}

type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeWorkspaceStore captures SetAPIKey calls.
type fakeWorkspaceStore struct {
	mu          sync.Mutex
	existingKey string
	apiKeyErr   error
	apiKeyCalls int
	setArgs     *struct {
		WorkspaceID, APIKey, ConfiguredBy string
	}
	setErr error
}

func (f *fakeWorkspaceStore) APIKey(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apiKeyCalls++
	if f.apiKeyErr != nil {
		return "", f.apiKeyErr
	}
	if f.existingKey == "" {
		return "", auth.ErrWorkspaceNotConfigured
	}
	return f.existingKey, nil
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
	bindingBacked            bool
	mintCalls                int
	mintMu                   sync.Mutex
	revoked                  bool
	revokeMu                 sync.Mutex
	validateErr              error
	validateCalls            int
	validateMu               sync.Mutex
}

func (f *fakeMinter) ValidateAPIKey(_ context.Context, _ string) error {
	f.validateMu.Lock()
	defer f.validateMu.Unlock()
	f.validateCalls++
	return f.validateErr
}
func (f *fakeMinter) MintWorkspaceAPIKey(_ context.Context, _, _ string) (WorkspaceAPIKeyMint, error) {
	f.mintMu.Lock()
	f.mintCalls++
	f.mintMu.Unlock()
	return WorkspaceAPIKeyMint{
		APIKey:        f.apiKey,
		KeyID:         f.keyID,
		KeyPrefix:     f.keyPrefix,
		BindingBacked: f.bindingBacked,
	}, f.mintErr
}
func (f *fakeMinter) RevokeAPIKey(_ context.Context, _, _ string) error {
	f.revokeMu.Lock()
	defer f.revokeMu.Unlock()
	f.revoked = true
	return nil
}

// fakeIDTokenVerifier always returns the configured email/sub or err.
type fakeIDTokenVerifier struct {
	email  string
	sub    string
	err    error
	subErr error
}

func (f *fakeIDTokenVerifier) VerifyEmail(_ context.Context, _ string) (string, error) {
	return f.email, f.err
}

func (f *fakeIDTokenVerifier) VerifySub(_ context.Context, _ string) (string, error) {
	if f.subErr != nil {
		return "", f.subErr
	}
	return f.sub, nil
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
	minter := &fakeMinter{apiKey: testAPIKey, keyID: testKeyID, keyPrefix: testKeyPrefix, bindingBacked: true}

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

func mintTestStateWithEmail(t *testing.T, cfg *Config, email string) string {
	t.Helper()
	state, err := MintStateWithEmail(cfg.OAuthStateSecret, testTeamID, testUserID, email, cfg.Now())
	if err != nil {
		t.Fatalf("MintStateWithEmail: %v", err)
	}
	return state
}

func callbackRequest(state string) *http.Request {
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/qurl/callback?code=abc&state="+url.QueryEscape(state), http.NoBody)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: state, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	return req
}

// assertSecurityHeaders checks the defense-in-depth header set every OAuth-
// callback HTML response must carry. renderSuccess, renderRebindRefused, and
// renderOAuthErrorPage all set the same six; centralizing the assertion means
// a render path that silently drops one fails here instead of slipping past a
// status-and-body-only test.
func assertSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	want := map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"Cache-Control":          "no-store",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
		"X-Content-Type-Options": "nosniff",
	}
	for h, v := range want {
		if got := rec.Header().Get(h); got != v {
			t.Errorf("%s: got %q want %q", h, got, v)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP missing default-src 'none': %q", csp)
	}
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
	assertSecurityHeaders(t, rec)

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

func TestCallbackEmailSetupRequiresMatchingVerifiedEmail(t *testing.T) {
	cfg, _, store, minter := newCallbackCfg(t)
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{email: "different@example.com"}
	state := mintTestStateWithEmail(t, &cfg, "admin@example.com")
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs != nil {
		t.Error("SetAPIKey must not run when the Auth0 email does not match setup state")
	}
	minter.mintMu.Lock()
	defer minter.mintMu.Unlock()
	if minter.mintCalls != 0 {
		t.Errorf("MintWorkspaceAPIKey calls: got %d want 0 on email mismatch", minter.mintCalls)
	}
}

func TestCallbackEmailSetupRequiresNonEmptyVerifiedEmail(t *testing.T) {
	cfg, _, store, minter := newCallbackCfg(t)
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{email: ""}
	state := mintTestStateWithEmail(t, &cfg, "admin@example.com")
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs != nil {
		t.Error("SetAPIKey must not run when Auth0 does not return a verified setup email")
	}
	minter.mintMu.Lock()
	defer minter.mintMu.Unlock()
	if minter.mintCalls != 0 {
		t.Errorf("MintWorkspaceAPIKey calls: got %d want 0 on empty verified email", minter.mintCalls)
	}
}

func TestCallbackEmailSetupAcceptsCaseInsensitiveVerifiedEmail(t *testing.T) {
	cfg, _, store, _ := newCallbackCfg(t)
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{email: "Admin@Example.COM"}
	state := mintTestStateWithEmail(t, &cfg, "admin@example.com")
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs == nil {
		t.Fatal("SetAPIKey not called")
	}
}

// TestTruncateForLogRuneBoundary locks the UTF-8 boundary backup. A
// truncation that splits a multi-byte rune (Auth0 SAML
// error_description is UTF-8; emoji or accented chars are common in
// federated identity providers' error strings) would corrupt the
// slog attribute. The function backs up to a utf8.RuneStart byte.
func TestTruncateForLogRuneBoundary(t *testing.T) {
	// 'é' = 0xC3 0xA9 (2 bytes). Build a string where the limit falls
	// mid-rune, then verify the result ends at a rune boundary.
	prefix := strings.Repeat("a", 10) // 10 ASCII bytes
	s := prefix + "é" + "tail"        // total = 10 + 2 + 4 = 16 bytes
	// limit=11 falls in the middle of the 'é' rune.
	got := truncateForLog(s, 11)
	if !strings.HasSuffix(strings.TrimSuffix(got, "…[truncated]"), prefix) {
		t.Errorf("truncate split a UTF-8 rune: got %q", got)
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("expected truncation marker, got %q", got)
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

// TestOAuthErrorPageHTMLEscapesInterpolations is the symmetric lock for
// renderOAuthErrorPage. Heading/Message are operator-authored today, but the
// template doc comment notes a future caller could pass an upstream string
// through — so the html/template auto-escape is the load-bearing defense if
// that happens. A swap to text/template (or string concat) would let a
// payload render raw; this fails first.
func TestOAuthErrorPageHTMLEscapesInterpolations(t *testing.T) {
	rec := httptest.NewRecorder()
	renderOAuthErrorPage(rec, http.StatusBadGateway,
		"<script>alert('h')</script>", "<img src=x onerror=alert('m')>")
	body := rec.Body.String()
	if strings.Contains(body, "<script>") || strings.Contains(body, "<img src=x") {
		t.Errorf("raw HTML rendered — auto-escape regressed:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected escaped <script> from Heading in body:\n%s", body)
	}
	if !strings.Contains(body, "&lt;img") {
		t.Errorf("expected escaped <img> from Message in body:\n%s", body)
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
	tracker := &countingTracker{}
	cfg.AsyncTracker = tracker
	minter.mintErr = errors.New("qurl-service down")
	state := mintTestState(t, &cfg)

	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d want 502 (mint failure → 502)", rec.Code)
	}
	// The non-limit failure now renders a styled HTML page, not bare
	// http.Error — pin the exact heading so a regression back to plain text
	// (or a reworded heading) is caught. The apostrophe in "Couldn't" is
	// html/template-escaped to &#39;, hence the entity form here.
	if body := rec.Body.String(); !strings.Contains(body, "Couldn&#39;t connect qURL") {
		t.Errorf("502 body should render the styled error page heading; got: %q", body)
	}
	assertSecurityHeaders(t, rec)
	tracker.wg.Wait()
	tracker.mu.Lock()
	used := tracker.used
	tracker.mu.Unlock()
	if used != 0 {
		t.Fatalf("mint failure must not schedule async revoke work; got %d async calls", used)
	}
	minter.revokeMu.Lock()
	defer minter.revokeMu.Unlock()
	if minter.revoked {
		t.Error("RevokeAPIKey must NOT be called when mint itself failed (no keyID exists)")
	}
}

// TestCallbackMintAPIKeyLimitRendersGuidance locks the actionable-error
// contract: when the mint fails because the account is at its API-key cap,
// the callback renders a 409 page that names the limit and tells the admin
// to revoke a key — NOT the generic "run setup again to retry" (which never
// clears a quota). No key is minted, so nothing is persisted.
func TestCallbackMintAPIKeyLimitRendersGuidance(t *testing.T) {
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	minter.mintErr = ErrAPIKeyLimitReached
	state := mintTestState(t, &cfg)

	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))

	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d want 409 (api-key limit → 409)", rec.Code)
	}
	body := strings.ToLower(rec.Body.String())
	if !strings.Contains(body, "limit") || !strings.Contains(body, "revoke") {
		t.Errorf("body should name the key limit and how to clear it (revoke); got: %q", rec.Body.String())
	}
	assertSecurityHeaders(t, rec)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs != nil {
		t.Error("SetAPIKey must NOT run when the mint hit the API-key limit")
	}
}

func TestCallbackExternalIdentityAlreadyBoundRendersRecoveryGuidance(t *testing.T) {
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	minter.mintErr = ErrExternalIdentityAlreadyBound
	state := mintTestState(t, &cfg)

	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))

	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d want 409 (already-bound workspace → 409)", rec.Code)
	}
	body := strings.ToLower(rec.Body.String())
	if !strings.Contains(body, "already connected") || !strings.Contains(body, "administrator") {
		t.Errorf("body should explain the existing connection and recovery path; got: %q", rec.Body.String())
	}
	assertSecurityHeaders(t, rec)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs != nil {
		t.Error("SetAPIKey must NOT run when qurl-service reports an existing workspace binding")
	}
}

func TestCallbackKeepsBindingBackedKeyOnPersistFailure(t *testing.T) {
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	tracker := &countingTracker{}
	cfg.AsyncTracker = tracker
	store.setErr = errors.New("ddb down")
	state := mintTestState(t, &cfg)
	logs := captureDefaultSlogJSON(t)

	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d want 500", rec.Code)
	}
	// Binding-backed keys must stay in qurl-service so the admin can retry
	// setup and replay the binding idempotency record into Slack storage.
	tracker.wg.Wait()
	tracker.mu.Lock()
	used := tracker.used
	tracker.mu.Unlock()
	if used != 0 {
		t.Fatalf("binding-backed persist failure must not schedule async revoke work; got %d async calls", used)
	}
	minter.revokeMu.Lock()
	defer minter.revokeMu.Unlock()
	if minter.revoked {
		t.Error("binding-backed persist failure must not revoke; retry needs the binding record")
	}
	assertSetupBindingPersistFailureLogged(t, logs())
}

func assertSetupBindingPersistFailureLogged(t *testing.T, records []map[string]any) {
	t.Helper()
	matches := 0
	for _, rec := range records {
		if rec["event"] != setupBindingPersistFailureEvent {
			continue
		}
		matches++
		if rec["team_id"] != testTeamID {
			t.Errorf("team_id = %v, want %q", rec["team_id"], testTeamID)
		}
		if rec["key_id"] != testKeyID {
			t.Errorf("key_id = %v, want %q", rec["key_id"], testKeyID)
		}
		if got, ok := rec["error"].(string); !ok || got == "" {
			t.Errorf("error = %v, want non-empty string", rec["error"])
		}
		if rec["retry_window_hours"] != float64(setupBindingRetryWindowHours) {
			t.Errorf("retry_window_hours = %v, want %d", rec["retry_window_hours"], setupBindingRetryWindowHours)
		}
		if rec["cleanup_after_window_hours"] != float64(setupBindingCleanupAfterWindowHours) {
			t.Errorf("cleanup_after_window_hours = %v, want %d", rec["cleanup_after_window_hours"], setupBindingCleanupAfterWindowHours)
		}
		if rec["operator_action"] != setupBindingPersistFailureOperatorAction {
			t.Errorf("operator_action = %v", rec["operator_action"])
		}
	}
	if matches == 0 {
		t.Fatalf("missing %q log event in records: %#v", setupBindingPersistFailureEvent, records)
	}
	if matches > 1 {
		t.Errorf("found %d %q log events, want 1", matches, setupBindingPersistFailureEvent)
	}
}

func TestCallbackRevokesLegacyFallbackKeyOnPersistFailure(t *testing.T) {
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	minter.bindingBacked = false
	store.setErr = errors.New("ddb down")
	state := mintTestState(t, &cfg)

	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d want 500", rec.Code)
	}
	// Legacy fallback has no qurl-service binding replay path, so the
	// unstored key is an orphan and should still be revoked asynchronously.
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
	t.Error("expected RevokeAPIKey to be called after legacy persist failure")
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

// TestExchangeAuth0CodeAcceptsExactCapBody locks the off-by-one fix on
// the body cap: a response that is exactly auth0TokenBodyLimit bytes
// long must NOT be misclassified as truncated. We pad an otherwise-
// valid JSON token response to exactly 8 KiB and assert the success
// path runs.
func TestExchangeAuth0CodeAcceptsExactCapBody(t *testing.T) {
	cfg := newCallbackCfgOnly(t)
	atCapAuth0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Build a payload exactly auth0TokenBodyLimit bytes long. The
		// id_token claim is padding; the parser only reads
		// access_token and id_token, so the padding length only
		// affects byte count.
		core := `{"access_token":"a","id_token":"`
		closing := `"}`
		pad := auth0TokenBodyLimit - len(core) - len(closing)
		if pad < 0 {
			t.Fatalf("payload core too long: %d > limit %d", len(core)+len(closing), auth0TokenBodyLimit)
		}
		body := core + strings.Repeat("x", pad) + closing
		if len(body) != auth0TokenBodyLimit {
			t.Fatalf("setup miscalculated: got %d want %d", len(body), auth0TokenBodyLimit)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(atCapAuth0.Close)
	cfg.HTTPClient = &http.Client{
		Transport: &rewriteTransport{target: atCapAuth0.URL},
		Timeout:   2 * time.Second,
	}
	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusOK {
		t.Errorf("exact-cap body got %d want 200; body=%s", rec.Code, rec.Body.String())
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

const testAdminSub = "auth0|abc123def456"

type fakeAdminStore struct {
	mu        sync.Mutex
	gotTeamID string
	gotOwner  string
	gotSeed   string
	calls     int
	err       error
}

func (f *fakeAdminStore) BindWorkspace(_ context.Context, m *WorkspaceMapping, seedAdmin string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotTeamID = m.TeamID
	f.gotOwner = m.OwnerID
	f.gotSeed = seedAdmin
	return f.err
}

// TestCallbackSeedsAdminOnBind fences the load-bearing collapse: a
// successful /qurl setup must call BindWorkspace with the verified
// state's userID as seedAdmin and the Auth0 sub as OwnerID. Without
// this, the workspace gets an API key but no admin row — every
// subsequent /qurl admin command would reply "you are not an admin."
func TestCallbackSeedsAdminOnBind(t *testing.T) {
	cfg, _, _, _ := newCallbackCfg(t) //nolint:dogsled // intentional discard
	admin := &fakeAdminStore{}
	cfg.AdminStore = admin
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{email: testAdminEmail, sub: testAdminSub}

	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	admin.mu.Lock()
	defer admin.mu.Unlock()
	if admin.calls != 1 {
		t.Fatalf("BindWorkspace calls: got %d want 1", admin.calls)
	}
	if admin.gotTeamID != testTeamID {
		t.Errorf("BindWorkspace TeamID: got %q want %q", admin.gotTeamID, testTeamID)
	}
	// OwnerID is the Slack user ID of the /setup invoker (workspace
	// owner in the LayerV admin model). It must equal verified.UserID
	// — the same value as seedAdmin — by construction at first bind.
	// The id_token sub claim is still required at OAuth-callback time
	// (verifier runs upstream of this) but no longer persisted; the
	// security gate it provides is still in place via the
	// `qurlSub == ""` check in checkBindAllowed.
	if admin.gotOwner != testUserID {
		t.Errorf("BindWorkspace OwnerID: got %q want %q (must equal verified.UserID — the Slack user who ran /setup)", admin.gotOwner, testUserID)
	}
	if admin.gotSeed != testUserID {
		t.Errorf("BindWorkspace seedAdmin: got %q want %q (must come from verified state's userID)", admin.gotSeed, testUserID)
	}
}

// TestCallbackBindFailureSkipsMint fences the bind-before-mint
// invariant: a generic BindWorkspace failure must return 500
// WITHOUT minting an API key or writing to the workspace store.
// Pre-PR the order was mint → persist → bind, which left a revoked
// key in the workspace's DDB row when bind classified as a generic
// failure. Now the bind runs first; if it fails we never reach the
// mint, so the existing key state is untouched.
func TestCallbackBindFailureSkipsMint(t *testing.T) {
	cfg, store := newCallbackCfgStore(t)
	admin := &fakeAdminStore{err: errors.New("ddb timeout")}
	cfg.AdminStore = admin
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{email: testAdminEmail, sub: testAdminSub}
	cfg.BindClassifyError = func(_ error) BindConflictCode { return "" }

	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs != nil {
		t.Error("SetAPIKey must not run when BindWorkspace failed (would overwrite an existing admin's key row with one we can't even prove ownership of)")
	}
}

// TestCallbackBindIdempotentForSameCallerReusesExistingKey fences the leak
// fix: the same Slack user re-running /qurl setup must succeed without
// minting a second qurl-service API key when the workspace already has a
// healthy stored key.
func TestCallbackBindIdempotentForSameCallerReusesExistingKey(t *testing.T) {
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	store.existingKey = testAPIKey
	// The error text is irrelevant — the stubbed classifier returns
	// the same-caller code regardless. Use sentinel text so a future
	// reader doesn't think production reads the message string.
	admin := &fakeAdminStore{err: errors.New("classified as same-caller")}
	cfg.AdminStore = admin
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{email: testAdminEmail, sub: testAdminSub}
	cfg.BindClassifyError = func(_ error) BindConflictCode { return BindConflictAlreadyBoundToCaller }

	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (idempotent reuse, body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), testKeyPrefix) {
		t.Errorf("reuse success body should render a safe key prefix, got: %s", rec.Body.String())
	}
	store.mu.Lock()
	if store.setArgs != nil {
		t.Fatal("SetAPIKey must not run when a valid existing key can be reused")
	}
	if store.apiKeyCalls != 1 {
		t.Errorf("APIKey calls: got %d want 1", store.apiKeyCalls)
	}
	store.mu.Unlock()
	minter.mintMu.Lock()
	if minter.mintCalls != 0 {
		t.Errorf("MintWorkspaceAPIKey calls: got %d want 0 when existing key is valid", minter.mintCalls)
	}
	minter.mintMu.Unlock()
	minter.validateMu.Lock()
	if minter.validateCalls != 1 {
		t.Errorf("ValidateAPIKey calls: got %d want 1", minter.validateCalls)
	}
	minter.validateMu.Unlock()
	minter.revokeMu.Lock()
	defer minter.revokeMu.Unlock()
	if minter.revoked {
		t.Error("idempotent reuse must not revoke")
	}
}

// TestCallbackBindIdempotentForSameCallerMintsWhenKeyMissing preserves the
// recovery path for a bind-only workspace state: if an earlier setup seeded
// workspace_mappings but failed before storing a workspace key, the owner can
// re-run setup and mint the missing key.
func TestCallbackBindIdempotentForSameCallerMintsWhenKeyMissing(t *testing.T) {
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	admin := &fakeAdminStore{err: errors.New("classified as same-caller")}
	cfg.AdminStore = admin
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{email: testAdminEmail, sub: testAdminSub}
	cfg.BindClassifyError = func(_ error) BindConflictCode { return BindConflictAlreadyBoundToCaller }

	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (missing-key recovery, body=%s)", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	if store.setArgs == nil {
		t.Fatal("SetAPIKey must run when no stored workspace key exists")
	}
	if store.setArgs.APIKey != testAPIKey {
		t.Errorf("SetAPIKey APIKey: got %q want %q", store.setArgs.APIKey, testAPIKey)
	}
	store.mu.Unlock()
	minter.mintMu.Lock()
	defer minter.mintMu.Unlock()
	if minter.mintCalls != 1 {
		t.Errorf("MintWorkspaceAPIKey calls: got %d want 1", minter.mintCalls)
	}
}

func TestCallbackInvalidStoredKeyMintsReplacement(t *testing.T) {
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	store.existingKey = "lv_live_revoked"
	minter.validateErr = ErrStoredAPIKeyInvalid
	state := mintTestState(t, &cfg)

	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (invalid stored key should be replaceable, body=%s)", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	if store.setArgs == nil {
		t.Fatal("SetAPIKey must run after replacing an invalid stored key")
	}
	if store.setArgs.APIKey != testAPIKey {
		t.Errorf("SetAPIKey APIKey: got %q want %q", store.setArgs.APIKey, testAPIKey)
	}
	store.mu.Unlock()
	minter.mintMu.Lock()
	defer minter.mintMu.Unlock()
	if minter.mintCalls != 1 {
		t.Errorf("MintWorkspaceAPIKey calls: got %d want 1", minter.mintCalls)
	}
}

func TestCallbackStoredKeyValidationFailureDoesNotMint(t *testing.T) {
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	store.existingKey = "lv_live_existing"
	minter.validateErr = errors.New("qurl-service timeout")
	state := mintTestState(t, &cfg)

	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502 (transient validation failure, body=%s)", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	if store.setArgs != nil {
		t.Fatal("SetAPIKey must not run when stored-key validation had a transient failure")
	}
	store.mu.Unlock()
	minter.mintMu.Lock()
	defer minter.mintMu.Unlock()
	if minter.mintCalls != 0 {
		t.Errorf("MintWorkspaceAPIKey calls: got %d want 0 on transient validation failure", minter.mintCalls)
	}
}

func TestCallbackStoredKeyForbiddenDoesNotMint(t *testing.T) {
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	store.existingKey = "lv_live_existing"
	minter.validateErr = errors.New("qurl-service GET /v1/quota returned 403")
	state := mintTestState(t, &cfg)

	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502 (403 validation failure must not mint, body=%s)", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	if store.setArgs != nil {
		t.Fatal("SetAPIKey must not run when stored-key validation returned 403")
	}
	store.mu.Unlock()
	minter.mintMu.Lock()
	defer minter.mintMu.Unlock()
	if minter.mintCalls != 0 {
		t.Errorf("MintWorkspaceAPIKey calls: got %d want 0 on 403 validation failure", minter.mintCalls)
	}
}

func TestCallbackStoredKeyLookupFailureDoesNotMint(t *testing.T) {
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	store.apiKeyErr = errors.New("kms decrypt failed")
	state := mintTestState(t, &cfg)

	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500 (stored-key lookup failure, body=%s)", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	if store.setArgs != nil {
		t.Fatal("SetAPIKey must not run when stored-key lookup failed")
	}
	store.mu.Unlock()
	minter.mintMu.Lock()
	defer minter.mintMu.Unlock()
	if minter.mintCalls != 0 {
		t.Errorf("MintWorkspaceAPIKey calls: got %d want 0 on stored-key lookup failure", minter.mintCalls)
	}
}

func TestStoredAPIKeyPrefix(t *testing.T) {
	if got := storedAPIKeyPrefix("  " + testAPIKey + "  "); got != testKeyPrefix {
		t.Fatalf("storedAPIKeyPrefix = %q, want %q", got, testKeyPrefix)
	}
	for _, short := range []string{"", "lv_live_abcd", "short"} {
		if got := storedAPIKeyPrefix(short); got != "" {
			t.Errorf("storedAPIKeyPrefix(%q) = %q, want empty (must not expose a whole short key)", short, got)
		}
	}
}

// TestCallbackBindRefusedForDifferentAdmin fences the cross-admin
// rebind guard. A different Slack user attempting to install into
// an already-bound workspace must NOT overwrite the admin set or
// the workspace's stored API key; instead the rebind-refused page
// renders and mint is skipped entirely. (Pre-reorder this path
// would mint K2, overwrite K1 in DDB, then revoke K2 — bricking
// the workspace for the existing admin. The reorder makes that
// failure mode impossible by construction.)
func TestCallbackBindRefusedForDifferentAdmin(t *testing.T) {
	cfg, store := newCallbackCfgStore(t)
	admin := &fakeAdminStore{err: errors.New("workspace already bound")}
	cfg.AdminStore = admin
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{email: testAdminEmail, sub: testAdminSub}
	cfg.BindClassifyError = func(_ error) BindConflictCode { return BindConflictAlreadyBound }

	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusConflict {
		t.Errorf("status: got %d want 409 (rebind-refused page)", rec.Code)
	}
	// Fence the page body: a refactor swapping renderRebindRefused
	// for a bare http.Error with status 409 would slip past the
	// status-only check and leave operators staring at the default
	// error string instead of the rebind-refused copy.
	if !strings.Contains(rec.Body.String(), "qURL setup blocked") {
		t.Errorf("rebind-refused page body missing 'qURL setup blocked' headline: %s", rec.Body.String())
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs != nil {
		t.Error("SetAPIKey must not run on rebind-refused — would overwrite the existing admin's encrypted key")
	}
}

// TestCallbackBindRefusedWhenUnverified fences the unverified-conflict
// posture: when the post-CCFE disambiguation can't read the existing
// row's admin set, BindConflictUnverified arrives. The handler must
// treat it the same as the cross-admin rebind (refuse + 409), not
// as the idempotent same-caller case — the safer default is to
// refuse than to potentially overwrite. The two cases share a switch
// arm today, but this fence prevents a future split that silently
// downgrades unverified to "idempotent success."
func TestCallbackBindRefusedWhenUnverified(t *testing.T) {
	cfg, store := newCallbackCfgStore(t)
	admin := &fakeAdminStore{err: errors.New("could not confirm bind")}
	cfg.AdminStore = admin
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{email: testAdminEmail, sub: testAdminSub}
	cfg.BindClassifyError = func(_ error) BindConflictCode { return BindConflictUnverified }

	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusConflict {
		t.Errorf("status: got %d want 409 (rebind-refused page, unverified arm)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "qURL setup blocked") {
		t.Errorf("rebind-refused page body missing 'qURL setup blocked' headline: %s", rec.Body.String())
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs != nil {
		t.Error("SetAPIKey must not run on unverified-rebind — same posture as the cross-admin arm")
	}
}

// TestCallbackBindSkippedWhenSubMissing fences the hard-failure
// posture: if the id_token's sub claim can't be verified, we have
// no OwnerID to bind with. The handler must NOT silently skip the
// bind (which would half-install the workspace) and — under the
// bind-before-mint ordering — must also skip the mint entirely
// rather than mint then revoke.
func TestCallbackBindSkippedWhenSubMissing(t *testing.T) {
	cfg, store := newCallbackCfgStore(t)
	admin := &fakeAdminStore{}
	cfg.AdminStore = admin
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{email: testAdminEmail, subErr: errors.New("jwks failed")}

	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500 (cannot bind without sub)", rec.Code)
	}
	admin.mu.Lock()
	if admin.calls != 0 {
		t.Errorf("BindWorkspace must not be called with empty OwnerID; got %d calls", admin.calls)
	}
	admin.mu.Unlock()

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs != nil {
		t.Error("SetAPIKey must not run when sub is unavailable — bind-before-mint reorder gates the mint on bind eligibility")
	}
}

// TestCallbackBindSucceedsThenMintFails fences the recovery path
// after the bind-before-mint reorder. With bind running first, a
// fresh install where bind succeeds but mint then 502s leaves the
// workspace_mappings row seeded but no workspace_keys row. The user
// re-runs /qurl setup; bind returns BindConflictAlreadyBoundToCaller
// (idempotent), the callback continues to mint, mint now succeeds,
// and the workspace lands in the fully-installed state. Without
// this fence the new ordering's recovery story rests on inspection
// alone.
func TestCallbackBindSucceedsThenMintFails(t *testing.T) {
	// First attempt: bind succeeds (admin.err=nil), mint fails (502).
	cfg, store, minter := newCallbackCfgStoreMinter(t)
	admin := &fakeAdminStore{}
	cfg.AdminStore = admin
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{email: testAdminEmail, sub: testAdminSub}
	minter.mintErr = errors.New("simulated qurl-service 502")

	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("first attempt status: got %d want 502 (mint failure)", rec.Code)
	}
	admin.mu.Lock()
	if admin.calls != 1 || admin.gotSeed != testUserID {
		t.Errorf("first attempt: BindWorkspace must run BEFORE mint and seed the admin row (calls=%d seed=%q)", admin.calls, admin.gotSeed)
	}
	admin.mu.Unlock()
	store.mu.Lock()
	if store.setArgs != nil {
		t.Error("first attempt: SetAPIKey must NOT run when mint failed")
	}
	store.mu.Unlock()

	// Second attempt: same caller re-runs /qurl setup. Bind now
	// returns the same-caller-already-bound classifier code; mint
	// succeeds and the missing workspace key is stored. This is the
	// documented recovery.
	admin.err = errors.New("classified as same-caller")
	cfg.BindClassifyError = func(_ error) BindConflictCode { return BindConflictAlreadyBoundToCaller }
	minter.mintErr = nil
	state2 := mintTestState(t, &cfg)
	h2 := Callback(cfg)
	rec2 := httptest.NewRecorder()
	h2(rec2, callbackRequest(state2))
	if rec2.Code != http.StatusOK {
		t.Fatalf("retry status: got %d want 200 (recovery path, body=%s)", rec2.Code, rec2.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs == nil {
		t.Fatal("retry: SetAPIKey must run after bind classifies as same-caller and mint succeeds")
	}
	if store.setArgs.APIKey != testAPIKey {
		t.Errorf("retry: SetAPIKey APIKey: got %q want %q", store.setArgs.APIKey, testAPIKey)
	}
}

// TestCallbackSkipsBindWhenAdminStoreNil fences the sandbox / no-DDB
// contract: AdminStore=nil is the documented degraded path (cmd/main.go
// surfaces it when slackdata.NewStore fails). The callback must still
// complete the mint + render the success page so the API-key surface
// stays functional; the bind is logged-and-skipped.
func TestCallbackSkipsBindWhenAdminStoreNil(t *testing.T) {
	cfg, store, _ := newCallbackCfgStoreMinter(t)
	cfg.AdminStore = nil
	cfg.IDTokenVerifier = &fakeIDTokenVerifier{email: testAdminEmail, sub: testAdminSub}

	state := mintTestState(t, &cfg)
	h := Callback(cfg)
	rec := httptest.NewRecorder()
	h(rec, callbackRequest(state))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (sandbox path must render success, body=%s)", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.setArgs == nil {
		t.Error("SetAPIKey must still run in the sandbox path so the API-key surface is functional")
	}
}
