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

	"github.com/layervai/qurl-integrations/shared/auth"
)

const (
	drainCap                             = 32 << 10
	auth0TokenTimeout                    = 15 * time.Second
	revokeTimeout                        = 15 * time.Second
	persistTimeout                       = 15 * time.Second
	bindTimeout                          = 15 * time.Second
	mintTimeout                          = 15 * time.Second
	existingKeyTimeout                   = 5 * time.Second
	revokedKeyScanTimeout                = 20 * time.Second
	auth0TokenBodyLimit                  = 8 << 10
	DefaultSetupBindingReplayWindowHours = 24
	DefaultAPIKeyMintReplayWindowHours   = 24
	keyPrefixLength                      = len("lv_live_abcd")
)

const oauthPageCSS = `
:root{color-scheme:dark;--bg:#030712;--panel:rgba(255,255,245,.035);--panel-strong:rgba(255,255,245,.055);--hairline:rgba(255,255,245,.13);--text:#f5f5f0;--muted:#b8c0cc;--tertiary:#aeb7c4;--lime:#7ec800;--cyan:#38bdf8;--danger:#f87171}
*{box-sizing:border-box}
body{min-height:100vh;margin:0;display:grid;place-items:start center;padding:2rem 1rem;background:radial-gradient(900px 600px at 78% 18%,rgba(255,255,245,.035),transparent 70%),radial-gradient(700px 500px at 22% 88%,rgba(126,200,0,.075),transparent 70%),var(--bg);color:var(--text);font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",system-ui,sans-serif}
body:before{content:"";position:fixed;inset:0;pointer-events:none;background-image:linear-gradient(90deg,rgba(255,255,245,.035) 1px,transparent 1px),linear-gradient(180deg,rgba(255,255,245,.035) 1px,transparent 1px);background-size:88px 88px;opacity:.7}
.card{position:relative;width:100%;max-width:480px;border:1px solid var(--hairline);border-radius:14px;padding:2rem;background:linear-gradient(180deg,var(--panel-strong),var(--panel));box-shadow:0 0 0 1px rgba(255,255,245,.04),0 24px 60px -18px rgba(0,0,0,.65);overflow:hidden}
.card:before{content:"";position:absolute;left:0;right:0;top:0;height:3px;background:linear-gradient(90deg,var(--lime),var(--cyan))}
.brand{display:flex;align-items:center;gap:.65rem;margin-bottom:1.5rem;font-size:.75rem;font-weight:700;letter-spacing:.16em;text-transform:uppercase;color:var(--lime)}
.brand-mark{width:.7rem;height:.7rem;border-radius:50%;background:var(--lime);box-shadow:0 0 20px rgba(126,200,0,.55)}
h1{margin:0 0 .75rem;font-size:1.5rem;line-height:1.15;font-weight:700;letter-spacing:-.02em}
p{margin:.75rem 0 0;color:var(--muted);font-size:.95rem;line-height:1.55}
.kv{margin-top:1.25rem;padding-top:1rem;border-top:1px solid var(--hairline);font-size:.875rem;color:var(--muted)}
.kv div{display:flex;justify-content:space-between;gap:1rem;margin-top:.5rem}
.status{display:inline-flex;align-items:center;justify-content:center;width:1.5rem;height:1.5rem;margin-right:.35rem;border:1px solid currentColor;border-radius:999px;font-size:.85rem;vertical-align:.08em}
.ok{color:var(--lime)}
.warn{color:var(--danger)}
code{background:rgba(255,255,245,.08);border:1px solid rgba(255,255,245,.10);padding:.12rem .35rem;border-radius:5px;color:var(--text);font-size:.875em}
.footer{margin-top:1.5rem;color:var(--tertiary);font-size:.875rem}
@media (max-width:520px){.card{padding:1.5rem;border-radius:12px}h1{font-size:1.35rem}.kv div{display:block}}
`

