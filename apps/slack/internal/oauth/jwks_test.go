package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// emailClaim is the canonical JWT claim name; pulled into a const so
// goconst stays quiet when several tests embed it.
const emailClaim = "email"

// jwksTestFixture constructs an RSA keypair, an httptest server serving
// the matching JWKS, and a JWKSVerifier configured to consume it. The
// signer is returned so each test can issue tokens with custom claims.
type jwksTestFixture struct {
	verifier   *JWKSVerifier
	signKey    jwk.Key
	issuer     string
	audience   string
	jwksServer *httptest.Server
}

func newJWKSFixture(t *testing.T, audience string) *jwksTestFixture {
	t.Helper()
	rawKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa: %v", err)
	}
	signKey, err := jwk.FromRaw(rawKey)
	if err != nil {
		t.Fatalf("jwk.FromRaw: %v", err)
	}
	if err := signKey.Set(jwk.KeyIDKey, "test-key"); err != nil {
		t.Fatalf("set kid: %v", err)
	}
	if err := signKey.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatalf("set alg: %v", err)
	}

	pubKey, err := jwk.PublicKeyOf(signKey)
	if err != nil {
		t.Fatalf("PublicKeyOf: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(pubKey); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	jwksJSON, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/.well-known/jwks.json") {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksJSON)
	}))
	t.Cleanup(srv.Close)

	// JWKSVerifier expects issuer to end with "/" so issuer+".well-known/..."
	// yields the right URL. We use the httptest server URL as a pseudo-issuer.
	issuer := srv.URL + "/"
	v, err := NewJWKSVerifier(context.Background(), issuer, audience)
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %v", err)
	}
	return &jwksTestFixture{
		verifier:   v,
		signKey:    signKey,
		issuer:     issuer,
		audience:   audience,
		jwksServer: srv,
	}
}

func (f *jwksTestFixture) signToken(t *testing.T, claims map[string]any) []byte {
	t.Helper()
	tok := jwt.New()
	for k, v := range claims {
		if err := tok.Set(k, v); err != nil {
			t.Fatalf("tok.Set(%s): %v", k, err)
		}
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, f.signKey))
	if err != nil {
		t.Fatalf("jwt.Sign: %v", err)
	}
	return signed
}

func TestJWKSVerifierHappyPath(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	now := time.Now()
	signed := f.signToken(t, map[string]any{
		jwt.IssuerKey:     f.issuer,
		jwt.AudienceKey:   []string{f.audience},
		jwt.SubjectKey:    "sub-1",
		jwt.IssuedAtKey:   now,
		jwt.ExpirationKey: now.Add(5 * time.Minute),
		emailClaim:        testAdminEmail,
		"email_verified":  true,
	})

	email, err := f.verifier.VerifyEmail(context.Background(), string(signed))
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	if email != testAdminEmail {
		t.Errorf("email: got %q want %q", email, testAdminEmail)
	}
}

func TestJWKSVerifierRejectsWrongIssuer(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	now := time.Now()
	signed := f.signToken(t, map[string]any{
		jwt.IssuerKey:     "https://impostor.invalid/",
		jwt.AudienceKey:   []string{f.audience},
		jwt.IssuedAtKey:   now,
		jwt.ExpirationKey: now.Add(5 * time.Minute),
		emailClaim:        "x@example.com",
	})
	if _, err := f.verifier.VerifyEmail(context.Background(), string(signed)); err == nil {
		t.Fatal("expected verify failure on wrong issuer")
	}
}

func TestJWKSVerifierRejectsWrongAudience(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	now := time.Now()
	signed := f.signToken(t, map[string]any{
		jwt.IssuerKey:     f.issuer,
		jwt.AudienceKey:   []string{"someone-else"},
		jwt.IssuedAtKey:   now,
		jwt.ExpirationKey: now.Add(5 * time.Minute),
		emailClaim:        "x@example.com",
	})
	if _, err := f.verifier.VerifyEmail(context.Background(), string(signed)); err == nil {
		t.Fatal("expected verify failure on wrong audience")
	}
}

