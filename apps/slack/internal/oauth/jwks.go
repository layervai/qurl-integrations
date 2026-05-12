package oauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

const (
	// jwksRefreshInterval bounds how often the cache will re-fetch the
	// JWKS document from Auth0. Auth0's signing keys rotate at most
	// every ~few months in practice — 15m is well under that horizon
	// while keeping the per-task RPS against /.well-known/jwks.json
	// well below 1 req/min.
	jwksRefreshInterval = 15 * time.Minute
	// jwksPrimeTimeout caps how long NewJWKSVerifier may wait on the
	// initial refresh. If Auth0 is briefly unreachable at boot we'd
	// rather start up degraded and warm the cache on the first /callback
	// than wedge in init for the full request-timeout budget.
	jwksPrimeTimeout = 5 * time.Second
)

// JWKSVerifier verifies Auth0 id_tokens against the tenant's JWKS at
// https://<domain>/.well-known/jwks.json. The cache is refreshed at
// jwksRefreshInterval; the refresh goroutine is rooted at the context
// passed to NewJWKSVerifier so canceling it (typically SIGTERM via
// signalCtx) tears the goroutine down with the rest of the server.
type JWKSVerifier struct {
	Issuer   string // e.g. "https://layerv.us.auth0.com/" — must end with "/"
	Audience string // the Auth0 client_id
	jwksURL  string // computed once at construction; cache key and Refresh target
	cache    *jwk.Cache
}

// NewJWKSVerifier constructs a verifier and starts a background
// cache-refresh goroutine for the JWKS URI. The supplied ctx is the
// parent for the refresh goroutine — callers must pass a context that
// cancels on shutdown (e.g. the signal-canceled context from
// signal.NotifyContext) so the goroutine doesn't outlive the process.
//
// The initial prime fetch is bounded by jwksPrimeTimeout so a briefly
// unreachable Auth0 doesn't wedge startup; on prime-failure the
// returned error is surfaced and the caller decides whether to fall
// back to the no-verifier code path.
func NewJWKSVerifier(ctx context.Context, issuer, audience string) (*JWKSVerifier, error) {
	jwksURL := issuer + ".well-known/jwks.json"
	c := jwk.NewCache(ctx)
	if err := c.Register(jwksURL, jwk.WithMinRefreshInterval(jwksRefreshInterval)); err != nil {
		return nil, fmt.Errorf("register jwks: %w", err)
	}
	primeCtx, cancel := context.WithTimeout(ctx, jwksPrimeTimeout)
	defer cancel()
	if _, err := c.Refresh(primeCtx, jwksURL); err != nil {
		return nil, fmt.Errorf("refresh jwks: %w", err)
	}
	return &JWKSVerifier{Issuer: issuer, Audience: audience, jwksURL: jwksURL, cache: c}, nil
}

// VerifyEmail verifies the id_token signature + claims and returns the
// email claim. Returns ("", err) on any verify failure — the callback
// treats this as non-fatal (success page renders without the email).
func (v *JWKSVerifier) VerifyEmail(ctx context.Context, idToken string) (string, error) {
	if v.cache == nil {
		return "", errors.New("JWKSVerifier: cache not initialized")
	}
	set, err := v.cache.Get(ctx, v.jwksURL)
	if err != nil {
		return "", fmt.Errorf("get jwks: %w", err)
	}
	tok, err := jwt.Parse([]byte(idToken),
		jwt.WithKeySet(set),
		jwt.WithIssuer(v.Issuer),
		jwt.WithAudience(v.Audience),
		jwt.WithValidate(true),
	)
	if err != nil {
		return "", fmt.Errorf("parse/verify: %w", err)
	}
	// Fail-closed on email_verified: surface the email only when the
	// claim is present *and* a `bool` *and* explicitly true. Some
	// Auth0 enterprise/SAML connections omit the claim entirely;
	// treating that as "verified by default" would let a self-asserted
	// email surface as "qURL account: someone-else@target.tld" on the
	// success page (HTML-escaped — no XSS — but the readout would be
	// misleading). Auth0 ships email_verified as a JSON boolean; a
	// future federation that returned the string "true"/"false" would
	// fall through to the !isBool branch and suppress the email
	// (benign degradation — success page still renders).
	rawVerified, hasClaim := tok.Get("email_verified")
	if !hasClaim {
		return "", nil
	}
	verified, isBool := rawVerified.(bool)
	if !isBool || !verified {
		return "", nil
	}
	email, ok := tok.Get("email")
	if !ok {
		return "", nil // verified flag set but no email claim — surface as ""
	}
	s, ok := email.(string)
	if !ok {
		return "", errors.New("email claim is not a string")
	}
	return s, nil
}
