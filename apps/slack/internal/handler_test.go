package internal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testSigningSecret     = "test-secret"
	setupAdminExampleText = "setup admin@example.com"
)

// noopQURLServer is a stand-in upstream that 200s every request. Tests
// that exercise routing/auth (not the QURL API contract) use this so the
// handler can construct a *client.Client without making real network calls.
func noopQURLServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// countingQURLServer is like noopQURLServer but exposes the number of
// requests it received. Used by negative-path tests that want to fence
// "no upstream call leaked through" in addition to "401 returned".
func countingQURLServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// fixedNow pins the handler's clock so every signed-request test produces
// a stable timestamp. Arbitrary absolute value — tests inject h.now so the
// wall clock is irrelevant; this constant just needs to be the same in both
// sign-time and verify-time paths for any given test.
var fixedNow = time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

func newTestHandler(t *testing.T, qurlServer *httptest.Server) *Handler {
	t.Helper()
	h := NewHandler(Config{
		AuthProvider:       &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		SlackSigningSecret: testSigningSecret,
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlServer.URL, apiKey)
		},
	})
	h.now = func() time.Time { return fixedNow }
	// Tests target httptest servers (http://127.0.0.1:NNNNN) that the
	// production validator rejects. Override here so the async path
	// runs end-to-end; the production validator gets its own table
	// test in process_test.go.
	h.validateResponseURLFn = url.Parse
	// LIFO: this drain runs before any httptest server cleanup, so a
	// goroutine still mid-call to qurlServer doesn't race the close.
	t.Cleanup(h.Wait)
	return h
}

type recordingAuthProvider struct {
	apiKey            string
	deleteErr         error
	deleteCalls       int
	deleteWorkspaceID string
}

func (p *recordingAuthProvider) APIKey(_ context.Context, _ string) (string, error) {
	if p.apiKey == "" {
		return "", auth.ErrWorkspaceNotConfigured
	}
	return p.apiKey, nil
}

func (p *recordingAuthProvider) DeleteAPIKey(_ context.Context, workspaceID string) error {
	p.deleteCalls++
	p.deleteWorkspaceID = workspaceID
	return p.deleteErr
}

func newUninstallAdminTestHandler(t *testing.T, provider *recordingAuthProvider) *Handler {
	t.Helper()
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.cfg.AuthProvider = provider
	return h
}

func slashUninstallAsAdmin(t *testing.T, h *Handler) map[string]string {
	t.Helper()
	return slashResponseForWorkspaceUser(t, h, commandUser, uninstallVerb, testAdminTeamID, testAdminUserID)
}

// signSlackBody returns the pair of headers Slack would send to authenticate
// `body` at `fixedNow`. Using the same algorithm as the handler means any
// drift between them gets caught by the verification tests themselves.
func signSlackBody(t *testing.T, body string) (sig, ts string) {
	t.Helper()
	ts = strconv.FormatInt(fixedNow.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(slackSignatureVersion + ":" + ts + ":" + body))
	sig = slackSignatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
	return sig, ts
}

// newSignedRequest builds a request to `path` carrying `body` and the
// matching signature/timestamp headers for `body`. Caller-supplied
// `signBody` (if non-empty) is what gets signed — used by tamper tests
// where the wire body differs from the signed body.
func newSignedRequest(t *testing.T, path, body, signBody string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if signBody != "" {
		sig, ts := signSlackBody(t, signBody)
		r.Header.Set(headerSlackSignature, sig)
		r.Header.Set(headerSlackTimestamp, ts)
	}
	return r
}

func TestHealthEndpoint(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", http.NoBody))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHealthEndpoint_Returns503WhenUnhealthy(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	h.SetHealthy(false)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", http.NoBody))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /health while unhealthy: status = %d, want 503", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, `"status":"draining"`) {
		t.Fatalf("GET /health while unhealthy body = %q, want draining status", got)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodHead, "/health", http.NoBody))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("HEAD /health while unhealthy: status = %d, want 503", w.Code)
	}

	h.SetHealthy(true)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", http.NoBody))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /health after SetHealthy(true): status = %d, want 200", w.Code)
	}
}

func TestSlashCommandHelp(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	body := url.Values{
		"command": {"/qurl"},
		"text":    {"help"},
		"team_id": {"T123"},
	}.Encode()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["text"] == "" {
		t.Error("expected non-empty help text")
	}
}

// slashResponse drives a signed slash-command request for (command, text)
// and returns the JSON response envelope.
func slashResponse(t *testing.T, h *Handler, command, text string) map[string]string {
	t.Helper()
	return slashResponseForWorkspaceUser(t, h, command, text, "T123ABCDEF", "U_ADMIN1")
}

