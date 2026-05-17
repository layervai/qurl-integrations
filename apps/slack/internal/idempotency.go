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
// The helper's output is 64 hex chars regardless of which form is
// hashed — Slack's raw IDs are not 64-hex (trigger_id looks like
// `123.456.abc`, view.id like `V012…`), but the sha256 over them is.
//
// The qURL API server requires `Idempotency-Key` to be at least 32
// chars (verified at qurl-service/internal/api/handlers/apikey_handlers.go:151).
// 64 hex chars from sha256 satisfies that floor with margin.
func IdempotencyKey(teamID, channelID, userID, triggerOrViewID string) string {
	// The separator (NUL) keeps adjacent fields unambiguous —
	// `("ab", "c")` and `("a", "bc")` would otherwise hash equally.
	// Slack-issued IDs (team/channel/user/trigger/view) are
	// alphanumeric and never contain NUL, so concatenation around
	// the separator is guaranteed unambiguous regardless of what
	// future ID shape Slack ships.
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
