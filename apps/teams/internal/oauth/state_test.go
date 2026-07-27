package oauth

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMintAndVerifyStateRoundTrip(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tok, err := MintState(testSecret, testTenantID, testUserID, now)
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	got, err := VerifyState(testSecret, tok, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("VerifyState: %v", err)
	}
	if got.TeamID != testTenantID || got.UserID != testUserID {
		t.Fatalf("VerifyState() = %+v", got)
	}
	if got.Email != "" || got.Mode != SetupModeReuse {
		t.Fatalf("unexpected email/mode: %+v", got)
	}
}

func TestMintAndVerifyStateWithEmailRoundTrip(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tok, err := MintStateWithEmail(testSecret, testTenantID, testUserID, "Admin+Setup@Example.COM", now)
	if err != nil {
		t.Fatalf("MintStateWithEmail: %v", err)
	}
	got, err := VerifyState(testSecret, tok, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("VerifyState: %v", err)
	}
	if got.Email != testNormalizedMail || got.Mode != SetupModeReuse {
		t.Fatalf("VerifyState() = %+v", got)
	}
}

func TestMintAndVerifyStateWithExplicitModes(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cases := []SetupMode{SetupModeRotate, SetupModeRepoint}
	for _, mode := range cases {
		t.Run(string(mode), func(t *testing.T) {
			tok, err := MintStateWithEmailMode(testSecret, testTenantID, testUserID, "Admin+Setup@Example.COM", mode, now)
			if err != nil {
				t.Fatalf("MintStateWithEmailMode: %v", err)
			}
			got, err := VerifyState(testSecret, tok, now.Add(30*time.Second))
			if err != nil {
				t.Fatalf("VerifyState: %v", err)
			}
			if got.Email != testNormalizedMail || got.Mode != mode {
				t.Fatalf("VerifyState() = %+v", got)
			}
		})
	}
}

func TestSetupModeExplicit(t *testing.T) {
	cases := []struct {
		mode SetupMode
		want bool
	}{
		{SetupModeReuse, false},
		{"", false},
		{SetupModeRotate, true},
		{SetupModeRepoint, true},
	}
	for _, tc := range cases {
		if got := tc.mode.Explicit(); got != tc.want {
			t.Fatalf("SetupMode(%q).Explicit() = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestMintStateRejectsInvalidSetupMode(t *testing.T) {
	now := time.Unix(1700000000, 0)
	if _, err := MintStateWithEmailMode(testSecret, testTenantID, testUserID, testAdminEmail, SetupMode("bad"), now); !errors.Is(err, errStateBadMode) {
		t.Fatalf("want errStateBadMode, got %v", err)
	}
}

func TestVerifyStateRejectsExpired(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tok, err := MintState(testSecret, testTenantID, testUserID, now)
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	if _, err := VerifyState(testSecret, tok, now.Add(10*time.Minute)); !errors.Is(err, errStateExpired) {
		t.Fatalf("want errStateExpired, got %v", err)
	}
}

func TestVerifyStateRejectsFutureTimestamp(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tok, err := MintState(testSecret, testTenantID, testUserID, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	if _, err := VerifyState(testSecret, tok, now); !errors.Is(err, errStateFuture) {
		t.Fatalf("want errStateFuture, got %v", err)
	}
}

func TestVerifyStateRejectsBadHMAC(t *testing.T) {
	other := bytes.Repeat([]byte("x"), StateMinSecret)
	now := time.Unix(1700000000, 0)
	tok, err := MintState(testSecret, testTenantID, testUserID, now)
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	if _, err := VerifyState(other, tok, now); !errors.Is(err, errStateBadHMAC) {
		t.Fatalf("want errStateBadHMAC, got %v", err)
	}
}

func TestVerifyStateRejectsMalformed(t *testing.T) {
	for _, raw := range []string{"", "not-base64!!!", "QUFBQUE"} {
		if _, err := VerifyState(testSecret, raw, time.Now()); err == nil {
			t.Fatalf("VerifyState(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestMintAndVerifyStateRejectShortSecret(t *testing.T) {
	short := []byte("too-short")
	if _, err := MintState(short, testTenantID, testUserID, time.Now()); !errors.Is(err, errStateShortKey) {
		t.Fatalf("MintState short secret err = %v", err)
	}
	if _, err := VerifyState(short, "anything", time.Now()); !errors.Is(err, errStateShortKey) {
		t.Fatalf("VerifyState short secret err = %v", err)
	}
}

func TestMintStateRejectsSeparatorAndEmptyInputs(t *testing.T) {
	now := time.Now()
	if _, err := MintState(testSecret, "tenant|bad", testUserID, now); !errors.Is(err, errStateIDHasSeparator) {
		t.Fatalf("tenant separator err = %v", err)
	}
	if _, err := MintState(testSecret, testTenantID, "user|bad", now); !errors.Is(err, errStateIDHasSeparator) {
		t.Fatalf("user separator err = %v", err)
	}
	if _, err := MintStateWithEmail(testSecret, testTenantID, testUserID, "admin|bad@example.com", now); !errors.Is(err, errStateIDHasSeparator) {
		t.Fatalf("email separator err = %v", err)
	}
	if _, err := MintState(testSecret, "", testUserID, now); !errors.Is(err, errStateEmptyTeam) {
		t.Fatalf("empty team err = %v", err)
	}
	if _, err := MintState(testSecret, testTenantID, "", now); !errors.Is(err, errStateEmptyUser) {
		t.Fatalf("empty user err = %v", err)
	}
}

func TestMintStateProducesURLSafeToken(t *testing.T) {
	tok, err := MintState(testSecret, testTenantID, testUserID, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	if strings.ContainsAny(tok, "+/=") {
		t.Fatalf("token contains non-url-safe chars: %q", tok)
	}
}