func slashResponseForWorkspaceUser(t *testing.T, h *Handler, command, text, teamID, userID string) map[string]string {
	t.Helper()
	body := url.Values{
		fieldCommand:   {command},
		fieldText:      {text},
		fieldTeamID:    {teamID},
		fieldUserID:    {userID},
		fieldChannelID: {"C123"},
		fieldTriggerID: {"trig-split"},
	}.Encode()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("command=%q text=%q: status = %d, want 200; body=%s", command, text, w.Code, w.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return result
}

// slashReply returns only the Slack response text. Used by the dispatch-split
// tests below to assert that `command` (/qurl vs /qurl-admin) routes each verb
// to the right surface.
func slashReply(t *testing.T, h *Handler, command, text string) string {
	t.Helper()
	return slashResponse(t, h, command, text)[respFieldText]
}

// TestDispatchSplit_HelpPerCommand fences the help-text split: `/qurl
// help` advertises only the user verbs (and routes admins onward), while
// `/qurl-admin help` advertises only the admin verbs. A regression that
// merged the two back into one help message — or pointed either at the
// wrong verb set — fails here.
func TestDispatchSplit_HelpPerCommand(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))

	userHelp := slashReply(t, h, commandUser, "help")
	// `/qurl list` is an unconditional user verb; `/qurl get` and `/qurl
	// aliases` gate on AdminStore (not wired here) — their gating is fenced
	// by TestUserHelpGatesGetAndAliasesOnAdminStore.
	if !strings.Contains(userHelp, "/qurl list") {
		t.Errorf("/qurl help missing user verbs: %q", userHelp)
	}
	// setup is a user verb (first-come-claims) — the user surface must
	// advertise it so the first claimant of an unbound workspace can find
	// it.
	if !strings.Contains(userHelp, "/qurl setup <email>") {
		t.Errorf("/qurl help missing setup verb: %q", userHelp)
	}
	if !strings.Contains(userHelp, "/qurl uninstall") {
		t.Errorf("/qurl help missing uninstall verb: %q", userHelp)
	}
	if !strings.Contains(userHelp, "/qurl-admin help") {
		t.Errorf("/qurl help should route admins to /qurl-admin help: %q", userHelp)
	}
	// User help must NOT advertise admin verbs as runnable commands —
	// they live on /qurl-admin. Check for the command-line forms (the
	// `/qurl-admin protect-connector` advert and the `/qurl set-alias`
	// bullet), not bare words.
	for _, leaked := range []string{"/qurl-admin protect-connector", "/qurl set-alias", "/qurl-admin admin", "/qurl-admin set-alias"} {
		if strings.Contains(userHelp, leaked) {
			t.Errorf("/qurl help leaked admin command %q: %q", leaked, userHelp)
		}
	}

	adminHelp := slashReply(t, h, commandAdmin, "help")
	// The always-present admin-help line (the optional verb blocks are
	// gated on sandbox wiring, which newTestHandler doesn't supply).
	if !strings.Contains(adminHelp, "/qurl-admin help") {
		t.Errorf("/qurl-admin help missing its own help line: %q", adminHelp)
	}
	// Admin help must NOT advertise the setup verb (it's a user verb now)
	// or the user mint verbs. Match the command forms — bare "setup" would
	// false-positive on the "Guided tunnel setup" copy.
	for _, leaked := range []string{"/qurl-admin setup", "/qurl setup", "/qurl get"} {
		if strings.Contains(adminHelp, leaked) {
			t.Errorf("/qurl-admin help leaked user verb %q: %q", leaked, adminHelp)
		}
	}
	// `/qurl-admin help` must render the admin help, NOT a wrong-surface
	// redirect. `help` is dispatched explicitly at the top of each surface,
	// not via the userVerbs list, so a maintainer who "consistency"-adds
	// `help` to userVerbs (making isUserVerb("help") true) would route it to
	// the user-verb redirect here — "belongs on `/qurl`" — instead of the
	// admin help. Pin that it doesn't.
	if strings.Contains(adminHelp, "belongs on") {
		t.Errorf("/qurl-admin help was redirected instead of rendering admin help: %q", adminHelp)
	}
}

// TestDispatchSplit_WrongSurfaceRedirects fences the friendly redirects:
// an admin verb typed on `/qurl` points the user at `/qurl-admin`, and a
// user verb typed on `/qurl-admin` points the user at `/qurl`. Without
// these the user would get the bare "unknown subcommand" reply.
func TestDispatchSplit_WrongSurfaceRedirects(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))

	// Admin verbs on /qurl → redirect to /qurl-admin. (setup is NOT here:
	// it's a user verb now, so `/qurl setup` is handled, not redirected.)
	for _, text := range []string{"protect", "protect-connector foo", "set-alias $a $b", "unset-alias $a", "admin list"} {
		reply := slashReply(t, h, commandUser, text)
		if !strings.Contains(reply, "admin command") || !strings.Contains(reply, "/qurl-admin") {
			t.Errorf("/qurl %q: want admin-command redirect, got %q", text, reply)
		}
	}

	// User verbs on /qurl-admin → redirect to /qurl.
	for _, text := range []string{string(SubcmdGet) + " $prod-db", string(SubcmdList), string(SubcmdAliases), setupAdminExampleText, uninstallVerb} {
		reply := slashReply(t, h, commandAdmin, text)
		if !strings.Contains(reply, "belongs on `/qurl`") || !strings.Contains(reply, "/qurl ") {
			t.Errorf("/qurl-admin %q: want /qurl-command redirect, got %q", text, reply)
		}
	}
	reply := slashReply(t, h, commandAdmin, "setup")
	if !strings.Contains(reply, "Use `/qurl setup <email>` instead") {
		t.Errorf("/qurl-admin setup: want email-required redirect, got %q", reply)
	}
}

// TestDispatchSplit_UnknownCommandDefaultsToUserSurface fences the
// defensive default: an unrecognized `command` value (Slack only sends
// the two we register, so this is a misconfiguration / probe) routes to
// the user surface, which never mutates admin state. `help` on an unknown
// command yields the user help.
func TestDispatchSplit_UnknownCommandDefaultsToUserSurface(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	reply := slashReply(t, h, "/qurl-bogus", "help")
	// Falls back to the user surface. The user help header is
	// command-name-agnostic (the invoked command name is echoed into the
	// verb lines), so assert on it rather than a literal command token.
	if !strings.Contains(reply, "Create and manage qURLs from Slack") {
		t.Errorf("unknown command did not fall back to user help: %q", reply)
	}
	if strings.Contains(reply, "Admin commands for qURL in Slack") {
		t.Errorf("unknown command rendered the admin surface: %q", reply)
	}
}

