package oauth

import (
	"bytes"
	"context"
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

// New setup links use an opaque random state handle stored server-side via
// StateStore. The handle is the only state value carried in Slack/Auth0 URLs;
// team/user/email/mode/nonce/PKCE verifier stay in the backend store and the
// callback consumes the row atomically.
//
// Legacy signed state token format, accepted only for short deploy overlap:
//
//	base64url( teamID + "|" + userID + "|" + nonce + "|" + unix_timestamp + "|" + code_verifier + "|" + hmac_hex )
//	base64url( teamID + "|" + userID + "|" + nonce + "|" + unix_timestamp + "|" + email + "|" + code_verifier + "|" + hmac_hex )
//	base64url( teamID + "|" + userID + "|" + nonce + "|" + unix_timestamp + "|" + email + "|" + mode + "|" + code_verifier + "|" + hmac_hex )
//
// where hmac_hex signs every payload field before it.
//
// Pre-PKCE state formats are still parsed for the short deploy overlap, but
// they do not complete end-to-end: Callback requires a state-bound verifier
// before exchanging the Auth0 code, so a user with an old in-flight link reruns
// /qurl setup rather than completing a no-PKCE token exchange.
//
// teamID + userID are carried in the signed payload (recovered at
// /callback) so the workspace identity isn't taken from an unsigned
// query parameter. The only thing that can mint a valid state is the
// /qurl setup slash-command handler, which has already verified the
// Slack signing secret and therefore the caller's workspace identity.
//
// Expiry: 1 hour from mint matches the qURL Connector bootstrap key lifetime
// used by /qurl-admin protect-connector, giving admins the same setup window
// whether they are connecting the workspace account or bootstrapping a sidecar.
//
// Replay posture: StateStore-backed states are consumed once on callback. The
// store writes a `ttl` cleanup hint for abandoned states, but successful
// callbacks delete the row immediately and every read/consume path checks expiry
// conditionally because table TTL is best-effort and may lag. The double-submit
// cookie still binds the Auth0 callback to the browser that opened /start.
const (
	stateMaxAge          = time.Hour
	stateLegacyParts     = 5
	stateEmailParts      = 6
	stateEmailModeParts  = 7
	stateNonceLen        = 16 // 16 bytes → 32 hex chars; plenty for one-shot CSRF.
	statePKCEVerifierLen = 32 // 32 bytes base64url-encode to 43 chars, RFC 7636's lower bound.
	stateHandleLen       = 32 // 256-bit opaque handle; no payload is encoded in the URL.
	StateMinSecret       = 32 // bytes — HMAC-SHA256 output size; floor against ergonomically-weak operator secrets.
	stateFutureSkew      = 30 * time.Second
	stateSeparator       = "|"
	stateSeparatorB      = byte('|')
	stateSeparatorRune   = '|'
	stateUserIDIndex     = 1
	stateTeamIDIndex     = 0
	stateNonceIndex      = 2
	stateTSIndex         = 3
	// Slot 4 is the legacy signature; email states put the email there and
	// shift the signature to slot 5.
	stateLegacySigIndex    = 4
	stateEmailIndex        = 4
	stateEmailSigIndex     = 5
	stateModeIndex         = 5
	stateEmailModeSigIndex = 6
	statePKCEVerifierIndex = 4
)

// SetupMode is the signed /qurl setup intent carried through Auth0.
type SetupMode string

const (
	// SetupModeReuse is the default idempotent setup path that reuses a valid stored key.
	SetupModeReuse SetupMode = "reuse"
	// SetupModeRotate is the explicit owner-requested same-account key replacement path.
	SetupModeRotate SetupMode = "rotate"
	// SetupModeRepoint is the explicit owner-requested account-move path. It
	// resolves to a same-account rotation when the signed-in qURL account already
	// holds the key, and detects a genuine cross-account move (different qURL
	// account) to route the owner to the operator-assisted transfer — qurl-service
	// has no tenant-facing cross-account binding transfer (cross-tenant refusal by
	// design, see layervai/qurl-service#910).
	SetupModeRepoint SetupMode = "repoint"
)

// Explicit reports whether the mode is an explicit owner-requested key
// operation (--rotate or --repoint) rather than the default reuse setup. The
// slash-command surface gates both explicit modes identically: they require an
// AdminStore (owner check) and cannot run against an unreclaimed legacy owner.
func (m SetupMode) Explicit() bool {
	return m == SetupModeRotate || m == SetupModeRepoint
}

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
	// errStateBadMode is returned to local minters that pass an invalid mode.
	// VerifyState collapses bad wire modes into errStateMalformed so callback
	// logs do not distinguish malformed links from deliberately forged modes.
	errStateBadMode = errors.New("state: invalid setup mode")
)

