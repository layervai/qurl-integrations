package oauth

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// drainCap bounds the post-LimitReader keep-alive drain. We've
	// already enforced the body cap (auth0TokenBodyLimit / minterBodyLimit
	// are both 8 KiB); the deferred drain just discards anything the
	// LimitReader didn't consume so the connection can be reused. An
	// unbounded io.Copy here would let a misbehaving upstream stream
	// indefinitely within the request timeout, holding the connection.
	// Worst-case total bytes-read per call: 8 KiB (body cap) + 32 KiB
	// (drain) = 40 KiB. Bounded commitment to a hostile/runaway upstream.
	drainCap          = 32 << 10
	auth0TokenTimeout = 15 * time.Second
	// revokeTimeout bounds the orphan-key DELETE on qurl-service when a
	// post-mint persist failure forces us to clean up. Named separately
	// from auth0TokenTimeout because the call targets qurl-service, not
	// Auth0 — drift between the two budgets is fine.
	revokeTimeout = 15 * time.Second
	// persistTimeout bounds the DDB PutItem from mintAndPersist. Fresh
	// context (not the request context) so TimeoutHandler can't cancel
	// the write mid-PutItem and leave the row state misaligned with
	// what we report to the user. 15s is well over typical DDB PutItem
	// latency.
	persistTimeout = 15 * time.Second
	// bindTimeout bounds the DDB PutItem (+ optional disambiguation
	// GetItem) from checkBindAllowed. Same fresh-context posture as
	// persistTimeout — the two are separate constants so a future
	// adjustment of one doesn't accidentally retune the other.
	bindTimeout = 15 * time.Second
	// mintTimeout bounds the qurl-service POST /v1/api-keys call from
	// mintAndPersist. Same fresh-context rationale as persistTimeout:
	// TimeoutHandler canceling mid-mint would orphan a key the bot
	// can no longer revoke (no keyID to DELETE against).
	mintTimeout         = 15 * time.Second
	dmTimeout           = 5 * time.Second
	auth0TokenBodyLimit = 8 << 10 // 8 KiB — Auth0's /oauth/token response is ~2 KiB; tighter than the previous 64 KiB.
)