// TestDispatchSplit_EmptyCommandDefaultsToUserHelp fences the empty-command
// normalization in handleSlashCommand: a malformed/synthetic payload with no
// `command` field is coerced to commandUser, so it renders /qurl user help
// (the safe read-only surface) with the prod command name rather than a
// dangling empty name from the ReplaceAll rewrite.
func TestDispatchSplit_EmptyCommandDefaultsToUserHelp(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	reply := slashReply(t, h, "", "help")
	if !strings.Contains(reply, "Create and manage qURLs from Slack") {
		t.Errorf("empty command did not fall back to user help: %q", reply)
	}
	if !strings.Contains(reply, "/qurl list") {
		t.Errorf("empty command did not render the prod /qurl command name: %q", reply)
	}
}

// TestCommandNameConstantsStayInSync pins the invariant the help-text
// ReplaceAll rewrite leans on: the admin command is exactly the user command
// plus adminCommandSuffix. A rename of either constant that desynced this
// would silently break adminCommandName and the non-prod help rewrite — fail
// here first.
func TestCommandNameConstantsStayInSync(t *testing.T) {
	if commandAdmin != commandUser+adminCommandSuffix {
		t.Errorf("commandAdmin %q != commandUser %q + adminCommandSuffix %q", commandAdmin, commandUser, adminCommandSuffix)
	}
}

// TestHelpMessagesContainOnlyCommandTokens guards the MAINTAINER INVARIANT
// documented on userHelpMessage / adminHelpMessage: the help lines are
// authored with prod command names and rewritten to the invoked command via
// a blind strings.ReplaceAll, so every `/qurl…` token in the rendered help
// must be a real command literal (`/qurl` or `/qurl-admin`). A future help
// line introducing a non-command slash token — a `/qurl-docs` link, a
// `/qurl-foo` example — would be silently mangled in a non-prod env (where
// the command is named e.g. `/qurl-sandbox`). Fail here instead of shipping
// a garbled help message.
func TestHelpMessagesContainOnlyCommandTokens(t *testing.T) {
	// Fully wire the handler (aliasStore + AdminStore + OpenView) so every
	// gated help line renders — maximizes the set of tokens under test.
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	// Matches `/qurl` plus the run of bytes up to the next formatting
	// delimiter (space, the `*` bold marker, or a backtick fence). Captured
	// broadly — not just [a-z0-9-] — so a mangle-able token like `/qurl_admin`
	// or `/qurl.docs` (which would otherwise slip past as a bare, allowed
	// `/qurl`) surfaces as its own non-allowed token and fails here.
	tokenRe := regexp.MustCompile(`/qurl[^\s*\x60]*`) // \x60 = backtick fence
	allowed := map[string]bool{commandUser: true, commandAdmin: true}

	for _, tc := range []struct {
		surface  string
		rendered string
	}{
		{"user", h.userHelpMessage(commandUser)},
		{"admin", h.adminHelpMessage(commandAdmin)},
	} {
		for _, tok := range tokenRe.FindAllString(tc.rendered, -1) {
			if !allowed[tok] {
				t.Errorf("%s help contains non-command slash token %q; the blind ReplaceAll rewrite would mangle it in a non-prod env (see the MAINTAINER INVARIANT on userHelpMessage)", tc.surface, tok)
			}
		}
	}
}

// TestDispatchSplit_NonProdCommandNamesRouteBySuffix fences env-prefix
// routing: a non-prod install whose commands carry an infix
// (`/qurl-sandbox`, `/qurl-sandbox-admin`) must route admin verbs to the
// admin surface via the `-admin` suffix — a literal `/qurl-admin` match
// would send `/qurl-sandbox-admin` down the user path and make every admin
// verb unreachable. Redirects and help must name the invoked command, not
// the prod `/qurl` / `/qurl-admin`.
func TestDispatchSplit_NonProdCommandNamesRouteBySuffix(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))

	// Admin verb on the sandbox admin command reaches the admin surface
	// (here: the no-AdminStore "not configured" reply from the verb body),
	// NOT the user-surface "is an admin command" redirect.
	reply := slashReply(t, h, "/qurl-sandbox-admin", "set-alias $a $b")
	if strings.Contains(reply, "is an admin command") {
		t.Errorf("/qurl-sandbox-admin set-alias hit the user-surface redirect (suffix routing broken): %q", reply)
	}

	// User verb on the sandbox admin command → redirect names the sandbox
	// user command, not the prod /qurl.
	reply = slashReply(t, h, "/qurl-sandbox-admin", "get $x")
	if !strings.Contains(reply, "/qurl-sandbox get") {
		t.Errorf("user-verb redirect should name /qurl-sandbox, got %q", reply)
	}
	if strings.Contains(reply, "/qurl-admin") {
		t.Errorf("redirect leaked the prod admin command name: %q", reply)
	}

	// Admin verb on the sandbox user command → redirect names the sandbox
	// admin command.
	reply = slashReply(t, h, "/qurl-sandbox", "protect-connector foo")
	if !strings.Contains(reply, "/qurl-sandbox-admin protect-connector") {
		t.Errorf("admin-verb redirect should name /qurl-sandbox-admin, got %q", reply)
	}

	// Help renders the invoked (sandbox) command names. Assert on the
	// unconditional `/qurl-sandbox list` verb (get/aliases gate on AdminStore,
	// not wired here) to prove the command-name rewrite.
	if userHelp := slashReply(t, h, "/qurl-sandbox", "help"); !strings.Contains(userHelp, "/qurl-sandbox list") {
		t.Errorf("/qurl-sandbox help should render the sandbox command name, got %q", userHelp)
	}
	if adminHelp := slashReply(t, h, "/qurl-sandbox-admin", "help"); !strings.Contains(adminHelp, "/qurl-sandbox-admin help") {
		t.Errorf("/qurl-sandbox-admin help should render the sandbox command name, got %q", adminHelp)
	}
}

