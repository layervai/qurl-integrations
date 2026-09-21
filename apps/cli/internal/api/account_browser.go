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

// SignInAccount runs authorization-code + PKCE only after explicit account
// setup. No account token is written to disk or passed to the browser launcher.
// TODO(upstream-contract): Auth0 serves /authorize and /oauth/token and honors
// prompt=login consent. The qURL audience accepts the scopes below; account
// linking requires qurl:agent as well as verified account identity.
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
	mux := accountCallbackHandler(state, codes)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 16384}
	defer func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = server.Shutdown(shutdownCtx)
		_ = server.Close()
	}()
	go func() { _ = server.Serve(listener) }()
	query := url.Values{"response_type": {"code"}, "prompt": {"login consent"}, "client_id": {settings.ClientID}, "redirect_uri": {accountCallback}, "audience": {settings.Audience}, "scope": {"openid email qurl:read qurl:write qurl:agent"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	if err := openBrowser(ctx, "https://"+settings.Domain+"/authorize?"+query.Encode()); err != nil {
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

func accountCallbackHandler(state string, codes chan<- string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method != http.MethodGet || r.Host != accountCallbackAddress || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(state)) != 1 {
			http.Error(w, msgAccountCallbackInvalid, http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		if len(code) > 4096 || r.URL.Query().Get("error") != "" {
			code = ""
		}
		select {
		case codes <- code:
		default:
		}
		if code == "" {
			http.Error(w, msgAccountCanceled, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, msgAccountCallbackComplete)
	})
	return mux
}

func validateAccountEndpoint(endpoint string) error {
	base, err := url.Parse(endpoint)
	if err != nil || base.Host == "" || (base.Scheme != "https" && (base.Scheme != "http" || (base.Hostname() != "localhost" && !net.ParseIP(base.Hostname()).IsLoopback()))) {
		return ErrAccountEndpoint
	}
	return nil
}
