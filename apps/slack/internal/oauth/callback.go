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
)

const (
	auth0TokenTimeout = 15 * time.Second
	// revokeTimeout bounds the orphan-key DELETE on qurl-service when a
	// post-mint persist failure forces us to clean up. Named separately
	// from auth0TokenTimeout because the call targets qurl-service, not
	// Auth0 — drift between the two budgets is fine.
	revokeTimeout       = 15 * time.Second
	dmTimeout           = 5 * time.Second
	auth0TokenBodyLimit = 8 << 10 // 8 KiB — Auth0's /oauth/token response is ~2 KiB; tighter than the previous 64 KiB.
)

// successPageTemplate mirrors the Discord-side success page
// (apps/discord/src/routes/qurl-oauth.js renderSuccess). Plain HTML with
// no external assets so it renders in the strictest CSP / no-JS
// environments. html/template auto-escapes every {{.Field}} interpolation
// — that's the entire XSS defense for the page, since teamID / keyPrefix
// / email all flow from user-influenced inputs (state param, qurl-service
// response, JWKS-verified id_token respectively).
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
<h1><span class="ok">&#10003;</span> qURL connected</h1>
<p>qURL is connected to your Slack workspace. Your team can now use <code>/qurl create</code> and <code>/qurl list</code>.</p>
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

// auth0TokenResponse is the slice of Auth0's /oauth/token response we read.
type auth0TokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
}

