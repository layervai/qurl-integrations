package qurlapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TODO(upstream-contract): Auth0 requires this exact registered callback URI.
const accountCallbackAddress = "127.0.0.1:8765"
const accountCallback = "http://" + accountCallbackAddress + "/callback"
const accountStateParam = "state"

const accountSignInPageHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in | LayerV</title>
<style>
body{margin:0;background:#f6f4ef;color:#0f1320;font:16px/1.6 system-ui,sans-serif;display:grid;min-height:100vh;place-items:center}
main{box-sizing:border-box;width:min(440px,calc(100% - 32px));padding:32px;background:white;border:1px solid #ddd;border-radius:18px}
h1{font-size:28px;line-height:1.2}p{color:#3d4250}.brand{font-weight:700;color:#0f1320}
a{color:#3a54cf}a:focus-visible{outline:3px solid #3a54cf;outline-offset:4px}
.button{display:flex;align-items:center;justify-content:center;gap:10px;margin:12px 0;padding:12px;text-align:center;text-decoration:none;border:1px solid #3a54cf;border-radius:30px;font-weight:600}
.button svg{width:20px;height:20px;flex:none}.apple{color:#000}.primary{background:#3a54cf;color:white}.footer{font-size:14px;margin-top:28px}.cancel{display:block;margin-top:20px}
</style></head><body><main>
<p class="brand">qURL™ by LayerV</p><h1>Sign in or create an account</h1>
<p>Choose how to sign in. Email uses a one-time code.</p>
<a class="button primary" href="{{.Email}}"><svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="m3 7 9 6 9-6"/></svg>Continue with Email</a>
<a class="button" href="{{.Google}}"><svg viewBox="0 0 24 24" aria-hidden="true"><path fill="#4285F4" d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92a5.06 5.06 0 0 1-2.2 3.32v2.77h3.57c2.08-1.92 3.27-4.74 3.27-8.1z"/><path fill="#34A853" d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84A11 11 0 0 0 12 23z"/><path fill="#FBBC05" d="M5.84 14.1a6.6 6.6 0 0 1 0-4.2V7.06H2.18a11 11 0 0 0 0 9.88l3.66-2.84z"/><path fill="#EA4335" d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1A11 11 0 0 0 2.18 7.06l3.66 2.84C6.71 7.31 9.14 5.38 12 5.38z"/></svg>Continue with Google</a>
<a class="button apple" href="{{.Apple}}"><svg viewBox="0 0 24 24" aria-hidden="true" fill="#000"><path d="M16.37 12.6c-.02-2.2 1.8-3.26 1.88-3.31-1.02-1.5-2.6-1.7-3.17-1.73-1.35-.14-2.63.8-3.31.8-.69 0-1.74-.78-2.86-.76-1.47.02-2.82.86-3.58 2.17-1.53 2.65-.39 6.57 1.1 8.72.73 1.05 1.6 2.23 2.74 2.19 1.1-.04 1.52-.71 2.85-.71s1.7.71 2.86.69c1.18-.02 1.93-1.07 2.65-2.13.84-1.22 1.18-2.4 1.2-2.46-.03-.01-2.3-.88-2.36-3.47zM14.2 6.1c.6-.73 1.01-1.75.9-2.76-.87.04-1.92.58-2.54 1.31-.56.65-1.05 1.68-.92 2.67.97.08 1.96-.49 2.56-1.22z"/></svg>Continue with Apple</a>
<a href="{{.Password}}">Use your password</a>
<p class="footer">Use the same email address as your LayerV account. Only continue if you started this request.</p>
<a class="cancel" href="{{.Cancel}}">Cancel</a>
</main></body></html>`

var accountSignInPage = template.Must(template.New("account-signin").Parse(accountSignInPageHTML))

// SignInAccount runs authorization-code + PKCE only after explicit account
// setup. No account token is written to disk or passed to the browser launcher.
// TODO(upstream-contract): Auth0 serves /authorize and /oauth/token and honors
// prompt=login consent with an explicit connection. The qURL audience accepts
// the scopes below; linking requires qurl:agent and verified account identity.
func SignInAccount(ctx context.Context, cfg *Config, openBrowser func(context.Context, string) error) (_ string, retErr error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	defer func() {
		if retErr != nil && errors.Is(ctx.Err(), context.Canceled) {
			retErr = context.Canceled
		}
	}()
	if err := validateAccountEndpoint(cfg.BaseURL); err != nil {
		return "", err
	}
	browserConfig := *cfg
	browserConfig.APIKey = ""
	browserConfig.OwnerID = ""
	client := newTransport(&browserConfig)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, trimBaseURL(cfg.BaseURL)+"/v1/account/auth", http.NoBody)
	if err != nil {
		return "", err
	}
	response, err := client.DoOnce(request)
	if err != nil {
		return "", ErrAccountLoad
	}
	var settings struct {
		Domain   string `json:"domain"`
		ClientID string `json:"client_id"`
		Audience string `json:"audience"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 16384)).Decode(&settings)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || settings.ClientID == "" || settings.Audience == "" || settings.Domain == "" || strings.ContainsAny(settings.Domain, "/@?#\\") {
		return "", ErrAccountUnavailable
	}
	nonce := make([]byte, 64)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(nonce[:32])
	state := base64.RawURLEncoding.EncodeToString(nonce[32:])
	challenge := sha256.Sum256([]byte(verifier))
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", accountCallbackAddress)
	if err != nil {
		return "", ErrAccountPort
	}
	defer func() { _ = listener.Close() }()
	codes := make(chan string, 1)
	query := url.Values{"response_type": {"code"}, "prompt": {"login consent"}, "client_id": {settings.ClientID}, "redirect_uri": {accountCallback}, "audience": {settings.Audience}, "scope": {"openid email qurl:read qurl:write qurl:agent"}, accountStateParam: {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	authorizeURL := &url.URL{Scheme: schemeHTTPS, Host: settings.Domain, Path: "/authorize", RawQuery: query.Encode()}
	mux := accountBrowserHandler(state, authorizeURL, codes)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 16384}
	defer func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = server.Shutdown(shutdownCtx)
		_ = server.Close()
	}()
	go func() { _ = server.Serve(listener) }()
	if err := openBrowser(ctx, "http://"+accountCallbackAddress+"/?"+url.Values{accountStateParam: {state}}.Encode()); err != nil {
		return "", fmt.Errorf(msgAccountBrowserFailed, err)
	}
	var code string
	select {
	case code = <-codes:
	case <-ctx.Done():
		return "", fmt.Errorf("%s: %w", msgAccountTimedOut, ctx.Err())
	}
	if code == "" {
		return "", ErrAccountDenied
	}
	return exchangeAccountCode(ctx, client, settings.Domain, settings.ClientID, code, verifier)
}

func exchangeAccountCode(ctx context.Context, client *transport, domain, clientID, code, verifier string) (string, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {accountCallback}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+domain+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.DoOnce(request)
	if err != nil {
		return "", ErrAccountExchange
	}
	defer func() { _ = response.Body.Close() }()
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 32768)).Decode(&token) != nil || token.AccessToken == "" || !strings.EqualFold(token.TokenType, "Bearer") {
		return "", ErrAccountExchange
	}
	return token.AccessToken, nil
}

func accountBrowserHandler(state string, authorizeURL *url.URL, codes chan<- string) http.Handler {
	links := make(map[string]string, 5)
	// TODO(upstream-contract): These connection names must match the qURL Auth0 tenant.
	for method, connection := range map[string]string{"Email": "email", "Google": "google-oauth2", "Apple": "apple", "Password": "Username-Password-Authentication"} {
		target := *authorizeURL
		query := target.Query()
		query.Set("connection", connection)
		target.RawQuery = query.Encode()
		links[method] = target.String()
	}
	links["Cancel"] = accountCallback + "?" + url.Values{accountStateParam: {state}, "error": {"access_denied"}}.Encode()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet || r.Host != accountCallbackAddress || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get(accountStateParam)), []byte(state)) != 1 {
			http.Error(w, msgAccountCallbackInvalid, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// With fixed string data, Execute can fail only if the response write fails.
		_ = accountSignInPage.Execute(w, links)
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method != http.MethodGet || r.Host != accountCallbackAddress || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get(accountStateParam)), []byte(state)) != 1 {
			http.Error(w, msgAccountCallbackInvalid, http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		invalidCode := len(code) > 4096
		signInError := r.URL.Query().Get("error")
		if invalidCode || signInError != "" {
			code = ""
		}
		select {
		case codes <- code:
		default:
		}
		if code == "" {
			if signInError == "access_denied" && !invalidCode {
				_, _ = io.WriteString(w, msgAccountCanceledPage)
				return
			}
			http.Error(w, msgAccountCanceled, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, msgAccountCallbackComplete)
	})
	return mux
}

func validateAccountEndpoint(endpoint string) error {
	base, err := url.Parse(endpoint)
	if err != nil || base.Host == "" || (base.Scheme != schemeHTTPS && (base.Scheme != "http" || (base.Hostname() != "localhost" && !net.ParseIP(base.Hostname()).IsLoopback()))) {
		return ErrAccountEndpoint
	}
	return nil
}
