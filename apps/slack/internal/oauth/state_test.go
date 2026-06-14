package oauth

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// testSecret is the 32-byte HMAC key used across state tests — the
// StateMinSecret floor rejects anything shorter, so the same constant
// stands in for "valid" everywhere.
var testSecret = []byte("hmac-secret-32-bytes-or-whatever")

const (
	testStateTeamID          = "T123ABCDEF"
	testStateUserID          = "U_ADMIN1"
	testNormalizedSetupEmail = "admin+setup@example.com"
)

func TestMintAndVerifyStateRoundTrip(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tok, err := MintState(testSecret, testStateTeamID, testStateUserID, now)
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	got, err := VerifyState(testSecret, tok, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("VerifyState: %v", err)
	}
	if got.TeamID != testStateTeamID {
		t.Errorf("teamID round-trip: got %q want %q", got.TeamID, testStateTeamID)
	}
	if got.UserID != testStateUserID {
		t.Errorf("userID round-trip: got %q want %q", got.UserID, testStateUserID)
	}
	if got.Email != "" {
		t.Errorf("legacy state email: got %q want empty", got.Email)
	}
	if got.Mode != SetupModeReuse {
		t.Errorf("legacy state mode: got %q want reuse", got.Mode)
	}
}

func TestMintAndVerifyStateWithEmailRoundTrip(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tok, err := MintStateWithEmail(testSecret, testStateTeamID, testStateUserID, "Admin+Setup@Example.COM", now)
	if err != nil {
		t.Fatalf("MintStateWithEmail: %v", err)
	}
	got, err := VerifyState(testSecret, tok, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("VerifyState: %v", err)
	}
	if got.TeamID != testStateTeamID {
		t.Errorf("teamID round-trip: got %q want %q", got.TeamID, testStateTeamID)
	}
	if got.UserID != testStateUserID {
		t.Errorf("userID round-trip: got %q want %q", got.UserID, testStateUserID)
	}
	if got.Email != testNormalizedSetupEmail {
		t.Errorf("email round-trip: got %q want normalized email", got.Email)
	}
	if got.Mode != SetupModeReuse {
		t.Errorf("email state mode: got %q want reuse", got.Mode)
	}
}

func TestMintAndVerifyStateWithEmailRotateModeRoundTrip(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tok, err := MintStateWithEmailMode(testSecret, testStateTeamID, testStateUserID, "Admin+Setup@Example.COM", SetupModeRotate, now)
	if err != nil {
		t.Fatalf("MintStateWithEmailMode: %v", err)
	}
	got, err := VerifyState(testSecret, tok, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("VerifyState: %v", err)
	}
	if got.TeamID != testStateTeamID {
		t.Errorf("teamID round-trip: got %q want %q", got.TeamID, testStateTeamID)
	}
	if got.UserID != testStateUserID {
		t.Errorf("userID round-trip: got %q want %q", got.UserID, testStateUserID)
	}
	if got.Email != testNormalizedSetupEmail {
		t.Errorf("email round-trip: got %q want normalized email", got.Email)
	}
	if got.Mode != SetupModeRotate {
		t.Errorf("mode round-trip: got %q want rotate", got.Mode)
	}
}