// Callback returns the http.HandlerFunc for GET /oauth/qurl/callback.
//
// Steps mirror the Discord LIVE flow (apps/discord/src/routes/qurl-oauth.js):
//  1. Validate cookie + query.state via timing-safe compare.
//  2. Validate the state's HMAC + recover (teamID, userID).
//  3. POST to Auth0 /oauth/token to exchange code → access_token + id_token.
//  4. Verify id_token signature against Auth0 JWKS (non-fatal — success
//     page renders without the email line if verify fails).
//  5. POST to qurl-service /v1/api-keys to mint the workspace key.
//  6. Upsert via WorkspaceStore.SetAPIKey; on failure, fire-and-forget
//     revoke on qurl-service.
//  7. DM the admin (fire-and-forget; failure doesn't block the page).
//  8. Render success HTML.
//
//nolint:gocritic // hugeParam: Config is value-passed at startup once; pointer churn here isn't worth the API-surface friction.
func Callback(cfg Config) http.HandlerFunc {
	now := cfg.now()
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: auth0TokenTimeout}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		q := r.URL.Query()
		if errParam := q.Get("error"); errParam != "" {
			//nolint:gosec // G706: slog's JSON handler escapes control bytes in attribute values, same posture as the request-path slog sites.
			slog.Warn("oauth/callback Auth0 returned error",
				"error", errParam,
				"error_description", q.Get("error_description"))
			http.Error(w, "authorization declined — run /qurl setup again to retry", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		stateParam := q.Get("state")
		if code == "" || stateParam == "" {
			http.Error(w, "missing code or state", http.StatusBadRequest)
			return
		}

		cookieState := readStateCookie(r)
		if cookieState == "" {
			slog.Warn("oauth/callback missing state cookie")
			clearStateCookie(w)
			http.Error(w, "setup must be completed in the same browser", http.StatusBadRequest)
			return
		}
		if len(cookieState) != len(stateParam) ||
			!hmac.Equal([]byte(cookieState), []byte(stateParam)) {
			slog.Warn("oauth/callback cookie/state mismatch")
			clearStateCookie(w)
			http.Error(w, "setup must be completed in the same browser", http.StatusBadRequest)
			return
		}

		// HMAC + expiry on the state token itself; the cookie check
		// above proves "same browser" but not "minted by us".
		verified, err := VerifyState(cfg.OAuthStateSecret, stateParam, now())
		if err != nil {
			slog.Warn("oauth/callback rejected invalid state", "reason", err.Error()) //nolint:gosec // G706: slog escapes control bytes in attribute values.
			clearStateCookie(w)
			http.Error(w, "invalid or expired setup link", http.StatusBadRequest)
			return
		}

		// Cookie has done its job — clear so a refresh can't re-bind.
		clearStateCookie(w)

		accessToken, idToken, err := exchangeAuth0Code(r.Context(), httpClient, cfg, code)
		if err != nil {
			slog.Error("oauth/callback Auth0 token exchange failed", "error", err)
			http.Error(w, "authorization failed — run /qurl setup again to retry", http.StatusBadGateway)
			return
		}

		// id_token verification is best-effort; failure suppresses the
		// success-page email line but never blocks key mint or persist.
		var qurlEmail string
		if idToken != "" && cfg.IDTokenVerifier != nil {
			email, verr := cfg.IDTokenVerifier.VerifyEmail(r.Context(), idToken)
			if verr != nil {
				slog.Warn("oauth/callback id_token verify failed (non-fatal)", "error", verr)
			} else {
				qurlEmail = email
			}
		}

		keyPrefix, ok := mintAndPersist(r.Context(), w, cfg, accessToken, verified.TeamID, verified.UserID)
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

// spawnAsync routes a goroutine through the AsyncTracker if one is
// wired, falling back to plain `go` for tests / unconfigured callers.
func spawnAsync(tracker AsyncTracker, fn func()) {
	if tracker != nil {
		tracker.Go(fn)
		return
	}
	go fn()
}

// mintAndPersist runs steps 5 + 6: mint key on qurl-service, persist via
// WorkspaceStore, fire-and-forget revoke if persist fails. Returns
// (keyPrefix, true) on success; on failure writes the HTTP error response
// and returns (_, false).
//
// Extracted so Callback stays straight-line and the linter doesn't need
// gocognit/gocyclo suppressors.
//
//nolint:gocritic // hugeParam: see Callback — Config is value-passed.
func mintAndPersist(ctx context.Context, w http.ResponseWriter, cfg Config, accessToken, teamID, userID string) (string, bool) {
	keyName := "Slack workspace " + teamID
	apiKey, keyID, keyPrefix, err := cfg.Minter.MintAPIKey(ctx, accessToken,
		keyName, apiKeyScopes)
	if err != nil {
		slog.Error("oauth/callback qurl-service mint failed", "error", err, "team_id", teamID) //nolint:gosec // G706: slog escapes control bytes in attribute values.
		http.Error(w, "could not provision qURL key — run /qurl setup again to retry", http.StatusBadGateway)
		return "", false
	}

	if perr := cfg.Provider.SetAPIKey(ctx, teamID, apiKey, userID); perr != nil {
		slog.Error("oauth/callback persist failed — revoking minted key", //nolint:gosec // G706: slog escapes control bytes in attribute values.
			"error", perr, "team_id", teamID, "key_id", keyID)
		// Fire-and-forget revoke. Fresh context (not the request
		// context) because the request context may be canceling by
		// the time we get to write the error response; we still want
		// the revoke to run to bound the orphan-key window.
		spawnAsync(cfg.AsyncTracker, func() {
			revokeOrphanKeyAsync(cfg.Minter, accessToken, keyID, teamID)
		})
		http.Error(w, "qURL key provisioned but not stored — run /qurl setup again", http.StatusInternalServerError)
		return "", false
	}
	return keyPrefix, true
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
	msg := "qURL is connected to your Slack workspace. Your team can now use `/qurl create`."
	if keyPrefix != "" {
		msg += "\nKey prefix: `" + keyPrefix + "`"
	}
	if err := client.PostDirectMessage(ctx, userID, msg); err != nil {
		slog.Warn("oauth/callback DM failed", "error", err, "user_id", userID, "team_id", teamID)
	}
}

func renderSuccess(w http.ResponseWriter, teamID, keyPrefix, email string) {
	// html/template handles all escaping. teamID is HMAC-verified upstream;
	// keyPrefix comes from qurl-service's JSON response; email is JWKS-
	// verified — but the template's context-aware auto-escape is the
	// load-bearing XSS defense here.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Defense-in-depth headers: success page is rendered post-auth so it
	// shouldn't be framable (clickjacking), shouldn't leak the URL via
	// Referer to anything the page links to, and shouldn't load any
	// off-origin resources beyond the inline style.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	if err := successPageTemplate.Execute(w, successPageData{
		TeamID:    teamID,
		KeyPrefix: keyPrefix,
		Email:     email,
	}); err != nil {
		slog.Warn("oauth/callback success-page write failed", "error", err)
	}
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
		// so the connection can be reused under keep-alive.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, auth0TokenBodyLimit))
	if err != nil {
		return "", "", fmt.Errorf("read body: %w", err)
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
