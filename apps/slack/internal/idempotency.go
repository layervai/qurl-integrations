package internal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
)

// IdempotencyKey derives a deterministic 64-hex Idempotency-Key for a
// Slack-originated request from the four scope-distinguishing fields
// Slack hands the bot. The same `(team, channel, user,
// triggerOrViewID)` tuple always hashes to the same key — which the
// qURL service uses to dedupe a request that the user-side retries
// (double-clicking the submit button on a modal within the same
// trigger_id lifetime) or that the bot itself retries (e.g. when an
// upstream call fails after the user-visible roundtrip has already
// committed). Slack does not auto-retry slash commands the way it
// auto-retries Events API deliveries — a slow slash command simply
// shows the user "this command is not responding" — so the dedupe
// surface here defends against double-submit and bot-side retry, not
// against Slack-side retry delivery.
//
// `triggerOrViewID` accepts either a Slack `trigger_id` (for the
// initial slash-command roundtrip) or a `view.id` (for the
// view-submission roundtrip). Slack reuses the trigger_id once and
// then rotates it before the user submits the form, so the stable
// identifier across an open-modal/submit-modal pair is `view.id`.
// The helper's output is 64 hex chars regardless of which form is
// hashed — Slack's raw IDs are not 64-hex (trigger_id looks like
// `123.456.abc`, view.id like `V012…`), but the sha256 over them is.
//
// The qURL API server requires `Idempotency-Key` to be at least 32
// chars (verified at qurl-service/internal/api/handlers/apikey_handlers.go:151).
// 64 hex chars from sha256 satisfies that floor with margin.
//
// Note: a second helper, `idempotencyKeyForCreate` in process.go,
// computes a 2-field (team, trigger) key for the legacy `create`
// flow. Kept separate here so this PR stays purely additive — the
// `create` handler's call site will migrate to `IdempotencyKey`
// (using its in-account channel/user fields too) when the
// alias-aware dispatcher in PR-3c.3+ replaces `processCreate`.
func IdempotencyKey(teamID, channelID, userID, triggerOrViewID string) string {
	// Length-prefix each field with a fixed-width 4-byte big-endian
	// uint32 before its bytes. This makes the encoding unambiguous
	// regardless of what character set the fields use: `("ab", "c")`
	// and `("a", "bc")` produce different prefix bytes
	// (`\x00\x00\x00\x02ab` vs `\x00\x00\x00\x01a`) before either
	// field's content matters, so the collision is structurally
	// impossible — no NUL-separator-style invariant ("Slack IDs
	// remain alphanumeric forever") to maintain.
	h := sha256.New()
	writeLengthPrefixed(h, teamID)
	writeLengthPrefixed(h, channelID)
	writeLengthPrefixed(h, userID)
	writeLengthPrefixed(h, triggerOrViewID)
	return hex.EncodeToString(h.Sum(nil))
}

// writeLengthPrefixed writes `len(s)` as a 4-byte big-endian uint32
// followed by `s`'s bytes. A field longer than 2^32-1 bytes is not
// representable in a uint32, and silently truncating the length here
// would let `("aaa…2^32+1 'a's", "")` collide with `("", "aaa…")` —
// the exact field-boundary failure mode the length-prefix scheme is
// designed to prevent. Slack IDs are bounded well below 4 GiB, so
// the panic is unreachable in practice; this is defense-in-depth
// against a future caller wiring this up to a non-Slack source.
//
// Takes an [io.Writer] so the encoding logic is decoupled from the
// specific hash implementation. In practice the only caller passes
// a [hash.Hash] (which embeds io.Writer), and hash.Hash.Write is
// documented to never return an error — so the ignored returns are
// safe by contract, not by hope.
func writeLengthPrefixed(w io.Writer, s string) {
	if uint64(len(s)) > math.MaxUint32 {
		panic(fmt.Sprintf("writeLengthPrefixed: field of %d bytes exceeds uint32 max — would silently collide on length-prefix encoding", len(s)))
	}
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(len(s)))
	_, _ = w.Write(buf[:])
	_, _ = w.Write([]byte(s))
}