func TestMintAndVerifyStateWithEmailRepointModeRoundTrip(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tok, err := MintStateWithEmailMode(testSecret, testStateTeamID, testStateUserID, "Admin+Setup@Example.COM", SetupModeRepoint, now)
	if err != nil {
		t.Fatalf("MintStateWithEmailMode: %v", err)
	}
	got, err := VerifyState(testSecret, tok, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("VerifyState: %v", err)
	}
	if got.Email != testNormalizedSetupEmail {
		t.Errorf("email round-trip: got %q want normalized email", got.Email)
	}
	if got.Mode != SetupModeRepoint {
		t.Errorf("mode round-trip: got %q want repoint", got.Mode)
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
			t.Errorf("SetupMode(%q).Explicit() = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestMintStateRejectsInvalidSetupMode(t *testing.T) {
	now := time.Unix(1700000000, 0)
	if _, err := MintStateWithEmailMode(testSecret, testStateTeamID, testStateUserID, "admin@example.com", SetupMode("bad"), now); !errors.Is(err, errStateBadMode) {
		t.Fatalf("want errStateBadMode, got %v", err)
	}
}

func TestVerifyStateRejectsExpired(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tok, err := MintState(testSecret, testStateTeamID, testStateUserID, now)
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	if _, err := VerifyState(testSecret, tok, now.Add(10*time.Minute)); !errors.Is(err, errStateExpired) {
		t.Fatalf("want errStateExpired, got %v", err)
	}
}

func TestVerifyStateRejectsFutureTimestamp(t *testing.T) {
	now := time.Unix(1700000000, 0)
	// Mint from a future clock, verify from "now" → mintedAt is past
	// stateFutureSkew → rejected.
	tok, err := MintState(testSecret, testStateTeamID, testStateUserID, now.Add(time.Hour))
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
	tok, err := MintState(testSecret, testStateTeamID, testStateUserID, now)
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	if _, err := VerifyState(other, tok, now); !errors.Is(err, errStateBadHMAC) {
		t.Fatalf("want errStateBadHMAC, got %v", err)
	}
}

func TestVerifyStateRejectsMalformed(t *testing.T) {
	cases := []string{"", "not-base64!!!", "QUFBQUE"}
	for _, c := range cases {
		if _, err := VerifyState(testSecret, c, time.Now()); err == nil {
			t.Errorf("input %q: want error, got nil", c)
		}
	}
}

func TestMintStateRejectsShortSecret(t *testing.T) {
	short := []byte("too-short")
	if _, err := MintState(short, testStateTeamID, testStateUserID, time.Now()); !errors.Is(err, errStateShortKey) {
		t.Errorf("want errStateShortKey, got %v", err)
	}
}

func TestVerifyStateRejectsShortSecret(t *testing.T) {
	short := []byte("too-short")
	if _, err := VerifyState(short, "anything", time.Now()); !errors.Is(err, errStateShortKey) {
		t.Errorf("want errStateShortKey, got %v", err)
	}
}

// TestMintStateRejectsSeparatorInIDs locks the wire-format invariant:
// if an attacker (or a Slack spec change) ever embeds '|' in a team/
// user ID, the token would split into more parts than VerifyState
// expects, silently mismatching. We reject the mint instead.
func TestMintStateRejectsSeparatorInIDs(t *testing.T) {
	now := time.Now()
	if _, err := MintState(testSecret, "T|EVIL", testStateUserID, now); !errors.Is(err, errStateIDHasSeparator) {
		t.Errorf("teamID with separator: want errStateIDHasSeparator, got %v", err)
	}
	if _, err := MintState(testSecret, testStateTeamID, "U|EVIL", now); !errors.Is(err, errStateIDHasSeparator) {
		t.Errorf("userID with separator: want errStateIDHasSeparator, got %v", err)
	}
	if _, err := MintStateWithEmail(testSecret, testStateTeamID, testStateUserID, "admin|evil@example.com", now); !errors.Is(err, errStateIDHasSeparator) {
		t.Errorf("email with separator: want errStateIDHasSeparator, got %v", err)
	}
}

func TestMintStateRejectsEmptyInputs(t *testing.T) {
	now := time.Now()
	if _, err := MintState(testSecret, "", testStateUserID, now); !errors.Is(err, errStateEmptyTeam) {
		t.Errorf("empty teamID: want errStateEmptyTeam, got %v", err)
	}
	if _, err := MintState(testSecret, testStateTeamID, "", now); !errors.Is(err, errStateEmptyUser) {
		t.Errorf("empty userID: want errStateEmptyUser, got %v", err)
	}
}

func TestMintStateProducesURLSafeToken(t *testing.T) {
	tok, err := MintState(testSecret, testStateTeamID, testStateUserID, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	// base64.RawURLEncoding uses '-' and '_', never '+' '/' or '='.
	if strings.ContainsAny(tok, "+/=") {
		t.Errorf("token contains non-url-safe chars: %q", tok)
	}
}
