package internal

import (
	"crypto/sha256"
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
// The helper hashes whatever string it gets — both forms are 64 hex
// chars on the wire from the API server's perspective.
//
// The qURL API server requires `Idempotency-Key` to be at least 32
// chars (verified at qurl-service/internal/api/handlers/apikey_handlers.go:151).
// 64 hex chars from sha256 satisfies that floor with margin.
func IdempotencyKey(teamID, channelID, userID, triggerOrViewID string) string {
	// The separator (NUL) keeps adjacent fields unambiguous —
	// `("ab", "c")` and `("a", "bc")` would otherwise hash equally.
	// Plain '|' would work too, but NUL is reserved against any
	// future field that happens to contain a pipe.
	const sep = "\x00"
	h := sha256.New()
	h.Write([]byte(teamID))
	h.Write([]byte(sep))
	h.Write([]byte(channelID))
	h.Write([]byte(sep))
	h.Write([]byte(userID))
	h.Write([]byte(sep))
	h.Write([]byte(triggerOrViewID))
	return hex.EncodeToString(h.Sum(nil))
}