func TestSlashCommandUninstallDeletesWorkspaceAPIKey(t *testing.T) {
	provider := &recordingAuthProvider{apiKey: "test-key"}
	h := newUninstallAdminTestHandler(t, provider)

	resp := slashUninstallAsAdmin(t, h)

	if provider.deleteCalls != 1 {
		t.Fatalf("DeleteAPIKey calls = %d, want 1", provider.deleteCalls)
	}
	if provider.deleteWorkspaceID != testAdminTeamID {
		t.Fatalf("DeleteAPIKey workspaceID = %q, want %q", provider.deleteWorkspaceID, testAdminTeamID)
	}
	if resp[respFieldResponseType] != respTypeEphemeral {
		t.Fatalf("response_type = %q, want %q", resp[respFieldResponseType], respTypeEphemeral)
	}
	if !strings.Contains(resp[respFieldText], "disconnected from this workspace") {
		t.Fatalf("uninstall reply missing confirmation: %q", resp[respFieldText])
	}
}

func TestSlashCommandUninstallNotConfigured(t *testing.T) {
	provider := &recordingAuthProvider{
		apiKey:    "test-key",
		deleteErr: auth.ErrWorkspaceNotConfigured,
	}
	h := newUninstallAdminTestHandler(t, provider)

	resp := slashUninstallAsAdmin(t, h)

	if provider.deleteCalls != 1 {
		t.Fatalf("DeleteAPIKey calls = %d, want 1", provider.deleteCalls)
	}
	if resp[respFieldResponseType] != respTypeEphemeral {
		t.Fatalf("response_type = %q, want %q", resp[respFieldResponseType], respTypeEphemeral)
	}
	if !strings.Contains(resp[respFieldText], "isn't currently connected") {
		t.Fatalf("uninstall reply missing not-connected message: %q", resp[respFieldText])
	}
}

func TestSlashCommandUninstallUnsupportedProvider(t *testing.T) {
	provider := &recordingAuthProvider{
		apiKey:    "test-key",
		deleteErr: auth.ErrWorkspaceAPIKeyDeleteUnsupported,
	}
	h := newUninstallAdminTestHandler(t, provider)

	resp := slashUninstallAsAdmin(t, h)

	if provider.deleteCalls != 1 {
		t.Fatalf("DeleteAPIKey calls = %d, want 1", provider.deleteCalls)
	}
	if !strings.Contains(resp[respFieldText], "environment-backed qURL key") {
		t.Fatalf("unsupported-provider reply missing deployment hint: %q", resp[respFieldText])
	}
}

func TestSlashCommandUninstallDeleteFailure(t *testing.T) {
	provider := &recordingAuthProvider{
		apiKey:    "test-key",
		deleteErr: errors.New("delete failed"),
	}
	h := newUninstallAdminTestHandler(t, provider)

	resp := slashUninstallAsAdmin(t, h)

	if provider.deleteCalls != 1 {
		t.Fatalf("DeleteAPIKey calls = %d, want 1", provider.deleteCalls)
	}
	if !strings.Contains(resp[respFieldText], "could not disconnect qURL") {
		t.Fatalf("delete failure reply missing retry message: %q", resp[respFieldText])
	}
}

func TestSlashCommandUninstallAuthProviderNotConfigured(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	h.cfg.AuthProvider = nil

	resp := slashResponse(t, h, commandUser, uninstallVerb)

	if !strings.Contains(resp[respFieldText], "qURL credential storage is not configured") {
		t.Fatalf("nil-provider reply missing operator hint: %q", resp[respFieldText])
	}
}

func TestSlashCommandUninstallRequiresOwnerStoreForMutableProvider(t *testing.T) {
	provider := &recordingAuthProvider{apiKey: "test-key"}
	h := newTestHandler(t, noopQURLServer(t))
	h.cfg.AuthProvider = provider

	resp := slashResponse(t, h, commandUser, uninstallVerb)

	if provider.deleteCalls != 0 {
		t.Fatalf("DeleteAPIKey calls = %d, want 0", provider.deleteCalls)
	}
	if !strings.Contains(resp[respFieldText], "owner verification is not configured") {
		t.Fatalf("missing-owner-store reply missing fail-closed hint: %q", resp[respFieldText])
	}
}

func TestSlashCommandUninstallEnvProviderWithoutOwnerStore(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	h := newTestHandler(t, noopQURLServer(t))

	resp := slashResponse(t, h, commandUser, uninstallVerb)

	if !strings.Contains(resp[respFieldText], "environment-backed qURL key") {
		t.Fatalf("env-provider reply missing unsupported hint: %q", resp[respFieldText])
	}
}

