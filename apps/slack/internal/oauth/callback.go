package oauth

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	auth0TokenTimeout = 15 * time.Second
	dmTimeout         = 5 * time.Second
)

// successHTML mirrors the Discord-side success page (apps/discord/src/
// routes/qurl-oauth.js renderSuccess). Plain HTML with no external
// assets so it renders in the strictest CSP / no-JS environments.
const successHTML = `<!DOCTYPE html>
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
<div>Slack workspace: <code>%s</code></div>
%s
%s
</div>
<p style="margin-top:1.5rem;font-size:.875rem;color:#6b7280">You can close this tab and return to Slack.</p>
</div>
</body>
</html>`

// auth0TokenResponse is the slice of Auth0's /oauth/token response we read.
type auth0TokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
}

// Callback returns the http.HandlerFunc for GET /oauth/qurl/callback.
//
// Steps mirror the Discord LIVE flow (apps/discord/src/routes/qurl-oauth.js):
//  1. Validate cookie + query.state via timing-safe compare.
//  2. Validate the state's HMAC + recover teamID.
//  3. POST to Auth0 /oauth/token to exchange code → access_token + id_token.
//  4. Verify id_token signature against Auth0 JWKS (non-fatal — success
//     page renders without the email line if verify fails).
//  5. POST to qurl-service /v1/api-keys to mint the workspace key.
//  6. Upsert via WorkspaceStore.SetAPIKey; on failure, fire-and-forget
//     revoke on qurl-service.
//  7. DM the admin (fire-and-forget; failure doesn't block the page).
//  8. Render success HTML.
//
// even at this length — every branch is a single early-return error path,
// and splitting into separate funcs would obscure the linear flow.
//
//nolint:gocognit,gocyclo // the linear OAuth callback shape is straightforward
func Callback(cfg Config) http.HandlerFunc { //nolint:gocritic // hugeParam: Config is value-passed at startup once; pointer churn here isn't worth the API-surface friction.
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
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

		// Step 1: extract query params + Auth0-side error pass-through.
		q := r.URL.Query()
		if errParam := q.Get("error"); errParam != "" {
			slog.Warn("oauth/callback Auth0 returned error",
				"error", errParam,
				"error_description", q.Get("error_description"))
			http.Error(w, "authorization declined", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		stateParam := q.Get("state")
		if code == "" || stateParam == "" {
			http.Error(w, "missing code or state", http.StatusBadRequest)
			return
		}

		// Step 2: double-submit cookie check.
		cookieState := readStateCookie(r)
		if cookieState == "" {
			slog.Warn("oauth/callback missing state cookie")
			http.Error(w, "setup must be completed in the same browser", http.StatusBadRequest)
			return
		}
		if len(cookieState) != len(stateParam) ||
			!hmac.Equal([]byte(cookieState), []byte(stateParam)) {
			slog.Warn("oauth/callback cookie/state mismatch")
			http.Error(w, "setup must be completed in the same browser", http.StatusBadRequest)
			return
		}

		// Step 2b: HMAC + expiry on the state token itself.
		teamID, err := verifyState(cfg.OAuthStateSecret, stateParam, now())
		if err != nil {
			slog.Warn("oauth/callback rejected invalid state", "reason", err.Error())
			http.Error(w, "invalid or expired setup link", http.StatusBadRequest)
			return
		}

		// Cookie has done its job — clear so a refresh can't re-bind.
		clearStateCookie(w)

		// Step 3: Auth0 token exchange.
		accessToken, idToken, err := exchangeAuth0Code(r.Context(), httpClient, cfg, code)
		if err != nil {
			slog.Error("oauth/callback Auth0 token exchange failed", "error", err)
			http.Error(w, "authorization failed", http.StatusBadGateway)
			return
		}

		// Step 4: verify id_token (best-effort).
		var qurlEmail string
		if idToken != "" && cfg.IDTokenVerifier != nil {
			email, verr := cfg.IDTokenVerifier.VerifyEmail(r.Context(), idToken)
			switch {
			case verr != nil:
				slog.Warn("oauth/callback id_token verify failed (non-fatal)", "error", verr)
			default:
				qurlEmail = email
			}
		}

		// Step 5: mint qURL API key.
		keyName := "Slack workspace " + teamID
		apiKey, keyID, keyPrefix, err := cfg.Minter.MintAPIKey(r.Context(), accessToken,
			keyName, []string{"qurl:read", "qurl:write"})
		if err != nil {
			slog.Error("oauth/callback qurl-service mint failed", "error", err, "team_id", teamID)
			http.Error(w, "could not provision qURL key", http.StatusBadGateway)
			return
		}

		// Step 6: persist. On failure, best-effort revoke + 500 to user.
		configuredBy := r.URL.Query().Get("admin_user") // optional hint; Slack OAuth provides via separate channel
		if perr := cfg.Provider.SetAPIKey(r.Context(), teamID, apiKey, configuredBy); perr != nil {
			slog.Error("oauth/callback persist failed — revoking minted key",
				"error", perr, "team_id", teamID, "key_id", keyID)
			// Fire-and-forget revoke. Using a fresh context (not r.Context())
			// because the request context may be canceling by the time we
			// get to write the error response; we still want the revoke to
			// run to bound the orphan-key window.
			go func() { //nolint:gosec // G118: intentional — request ctx is about to cancel; we want the revoke to outlive it within its own bounded budget.
				revokeCtx, cancel := context.WithTimeout(context.Background(), auth0TokenTimeout)
				defer cancel()
				if rerr := cfg.Minter.RevokeAPIKey(revokeCtx, accessToken, keyID); rerr != nil {
					slog.Warn("oauth/callback orphan-key revoke failed",
						"error", rerr, "key_id", keyID, "team_id", teamID)
				}
			}()
			http.Error(w, "qURL key provisioned but not stored — run setup again", http.StatusInternalServerError)
			return
		}

		slog.Info("oauth/callback completed", "team_id", teamID, "key_prefix", keyPrefix)

		// Step 7: DM the admin (fire-and-forget; failure non-fatal).
		if cfg.SlackClient != nil && configuredBy != "" {
			go func() { //nolint:gosec // G118: intentional — DM should outlive the HTTP request's lifecycle within its own short budget.
				dmCtx, cancel := context.WithTimeout(context.Background(), dmTimeout)
				defer cancel()
				msg := "qURL is connected to your Slack workspace. Your team can now use `/qurl create`."
				if keyPrefix != "" {
					msg += "\nKey prefix: `" + keyPrefix + "`"
				}
				if derr := cfg.SlackClient.PostDirectMessage(dmCtx, configuredBy, msg); derr != nil {
					slog.Warn("oauth/callback DM failed", "error", derr, "user_id", configuredBy)
				}
			}()
		}

		// Step 8: success HTML.
		renderSuccess(w, teamID, keyPrefix, qurlEmail)
	}
}

func renderSuccess(w http.ResponseWriter, teamID, keyPrefix, email string) {
	keyLine := ""
	if keyPrefix != "" {
		keyLine = fmt.Sprintf("<div>API key prefix: <code>%s</code></div>", htmlEscape(keyPrefix))
	}
	emailLine := ""
	if email != "" {
		emailLine = fmt.Sprintf("<div>qURL account: <code>%s</code></div>", htmlEscape(email))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// keyLine / emailLine are already-escaped HTML fragments (htmlEscape
	// applied at build time below); teamID gets escaped inline. The
	// successHTML template only uses %s with these sanitized inputs.
	if _, err := fmt.Fprintf(w, successHTML, htmlEscape(teamID), keyLine, emailLine); err != nil { //nolint:gosec // G705: all interpolations are htmlEscape'd by construction at the call sites.
		slog.Warn("oauth/callback success-page write failed", "error", err)
	}
}

// htmlEscape is a tiny shim — the values we pass come from validated
// sources (teamID matches teamIDPattern, keyPrefix comes from qurl-
// service's JSON response, email comes from a JWKS-verified id_token)
// but defense-in-depth: nothing user-controlled reaches the page
// without going through this.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

// exchangeAuth0Code POSTs application/x-www-form-urlencoded to
// /oauth/token and returns (access_token, id_token, err).
//
//nolint:gocritic // hugeParam: see Callback above — value-passing is intentional.
func exchangeAuth0Code(ctx context.Context, httpClient *http.Client, cfg Config, code string) (accessToken, idToken string, err error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", cfg.SlackBaseURL+"/oauth/qurl/callback")
	form.Set("client_id", cfg.Auth0ClientID)
	form.Set("client_secret", cfg.Auth0ClientSecret)

	tokenURL := "https://" + cfg.Auth0Domain + "/oauth/token"
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
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
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
