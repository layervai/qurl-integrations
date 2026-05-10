package internal

import (
	"crypto/sha256"
	"encoding/hex"
)

// IdempotencyKey derives a deterministic 64-hex Idempotency-Key for a
// Slack-originated request from the four scope-distinguishing fields
// Slack hands the bot. The same `(team, channel, user, trigger)` tuple
// always hashes to the same key, so a slash command that times out on
// our side and gets retried by Slack converges on the same key — which
// the qURL service uses to dedupe the resulting create.
//
// For view-submission flows (the `setalias` rebind modal, the
// `admin claim` modal), Slack reuses the trigger_id once and then
// rotates it before the user submits the form. The stable identifier
// across an open-modal/submit-modal pair is `view.id`. Callers
// substitute `view.id` for `triggerID` when building the key for
// view-submission idempotency. Both forms are 64 hex chars from the
// caller's perspective — this helper doesn't care whether it's a
// trigger or a view ID, it just hashes whatever it gets.
//
// The qURL API server requires `Idempotency-Key` to be at least 32
// chars (verified at qurl-service/internal/api/handlers/apikey_handlers.go:151).
// 64 hex chars from sha256 satisfies that floor with margin.
func IdempotencyKey(teamID, channelID, userID, triggerID string) string {
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
	h.Write([]byte(triggerID))
	return hex.EncodeToString(h.Sum(nil))
}
