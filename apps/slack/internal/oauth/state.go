package oauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// State token format:
//
//	base64url( teamID + "|" + nonce + "|" + unix_timestamp + "|" + hmac_hex )
//
// where hmac_hex = HMAC-SHA256(secret, teamID + "|" + nonce + "|" + ts).
//
// We keep the parts un-hashed so the callback can recover teamID from
// the state itself (it isn't stored in the cookie). The HMAC binds the
// triple so the callback rejects any tampered or forged state value.
//
// Expiry is enforced by checking (now - ts) <= stateMaxAge. Five minutes
// matches the cookie max-age set in cookie.go.
const (
	stateMaxAge    = 5 * time.Minute
	statePartCount = 4
	stateNonceLen  = 16 // 16 bytes → 32 hex chars; plenty for one-shot CSRF.
)

// Sentinel errors so callers can log a stable reason without parsing
// error strings. Kept un-exported because no caller outside this package
// branches on them today — promote when one does.
var (
	errStateMalformed = errors.New("state: malformed")
	errStateBadHMAC   = errors.New("state: HMAC mismatch")
	errStateExpired   = errors.New("state: expired")
)

// mintState produces a fresh state token for the given teamID.
func mintState(secret []byte, teamID string, now time.Time) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("state: empty secret")
	}
	if teamID == "" {
		return "", errors.New("state: empty teamID")
	}
	nonceBytes := make([]byte, stateNonceLen)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("state: read nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	ts := strconv.FormatInt(now.Unix(), 10)
	signed := teamID + "|" + nonce + "|" + ts
	mac := hmac.New(sha256.New, secret)
	if _, err := mac.Write([]byte(signed)); err != nil {
		return "", fmt.Errorf("state: hmac write: %w", err)
	}
	sig := hex.EncodeToString(mac.Sum(nil))
	raw := signed + "|" + sig
	return base64.RawURLEncoding.EncodeToString([]byte(raw)), nil
}

// verifyState validates and decodes a state token. Returns the teamID on
// success or one of the sentinel errors above on failure.
func verifyState(secret []byte, encoded string, now time.Time) (string, error) {
	if encoded == "" {
		return "", errStateMalformed
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", errStateMalformed
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != statePartCount {
		return "", errStateMalformed
	}
	teamID, nonce, ts, sigHex := parts[0], parts[1], parts[2], parts[3]
	if teamID == "" || nonce == "" || ts == "" || sigHex == "" {
		return "", errStateMalformed
	}
	wantSig, err := hex.DecodeString(sigHex)
	if err != nil {
		return "", errStateMalformed
	}
	signed := teamID + "|" + nonce + "|" + ts
	mac := hmac.New(sha256.New, secret)
	if _, err := mac.Write([]byte(signed)); err != nil {
		return "", errStateMalformed
	}
	gotSig := mac.Sum(nil)
	if !hmac.Equal(wantSig, gotSig) {
		return "", errStateBadHMAC
	}
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", errStateMalformed
	}
	if now.Sub(time.Unix(tsInt, 0)) > stateMaxAge {
		return "", errStateExpired
	}
	return teamID, nil
}