const oauthPageTemplateBeforeTitle = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>`

const oauthPageTemplateAfterTitle = `</title>
<meta name="robots" content="noindex">
<style>` + oauthPageCSS + `</style>
</head>
<body>
<div class="card">
<div class="brand"><span class="brand-mark" aria-hidden="true"></span><span>LayerV</span></div>
`

const oauthPageTemplateEnd = `</div>
</body>
</html>`

func mustOAuthPageTemplate(name, title, body string) *template.Template {
	return template.Must(template.New(name).Parse(oauthPageTemplateBeforeTitle + title + oauthPageTemplateAfterTitle + body + oauthPageTemplateEnd))
}

var successPageTemplate = mustOAuthPageTemplate("oauth-success", "qURL Connected", `
<h1><span class="status ok" aria-hidden="true">&#10003;</span> qURL Connected</h1>
<p>qURL is connected to your Microsoft Teams tenant. You can close this tab and return to Teams.</p>
<div class="kv">
<div>Teams tenant: <code>{{.TenantID}}</code></div>
{{if .KeyPrefix}}<div>API key prefix: <code>{{.KeyPrefix}}</code></div>{{end}}
{{if .Email}}<div>qURL account: <code>{{.Email}}</code></div>{{end}}
</div>
<p class="footer">Return to Teams and continue from the bot conversation.</p>
`)

type successPageData struct {
	TenantID  string
	KeyPrefix string
	Email     string
}

var rebindRefusedPageTemplate = mustOAuthPageTemplate("oauth-rebind-refused", "qURL setup blocked", `
<h1><span class="status warn" aria-hidden="true">&#9888;</span> qURL setup blocked</h1>
<p>This Teams tenant is already connected to qURL under a different owner. To avoid silently overwriting that binding, this setup attempt was refused.</p>
<div class="kv">
<div>Teams tenant: <code>{{.TenantID}}</code></div>
</div>
<p class="footer">Contact the existing qURL owner or your qURL operator.</p>
`)

type rebindRefusedPageData struct {
	TenantID string
}

var oauthErrorPageTemplate = mustOAuthPageTemplate("oauth-error", "{{.Heading}}", `
<h1><span class="status warn" aria-hidden="true">&#9888;</span> {{.Heading}}</h1>
{{range .Messages}}<p>{{.}}</p>{{end}}
<p class="footer">You can close this tab and return to Teams.</p>
`)

type oauthErrorPageData struct {
	Heading  string
	Messages []string
}

type auth0TokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
}

// Callback completes Teams setup after the Auth0 redirect returns with a code.
func Callback(cfg Config) http.HandlerFunc {
	now := cfg.now()
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: auth0TokenTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		verified, code, ok := validateCallbackRequest(w, r, &cfg, now)
		if !ok {
			return
		}
		accessToken, idToken, err := exchangeAuth0Code(r.Context(), httpClient, cfg, code)
		if err != nil {
			slog.Error("oauth/callback Auth0 token exchange failed", "error", err)
			renderOAuthErrorPage(w, http.StatusBadGateway, "Couldn't connect qURL",
				"qURL could not complete authorization.",
				"Return to Teams and run `setup <email>` again in a few minutes.")
			return
		}
		qurlEmail, qurlSub := verifyIDTokenClaims(r.Context(), &cfg, idToken)
		if !checkSetupEmailMatches(w, verified, qurlEmail) {
			return
		}
		if !checkBindAllowed(w, &cfg, verified, qurlSub) {
			return
		}
		keyPrefix, ok := ensureWorkspaceAPIKey(w, cfg, accessToken, verified.TeamID, verified.UserID, qurlSub, verified.Mode)
		if !ok {
			return
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
		renderOAuthErrorPage(w, http.StatusBadRequest, "qURL account mismatch",
			"The signed-in qURL account did not match the email used to start setup.",
			"Return to Teams and run `setup <email>` again with the same qURL account.")
		return false
	}
	return true
}

func validateCallbackRequest(w http.ResponseWriter, r *http.Request, cfg *Config, now func() time.Time) (verified VerifiedState, code string, ok bool) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		renderOAuthErrorPage(w, http.StatusMethodNotAllowed, "Use the Teams setup link",
			"This qURL setup callback only works from the browser redirect opened by the Teams bot.")
		return VerifiedState{}, "", false
	}
	q := r.URL.Query()
	if errParam := q.Get("error"); errParam != "" {
		clearStateCookie(w)
		renderOAuthErrorPage(w, http.StatusBadRequest, "Authorization didn't complete",
			"qURL setup was not authorized.",
			"Return to Teams and run `setup <email>` again.")
		return VerifiedState{}, "", false
	}
	code = q.Get("code")
	stateParam := q.Get("state")
	if code == "" || stateParam == "" {
		renderOAuthErrorPage(w, http.StatusBadRequest, "Setup link is incomplete",
			"The qURL authorization link is missing required setup details.",
			"Return to Teams and run `setup <email>` again.")
		return VerifiedState{}, "", false
	}
	cookieState := readStateCookie(r)
	if cookieState == "" {
		clearStateCookie(w)
		renderOAuthErrorPage(w, http.StatusBadRequest, "Continue setup in the same browser",
			"This qURL setup link must be completed in the same browser where it was opened.",
			"Return to Teams and run `setup <email>` again.")
		return VerifiedState{}, "", false
	}
	if !hmac.Equal([]byte(cookieState), []byte(stateParam)) {
		clearStateCookie(w)
		renderOAuthErrorPage(w, http.StatusBadRequest, "Continue setup in the same browser",
			"This qURL setup link must be completed in the same browser where it was opened.",
			"Return to Teams and run `setup <email>` again.")
		return VerifiedState{}, "", false
	}
	v, err := VerifyState(cfg.OAuthStateSecret, stateParam, now())
	if err != nil {
		clearStateCookie(w)
		renderOAuthErrorPage(w, http.StatusBadRequest, "Setup link is invalid or expired",
			"This qURL setup link is invalid or expired.",
			"Return to Teams and run `setup <email>` again.")
		return VerifiedState{}, "", false
	}
	clearStateCookie(w)
	return v, code, true
}

func verifyIDTokenClaims(ctx context.Context, cfg *Config, idToken string) (email, sub string) {
	if idToken == "" || cfg.IDTokenVerifier == nil {
		return "", ""
	}
	if e, err := cfg.IDTokenVerifier.VerifyEmail(ctx, idToken); err == nil {
		email = e
	} else {
		slog.Warn("oauth/callback id_token email verify failed", "error", err)
	}
	if s, err := cfg.IDTokenVerifier.VerifySub(ctx, idToken); err == nil {
		sub = s
	} else {
		slog.Warn("oauth/callback id_token sub verify failed", "error", err)
	}
	return email, sub
}

func checkBindAllowed(w http.ResponseWriter, cfg *Config, verified VerifiedState, qurlSub string) bool {
	if cfg.AdminStore == nil {
		return true
	}
	if qurlSub == "" {
		renderOAuthErrorPage(w, http.StatusInternalServerError, "Couldn't confirm your qURL account",
			"qURL could not confirm the signed-in account needed to bind this Teams tenant.",
			"Return to Teams and run `setup <email>` again.")
		return false
	}
	bindCtx, cancel := context.WithTimeout(context.Background(), bindTimeout)
	defer cancel()
	err := cfg.AdminStore.BindWorkspace(bindCtx, &WorkspaceMapping{
		TenantID: verified.TeamID,
		OwnerID:  verified.UserID,
	}, verified.UserID)
	if err == nil {
		return true
	}
	return handleBindError(w, cfg, err, verified.TeamID)
}

func handleBindError(w http.ResponseWriter, cfg *Config, bindErr error, tenantID string) bool {
	var code BindConflictCode
	if cfg.BindClassifyError != nil {
		code = cfg.BindClassifyError(bindErr)
	}
	switch code {
	case BindConflictAlreadyBoundToCaller:
		return true
	case BindConflictAlreadyBound:
		renderRebindRefused(w, tenantID)
		return false
	default:
		renderOAuthErrorPage(w, http.StatusInternalServerError, "Couldn't bind this Teams tenant",
			"qURL could not finish binding this Teams tenant.",
			"Return to Teams and run `setup <email>` again in a few minutes.")
		return false
	}
}

func ensureWorkspaceAPIKey(w http.ResponseWriter, cfg Config, accessToken, tenantID, userID, qurlAccountID string, mode SetupMode) (string, bool) {
	switch mode {
	case SetupModeRotate, SetupModeRepoint:
		return replaceWorkspaceAPIKey(w, cfg, accessToken, tenantID, userID, qurlAccountID, mode)
	case SetupModeReuse:
	default:
		renderOAuthErrorPage(w, http.StatusBadRequest, "Unsupported qURL setup mode",
			"qURL could not determine how this Teams setup request should handle the stored workspace key.",
			"Return to Teams and run `setup <email>` again.")
		return "", false
	}
	keyPrefix, reused, ok := reuseStoredWorkspaceKey(w, cfg, tenantID)
	if !ok {
		return "", false
	}
	if reused {
		return keyPrefix, true
	}
	return mintAndPersist(w, cfg, accessToken, tenantID, userID, qurlAccountID)
}

func replaceWorkspaceAPIKey(w http.ResponseWriter, cfg Config, accessToken, tenantID, userID, qurlAccountID string, mode SetupMode) (string, bool) {
	readCtx, cancel := context.WithTimeout(context.Background(), existingKeyTimeout)
	defer cancel()
	keyID, storedAccountID, err := cfg.Provider.APIKeyIdentity(readCtx, teamsWorkspaceID(tenantID))
	if errors.Is(err, auth.ErrWorkspaceNotConfigured) {
		return mintAndPersist(w, cfg, accessToken, tenantID, userID, qurlAccountID)
	}
	if err != nil {
		renderOAuthErrorPage(w, http.StatusInternalServerError, "Couldn't update qURL key",
			"qURL could not read the stored Teams workspace key.",
			"Return to Teams and run `setup <email> --rotate` or `--repoint` again.")
		return "", false
	}
	if storedAccountID != "" && storedAccountID != qurlAccountID {
		renderOAuthErrorPage(w, http.StatusConflict, "qURL key belongs to a different account",
			"This Teams tenant's qURL key belongs to a different qURL account, so qURL will not move it automatically.",
			"If you meant to replace the key on the account that already holds it, run `setup <email> --rotate` signed in as that account. For a real account move, contact your qURL operator.")
		return "", false
	}
	if storedAccountID == "" && mode == SetupModeRepoint {
		renderOAuthErrorPage(w, http.StatusConflict, "Can't repoint qURL key from Teams",
			"This tenant was connected before Teams stored which qURL account owns the key, so a cross-account move can't be verified safely.",
			"If you own the current account, run `setup <email> --rotate` first.")
		return "", false
	}
	if keyID == "" {
		renderOAuthErrorPage(w, http.StatusConflict, "Can't rotate qURL key from Teams",
			"This Teams tenant was connected before Teams stored qURL key identity, so Teams cannot revoke the old key safely.",
			"Rotate the key from qURL account/API-key management or contact your qURL operator.")
		return "", false
	}
	revokeCtx, revokeCancel := context.WithTimeout(context.Background(), revokeTimeout)
	defer revokeCancel()
	if err := cfg.Minter.RevokeAPIKey(revokeCtx, accessToken, keyID); err != nil {
		if !confirmStoredKeyAlreadyRevoked(w, cfg, accessToken, keyID, err) {
			return "", false
		}
	}
	return mintReplacementAndPersist(w, cfg, accessToken, tenantID, keyID, userID, qurlAccountID)
}

func confirmStoredKeyAlreadyRevoked(w http.ResponseWriter, cfg Config, accessToken, keyID string, revokeErr error) bool {
	if errors.Is(revokeErr, ErrAPIKeyNotFound) {
		statusCtx, cancel := context.WithTimeout(context.Background(), revokedKeyScanTimeout)
		defer cancel()
		revoked, err := cfg.Minter.APIKeyRevoked(statusCtx, accessToken, keyID)
		if err == nil && revoked {
			return true
		}
		if err != nil {
			renderOAuthErrorPage(w, http.StatusBadGateway, "Couldn't rotate qURL key",
				"qURL could not confirm whether the previous key was already revoked.",
				"Return to Teams and run `setup <email> --rotate` again in a few minutes.")
			return false
		}
		renderOAuthErrorPage(w, http.StatusConflict, "Couldn't rotate qURL key",
			"The current workspace key could not be confirmed as revoked under this qURL account.",
			"Return to Teams and retry with the account that owns the existing key.")
		return false
	}
	renderOAuthErrorPage(w, http.StatusBadGateway, "Couldn't rotate qURL key",
		"qURL could not revoke the previous workspace key.",
		"Return to Teams and run `setup <email> --rotate` again in a few minutes.")
	return false
}

func reuseStoredWorkspaceKey(w http.ResponseWriter, cfg Config, tenantID string) (keyPrefix string, reused bool, ok bool) {
	readCtx, cancel := context.WithTimeout(context.Background(), existingKeyTimeout)
	defer cancel()
	apiKey, err := cfg.Provider.APIKey(readCtx, teamsWorkspaceID(tenantID))
	if errors.Is(err, auth.ErrWorkspaceNotConfigured) {
		return "", false, true
	}
	if err != nil {
		renderOAuthErrorPage(w, http.StatusInternalServerError, "Couldn't connect qURL",
			"qURL is already connected to this Teams tenant, but the stored key could not be read.",
			"Return to Teams and run `setup <email>` again in a few minutes.")
		return "", false, false
	}
	validateCtx, validateCancel := context.WithTimeout(context.Background(), existingKeyTimeout)
	defer validateCancel()
	if err := cfg.Minter.ValidateAPIKey(validateCtx, apiKey); err != nil {
		if errors.Is(err, ErrStoredAPIKeyInvalid) {
			keyIDCtx, keyIDCancel := context.WithTimeout(context.Background(), existingKeyTimeout)
			defer keyIDCancel()
			keyID, keyIDErr := cfg.Provider.APIKeyID(keyIDCtx, teamsWorkspaceID(tenantID))
			if keyIDErr != nil && !errors.Is(keyIDErr, auth.ErrWorkspaceNotConfigured) {
				renderOAuthErrorPage(w, http.StatusInternalServerError, "Couldn't connect qURL",
					"qURL is connected to this Teams tenant, but the stored workspace key metadata could not be read.",
					"Return to Teams and run `setup <email> --rotate` again.")
				return "", false, false
			}
			if keyID != "" {
				renderOAuthErrorPage(w, http.StatusConflict, "qURL key needs rotation",
					"The stored Teams workspace key is no longer accepted by qURL. Run `setup <email> --rotate` or `--repoint` to recover safely.")
				return "", false, false
			}
			return "", false, true
		}
		renderOAuthErrorPage(w, http.StatusBadGateway, "Couldn't connect qURL",
			"qURL is already connected to this Teams tenant, but the stored workspace key could not be verified.",
			"Return to Teams and run `setup <email>` again in a few minutes.")
		return "", false, false
	}
	return storedAPIKeyPrefix(apiKey), true, true
}

func storedAPIKeyPrefix(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if len(apiKey) <= keyPrefixLength {
		return ""
	}
	return apiKey[:keyPrefixLength]
}

func mintAndPersist(w http.ResponseWriter, cfg Config, accessToken, tenantID, userID, qurlAccountID string) (string, bool) {
	mintCtx, cancel := context.WithTimeout(context.Background(), mintTimeout)
	defer cancel()
	minted, err := cfg.Minter.MintWorkspaceAPIKey(mintCtx, accessToken, tenantID)
	if err != nil {
		switch {
		case errors.Is(err, ErrAPIKeyProvisioningQuotaReached):
			renderOAuthErrorPage(w, http.StatusConflict, "qURL key limit reached",
				"Your qURL account is already at its API-key limit.",
				"Revoke one you no longer use, then run `setup <email>` again.")
		case errors.Is(err, ErrExternalIdentityAlreadyBound):
			renderOAuthErrorPage(w, http.StatusConflict, "qURL already connected",
				"qURL is already connected for this Teams tenant, but this setup attempt could not recover the workspace key.",
				"Contact your qURL administrator for help.")
		default:
			renderOAuthErrorPage(w, http.StatusBadGateway, "Couldn't connect qURL",
				"Something went wrong while creating your qURL API key.",
				"Return to Teams and run `setup <email>` again in a few minutes.")
		}
		return "", false
	}
	persistCtx, persistCancel := context.WithTimeout(context.Background(), persistTimeout)
	defer persistCancel()
	if err := cfg.Provider.SetAPIKeyWithMetadata(persistCtx, teamsWorkspaceID(tenantID), minted.APIKey, minted.KeyID, minted.KeyPrefix, qurlAccountID, userID); err != nil {
		if !minted.BindingBacked {
			revokeCtx, revokeCancel := context.WithTimeout(context.Background(), revokeTimeout)
			defer revokeCancel()
			_ = cfg.Minter.RevokeAPIKey(revokeCtx, accessToken, minted.KeyID)
		}
		renderOAuthErrorPage(w, http.StatusInternalServerError, "qURL setup did not finish",
			"qURL created a workspace key, but the Teams integration could not save it before setup finished.",
			"Return to Teams and run `setup <email>` again in a few minutes.")
		return "", false
	}
	return minted.KeyPrefix, true
}

func mintReplacementAndPersist(w http.ResponseWriter, cfg Config, accessToken, tenantID, oldKeyID, userID, qurlAccountID string) (string, bool) {
	mintCtx, cancel := context.WithTimeout(context.Background(), mintTimeout)
	defer cancel()
	minted, err := cfg.Minter.MintWorkspaceReplacementAPIKey(mintCtx, accessToken, tenantID, oldKeyID)
	if err != nil {
		if errors.Is(err, ErrAPIKeyProvisioningQuotaReached) {
			renderOAuthErrorPage(w, http.StatusConflict, "qURL key limit reached",
				"The previous Teams workspace key was revoked, but your qURL account is at its API-key limit, so a replacement couldn't be created.",
				"Revoke one you no longer use, then run `setup <email> --rotate` again.")
		} else {
			renderOAuthErrorPage(w, http.StatusBadGateway, "Couldn't rotate qURL key",
				"The previous Teams workspace key was revoked, but a replacement could not be created.",
				"Return to Teams and run `setup <email> --rotate` again in a few minutes.")
		}
		return "", false
	}
	persistCtx, persistCancel := context.WithTimeout(context.Background(), persistTimeout)
	defer persistCancel()
	if err := cfg.Provider.SetAPIKeyWithMetadata(persistCtx, teamsWorkspaceID(tenantID), minted.APIKey, minted.KeyID, minted.KeyPrefix, qurlAccountID, userID); err != nil {
		renderOAuthErrorPage(w, http.StatusInternalServerError, "Couldn't finish qURL key rotation",
			"The previous Teams workspace key was revoked, but the replacement could not be stored.",
			"Return to Teams and run `setup <email> --rotate` again soon.")
		return "", false
	}
	return minted.KeyPrefix, true
}

func setOAuthPageSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func renderRebindRefused(w http.ResponseWriter, tenantID string) {
	setOAuthPageSecurityHeaders(w)
	w.WriteHeader(http.StatusConflict)
	_ = rebindRefusedPageTemplate.Execute(w, rebindRefusedPageData{TenantID: tenantID})
}

func renderOAuthErrorPage(w http.ResponseWriter, status int, heading, firstParagraph string, rest ...string) {
	setOAuthPageSecurityHeaders(w)
	w.WriteHeader(status)
	_ = oauthErrorPageTemplate.Execute(w, oauthErrorPageData{
		Heading:  heading,
		Messages: oauthErrorMessages(firstParagraph, rest...),
	})
}

func oauthErrorMessages(firstParagraph string, rest ...string) []string {
	messages := make([]string, 0, 1+len(rest))
	if trimmed := strings.TrimSpace(firstParagraph); trimmed != "" {
		messages = append(messages, trimmed)
	}
	for _, msg := range rest {
		if trimmed := strings.TrimSpace(msg); trimmed != "" {
			messages = append(messages, trimmed)
		}
	}
	if len(messages) == 0 {
		return []string{"qURL setup could not finish. Try again or contact your qURL administrator if this keeps happening."}
	}
	return messages
}

func renderSuccess(w http.ResponseWriter, tenantID, keyPrefix, email string) {
	setOAuthPageSecurityHeaders(w)
	w.WriteHeader(http.StatusOK)
	_ = successPageTemplate.Execute(w, successPageData{
		TenantID:  tenantID,
		KeyPrefix: keyPrefix,
		Email:     email,
	})
}

func truncateForLog(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…[truncated]"
}

func exchangeAuth0Code(ctx context.Context, httpClient *http.Client, cfg Config, code string) (accessToken, idToken string, err error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", callbackURL(cfg.TeamsBaseURL))
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
		return "", "", fmt.Errorf("request Auth0 token: %w", err)
	}
	defer drainAndCloseResponse(resp)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, auth0TokenBodyLimit+1))
	if err != nil {
		return "", "", fmt.Errorf("read Auth0 token body: %w", err)
	}
	if len(raw) > auth0TokenBodyLimit {
		return "", "", fmt.Errorf("Auth0 token response exceeded %d bytes", auth0TokenBodyLimit)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("Auth0 token endpoint returned %d: %s", resp.StatusCode, truncateForLog(strings.TrimSpace(string(raw)), 256))
	}
	var out auth0TokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("parse Auth0 token body: %w", err)
	}
	if out.AccessToken == "" {
		return "", "", errors.New("Auth0 token response missing access_token")
	}
	return out.AccessToken, out.IDToken, nil
}