func TestSlashCommandUninstallRejectsUnexpectedArgs(t *testing.T) {
	provider := &recordingAuthProvider{apiKey: "test-key"}
	h := newTestHandler(t, noopQURLServer(t))
	h.cfg.AuthProvider = provider

	resp := slashResponse(t, h, commandUser, uninstallVerb+" now")

	if provider.deleteCalls != 0 {
		t.Fatalf("DeleteAPIKey calls = %d, want 0", provider.deleteCalls)
	}
	if !strings.Contains(resp[respFieldText], "Usage: `/qurl uninstall`.") {
		t.Fatalf("uninstall args reply missing usage: %q", resp[respFieldText])
	}
}

func TestSlashCommandUninstallFailsClosedWhenOwnerCheckErrors(t *testing.T) {
	provider := &recordingAuthProvider{apiKey: "test-key"}
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.ddb.SetGetItemErr(ts.tableNames.workspace, errors.New("workspace mapping read failed"))
	h := newAdminTestHandler(t, ts)
	h.cfg.AuthProvider = provider

	resp := slashResponseForWorkspaceUser(t, h, commandUser, uninstallVerb, testAdminTeamID, testAdminUserID)

	if provider.deleteCalls != 0 {
		t.Fatalf("DeleteAPIKey calls = %d, want 0", provider.deleteCalls)
	}
	if !strings.Contains(resp[respFieldText], "Try again in a moment") {
		t.Fatalf("owner-check-error reply missing retry hint: %q", resp[respFieldText])
	}
}

func TestSlashCommandUninstallFailsClosedWithoutWorkspaceOwner(t *testing.T) {
	provider := &recordingAuthProvider{apiKey: "test-key"}
	ts := newAdminTestServers(t)
	ts.seedWorkspace(t, testAdminTeamID, "", testAdminUserID, testWorkspaceConfiguredAt)
	h := newAdminTestHandler(t, ts)
	h.cfg.AuthProvider = provider

	resp := slashResponseForWorkspaceUser(t, h, commandUser, uninstallVerb, testAdminTeamID, testAdminUserID)

	if provider.deleteCalls != 0 {
		t.Fatalf("DeleteAPIKey calls = %d, want 0", provider.deleteCalls)
	}
	if !strings.Contains(resp[respFieldText], "isn't connected to a workspace owner yet") {
		t.Fatalf("missing-owner reply missing setup recovery hint: %q", resp[respFieldText])
	}
}

func TestSlashCommandUninstallFailsClosedForShapeBadOwner(t *testing.T) {
	provider := &recordingAuthProvider{apiKey: "test-key"}
	ts := newAdminTestServers(t)
	ts.seedWorkspace(t, testAdminTeamID, "auth0|legacy-owner", testAdminUserID, testWorkspaceConfiguredAt)
	h := newAdminTestHandler(t, ts)
	h.cfg.AuthProvider = provider

	resp := slashResponseForWorkspaceUser(t, h, commandUser, uninstallVerb, testAdminTeamID, testAdminUserID)

	if provider.deleteCalls != 0 {
		t.Fatalf("DeleteAPIKey calls = %d, want 0", provider.deleteCalls)
	}
	if !strings.Contains(resp[respFieldText], "Ask the owner to run `/qurl setup <email>`, then retry") {
		t.Fatalf("shape-bad-owner reply missing setup recovery hint: %q", resp[respFieldText])
	}
}

func TestSlashCommandUninstallAllowsWorkspaceAdminOrOwner(t *testing.T) {
	provider := &recordingAuthProvider{apiKey: "test-key"}
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.cfg.AuthProvider = provider

	stranger := slashResponseForWorkspaceUser(t, h, commandUser, uninstallVerb, testAdminTeamID, "USTRANGER000")
	if provider.deleteCalls != 0 {
		t.Fatalf("stranger DeleteAPIKey calls = %d, want 0", provider.deleteCalls)
	}
	if !strings.Contains(stranger[respFieldText], "qURL workspace admin") {
		t.Fatalf("stranger reply missing admin-or-owner message: %q", stranger[respFieldText])
	}

	admin := slashResponseForWorkspaceUser(t, h, commandUser, uninstallVerb, testAdminTeamID, testAdminUserID)
	if provider.deleteCalls != 1 {
		t.Fatalf("admin DeleteAPIKey calls = %d, want 1", provider.deleteCalls)
	}
	if !strings.Contains(admin[respFieldText], "disconnected from this workspace") {
		t.Fatalf("admin reply missing confirmation: %q", admin[respFieldText])
	}

	owner := slashResponseForWorkspaceUser(t, h, commandUser, uninstallVerb, testAdminTeamID, testAdminOwnerID)
	if provider.deleteCalls != 2 {
		t.Fatalf("owner DeleteAPIKey calls = %d, want 2", provider.deleteCalls)
	}
	if provider.deleteWorkspaceID != testAdminTeamID {
		t.Fatalf("DeleteAPIKey workspaceID = %q, want %q", provider.deleteWorkspaceID, testAdminTeamID)
	}
	if !strings.Contains(owner[respFieldText], "disconnected from this workspace") {
		t.Fatalf("owner reply missing confirmation: %q", owner[respFieldText])
	}
}

