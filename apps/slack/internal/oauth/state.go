package oauth

import (
	"bytes"
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
//	base64url( teamID + "|" + userID + "|" + nonce + "|" + unix_timestamp + "|" + hmac_hex )
//
// where hmac_hex = HMAC-SHA256(secret, teamID + "|" + userID + "|" + nonce + "|" + ts).
//
// teamID + userID are carried in the signed payload (recovered at
// /callback) so the workspace identity isn't taken from an unsigned
// query parameter. The only thing that can mint a valid state is the
// /qurl setup slash-command handler, which has already verified the
// Slack signing secret and therefore the caller's workspace identity.
//
// Expiry: 5 minutes from mint covers the slash-command-reply → click →
// Auth0 authenticate → callback round-trip.
const (
	stateMaxAge        = 5 * time.Minute
	statePartCount     = 5
	stateNonceLen      = 16 // 16 bytes → 32 hex chars; plenty for one-shot CSRF.
	StateMinSecret     = 32 // bytes — HMAC-SHA256 block-size floor; rejects ergonomically-weak operator secrets.
	stateFutureSkew    = 30 * time.Second
	stateSeparator     = "|"
	stateSeparatorB    = byte('|')
	stateSeparatorRune = '|'
	stateUserIDIndex   = 1
	stateTeamIDIndex   = 0
	stateNonceIndex    = 2
	stateTSIndex       = 3
	stateSigIndex      = 4
)

// Sentinel errors so callers can log a stable reason without parsing
// error strings. Kept un-exported because no caller outside this package
// branches on them today — promote when one does.
var (
	errStateMalformed      = errors.New("state: malformed")
	errStateBadHMAC        = errors.New("state: HMAC mismatch")
	errStateExpired        = errors.New("state: expired")
	errStateFuture         = errors.New("state: timestamp in future")
	errStateShortKey       = errors.New("state: secret too short")
	errStateEmptyTeam      = errors.New("state: empty teamID")
	errStateEmptyUser      = errors.New("state: empty userID")
	errStateIDHasSeparator = errors.New("state: teamID or userID contains pipe separator")
)

// signedPayload returns the canonical "teamID|userID|nonce|ts" byte
// slice that both MintState and VerifyState HMAC over. Sharing the
// construction means the two paths can't drift on separator or order.
func signedPayload(teamID, userID, nonce, ts string) []byte {
	return []byte(teamID + stateSeparator + userID + stateSeparator + nonce + stateSeparator + ts)
}

// MintState produces a fresh state token binding (teamID, userID) under
// secret. Exported so the slash-command handler in package internal can
// mint state from a Slack-signature-verified /qurl setup dispatch.
//
// Returns errStateShortKey if secret is shorter than StateMinSecret.
func MintState(secret []byte, teamID, userID string, now time.Time) (string, error) {
	if len(secret) < StateMinSecret {
		return "", errStateShortKey
	}
	if teamID == "" {
		return "", errStateEmptyTeam
	}
	if userID == "" {
		return "", errStateEmptyUser
	}
	// The wire format uses '|' as the separator between payload parts.
	// Today's Slack team/user IDs are pure [A-Z0-9], but if Slack ever
	// extends the alphabet a stray '|' would split into more parts than
	// VerifyState expects and silently mismatch. Reject up front.
	if strings.ContainsRune(teamID, stateSeparatorRune) || strings.ContainsRune(userID, stateSeparatorRune) {
		return "", errStateIDHasSeparator
	}
	nonceBytes := make([]byte, stateNonceLen)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("state: read nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	ts := strconv.FormatInt(now.Unix(), 10)
	signed := signedPayload(teamID, userID, nonce, ts)
	mac := hmac.New(sha256.New, secret)
	// hmac.Hash.Write never returns an error (documented in stdlib); the
	// signature satisfies io.Writer so the result is discarded.
	mac.Write(signed)
	sig := hex.EncodeToString(mac.Sum(nil))
	raw := make([]byte, 0, len(signed)+1+len(sig))
	raw = append(raw, signed...)
	raw = append(raw, stateSeparatorB)
	raw = append(raw, sig...)
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// VerifiedState is the (teamID, userID) pair recovered from a valid
// state token.
type VerifiedState struct {
	TeamID string
	UserID string
}

// VerifyState validates and decodes a state token. Returns the recovered
// (teamID, userID) on success or one of the sentinel errors on failure.
//
// Rejects future timestamps beyond stateFutureSkew so a clock-skewed
// minter can't produce links that outlive stateMaxAge.
func VerifyState(secret []byte, encoded string, now time.Time) (VerifiedState, error) {
	if len(secret) < StateMinSecret {
		return VerifiedState{}, errStateShortKey
	}
	if encoded == "" {
		return VerifiedState{}, errStateMalformed
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return VerifiedState{}, errStateMalformed
	}
	parts := bytes.Split(raw, []byte{stateSeparatorB})
	if len(parts) != statePartCount {
		return VerifiedState{}, errStateMalformed
	}
	teamID := string(parts[stateTeamIDIndex])
	userID := string(parts[stateUserIDIndex])
	nonce := parts[stateNonceIndex]
	tsBytes := parts[stateTSIndex]
	sigHex := parts[stateSigIndex]
	if teamID == "" || userID == "" || len(nonce) == 0 || len(tsBytes) == 0 || len(sigHex) == 0 {
		return VerifiedState{}, errStateMalformed
	}
	wantSig, err := hex.DecodeString(string(sigHex))
	if err != nil {
		return VerifiedState{}, errStateMalformed
	}
	signed := signedPayload(teamID, userID, string(nonce), string(tsBytes))
	mac := hmac.New(sha256.New, secret)
	mac.Write(signed)
	if !hmac.Equal(wantSig, mac.Sum(nil)) {
		return VerifiedState{}, errStateBadHMAC
	}
	tsInt, err := strconv.ParseInt(string(tsBytes), 10, 64)
	if err != nil {
		return VerifiedState{}, errStateMalformed
	}
	mintedAt := time.Unix(tsInt, 0)
	if mintedAt.After(now.Add(stateFutureSkew)) {
		return VerifiedState{}, errStateFuture
	}
	if now.Sub(mintedAt) > stateMaxAge {
		return VerifiedState{}, errStateExpired
	}
	return VerifiedState{TeamID: teamID, UserID: userID}, nil
}
