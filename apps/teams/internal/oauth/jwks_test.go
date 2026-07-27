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

const emailClaim = "email"

type jwksTestFixture struct {
	verifier *JWKSVerifier
	signKey  jwk.Key
	issuer   string
	audience string
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
	_ = signKey.Set(jwk.KeyIDKey, "test-key")
	_ = signKey.Set(jwk.AlgorithmKey, jwa.RS256)

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

	issuer := srv.URL + "/"
	v, err := NewJWKSVerifier(context.Background(), issuer, audience)
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %v", err)
	}
	return &jwksTestFixture{verifier: v, signKey: signKey, issuer: issuer, audience: audience}
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
		t.Fatalf("email = %q", email)
	}
	sub, err := f.verifier.VerifySub(context.Background(), string(signed))
	if err != nil {
		t.Fatalf("VerifySub: %v", err)
	}
	if sub != "sub-1" {
		t.Fatalf("sub = %q", sub)
	}
}

func TestJWKSVerifierRejectsWrongIssuerOrAudience(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	now := time.Now()
	for name, claims := range map[string]map[string]any{
		"issuer": {
			jwt.IssuerKey:     "https://impostor.invalid/",
			jwt.AudienceKey:   []string{f.audience},
			jwt.IssuedAtKey:   now,
			jwt.ExpirationKey: now.Add(5 * time.Minute),
			emailClaim:        testAdminEmail,
		},
		"audience": {
			jwt.IssuerKey:     f.issuer,
			jwt.AudienceKey:   []string{"someone-else"},
			jwt.IssuedAtKey:   now,
			jwt.ExpirationKey: now.Add(5 * time.Minute),
			emailClaim:        testAdminEmail,
		},
	} {
		t.Run(name, func(t *testing.T) {
			signed := f.signToken(t, claims)
			if _, err := f.verifier.VerifyEmail(context.Background(), string(signed)); err == nil {
				t.Fatal("expected verify failure")
			}
		})
	}
}

func TestJWKSVerifierRejectsBadSignature(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
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
	if _, err := jws.Verify(signed, jws.WithKey(jwa.RS256, otherKey)); err != nil {
		t.Fatalf("rogue-signed token failed self-verify: %v", err)
	}
}

func TestJWKSVerifierEmailVisibilityRules(t *testing.T) {
	f := newJWKSFixture(t, "client-aud")
	now := time.Now()
	cases := []struct {
		name  string
		claim map[string]any
		want  string
	}{
		{
			name: "verified email",
			claim: map[string]any{
				jwt.IssuerKey:     f.issuer,
				jwt.AudienceKey:   []string{f.audience},
				jwt.IssuedAtKey:   now,
				jwt.ExpirationKey: now.Add(5 * time.Minute),
				emailClaim:        testAdminEmail,
				"email_verified":  true,
			},
			want: testAdminEmail,
		},
		{
			name: "unverified email",
			claim: map[string]any{
				jwt.IssuerKey:     f.issuer,
				jwt.AudienceKey:   []string{f.audience},
				jwt.IssuedAtKey:   now,
				jwt.ExpirationKey: now.Add(5 * time.Minute),
				emailClaim:        testAdminEmail,
				"email_verified":  false,
			},
			want: "",
		},
		{
			name: "missing verified flag",
			claim: map[string]any{
				jwt.IssuerKey:     f.issuer,
				jwt.AudienceKey:   []string{f.audience},
				jwt.IssuedAtKey:   now,
				jwt.ExpirationKey: now.Add(5 * time.Minute),
				emailClaim:        testAdminEmail,
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.verifier.VerifyEmail(context.Background(), string(f.signToken(t, tc.claim)))
			if err != nil {
				t.Fatalf("VerifyEmail: %v", err)
			}
			if got != tc.want {
				t.Fatalf("VerifyEmail() = %q, want %q", got, tc.want)
			}
		})
	}
}