func TestSlashCommandGetToken_AcksWithWorkingOnIt(t *testing.T) {
	// Ack contract for /qurl get $<alias>: the synchronous response is
	// the ephemeral working-on-it message. The actual qURL link is
	// delivered later via response_url (covered in process_test.go).
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"data": map[string]any{
				"resource_id": "r_abc123test",
				"qurl_link":   "https://qurl.link/at_testtoken",
				"qurl_site":   "https://r_abc123test.qurl.site",
			},
			"meta": map[string]string{"request_id": "req_test"},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(qurlSrv.Close)

	t.Setenv("QURL_API_KEY", "test-key")

	h := newTestHandler(t, qurlSrv)
	seedGetAliasBinding(t, h, "T123")
	// Wire response_url to a local recorder so the async worker's
	// follow-up POST stays in-process. The literal `hooks.slack.com`
	// URL the migration left here would otherwise dial Slack on every
	// CI run (postResponse's `Wait()` cleanup blocks for the goroutine,
	// the recorder mock just captures and discards).
	rec := newResponseURLRecorder(t)
	body := getTokenCommandBody("T123", "trig-1", rec.URL)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result[respFieldResponseType] != respTypeEphemeral {
		t.Errorf("expected ephemeral response, got %q", result["response_type"])
	}
	if result["text"] != ackWorkingOnIt {
		t.Errorf("ack text = %q, want %q", result["text"], ackWorkingOnIt)
	}
}

// TestSlashCommandCreate_DeprecationHint fences the redirect copy
// surfaced when a user types `/qurl create …` after the consolidation
// to `/qurl get`. The reply is synchronous (no ack-then-async needed —
// nothing to mint) and points the user at the new verb. Both the bare
// verb and the with-tail forms route to the same hint — that way an
// existing user typing the old grammar gets the redirect, not an
// "unknown subcommand" reply.
func TestSlashCommandCreate_DeprecationHint(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	const createWithURL = "create https://example.com"
	cases := []string{"create", createWithURL}
	for _, text := range cases {
		body := url.Values{
			fieldCommand:     {"/qurl"},
			fieldText:        {text},
			fieldTeamID:      {"T123"},
			fieldChannelID:   {"C123"},
			fieldTriggerID:   {"trig-deprecation"},
			fieldResponseURL: {"https://hooks.slack.com/services/x"},
		}.Encode()

		w := httptest.NewRecorder()
		h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))

		var result map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("text=%q unmarshal: %v", text, err)
		}
		if !strings.Contains(result["text"], "no longer supported") {
			t.Errorf("text=%q: deprecation copy missing: %q", text, result["text"])
		}
		if !strings.Contains(result["text"], "/qurl get") {
			t.Errorf("text=%q: redirect to /qurl get missing: %q", text, result["text"])
		}
	}
}

func TestURLVerificationChallenge(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	body := `{"type":"url_verification","challenge":"test-challenge-123"}`

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/events", body, body))

	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["challenge"] != "test-challenge-123" {
		t.Errorf("expected challenge echo, got %q", result["challenge"])
	}
}

// TestSlackEndpoints_Reject401 is the main negative-path fence. Every row
// is a request the handler must reject; the three paths (commands / events
// / interactions) ensure a future endpoint addition can't silently skip
// signature verification.
func TestSlackEndpoints_Reject401(t *testing.T) {
	srv, hits := countingQURLServer(t)

	body := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()
	tamperedBody := url.Values{"command": {"/qurl"}, "text": {"create https://evil.example"}, "team_id": {"T999"}}.Encode()
	replayBody := url.Values{"command": {"/qurl"}, "text": {"list"}, "team_id": {"T_attacker"}}.Encode()
	origReplayBody := url.Values{"command": {"/qurl"}, "text": {"list"}, "team_id": {"T_victim"}}.Encode()

	cases := []struct {
		name      string
		path      string
		body      string
		signBody  string // if set, sign this body and send with `body` (tamper cases)
		nowOffset time.Duration
	}{
		{name: "unsigned /slack/commands", path: "/slack/commands", body: body},
		{name: "unsigned /slack/events", path: "/slack/events", body: `{"type":"url_verification","challenge":"attacker-chosen"}`},
		{name: "unsigned /slack/interactions", path: "/slack/interactions", body: `{"type":"block_actions"}`},
		{name: "tampered body (text swap)", path: "/slack/commands", body: tamperedBody, signBody: body},
		{name: "body swap with different team_id", path: "/slack/commands", body: replayBody, signBody: origReplayBody},
		{name: "stale timestamp (10m outside skew)", path: "/slack/commands", body: body, signBody: body, nowOffset: 10 * time.Minute},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t, srv)
			if tc.nowOffset != 0 {
				h.now = func() time.Time { return fixedNow.Add(tc.nowOffset) }
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, tc.path, tc.body, tc.signBody))
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", w.Code)
			}
		})
	}

	// Property fence: every 401 above means we rejected before
	// dispatching to the qURL upstream. The status check alone wouldn't
	// catch a regression that 401'd at the wire while leaking the call.
	if got := hits.Load(); got != 0 {
		t.Errorf("upstream qURL hits during auth-failure suite = %d, want 0", got)
	}
}

// Empty signing secret must 401 every request — deployment-is-open fence.
func TestHandle_EmptySigningSecret(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	h.cfg.SlackSigningSecret = ""

	body := url.Values{"command": {"/qurl"}, "text": {"help"}, "team_id": {"T123"}}.Encode()
	// Even with "correct-looking" headers — an empty secret means no message
	// can verify. We include them to prove the 401 isn't coming from the
	// "missing headers" path.
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(body))
	r.Header.Set(headerSlackSignature, "v0=aaaa")
	r.Header.Set(headerSlackTimestamp, "1761998400")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("empty signing secret: status = %d, want 401", w.Code)
	}
}