// successPageTemplate mirrors the Discord-side success page
// (apps/discord/src/routes/qurl-oauth.js renderSuccess). Plain HTML with
// no external assets so it renders in the strictest CSP / no-JS
// environments. html/template auto-escapes every {{.Field}} interpolation
// — that's the load-bearing XSS defense for keyPrefix (qurl-service
// JSON response) and email (JWKS-verified id_token). teamID is
// HMAC-recovered from the signed state so it's already a trusted
// input, but the same auto-escape applies uniformly.
var successPageTemplate = template.Must(template.New("oauth-success").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>qURL Connected</title>
<meta name="robots" content="noindex">
<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:480px;margin:4rem auto;padding:0 1rem;color:#111}
.card{border:1px solid #d1d5db;border-radius:12px;padding:2rem;background:#f9fafb}
h1{margin:0 0 .5rem;font-size:1.5rem}
.kv{margin-top:1rem;font-size:.875rem;color:#374151}
.kv div{margin-top:.25rem}
.ok{color:#059669;font-weight:600}
code{background:#e5e7eb;padding:.1rem .3rem;border-radius:4px;font-size:.875em}
</style>
</head>
<body>
<div class="card">
<h1><span class="ok">&#10003;</span> qURL Connected</h1>
<p>qURL is connected to your Slack workspace. Your team can now use <code>/qurl get</code> and <code>/qurl list</code>.</p>
<div class="kv">
<div>Slack workspace: <code>{{.TeamID}}</code></div>
{{if .KeyPrefix}}<div>API key prefix: <code>{{.KeyPrefix}}</code></div>{{end}}
{{if .Email}}<div>qURL account: <code>{{.Email}}</code></div>{{end}}
</div>
<p style="margin-top:1.5rem;font-size:.875rem;color:#6b7280">You can close this tab and return to Slack.</p>
</div>
</body>
</html>`))

// successPageData is the model passed to successPageTemplate. Field names
// must match the template's {{.Field}} accessors.
type successPageData struct {
	TeamID    string
	KeyPrefix string
	Email     string
}

// rebindRefusedPageTemplate is the page rendered when BindWorkspace
// returns a 409 indicating a *different* admin already holds the
// workspace. We do NOT silently overwrite — that was the
// workspace-rebind primitive Justin flagged. The page tells the
// installer to coordinate with the existing admin instead.
//
// The API key minted earlier in the callback is revoked before we
// render this page so a refused install doesn't leave a half-installed
// key behind.
var rebindRefusedPageTemplate = template.Must(template.New("oauth-rebind-refused").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>qURL setup blocked</title>
<meta name="robots" content="noindex">
<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:480px;margin:4rem auto;padding:0 1rem;color:#111}
.card{border:1px solid #d1d5db;border-radius:12px;padding:2rem;background:#fef2f2}
h1{margin:0 0 .5rem;font-size:1.5rem}
.kv{margin-top:1rem;font-size:.875rem;color:#374151}
.warn{color:#b91c1c;font-weight:600}
code{background:#e5e7eb;padding:.1rem .3rem;border-radius:4px;font-size:.875em}
</style>
</head>
<body>
<div class="card">
<h1><span class="warn">&#9888;</span> qURL setup blocked</h1>
<p>This Slack workspace is already connected to qURL under a different admin. To avoid silently overwriting their configuration, this run of <code>/qurl setup</code> was not applied.</p>
<p>Please ask the existing qURL admin in your workspace to add you, or contact LayerV support if the original admin is no longer reachable.</p>
<div class="kv">
<div>Slack workspace: <code>{{.TeamID}}</code></div>
</div>
<p style="margin-top:1.5rem;font-size:.875rem;color:#6b7280">You can close this tab.</p>
</div>
</body>
</html>`))

// rebindRefusedPageData is the model passed to rebindRefusedPageTemplate.
type rebindRefusedPageData struct {
	TeamID string
}

// oauthErrorPageTemplate renders a styled, human-readable error page for
// OAuth-callback failures that previously fell through to bare http.Error
// (a blank white page with raw text — the experience operators flagged).
// Same no-asset / strict-CSP posture as the success and rebind-refused
// pages. Heading and Message are the only interpolations; html/template
// auto-escapes both (they're operator-authored today, but the escape keeps
// the page safe if a future caller passes an upstream string through).
var oauthErrorPageTemplate = template.Must(template.New("oauth-error").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>qURL setup</title>
<meta name="robots" content="noindex">
<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:480px;margin:4rem auto;padding:0 1rem;color:#111}
.card{border:1px solid #d1d5db;border-radius:12px;padding:2rem;background:#fef2f2}
h1{margin:0 0 .5rem;font-size:1.5rem}
p{color:#374151;font-size:.95rem;line-height:1.5}
.warn{color:#b91c1c;font-weight:600}
</style>
</head>
<body>
<div class="card">
<h1><span class="warn">&#9888;</span> {{.Heading}}</h1>
<p>{{.Message}}</p>
<p style="margin-top:1.5rem;font-size:.875rem;color:#6b7280">You can close this tab and return to Slack.</p>
</div>
</body>
</html>`))

// oauthErrorPageData is the model passed to oauthErrorPageTemplate.
type oauthErrorPageData struct {
	Heading string
	Message string
}

// auth0TokenResponse is the slice of Auth0's /oauth/token response we read.
type auth0TokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
}

// Callback returns the http.HandlerFunc for GET /oauth/qurl/callback.
//
// Steps:
//  1. Validate cookie + query.state via timing-safe compare; verify the
//     state's HMAC + expiry; recover (teamID, userID).
//  2. POST to Auth0 /oauth/token to exchange code → access_token + id_token.
//  3. Verify id_token signature against Auth0 JWKS — extract `sub`
//     (workspace OwnerID; mandatory) and `email` (best-effort for the
//     success-page readout).
//  4. If setup state carried an email, require the verified Auth0
//     email claim to match it before any bind or key mint.
//  5. Bind workspace_mappings via AdminStore.BindWorkspace — the
//     installer becomes the first admin. A rebind conflict against a
//     different admin (or unverified) renders the rebind-refused page
//     and short-circuits BEFORE we touch any key state, so a refused
//     install can't overwrite the existing admin's stored API key.
//     Same-caller re-entry is idempotent success.
//  6. POST to qurl-service /v1/api-keys to mint the workspace key.
//  7. Upsert via WorkspaceStore.SetAPIKey; on failure, fire-and-forget
//     revoke on qurl-service to bound the orphan-key window.
//  8. DM the admin (fire-and-forget; failure doesn't block the page).
//  9. Render success HTML.
//
//nolint:gocritic // hugeParam: Config is value-passed at startup once; pointer churn here isn't worth the API-surface friction.
func Callback(cfg Config) http.HandlerFunc {
	// `now` is a clock provider (a func, not a fixed time) so the
	// closure invokes time.Now per request — captured once here at
	// construction.
	now := cfg.now()
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: auth0TokenTimeout,
			// Mirror defaultResponseURLClient in apps/slack/internal:
			// Auth0 won't 30x /oauth/token in practice, but a
			// CheckRedirect that surfaces the response rather than
			// auto-following gives operator logs the actual status
			// code and removes a class of cross-host hop surprises.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		verified, code, ok := validateCallbackRequest(w, r, cfg, now)
		if !ok {
			return
		}

		accessToken, idToken, err := exchangeAuth0Code(r.Context(), httpClient, cfg, code)
		if err != nil {
			slog.Error("oauth/callback Auth0 token exchange failed", "error", err)
			http.Error(w, "authorization failed — run /qurl setup again to retry", http.StatusBadGateway)
			return
		}

		qurlEmail, qurlSub := verifyIDTokenClaims(r.Context(), cfg, idToken)
		if !checkSetupEmailMatches(w, verified, qurlEmail) {
			return
		}

		// Bind BEFORE mint so a rebind-refused install can't overwrite
		// the existing admin's encrypted qurl_api_key row with a key
		// that's about to be revoked. The OwnerID needed for the bind
		// (id_token sub) is already in hand pre-mint, so there's no
		// ordering blocker. On a refused outcome, no key is minted and
		// no DDB row is touched — refused-bind half-install becomes
		// impossible by construction (the lost-PutItem-race orphan-key
		// case is separate; tracked at #265).
		if !checkBindAllowed(w, cfg, verified, qurlSub) {
			return
		}

		keyPrefix, ok := mintAndPersist(w, cfg, accessToken, verified.TeamID, verified.UserID)
		if !ok {
			return
		}

		slog.Info("oauth/callback completed", "team_id", verified.TeamID, "user_id", verified.UserID, "key_prefix", keyPrefix) //nolint:gosec // G706: slog escapes control bytes in attribute values.

		// DM target is the Slack user_id from the signed state — never
		// from an unsigned query parameter. Goroutine deliberately uses
		// a fresh context (not r.Context()): r.Context() cancels the
		// moment we render the success page below. AsyncTracker scopes
		// it under handler.wg so SIGTERM during a callback waits for
		// the DM (and the revoke from the persist-failure path) to
		// drain rather than cutting them off mid-call.
		if cfg.SlackClient != nil {
			spawnAsync(cfg.AsyncTracker, func() {
				dmAdminAsync(cfg.SlackClient, verified.UserID, verified.TeamID, keyPrefix)
			})
		}

		renderSuccess(w, verified.TeamID, keyPrefix, qurlEmail)
	}
}

func checkSetupEmailMatches(w http.ResponseWriter, verified VerifiedState, qurlEmail string) bool {
	if verified.Email == "" {
		return true
	}
	normalized, err := NormalizeEmail(qurlEmail)
	if err != nil || normalized != verified.Email {
		slog.Warn("oauth/callback email mismatch for setup flow",
			"has_verified_email", qurlEmail != "")
		http.Error(w, "authenticated email did not match setup email — run /qurl setup again", http.StatusBadRequest)
		return false
	}
	return true
}

// validateCallbackRequest verifies the request envelope: method, query
// parameters, cookie/state HMAC pairing, and state HMAC+expiry. On
// failure it has already written the HTTP error response.
//
//nolint:gocritic // hugeParam: Config value-pass posture matches the rest of the package.
func validateCallbackRequest(w http.ResponseWriter, r *http.Request, cfg Config, now func() time.Time) (verified VerifiedState, code string, ok bool) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return VerifiedState{}, "", false
	}

	q := r.URL.Query()
	if errParam := q.Get("error"); errParam != "" {
		//nolint:gosec // G706: slog's JSON handler escapes control bytes in attribute values, same posture as the request-path slog sites.
		slog.Warn("oauth/callback Auth0 returned error",
			"error", errParam,
			// Auth0 enterprise SAML connections occasionally embed
			// the rejected username in error_description. Truncate
			// to bound PII exposure in operator logs.
			"error_description", truncateForLog(q.Get("error_description"), 128))
		// Clear the cookie even on the Auth0-error branch so the
		// stale state can't be replayed within the 5-minute TTL.
		// On the success path, the cookie clears after verify; this
		// closes the same-browser-replay window on Auth0 reject too.
		clearStateCookie(w)
		http.Error(w, "authorization failed — run /qurl setup again to retry", http.StatusBadRequest)
		return VerifiedState{}, "", false
	}
	code = q.Get("code")
	stateParam := q.Get("state")
	if code == "" || stateParam == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return VerifiedState{}, "", false
	}

	cookieState := readStateCookie(r)
	if cookieState == "" {
		slog.Warn("oauth/callback missing state cookie")
		clearStateCookie(w)
		http.Error(w, "setup must be completed in the same browser", http.StatusBadRequest)
		return VerifiedState{}, "", false
	}
	// Both values come from the same MintState call so canonical
	// length is fixed; hmac.Equal short-circuits to false on
	// length mismatch (length oracle is harmless here because an
	// attacker who can probe arbitrary cookie+state pairs already
	// has the HttpOnly cookie). Constant-time byte compare on
	// equal-length inputs.
	if !hmac.Equal([]byte(cookieState), []byte(stateParam)) {
		slog.Warn("oauth/callback cookie/state mismatch")
		clearStateCookie(w)
		http.Error(w, "setup must be completed in the same browser", http.StatusBadRequest)
		return VerifiedState{}, "", false
	}

	// HMAC + expiry on the state token itself; the cookie check
	// above proves "same browser" but not "minted by us".
	v, err := VerifyState(cfg.OAuthStateSecret, stateParam, now())
	if err != nil {
		slog.Warn("oauth/callback rejected invalid state", "reason", err.Error()) //nolint:gosec // G706: slog escapes control bytes in attribute values.
		clearStateCookie(w)
		http.Error(w, "invalid or expired setup link", http.StatusBadRequest)
		return VerifiedState{}, "", false
	}

	// Cookie has done its job — clear so a refresh can't re-bind.
	clearStateCookie(w)
	return v, code, true
}

// verifyIDTokenClaims extracts email + sub from the id_token. Email
// is best-effort for legacy setup (failure logged, returned ""), but
// becomes mandatory when the signed setup state carries an email; sub
// is mandatory for the downstream bind (failure returned as "" so
// checkBindAllowed can fail-closed). Both verifies are skipped cleanly
// when idToken is empty or the verifier is unwired.
//
// In production the verifier is non-nil by construction —
// cmd/main.go's buildOAuthConfig fails-fast at boot when AdminStore
// is wired and JWKS prime fails — so the nil-verifier branch is
// reachable only on the sandbox / no-DDB deploy path, where
// checkBindAllowed short-circuits on AdminStore==nil before reading
// the (empty) sub anyway.
//
//nolint:gocritic // hugeParam: Config value-pass posture matches the rest of the package.
func verifyIDTokenClaims(ctx context.Context, cfg Config, idToken string) (email, sub string) {
	if idToken == "" {
		// Auth0 should always return an id_token when openid is in
		// the scope set (see authorizeURL). An empty id_token here
		// signals a misconfigured Auth0 application — likely the
		// openid scope was dropped on the consent screen. Distinguish
		// from "verifier rejected it" so on-call triaging the
		// downstream 500 doesn't dig through JWKS logs that never
		// fired.
		slog.Warn("oauth/callback Auth0 returned empty id_token — sub-verify will be skipped (likely Auth0 application misconfigured without openid scope)")
		return "", ""
	}
	if cfg.IDTokenVerifier == nil {
		return "", ""
	}
	if e, verr := cfg.IDTokenVerifier.VerifyEmail(ctx, idToken); verr != nil {
		slog.Warn("oauth/callback id_token email-verify failed (non-fatal)", "error", verr)
	} else {
		email = e
	}
	if s, serr := cfg.IDTokenVerifier.VerifySub(ctx, idToken); serr != nil {
		slog.Warn("oauth/callback id_token sub-verify failed — bind will be skipped (fatal in production where AdminStore is wired)", "error", serr)
	} else {
		sub = s
	}
	return email, sub
}

// checkBindAllowed runs the BindWorkspace pre-flight. Returns true to
// continue to mint+persist; false when a response has already been
// written (rebind-refused, generic 500, or sub-missing 500). Because
// this runs BEFORE mint, a refused install never produces an orphan
// key and never overwrites an existing admin's stored credential.
//
// AdminStore=nil is the sandbox / no-DDB path — log and skip.
// qurlSub is the output of the upstream id_token verifier (VerifySub,
// logged on failure earlier in the callback), not the gate itself.
// qurlSub=="" therefore means that verification silently failed — we
// have no proof the OAuth flow originated from a legit Auth0 session,
// so we refuse the bind here rather than half-install. The Auth0
// identity check stays a security gate at OAuth time even though
// qurlSub is no longer persisted in workspace_mappings; without it,
// anyone with workspace OAuth-flow access could complete /setup
// without proving qURL service identity. Render a 500 — half-
// installing (API key minted, admin verbs broken) is the worst-of-
// both-worlds state.
//
// `verified.UserID` is the Slack user ID of the /setup invoker
// (HMAC-verified through the OAuth state token). That value becomes
// BOTH the workspace_mappings.owner_id (the long-lived "only this
// Slack user can re-run /setup" anchor) AND the initial entry in
// admin_slack_user_ids. The two are the same value at first bind
// by construction; /qurl admin add later grows the admin set but
// leaves owner_id immutable. See WorkspaceMapping doc in slackdata
// for the model rationale.
//
//nolint:gocritic // hugeParam: Config value-pass posture matches the rest of the package.
func checkBindAllowed(w http.ResponseWriter, cfg Config, verified VerifiedState, qurlSub string) bool {
	if cfg.AdminStore == nil {
		slog.Warn("oauth/callback AdminStore not wired — workspace_mappings not seeded", //nolint:gosec // G706: slog escapes control bytes in attribute values.
			"team_id", verified.TeamID)
		return true
	}
	if qurlSub == "" {
		slog.Error("oauth/callback bind skipped — id_token sub unavailable", //nolint:gosec // G706: slog escapes control bytes in attribute values.
			"team_id", verified.TeamID)
		http.Error(w, "workspace identity could not be confirmed — run /qurl setup again", http.StatusInternalServerError)
		return false
	}
	bindCtx, bindCancel := context.WithTimeout(context.Background(), bindTimeout)
	defer bindCancel()
	bindErr := cfg.AdminStore.BindWorkspace(bindCtx,
		&WorkspaceMapping{TeamID: verified.TeamID, OwnerID: verified.UserID},
		verified.UserID)
	if bindErr == nil {
		return true
	}
	return handleBindError(w, cfg, bindErr, verified.TeamID)
}

// handleBindError classifies the BindWorkspace error via cfg.BindClassifyError
// and writes the appropriate response. Returns true if the caller should
// continue (idempotent same-caller re-entry — bind held, no overwrite,
// success-page still appropriate) and false if a response has been
// written (rebind-refused page or generic 500). No key revoke wiring
// because this runs BEFORE mint by construction.
//
//nolint:gocritic // hugeParam: Config value-pass posture matches the rest of the package.
func handleBindError(w http.ResponseWriter, cfg Config, bindErr error, teamID string) bool {
	var code BindConflictCode
	if cfg.BindClassifyError != nil {
		code = cfg.BindClassifyError(bindErr)
	}
	switch code {
	case BindConflictAlreadyBoundToCaller:
		// The workspace owner is re-running /qurl setup — the stored
		// owner_id matches the verified caller (owner-only short-circuit;
		// added admins do NOT land here, they get AlreadyBound). We
		// continue to mint, which rotates their API key. Operator-visible
		// effect: "key rotated, owner + admin set unchanged."
		slog.Info("oauth/callback rebind idempotent (caller is the workspace owner)", //nolint:gosec // G706: slog escapes control bytes in attribute values.
			"team_id", teamID)
		return true
	case BindConflictAlreadyBound, BindConflictUnverified:
		// A different Slack user owns the workspace — the caller isn't
		// the stored owner_id (this fires for added admins too, not just
		// strangers), or we can't tell (treat unverified the same way;
		// the safer default is to refuse the rebind than to potentially
		// overwrite). No mint has happened yet, so nothing to revoke —
		// the existing owner's key row is untouched.
		slog.Warn("oauth/callback rebind refused — workspace owned by a different Slack user", //nolint:gosec // G706: slog escapes control bytes in attribute values.
			"team_id", teamID, "conflict", string(code), "error", bindErr)
		renderRebindRefused(w, teamID)
		return false
	default:
		slog.Error("oauth/callback BindWorkspace failed", //nolint:gosec // G706: slog escapes control bytes in attribute values.
			"team_id", teamID, "error", bindErr)
		http.Error(w, "workspace not bound — run /qurl setup again", http.StatusInternalServerError)
		return false
	}
}

// spawnAsync routes a goroutine through the AsyncTracker if one is
// wired, falling back to plain `go` for tests / unconfigured callers.
func spawnAsync(tracker AsyncTracker, fn func()) {
	if tracker != nil {
		tracker.Go(fn)
		return
	}
	go fn()
}

// mintAndPersist mints the API key on qurl-service and persists it via
// WorkspaceStore. Returns (keyPrefix, true) on success; on failure
// writes the HTTP error response and fires the orphan-key revoke
// when a mint succeeded but the persist did not. The plaintext apiKey
// and keyID stay internal: SetAPIKey is the only apiKey consumer and
// keyID's only use is the persist-failure revoke. With BindWorkspace
// running BEFORE this step (see Callback), no bind-failure path needs
// the keyID either.
//
// Post-timeout-completion footgun: the mint + persist contexts are
// fresh (decoupled from the request context) so a TimeoutHandler
// cancel doesn't desync row state from what we tell the user. The
// flip side: if oauthHandlerTimeout (60s) fires after we already
// started the mint, the goroutine continues for up to
// mintTimeout + persistTimeout (≈30s) after we've returned an error
// to the user. The user retries, mints K2, and the eventual
// completion of K1 races K2's persist as a row overwrite. K1 then
// becomes an orphan in qurl-service (no revoke wired for this case).
// Tracked at #265 alongside the lost-PutItem-race orphan path.
//
//nolint:gocritic // hugeParam: see Callback — Config is value-passed.
func mintAndPersist(w http.ResponseWriter, cfg Config, accessToken, teamID, userID string) (string, bool) {
	keyName := "Slack workspace " + teamID
	// Fresh bounded context for the mint, decoupled from the request
	// context. TimeoutHandler's 60s deadline could fire mid-mint;
	// qurl-service may have already created the key, but we'd surface
	// an error client-side with no keyID — making the key an
	// unbounded orphan (no revoke path possible without keyID). The
	// fresh context gives the mint its own budget; if it times out
	// distinctly, qurl-service's idempotency will eventually reconcile.
	mintCtx, mintCancel := context.WithTimeout(context.Background(), mintTimeout)
	defer mintCancel()
	apiKey, keyID, keyPrefix, err := cfg.Minter.MintAPIKey(mintCtx, accessToken,
		keyName, apiKeyScopes())
	if err != nil {
		limitReached := errors.Is(err, ErrAPIKeyLimitReached)
		//nolint:gosec // G706: slog escapes control bytes in attribute values.
		slog.Error("oauth/callback qurl-service mint failed", "error", err, "team_id", teamID, "api_key_limit_reached", limitReached)
		if limitReached {
			// Quota is a precondition the admin must clear themselves —
			// retrying does nothing (the old "run setup again" advice was
			// actively wrong here). 409 so an automated retry surfaces the
			// conflict rather than looping. Don't state a key count: the cap
			// is plan-dependent (free 3 / growth 50 / unlimited) and is
			// qurl-service's to own.
			renderOAuthErrorPage(w, http.StatusConflict, "qURL key limit reached",
				"Your qURL account already has the maximum number of API keys allowed on your plan, so a new one couldn't be created. Each run of /qurl setup creates a new key — revoke one you no longer use, then run /qurl setup again.")
			return "", false
		}
		renderOAuthErrorPage(w, http.StatusBadGateway, "Couldn't connect qURL",
			"Something went wrong while creating your qURL API key. Run /qurl setup again in a few minutes. If it keeps failing, please contact your qURL administrator.")
		return "", false
	}

	// Persist with a fresh bounded context, not the request context.
	// TimeoutHandler's 60s deadline could fire mid-PutItem; the write
	// would return context.Canceled but DDB may still have committed,
	// producing a row we then report to the user as "not stored." The
	// fresh context decouples the persist outcome from the handler-
	// level timeout — what we tell the user matches what's in DDB.
	persistCtx, persistCancel := context.WithTimeout(context.Background(), persistTimeout)
	defer persistCancel()
	if perr := cfg.Provider.SetAPIKey(persistCtx, teamID, apiKey, userID); perr != nil {
		slog.Error("oauth/callback persist failed — revoking minted key", //nolint:gosec // G706: slog escapes control bytes in attribute values.
			"error", perr, "team_id", teamID, "key_id", keyID)
		scheduleOrphanRevoke(cfg, accessToken, keyID, teamID)
		// TODO(#265): revoke is wired only for the persist-failure case.
		// A TimeoutHandler-induced abandon (outer 60s fires after mint
		// has started but before persist returns) escapes this branch
		// and leaks an orphan. Closes when #265's ConditionExpression
		// shift lets us detect the lost-race case end-to-end.
		http.Error(w, "qURL key provisioned but not stored — run /qurl setup again", http.StatusInternalServerError)
		return "", false
	}
	return keyPrefix, true
}

// scheduleOrphanRevoke fires a fire-and-forget revoke of a key that
// can't be left in place — persist failure, bind failure, or any
// other half-install state. Routed through the AsyncTracker so
// SIGTERM drains it under handler.wg rather than cutting mid-call.
//
//nolint:gocritic // hugeParam: see Callback — Config is value-passed.
func scheduleOrphanRevoke(cfg Config, accessToken, keyID, teamID string) {
	spawnAsync(cfg.AsyncTracker, func() {
		revokeOrphanKeyAsync(cfg.Minter, accessToken, keyID, teamID)
	})
}

func revokeOrphanKeyAsync(minter QURLAPIKeyMinter, accessToken, keyID, teamID string) {
	ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
	defer cancel()
	if err := minter.RevokeAPIKey(ctx, accessToken, keyID); err != nil {
		slog.Warn("oauth/callback orphan-key revoke failed",
			"error", err, "key_id", keyID, "team_id", teamID)
	}
}

func dmAdminAsync(client SlackClient, userID, teamID, keyPrefix string) {
	if userID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dmTimeout)
	defer cancel()
	msg := "qURL is connected to your Slack workspace. Your team can now use `/qurl get`."
	if keyPrefix != "" {
		msg += "\nKey prefix: `" + keyPrefix + "`"
	}
	if err := client.PostDirectMessage(ctx, userID, msg); err != nil {
		slog.Warn("oauth/callback DM failed", "error", err, "user_id", userID, "team_id", teamID)
	}
}

// setOAuthPageSecurityHeaders writes the defense-in-depth header set shared
// by every OAuth-callback HTML response (success, rebind-refused, error).
// These pages render post-redirect, so they shouldn't be framable
// (clickjacking), shouldn't leak the callback URL via Referer to anything
// they link to, and shouldn't load any off-origin resources beyond the
// inline style.
func setOAuthPageSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// renderRebindRefused writes the rebind-refused page. Same defense-in-
// depth headers as the success page — the only differences are the body
// template and a 409 status code (so an automated retry surfaces the
// conflict rather than silently looping).
func renderRebindRefused(w http.ResponseWriter, teamID string) {
	setOAuthPageSecurityHeaders(w)
	w.WriteHeader(http.StatusConflict)
	if err := rebindRefusedPageTemplate.Execute(w, rebindRefusedPageData{TeamID: teamID}); err != nil {
		slog.Warn("oauth/callback rebind-refused page write failed", "error", err)
	}
}

// renderOAuthErrorPage writes a styled error page with the given status.
// Replaces bare http.Error on the mint-failure path so a callback failure
// renders human-readable, actionable guidance instead of a blank page with
// raw text. Same defense-in-depth headers as renderRebindRefused.
func renderOAuthErrorPage(w http.ResponseWriter, status int, heading, message string) {
	setOAuthPageSecurityHeaders(w)
	w.WriteHeader(status)
	if err := oauthErrorPageTemplate.Execute(w, oauthErrorPageData{Heading: heading, Message: message}); err != nil {
		slog.Warn("oauth/callback error-page write failed", "error", err)
	}
}

func renderSuccess(w http.ResponseWriter, teamID, keyPrefix, email string) {
	// html/template handles all escaping. teamID is HMAC-verified upstream;
	// keyPrefix comes from qurl-service's JSON response; email is JWKS-
	// verified — but the template's context-aware auto-escape is the
	// load-bearing XSS defense here.
	setOAuthPageSecurityHeaders(w)
	w.WriteHeader(http.StatusOK)
	if err := successPageTemplate.Execute(w, successPageData{
		TeamID:    teamID,
		KeyPrefix: keyPrefix,
		Email:     email,
	}); err != nil {
		slog.Warn("oauth/callback success-page write failed", "error", err)
	}
}

// truncateForLog caps an arbitrary upstream string at limit bytes for
// operator-log inclusion. Defense against PII / noise leaks when
// surfacing third-party (Auth0/qurl-service) strings in slog
// attributes. Backs up to a UTF-8 rune boundary so the truncation
// doesn't split a multi-byte sequence (Auth0 SAML error_description
// is UTF-8).
func truncateForLog(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// Back up to a rune boundary. UTF-8 continuation bytes have the
	// pattern 10xxxxxx (0x80..0xBF); the first byte of a multi-byte
	// rune is either ASCII (< 0x80) or 11xxxxxx (>= 0xC0).
	cut := limit
	for cut > 0 && cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…[truncated]"
}

// exchangeAuth0Code POSTs application/x-www-form-urlencoded to
// /oauth/token and returns (access_token, id_token, err).
//
//nolint:gocritic // hugeParam: see Callback above — value-passing is intentional.
func exchangeAuth0Code(ctx context.Context, httpClient *http.Client, cfg Config, code string) (accessToken, idToken string, err error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", callbackURL(cfg.SlackBaseURL))
	form.Set("client_id", cfg.Auth0ClientID)
	form.Set("client_secret", cfg.Auth0ClientSecret)

	tokenURL := (&url.URL{Scheme: "https", Host: cfg.Auth0Domain, Path: "/oauth/token"}).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("do request: %w", err)
	}
	defer func() {
		// Drain any unread bytes (e.g. the >limit reject path below)
		// so the connection can be reused under keep-alive. Bounded by
		// drainCap so a misbehaving upstream that streams indefinitely
		// can't hold the connection for the full request timeout.
		_, _ = io.CopyN(io.Discard, resp.Body, drainCap)
		_ = resp.Body.Close()
	}()
	// Read up to limit+1 so we can distinguish "body is exactly limit
	// bytes long (legitimate)" from "body exceeded the cap and was
	// truncated". The naive LimitReader(_, limit) returns up to limit
	// inclusive, so a 8192-byte response would be misclassified as
	// truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, auth0TokenBodyLimit+1))
	if err != nil {
		return "", "", fmt.Errorf("read body: %w", err)
	}
	if len(body) > auth0TokenBodyLimit {
		return "", "", fmt.Errorf("auth0 token response exceeded %d bytes", auth0TokenBodyLimit)
	}
	if resp.StatusCode != http.StatusOK {
		// Don't surface the body to the browser — log only — could
		// contain a sub claim or other ID.
		return "", "", fmt.Errorf("auth0 token endpoint returned %d", resp.StatusCode)
	}
	var tr auth0TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", "", fmt.Errorf("parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", "", errors.New("auth0 returned empty access_token")
	}
	return tr.AccessToken, tr.IDToken, nil
}
