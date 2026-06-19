package oauth

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
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

// verifiedToken parses + verifies the id_token signature + claims
// against Auth0's JWKS. Shared by VerifyEmail and VerifySub so the
// verify posture (kid required, alg inferred from key, iss + aud
// validated) lives in one place.
func (v *JWKSVerifier) verifiedToken(ctx context.Context, idToken string) (jwt.Token, error) {
	if v.cache == nil {
		return nil, errors.New("JWKSVerifier: cache not initialized")
	}
	set, err := v.cache.Get(ctx, v.jwksURL)
	if err != nil {
		return nil, fmt.Errorf("get jwks: %w", err)
	}
	// WithRequireKid + WithInferAlgorithmFromKey: defense against an
	// alg-confusion variant where a future Auth0 misconfig publishes a
	// key without `alg`. jwx would otherwise infer from `kid` alone;
	// pinning both ensures the token's header alg matches the key's
	// declared alg, and that the token actually carries a kid header.
	tok, err := jwt.Parse([]byte(idToken),
		jwt.WithKeySet(set,
			jws.WithRequireKid(true),
			jws.WithInferAlgorithmFromKey(true)),
		jwt.WithIssuer(v.Issuer),
		jwt.WithAudience(v.Audience),
		jwt.WithValidate(true),
	)
	if err != nil {
		return nil, fmt.Errorf("parse/verify: %w", err)
	}
	return tok, nil
}

// VerifyEmail verifies the id_token signature + claims and returns the
// email claim. Returns ("", err) on any verify failure — the callback
// treats this as non-fatal (success page renders without the email).
func (v *JWKSVerifier) VerifyEmail(ctx context.Context, idToken string) (string, error) {
	tok, err := v.verifiedToken(ctx, idToken)
	if err != nil {
		return "", err
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

// VerifySub verifies the id_token and returns the `sub` claim — Auth0's
// stable identifier for the authenticated user, used as the workspace
// OwnerID when BindWorkspace seeds the admin row. Returns ("", err) on
// any verify failure or an absent / empty sub claim. The callback
// treats this as fatal-to-bind (unlike VerifyEmail's best-effort
// posture): an empty sub can't legitimately key a workspace.
func (v *JWKSVerifier) VerifySub(ctx context.Context, idToken string) (string, error) {
	tok, err := v.verifiedToken(ctx, idToken)
	if err != nil {
		return "", err
	}
	// jwt.Token.Subject() reads the standard `sub` claim (RFC 7519
	// §4.1.2). Auth0 always populates it on id_tokens; an empty value
	// signals a misconfigured federation upstream.
	sub := tok.Subject()
	if sub == "" {
		return "", errors.New("sub claim missing or empty")
	}
	return sub, nil
}

// VerifyNonce verifies the id_token nonce claim against the value carried in
// the signed setup state. A mismatch means the id_token belongs to a different
// authorization request and the callback must fail closed before binding or
// minting workspace credentials.
func (v *JWKSVerifier) VerifyNonce(ctx context.Context, idToken, expectedNonce string) error {
	if expectedNonce == "" {
		return errors.New("expected nonce is empty")
	}
	tok, err := v.verifiedToken(ctx, idToken)
	if err != nil {
		return err
	}
	rawNonce, ok := tok.Get("nonce")
	if !ok {
		return errors.New("nonce claim missing")
	}
	nonce, ok := rawNonce.(string)
	if !ok {
		return errors.New("nonce claim is not a string")
	}
	if !hmac.Equal([]byte(nonce), []byte(expectedNonce)) {
		return errors.New("nonce claim mismatch")
	}
	return nil
}