// classifySlackErr must emit stable, distinct labels for each sentinel —
// ops dashboards page on "secret_empty" distinctly from ordinary 401
// noise. A regression that collapsed labels (or downgraded the
// secret_empty slog.Error) would silently lose the page signal.
func TestClassifySlackErr_SentinelsMapToDistinctLabels(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errSlackSigningSecretEmpty, "secret_empty"},
		{errSlackSignatureMissing, "headers_missing"},
		{errSlackSignatureMalformed, "sig_malformed"},
		{errSlackTimestampMalformed, "ts_malformed"},
		{errSlackTimestampStale, "stale"},
		{errSlackSignatureMismatch, "mismatch"},
	}
	seen := make(map[string]error, len(cases))
	for _, tc := range cases {
		got := classifySlackErr(tc.err)
		if got != tc.want {
			t.Errorf("classifySlackErr(%v) = %q, want %q", tc.err, got, tc.want)
		}
		if prev, ok := seen[got]; ok {
			t.Errorf("label %q is shared by %v and %v — dashboards can't tell them apart", got, prev, tc.err)
		}
		seen[got] = tc.err
	}
}

// Note: an earlier "lowercase signature headers" test was dropped — net/http
// canonicalizes header names on wire parse via textproto.CanonicalMIMEHeaderKey,
// and httptest's Header.Set canonicalizes too, so the path is structurally
// covered by any signed request and a duplicate test added no coverage.

// Body-size cap rejects oversize requests with 413 before any read.
// httptest.NewRequest sets Content-Length from the reader, so this
// exercises the honest-sender pre-allocation guard. The dishonest-sender
// path (no/lying Content-Length) is caught by MaxBytesReader during the
// read; that defense-in-depth is structural rather than unit-testable.
func TestHandle_OversizeBodyReturns413(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	// 2 MiB — twice the 1 MiB cap.
	oversize := strings.Repeat("a", 2<<20)
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(oversize))
	// Headers intentionally absent — the body-size guard runs before
	// signature verification.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize body: status = %d, want 413", w.Code)
	}
}

// Routing fence: GET on a /slack/* path must 405 (the path exists, the
// method doesn't) with an Allow header pointing to POST. 404 would lie
// about the endpoint's existence; 401 would leak that the path is
// gated behind auth.
func TestHandle_GetOnSlackPathReturns405(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/slack/commands", http.NoBody))

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /slack/commands: status = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow header = %q, want %q", got, "POST")
	}
}

// /health must accept GET and HEAD (for ALB probes) and reject other
// methods with 405 + Allow.
func TestHealthEndpoint_RejectsNonGet(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/health", http.NoBody))

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /health: status = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow header = %q, want %q", got, "GET, HEAD")
	}
}

func TestHealthEndpoint_AcceptsHead(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/health", http.NoBody))

	if w.Code != http.StatusOK {
		t.Errorf("HEAD /health: status = %d, want 200", w.Code)
	}
}

// Boundary fence: a body exactly at the cap must succeed end-to-end.
// Off-by-one on MaxBytesReader would silently 400 legitimate large
// payloads — this row catches that regression.
func TestHandle_BodyAtCapAccepted(t *testing.T) {
	// noopQURLServer is required to populate Config.NewClient; this test
	// posts to /slack/events, which never calls out to qURL.
	h := newTestHandler(t, noopQURLServer(t))
	// /slack/events accepts arbitrary bytes; we're fencing read+verify,
	// not the event payload shape.
	body := strings.Repeat("a", maxRequestBodyBytes)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/events", body, body))

	if w.Code != http.StatusOK {
		t.Errorf("body at cap (%d bytes): status = %d, want 200", maxRequestBodyBytes, w.Code)
	}
}

// Pre-allocation fence: a client honestly declaring a too-large
// Content-Length must be rejected with 413 before any body is read.
func TestHandle_DeclaredOversizeReturns413(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader("ignored"))
	r.ContentLength = int64(maxRequestBodyBytes + 1)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("declared oversize: status = %d, want 413", w.Code)
	}
}

// Simulates the chunked-transfer / no-Content-Length path through
// MaxBytesReader — http.Server reads up to declared CL for non-chunked
// bodies, so the real-world dishonest case is chunked encoding (no CL,
// or a CL that doesn't reflect actual body size). The under-declared
// CL here forces ServeHTTP past the pre-allocation pre-check and
// exercises MaxBytesReader-during-read returning *http.MaxBytesError
// — which must surface as 413, not 400, so operator dashboards bucket
// it with the honest-oversize 413s.
func TestHandle_DishonestContentLengthReturns413(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	oversize := strings.Repeat("a", 2<<20)
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(oversize))
	r.ContentLength = 100

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("dishonest CL: status = %d, want 413", w.Code)
	}
}

// A 100 KiB signed body must reach handleEvent intact and 200. Locks
// the contract that no future refactor caps the read short of the body
// — a truncated read would silently 401 (HMAC mismatch on partial
// bytes) and look like a signature-secret-rotation bug.
func TestHandle_LargeSignedBodyAccepted(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	body := strings.Repeat("b", 100*1024)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/events", body, body))

	if w.Code != http.StatusOK {
		t.Errorf("100 KiB signed body: status = %d, want 200", w.Code)
	}
}

// Signed-but-malformed JSON event must 200 with the {"ok":"true"}
// envelope. Slack retries on non-2xx, so a regression to 400 would
// cause retry storms; a regression to 200-with-error-body would mask
// real failures from monitoring.
func TestHandle_MalformedEventJSON_Returns200(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	body := `{not json at all`

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/events", body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("malformed JSON event: status = %d, want 200", w.Code)
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result["ok"] != "true" {
		t.Errorf("malformed JSON event: body = %q, want ok=true", w.Body.String())
	}
}