func TestJWKSVerifierRejectsBadSignature(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	// Generate a separate key the verifier has never seen, sign with it.
	otherRaw, _ := rsa.GenerateKey(rand.Reader, 2048)
	otherKey, _ := jwk.FromRaw(otherRaw)
	_ = otherKey.Set(jwk.KeyIDKey, "rogue")
	_ = otherKey.Set(jwk.AlgorithmKey, jwa.RS256)
	now := time.Now()
	tok := jwt.New()
	_ = tok.Set(jwt.IssuerKey, f.issuer)
	_ = tok.Set(jwt.AudienceKey, []string{f.audience})
	_ = tok.Set(jwt.IssuedAtKey, now)
	_ = tok.Set(jwt.ExpirationKey, now.Add(5*time.Minute))
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, otherKey))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := f.verifier.VerifyEmail(context.Background(), string(signed)); err == nil {
		t.Fatal("expected verify failure on signature mismatch")
	}
	// Sanity: the token did sign — confirm by parsing with the rogue key.
	if _, err := jws.Verify(signed, jws.WithKey(jwa.RS256, otherKey)); err != nil {
		t.Fatalf("rogue-signed token failed self-verify: %v", err)
	}
}

// TestJWKSVerifierSuppressesUnverifiedEmail locks the contract: an
// id_token carrying email_verified=false (or absent verified-flag
// alongside a present email_verified key set to a non-bool) suppresses
// the email line on the success page. Without this gate, an Auth0
// connection that lets a user self-assert their email could surface a
// misleading "qURL account: someone-else@target.tld" line.
func TestJWKSVerifierSuppressesUnverifiedEmail(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	now := time.Now()
	signed := f.signToken(t, map[string]any{
		jwt.IssuerKey:     f.issuer,
		jwt.AudienceKey:   []string{f.audience},
		jwt.IssuedAtKey:   now,
		jwt.ExpirationKey: now.Add(5 * time.Minute),
		emailClaim:        testAdminEmail,
		"email_verified":  false,
	})
	got, err := f.verifier.VerifyEmail(context.Background(), string(signed))
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty email when email_verified=false, got %q", got)
	}
}

func TestJWKSVerifierAcceptsVerifiedEmail(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	now := time.Now()
	signed := f.signToken(t, map[string]any{
		jwt.IssuerKey:     f.issuer,
		jwt.AudienceKey:   []string{f.audience},
		jwt.IssuedAtKey:   now,
		jwt.ExpirationKey: now.Add(5 * time.Minute),
		emailClaim:        testAdminEmail,
		"email_verified":  true,
	})
	got, err := f.verifier.VerifyEmail(context.Background(), string(signed))
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	if got != testAdminEmail {
		t.Errorf("email: got %q want %q", got, testAdminEmail)
	}
}

// TestJWKSVerifierReturnsEmptyEmailWhenEmailVerifiedAbsent locks the
// fail-closed contract: Auth0 connections that omit email_verified
// (some enterprise/SAML configs do) get an empty email rather than a
// surfaced-as-verified one.
func TestJWKSVerifierReturnsEmptyEmailWhenEmailVerifiedAbsent(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	now := time.Now()
	signed := f.signToken(t, map[string]any{
		jwt.IssuerKey:     f.issuer,
		jwt.AudienceKey:   []string{f.audience},
		jwt.IssuedAtKey:   now,
		jwt.ExpirationKey: now.Add(5 * time.Minute),
		emailClaim:        testAdminEmail,
		// email_verified deliberately omitted.
	})
	got, err := f.verifier.VerifyEmail(context.Background(), string(signed))
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty email when email_verified is absent, got %q", got)
	}
}

// TestJWKSVerifierReturnsEmptyEmailWhenClaimMissing covers the verified-
// but-no-email path (a verified token that simply doesn't include the
// email claim at all).
func TestJWKSVerifierReturnsEmptyEmailWhenClaimMissing(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	now := time.Now()
	signed := f.signToken(t, map[string]any{
		jwt.IssuerKey:     f.issuer,
		jwt.AudienceKey:   []string{f.audience},
		jwt.IssuedAtKey:   now,
		jwt.ExpirationKey: now.Add(5 * time.Minute),
		"email_verified":  true,
	})
	got, err := f.verifier.VerifyEmail(context.Background(), string(signed))
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty email when claim missing, got %q", got)
	}
}

