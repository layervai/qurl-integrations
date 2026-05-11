package oauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// JWKSVerifier verifies Auth0 id_tokens against the tenant's JWKS at
// https://<domain>/.well-known/jwks.json. Cache TTL is 1h by default —
// JWKS rarely rotates, so a longer cache lowers Auth0 RPS without
// meaningfully extending the post-revocation window.
type JWKSVerifier struct {
	Issuer   string // e.g. "https://layerv.us.auth0.com/" — must end with "/"
	Audience string // the Auth0 client_id
	cache    *jwk.Cache
}

// NewJWKSVerifier constructs a verifier and starts a background
// cache-refresh goroutine for the JWKS URI. The returned ctx is the
// parent for the refresh; cancel it on shutdown.
func NewJWKSVerifier(ctx context.Context, issuer, audience string) (*JWKSVerifier, error) {
	jwksURL := issuer + ".well-known/jwks.json"
	c := jwk.NewCache(ctx)
	if err := c.Register(jwksURL, jwk.WithMinRefreshInterval(15*time.Minute)); err != nil {
		return nil, fmt.Errorf("register jwks: %w", err)
	}
	// Prime the cache so the first /callback doesn't pay a cold-start fetch.
	if _, err := c.Refresh(ctx, jwksURL); err != nil {
		return nil, fmt.Errorf("refresh jwks: %w", err)
	}
	return &JWKSVerifier{Issuer: issuer, Audience: audience, cache: c}, nil
}

// VerifyEmail verifies the id_token signature + claims and returns the
// email claim. Returns ("", err) on any verify failure — the callback
// treats this as non-fatal (success page renders without the email).
func (v *JWKSVerifier) VerifyEmail(ctx context.Context, idToken string) (string, error) {
	if v.cache == nil {
		return "", errors.New("JWKSVerifier: cache not initialized")
	}
	jwksURL := v.Issuer + ".well-known/jwks.json"
	set, err := v.cache.Get(ctx, jwksURL)
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
	email, ok := tok.Get("email")
	if !ok {
		return "", nil // verified but no email claim — surface as ""
	}
	s, ok := email.(string)
	if !ok {
		return "", errors.New("email claim is not a string")
	}
	return s, nil
}
