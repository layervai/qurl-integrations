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
//	base64url( teamID + "|" + userID + "|" + nonce + "|" + unix_timestamp + "|" + email + "|" + hmac_hex )
//
// where hmac_hex signs every payload field before it.
//
// State is integrity-protected, not encrypted: a copied setup link can be
// base64url-decoded to read the Slack IDs and optional setup email. Keep secrets
// out of state; the current fields are slash-command verified identifiers and
// the requester-typed email used only for Auth0 account selection.
//
// teamID + userID are carried in the signed payload (recovered at
// /callback) so the workspace identity isn't taken from an unsigned
// query parameter. The only thing that can mint a valid state is the
// /qurl setup slash-command handler, which has already verified the
// Slack signing secret and therefore the caller's workspace identity.
//
// Expiry: 5 minutes from mint covers the slash-command-reply → click →
// Auth0 authenticate → callback round-trip.
//
// Replay posture: within the 5-minute TTL the token *can* be replayed —
// the nonce is random-per-mint but not persisted to a one-shot store.
// The double-submit cookie blunts the replay surface (a second clicker
// needs the same browser, and clearStateCookie runs on every reject
// path plus on successful state-verify), so a leaked URL alone doesn't
// re-bind in a different browser. A nonce-store-backed one-shot would
// close the same-browser-twice case completely; the tradeoff is the
// extra storage dependency on a flow that's per-workspace-install rare.
// If we ever need it, DDB with a TTL attribute on the nonce keeps the
// storage cost bounded (~5min retention) and reuses the existing
// workspace_state plumbing. Acceptable for v1; revisit if install-flow
// logs show legitimate replay.
const (
	stateMaxAge        = 5 * time.Minute
	stateLegacyParts   = 5
	stateEmailParts    = 6
	stateNonceLen      = 16 // 16 bytes → 32 hex chars; plenty for one-shot CSRF.
	StateMinSecret     = 32 // bytes — HMAC-SHA256 output size; floor against ergonomically-weak operator secrets.
	stateFutureSkew    = 30 * time.Second
	stateSeparator     = "|"
	stateSeparatorB    = byte('|')
	stateSeparatorRune = '|'
	stateUserIDIndex   = 1
	stateTeamIDIndex   = 0
	stateNonceIndex    = 2
	stateTSIndex       = 3
	// Slot 4 is the legacy signature; email states put the email there and
	// shift the signature to slot 5.
	stateLegacySigIndex = 4
	stateEmailIndex     = 4
	stateEmailSigIndex  = 5
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
	errStateIDHasSeparator = errors.New("state: teamID, userID, or email contains pipe separator")
)

// signedPayload returns the canonical pipe-joined byte slice that the
// state HMAC covers.
func signedPayload(parts ...string) []byte {
	return []byte(strings.Join(parts, stateSeparator))
}

// mintState produces a fresh state token binding (teamID, userID) and,
// when non-empty, a normalized email address under secret.
func mintState(secret []byte, teamID, userID, email string, now time.Time) (string, error) {
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
	if strings.ContainsRune(teamID, stateSeparatorRune) ||
		strings.ContainsRune(userID, stateSeparatorRune) ||
		strings.ContainsRune(email, stateSeparatorRune) {
		return "", errStateIDHasSeparator
	}
	if email != "" {
		normalized, err := NormalizeEmail(email)
		if err != nil {
			return "", err
		}
		email = normalized
	}
	nonceBytes := make([]byte, stateNonceLen)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", fmt.Errorf("state: read nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	ts := strconv.FormatInt(now.Unix(), 10)
	payloadParts := []string{teamID, userID, nonce, ts}
	if email != "" {
		payloadParts = append(payloadParts, email)
	}
	signed := signedPayload(payloadParts...)
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

// MintState produces a fresh state token binding (teamID, userID) under
// secret. Exported so the slash-command handler in package internal can
// mint state from a Slack-signature-verified /qurl setup dispatch.
//
// Returns errStateShortKey if secret is shorter than StateMinSecret.
func MintState(secret []byte, teamID, userID string, now time.Time) (string, error) {
	return mintState(secret, teamID, userID, "", now)
}

// MintStateWithEmail produces a fresh state token binding the Slack team/user
// plus the normalized email address entered in the setup command. The callback
// requires the verified Auth0 email claim to match this value before it binds
// or mints a workspace key.
func MintStateWithEmail(secret []byte, teamID, userID, email string, now time.Time) (string, error) {
	return mintState(secret, teamID, userID, email, now)
}

// VerifiedState is the setup identity recovered from a valid state token.
type VerifiedState struct {
	TeamID string
	UserID string
	Email  string
}

// VerifyState validates and decodes a state token. Returns the recovered
// setup identity on success or one of the sentinel errors on failure.
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
	var (
		email    string
		sigIndex int
		signed   []byte
	)
	switch len(parts) {
	case stateLegacyParts:
		sigIndex = stateLegacySigIndex
		signed = signedPayload(
			string(parts[stateTeamIDIndex]),
			string(parts[stateUserIDIndex]),
			string(parts[stateNonceIndex]),
			string(parts[stateTSIndex]),
		)
	case stateEmailParts:
		sigIndex = stateEmailSigIndex
		email = string(parts[stateEmailIndex])
		normalized, err := NormalizeEmail(email)
		if err != nil || normalized != email {
			return VerifiedState{}, errStateMalformed
		}
		signed = signedPayload(
			string(parts[stateTeamIDIndex]),
			string(parts[stateUserIDIndex]),
			string(parts[stateNonceIndex]),
			string(parts[stateTSIndex]),
			email,
		)
	default:
		return VerifiedState{}, errStateMalformed
	}
	teamID := string(parts[stateTeamIDIndex])
	userID := string(parts[stateUserIDIndex])
	nonce := parts[stateNonceIndex]
	tsBytes := parts[stateTSIndex]
	sigHex := parts[sigIndex]
	if teamID == "" || userID == "" || len(nonce) == 0 || len(tsBytes) == 0 || len(sigHex) == 0 {
		return VerifiedState{}, errStateMalformed
	}
	wantSig, err := hex.DecodeString(string(sigHex))
	if err != nil {
		return VerifiedState{}, errStateMalformed
	}
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
	return VerifiedState{TeamID: teamID, UserID: userID, Email: email}, nil
}