// TestJWKSVerifierVerifySubReturnsSub fences the happy path: a verified
// id_token's sub claim becomes the workspace OwnerID at bind time. Drift
// the JWKS parse posture or strip the sub-extraction and this test fires.
func TestJWKSVerifierVerifySubReturnsSub(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	now := time.Now()
	const wantSub = "auth0|abc123def456"
	signed := f.signToken(t, map[string]any{
		jwt.IssuerKey:     f.issuer,
		jwt.AudienceKey:   []string{f.audience},
		jwt.SubjectKey:    wantSub,
		jwt.IssuedAtKey:   now,
		jwt.ExpirationKey: now.Add(5 * time.Minute),
	})
	got, err := f.verifier.VerifySub(context.Background(), string(signed))
	if err != nil {
		t.Fatalf("VerifySub: %v", err)
	}
	if got != wantSub {
		t.Errorf("sub: got %q want %q", got, wantSub)
	}
}

// TestJWKSVerifierVerifySubRejectsEmptySub fences the misconfigured-
// federation posture documented at jwks.go: an empty sub on an otherwise
// valid token must surface as an error so the callback's checkBindAllowed
// fail-closes (no OwnerID → no bind → 500) instead of silently writing
// a workspace_mappings row with an empty owner_id.
func TestJWKSVerifierVerifySubRejectsEmptySub(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	now := time.Now()
	signed := f.signToken(t, map[string]any{
		jwt.IssuerKey:     f.issuer,
		jwt.AudienceKey:   []string{f.audience},
		jwt.SubjectKey:    "",
		jwt.IssuedAtKey:   now,
		jwt.ExpirationKey: now.Add(5 * time.Minute),
	})
	got, err := f.verifier.VerifySub(context.Background(), string(signed))
	if err == nil {
		t.Errorf("VerifySub must return an error on empty sub; got %q nil", got)
	}
}

// TestJWKSVerifierVerifySubRejectsBadSignature mirrors VerifyEmail's
// bad-signature fence — a sub-extraction path that skipped signature
// verification (or silently degraded the parse posture) would let an
// attacker-controlled sub flow into BindWorkspace as OwnerID.
func TestJWKSVerifierVerifySubRejectsBadSignature(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	// Sign with a *different* RSA key — verifier's JWKS holds only
	// f.signKey's public half, so signature verify must fail.
	rogue, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rogue key: %v", err)
	}
	rogueJWK, err := jwk.FromRaw(rogue)
	if err != nil {
		t.Fatalf("jwk.FromRaw rogue: %v", err)
	}
	if err := rogueJWK.Set(jwk.KeyIDKey, "test-key"); err != nil {
		t.Fatalf("set rogue kid: %v", err)
	}
	if err := rogueJWK.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatalf("set rogue alg: %v", err)
	}
	now := time.Now()
	tok := jwt.New()
	_ = tok.Set(jwt.IssuerKey, f.issuer)
	_ = tok.Set(jwt.AudienceKey, []string{f.audience})
	_ = tok.Set(jwt.SubjectKey, "attacker-controlled-sub")
	_ = tok.Set(jwt.IssuedAtKey, now)
	_ = tok.Set(jwt.ExpirationKey, now.Add(5*time.Minute))
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, rogueJWK))
	if err != nil {
		t.Fatalf("rogue sign: %v", err)
	}
	if _, err := f.verifier.VerifySub(context.Background(), string(signed)); err == nil {
		t.Error("VerifySub must reject a token signed with the wrong key — silent acceptance lets attacker-controlled sub flow to BindWorkspace")
	}
}

// TestJWKSVerifierVerifySubRejectsExpired fences the exp claim. A
// stale id_token whose sub matches a known owner shouldn't be able
// to bind a workspace after the token's expiry.
func TestJWKSVerifierVerifySubRejectsExpired(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	now := time.Now()
	signed := f.signToken(t, map[string]any{
		jwt.IssuerKey:     f.issuer,
		jwt.AudienceKey:   []string{f.audience},
		jwt.SubjectKey:    "auth0|some-sub",
		jwt.IssuedAtKey:   now.Add(-10 * time.Minute),
		jwt.ExpirationKey: now.Add(-5 * time.Minute),
	})
	if _, err := f.verifier.VerifySub(context.Background(), string(signed)); err == nil {
		t.Error("VerifySub must reject an expired id_token")
	}
}
