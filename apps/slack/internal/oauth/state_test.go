package oauth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMintAndVerifyStateRoundTrip(t *testing.T) {
	secret := []byte("hmac-secret-32-bytes-or-whatever")
	now := time.Unix(1700000000, 0)
	tok, err := mintState(secret, "T123ABCDEF", now)
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	team, err := verifyState(secret, tok, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("verifyState: %v", err)
	}
	if team != "T123ABCDEF" {
		t.Errorf("teamID round-trip: got %q want %q", team, "T123ABCDEF")
	}
}

func TestVerifyStateRejectsExpired(t *testing.T) {
	secret := []byte("secret")
	now := time.Unix(1700000000, 0)
	tok, _ := mintState(secret, "T123ABCDEF", now)
	if _, err := verifyState(secret, tok, now.Add(10*time.Minute)); !errors.Is(err, errStateExpired) {
		t.Fatalf("want errStateExpired, got %v", err)
	}
}

func TestVerifyStateRejectsBadHMAC(t *testing.T) {
	secret := []byte("secret")
	other := []byte("different-secret")
	now := time.Unix(1700000000, 0)
	tok, _ := mintState(secret, "T123ABCDEF", now)
	if _, err := verifyState(other, tok, now); !errors.Is(err, errStateBadHMAC) {
		t.Fatalf("want errStateBadHMAC, got %v", err)
	}
}

func TestVerifyStateRejectsMalformed(t *testing.T) {
	cases := []string{"", "not-base64!!!", "QUFBQUE"}
	for _, c := range cases {
		if _, err := verifyState([]byte("k"), c, time.Now()); err == nil {
			t.Errorf("input %q: want error, got nil", c)
		}
	}
}

func TestMintStateRejectsEmptyInputs(t *testing.T) {
	if _, err := mintState(nil, "T1", time.Now()); err == nil {
		t.Error("empty secret should fail")
	}
	if _, err := mintState([]byte("k"), "", time.Now()); err == nil {
		t.Error("empty teamID should fail")
	}
}

func TestMintStateProducesURLSafeToken(t *testing.T) {
	tok, err := mintState([]byte("k"), "T123ABCDEF", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("mintState: %v", err)
	}
	// base64.RawURLEncoding uses '-' and '_', never '+' '/' or '='.
	if strings.ContainsAny(tok, "+/=") {
		t.Errorf("token contains non-url-safe chars: %q", tok)
	}
}
