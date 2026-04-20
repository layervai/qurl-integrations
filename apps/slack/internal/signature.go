package internal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	slackSignatureVersion = "v0"
	slackTimestampSkew    = 5 * time.Minute // Slack's recommended replay window.
)

// Sentinel errors so classifySlackErr can bucket metrics without
// string-matching log lines. Empty-secret gets its own because it means
// the deployment is effectively open — ops should page on it distinctly.
var (
	errSlackSigningSecretEmpty = errors.New("slack: signing secret is empty")
	errSlackSignatureMissing   = errors.New("slack: missing X-Slack-Signature or X-Slack-Request-Timestamp")
	errSlackSignatureMalformed = errors.New("slack: malformed X-Slack-Signature")
	errSlackTimestampStale     = errors.New("slack: request timestamp outside allowed skew")
	errSlackSignatureMismatch  = errors.New("slack: signature does not match body")
)

// verifySlackSignature authenticates a Slack request by recomputing
// HMAC-SHA256("v0:"+timestamp+":"+body) and comparing in constant time.
// `now` is injected so tests can pin the clock.
//
// Check order: empty-secret → missing-headers → malformed → stale-ts →
// HMAC. All checks before HMAC touch only caller-supplied data (never the
// signing secret), so there's no timing oracle on the secret. Don't
// reorder.
func verifySlackSignature(signingSecret, body, sigHeader, tsHeader string, now time.Time) error {
	if signingSecret == "" {
		return errSlackSigningSecretEmpty
	}
	if sigHeader == "" || tsHeader == "" {
		return errSlackSignatureMissing
	}
	if !strings.HasPrefix(sigHeader, slackSignatureVersion+"=") {
		return errSlackSignatureMalformed
	}
	providedHex := sigHeader[len(slackSignatureVersion)+1:]
	if len(providedHex) != sha256.Size*2 {
		return errSlackSignatureMalformed
	}
	providedSig, err := hex.DecodeString(providedHex)
	if err != nil {
		return errSlackSignatureMalformed
	}

	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return errSlackSignatureMalformed
	}
	// Bound ts structurally so a MinInt64 input can't wrap the skew check.
	// HMAC still gates the actual path; this makes the skew check literally
	// correct for all inputs.
	if ts < 0 || ts > now.Unix()+24*60*60 {
		return errSlackTimestampStale
	}
	delta := now.Sub(time.Unix(ts, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > slackTimestampSkew {
		return errSlackTimestampStale
	}

	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(slackSignatureVersion + ":" + tsHeader + ":" + body))
	if !hmac.Equal(mac.Sum(nil), providedSig) {
		return errSlackSignatureMismatch
	}
	return nil
}