func TestSlashCommandSetup_RequiresEmail(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	secret := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	h.SetOAuthSetup(oauth.SetupConfig{StateSecret: secret, SlackBaseURL: "https://slack-bot.example"})

	body := url.Values{
		"command": {"/qurl"},
		"text":    {"setup"},
		"team_id": {"T123ABCDEF"},
		"user_id": {"U_ADMIN1"},
	}.Encode()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result[respFieldResponseType] != respTypeEphemeral {
		t.Errorf("response_type: got %q want ephemeral", result["response_type"])
	}
	text := result["text"]
	if !strings.Contains(text, "Usage: `/qurl setup <email>`.") {
		t.Fatalf("missing setup email usage in setup reply: %q", text)
	}
	if strings.Contains(text, "/oauth/qurl/start?state=") {
		t.Fatalf("bare setup should not mint a setup URL: %q", text)
	}
}

func TestSlashCommandSetupWithEmail_RepliesWithPasswordlessStartURL(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	secret := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	h.SetOAuthSetup(oauth.SetupConfig{StateSecret: secret, SlackBaseURL: "https://slack-bot.example"})

	body := url.Values{
		"command": {commandUser},
		"text":    {"setup\tAdmin+Setup@Example.COM"},
		"team_id": {"T123ABCDEF"},
		"user_id": {"U_ADMIN1"},
	}.Encode()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	text := result["text"]
	if !strings.Contains(text, "Continue setup") {
		t.Fatalf("missing Continue setup copy: %q", text)
	}
	if !strings.Contains(text, "`admin+setup@example.com`") {
		t.Fatalf("setup reply missing normalized email: %q", text)
	}
	start := strings.Index(text, "state=")
	if start < 0 {
		t.Fatalf("no state= in reply: %q", text)
	}
	rest := text[start+len("state="):]
	end := strings.IndexAny(rest, "|>")
	if end >= 0 {
		rest = rest[:end]
	}
	stateRaw, err := url.QueryUnescape(rest)
	if err != nil {
		t.Fatalf("unescape state: %v", err)
	}
	verified, err := oauth.VerifyState(secret, stateRaw, fixedNow.Add(30*time.Second))
	if err != nil {
		t.Fatalf("minted state failed VerifyState: %v", err)
	}
	if verified.Email != "admin+setup@example.com" {
		t.Errorf("state email: got %q want normalized command email", verified.Email)
	}
}

func TestSlashCommandSetupWithEmail_RejectsInvalidEmail(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	secret := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	h.SetOAuthSetup(oauth.SetupConfig{StateSecret: secret, SlackBaseURL: "https://slack-bot.example"})

	body := url.Values{
		"command": {commandUser},
		"text":    {"setup not-an-email"},
		"team_id": {"T123ABCDEF"},
		"user_id": {"U_ADMIN1"},
	}.Encode()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(result["text"], "doesn't look like a valid email") {
		t.Errorf("expected invalid-email reply, got %q", result["text"])
	}
}

func TestSlashCommandSetupWithEmail_RejectsMultiArgUsage(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	secret := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	h.SetOAuthSetup(oauth.SetupConfig{StateSecret: secret, SlackBaseURL: "https://slack-bot.example"})

	body := url.Values{
		"command": {commandUser},
		"text":    {setupAdminExampleText + " extra"},
		"team_id": {"T123ABCDEF"},
		"user_id": {"U_ADMIN1"},
	}.Encode()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(result["text"], "Usage: `/qurl setup <email>`") {
		t.Errorf("expected setup usage reply, got %q", result["text"])
	}
}

// TestSetOAuthSetupPanicsOnDoubleCall locks the documented "called
// exactly once before Serve" contract. The field is read without a
// lock on the request hot path; the panic is the safety net for a
// future refactor that accidentally re-wires it.
func TestSetOAuthSetupPanicsOnDoubleCall(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	secret := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	h.SetOAuthSetup(oauth.SetupConfig{StateSecret: secret, SlackBaseURL: "https://slack-bot.example"})
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on second SetOAuthSetup call")
		}
	}()
	h.SetOAuthSetup(oauth.SetupConfig{StateSecret: secret, SlackBaseURL: "https://slack-bot.example"})
}

func TestSlashCommandSetup_RepliesNotConfiguredWhenOAuthOff(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	// SetOAuthSetup deliberately NOT called → oauthSetup == nil.
	body := url.Values{
		"command": {"/qurl"},
		"text":    {setupAdminExampleText},
		"team_id": {"T123ABCDEF"},
		"user_id": {"U_ADMIN1"},
	}.Encode()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(result["text"], "not configured") {
		t.Errorf("expected 'not configured' reply, got %q", result["text"])
	}
}

// Empty-body fence: locks the contract so a future ParseQuery
// substitution can't silently change the empty-text help fallback.
func TestSlashCommand_EmptyBodyShowsHelp(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(""))
	sig, ts := signSlackBody(t, "")
	r.Header.Set(headerSlackSignature, sig)
	r.Header.Set(headerSlackTimestamp, ts)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("signed empty body: status = %d, want 200 (help branch); body=%s", w.Code, w.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Assert on `/qurl help` — the unconditional help marker. (`/qurl get`
	// is now gated on AdminStore, which this handler doesn't wire, so it's
	// no longer a reliable "help rendered" signal here.)
	if !strings.Contains(result["text"], "/qurl help") {
		t.Errorf("signed empty body did not produce help; got: %q", result["text"])
	}
}