// signedPayload returns the canonical pipe-joined byte slice that the
// state HMAC covers.
func signedPayload(parts ...string) []byte {
	return []byte(strings.Join(parts, stateSeparator))
}

func normalizeSetupMode(mode SetupMode) (SetupMode, error) {
	switch mode {
	case "", SetupModeReuse:
		return SetupModeReuse, nil
	case SetupModeRotate:
		return SetupModeRotate, nil
	case SetupModeRepoint:
		return SetupModeRepoint, nil
	default:
		return "", errStateBadMode
	}
}

func mintRandomHex(bytesLen int, label string) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("state: read %s: %w", label, err)
	}
	return hex.EncodeToString(b), nil
}

func mintCodeVerifier() (string, error) {
	verifierBytes := make([]byte, statePKCEVerifierLen)
	if _, err := rand.Read(verifierBytes); err != nil {
		return "", fmt.Errorf("state: read PKCE verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(verifierBytes), nil
}

func normalizeStateInputs(teamID, userID, email string, mode SetupMode) (normalizedTeamID, normalizedUserID, normalizedEmail string, normalizedMode SetupMode, err error) {
	normalizedMode, err = normalizeSetupMode(mode)
	if err != nil {
		return "", "", "", "", err
	}
	if teamID == "" {
		return "", "", "", "", errStateEmptyTeam
	}
	if userID == "" {
		return "", "", "", "", errStateEmptyUser
	}
	// The legacy wire format uses '|' as the separator between payload parts.
	// Keep rejecting it for stored states too so callers see one stable input
	// contract regardless of the backing representation.
	if strings.ContainsRune(teamID, stateSeparatorRune) ||
		strings.ContainsRune(userID, stateSeparatorRune) ||
		strings.ContainsRune(email, stateSeparatorRune) {
		return "", "", "", "", errStateIDHasSeparator
	}
	if email != "" {
		normalized, err := NormalizeEmail(email)
		if err != nil {
			return "", "", "", "", err
		}
		email = normalized
	}
	if normalizedMode != SetupModeReuse && email == "" {
		return "", "", "", "", errStateBadMode
	}
	return teamID, userID, email, normalizedMode, nil
}

func newVerifiedState(teamID, userID, email string, mode SetupMode) (VerifiedState, error) {
	teamID, userID, email, normalizedMode, err := normalizeStateInputs(teamID, userID, email, mode)
	if err != nil {
		return VerifiedState{}, err
	}
	nonce, err := mintRandomHex(stateNonceLen, "nonce")
	if err != nil {
		return VerifiedState{}, err
	}
	codeVerifier, err := mintCodeVerifier()
	if err != nil {
		return VerifiedState{}, err
	}
	return VerifiedState{
		TeamID:       teamID,
		UserID:       userID,
		Nonce:        nonce,
		CodeVerifier: codeVerifier,
		Email:        email,
		Mode:         normalizedMode,
	}, nil
}

// mintState produces a fresh state token binding (teamID, userID) and,
// when non-empty, a normalized email address under secret.
func mintState(secret []byte, teamID, userID, email string, mode SetupMode, now time.Time) (string, error) {
	if len(secret) < StateMinSecret {
		return "", errStateShortKey
	}
	verified, err := newVerifiedState(teamID, userID, email, mode)
	if err != nil {
		return "", err
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	payloadParts := []string{verified.TeamID, verified.UserID, verified.Nonce, ts}
	if verified.Email != "" {
		payloadParts = append(payloadParts, verified.Email)
	}
	if verified.Mode != SetupModeReuse {
		payloadParts = append(payloadParts, string(verified.Mode))
	}
	payloadParts = append(payloadParts, verified.CodeVerifier)
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

// MintStoredStateWithEmailMode stores a fresh setup state server-side and
// returns the opaque handle that should be sent through the front-channel URL.
func MintStoredStateWithEmailMode(ctx context.Context, store StateStore, teamID, userID, email string, mode SetupMode, now time.Time) (string, error) {
	if store == nil {
		return "", errors.New("state: store is nil")
	}
	verified, err := newVerifiedState(teamID, userID, email, mode)
	if err != nil {
		return "", err
	}
	handleBytes := make([]byte, stateHandleLen)
	if _, err := rand.Read(handleBytes); err != nil {
		return "", fmt.Errorf("state: read handle: %w", err)
	}
	handle := base64.RawURLEncoding.EncodeToString(handleBytes)
	state := StoredState{
		VerifiedState: verified,
		CreatedAt:     now,
		ExpiresAt:     now.Add(stateMaxAge),
	}
	if err := store.PutState(ctx, handle, state); err != nil {
		return "", err
	}
	return handle, nil
}

// MintState produces a legacy no-email state token binding (teamID, userID)
// under secret. New /qurl setup dispatches require an email address and call
// MintStateWithEmail; this helper remains for tests that exercise the shared
// verifier and old no-email setup links minted before that requirement.
//
// Returns errStateShortKey if secret is shorter than StateMinSecret.
func MintState(secret []byte, teamID, userID string, now time.Time) (string, error) {
	return mintState(secret, teamID, userID, "", SetupModeReuse, now)
}

// MintStateWithEmail produces a fresh state token binding the Slack team/user
// plus the normalized email address entered in the setup command. The callback
// requires the verified Auth0 email claim to match this value before it binds
// or mints a workspace key.
func MintStateWithEmail(secret []byte, teamID, userID, email string, now time.Time) (string, error) {
	return mintState(secret, teamID, userID, email, SetupModeReuse, now)
}

// MintStateWithEmailMode is MintStateWithEmail plus a signed setup intent.
func MintStateWithEmailMode(secret []byte, teamID, userID, email string, mode SetupMode, now time.Time) (string, error) {
	return mintState(secret, teamID, userID, email, mode, now)
}

// VerifiedState is the setup identity recovered from a valid state token.
type VerifiedState struct {
	TeamID       string
	UserID       string
	Nonce        string
	CodeVerifier string
	Email        string
	Mode         SetupMode
}

// StoredState is the backend-only OAuth state payload. It intentionally mirrors
// VerifiedState plus timestamps so Start can build the Auth0 authorize URL and
// Callback can consume the same payload without putting sensitive fields in the
// front-channel URL.
type StoredState struct {
	VerifiedState
	CreatedAt time.Time
	ExpiresAt time.Time
}

// StateStore persists OAuth state by opaque handle. StartState may be called
// more than once before expiry (browser retry); ConsumeState must be atomic and
// one-shot.
type StateStore interface {
	PutState(ctx context.Context, handle string, state StoredState) error
	StartState(ctx context.Context, handle string, now time.Time) (VerifiedState, error)
	ConsumeState(ctx context.Context, handle string, now time.Time) (VerifiedState, error)
}

type parsedStateParts struct {
	email        string
	mode         SetupMode
	codeVerifier string
	sigIndex     int
	signed       []byte
}

func coreStatePayloadParts(parts [][]byte) []string {
	return []string{
		string(parts[stateTeamIDIndex]),
		string(parts[stateUserIDIndex]),
		string(parts[stateNonceIndex]),
		string(parts[stateTSIndex]),
	}
}

func parseStateParts(parts [][]byte) (parsedStateParts, error) {
	switch len(parts) {
	case stateLegacyParts:
		return parsedStateParts{
			mode:     SetupModeReuse,
			sigIndex: stateLegacySigIndex,
			signed:   signedPayload(coreStatePayloadParts(parts)...),
		}, nil
	case stateEmailParts:
		// Six parts can be either a pre-PKCE email state or a new
		// no-email PKCE state. The PKCE verifier alphabet cannot produce
		// a normalized email address, so the email check is the delimiter.
		emailOrVerifier := string(parts[stateEmailIndex])
		if !stateEmailNormalized(emailOrVerifier) {
			return parsePKCEStateParts(parts, "", SetupModeReuse, statePKCEVerifierIndex, stateEmailSigIndex)
		}
		payload := append(coreStatePayloadParts(parts), emailOrVerifier)
		return parsedStateParts{
			email:    emailOrVerifier,
			mode:     SetupModeReuse,
			sigIndex: stateEmailSigIndex,
			signed:   signedPayload(payload...),
		}, nil
	case stateEmailModeParts:
		// Seven parts can be either a pre-PKCE email+mode state or a new
		// email PKCE state. Explicit modes are fixed literals; anything
		// else in slot 5 is parsed as the PKCE verifier.
		email := string(parts[stateEmailIndex])
		if !stateEmailNormalized(email) {
			return parsedStateParts{}, errStateMalformed
		}
		mode, err := normalizeSetupMode(SetupMode(string(parts[stateModeIndex])))
		if err != nil || mode == SetupModeReuse {
			return parsePKCEStateParts(parts, email, SetupModeReuse, stateModeIndex, stateEmailModeSigIndex)
		}
		payload := append(coreStatePayloadParts(parts), email, string(mode))
		return parsedStateParts{
			email:    email,
			mode:     mode,
			sigIndex: stateEmailModeSigIndex,
			signed:   signedPayload(payload...),
		}, nil
	case stateEmailModeParts + 1:
		email := string(parts[stateEmailIndex])
		if !stateEmailNormalized(email) {
			return parsedStateParts{}, errStateMalformed
		}
		mode, err := normalizeSetupMode(SetupMode(string(parts[stateModeIndex])))
		if err != nil || mode == SetupModeReuse {
			return parsedStateParts{}, errStateMalformed
		}
		return parsePKCEStateParts(parts, email, mode, stateEmailModeSigIndex, stateEmailModeSigIndex+1)
	default:
		return parsedStateParts{}, errStateMalformed
	}
}

func parsePKCEStateParts(parts [][]byte, email string, mode SetupMode, verifierIndex, sigIndex int) (parsedStateParts, error) {
	codeVerifier := string(parts[verifierIndex])
	if !validPKCEVerifier(codeVerifier) {
		return parsedStateParts{}, errStateMalformed
	}
	payload := coreStatePayloadParts(parts)
	if email != "" {
		payload = append(payload, email)
	}
	if mode != SetupModeReuse {
		payload = append(payload, string(mode))
	}
	payload = append(payload, codeVerifier)
	return parsedStateParts{
		email:        email,
		mode:         mode,
		codeVerifier: codeVerifier,
		sigIndex:     sigIndex,
		signed:       signedPayload(payload...),
	}, nil
}

func validPKCEVerifier(verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, c := range verifier {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		switch c {
		case '-', '.', '_', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func stateEmailNormalized(email string) bool {
	normalized, err := NormalizeEmail(email)
	return err == nil && normalized == email
}

func isStateValidationError(err error) bool {
	return errors.Is(err, errStateExpired) ||
		errors.Is(err, errStateBadHMAC) ||
		errors.Is(err, errStateMalformed) ||
		errors.Is(err, errStateFuture)
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
	parsed, err := parseStateParts(parts)
	if err != nil {
		return VerifiedState{}, err
	}
	teamID := string(parts[stateTeamIDIndex])
	userID := string(parts[stateUserIDIndex])
	nonce := parts[stateNonceIndex]
	tsBytes := parts[stateTSIndex]
	sigHex := parts[parsed.sigIndex]
	if teamID == "" || userID == "" || len(nonce) == 0 || len(tsBytes) == 0 || len(sigHex) == 0 {
		return VerifiedState{}, errStateMalformed
	}
	wantSig, err := hex.DecodeString(string(sigHex))
	if err != nil {
		return VerifiedState{}, errStateMalformed
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(parsed.signed)
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
	return VerifiedState{
		TeamID:       teamID,
		UserID:       userID,
		Nonce:        string(nonce),
		CodeVerifier: parsed.codeVerifier,
		Email:        parsed.email,
		Mode:         parsed.mode,
	}, nil
}
