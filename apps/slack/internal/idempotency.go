package internal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// IdempotencyKey derives a deterministic 64-hex Idempotency-Key for a
// Slack-originated request from the four scope-distinguishing fields
// Slack hands the bot. The same `(team, channel, user,
// triggerOrViewID)` tuple always hashes to the same key, so a slash
// command that times out on our side and gets retried by Slack
// converges on the same key — which the qURL service uses to dedupe
// the resulting create.
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
// representable, but Slack IDs are bounded well below that — no
// runtime guard needed.
func writeLengthPrefixed(h interface{ Write([]byte) (int, error) }, s string) {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(len(s)))
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte(s))
}
