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

// slackSignatureVersion is the version prefix Slack uses on its request
// signatures. See https://api.slack.com/authentication/verifying-requests-from-slack.
const slackSignatureVersion = "v0"

// slackTimestampSkew bounds how far from now a Slack request's timestamp may
// be before it's treated as a replay. Slack's own recommendation is 5 minutes.
const slackTimestampSkew = 5 * time.Minute

// Sentinel errors so callers can tell "not signed" from "signed but wrong"
// without string matching. A missing header is distinguishable from a tampered
// body, which matters for operator metrics and for tests.
var (
	errSlackSignatureMissing   = errors.New("slack: missing X-Slack-Signature or X-Slack-Request-Timestamp")
	errSlackSignatureMalformed = errors.New("slack: malformed X-Slack-Signature")
	errSlackTimestampStale     = errors.New("slack: request timestamp outside allowed skew")
	errSlackSignatureMismatch  = errors.New("slack: signature does not match body")
)

// verifySlackSignature authenticates an incoming Slack HTTP request by
// recomputing HMAC-SHA256("v0:"+timestamp+":"+body) with the shared signing
// secret and comparing against the X-Slack-Signature header in constant time.
//
// `now` is taken as a parameter so tests can pin the clock without a global
// override.
func verifySlackSignature(signingSecret, body, sigHeader, tsHeader string, now time.Time) error {
	if signingSecret == "" {
		// Treat an empty secret as a fail-closed misconfig: the caller must
		// set one explicitly. Returning here prevents a blank secret from
		// silently accepting any request that happens to hash to "v0=".
		return errors.New("slack: signing secret is empty")
	}
	if sigHeader == "" || tsHeader == "" {
		return errSlackSignatureMissing
	}
	if !strings.HasPrefix(sigHeader, slackSignatureVersion+"=") {
		return errSlackSignatureMalformed
	}
	providedHex := sigHeader[len(slackSignatureVersion)+1:]
	providedSig, err := hex.DecodeString(providedHex)
	if err != nil {
		return errSlackSignatureMalformed
	}

	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return errSlackSignatureMalformed
	}
	delta := now.Unix() - ts
	if delta < 0 {
		delta = -delta
	}
	if time.Duration(delta)*time.Second > slackTimestampSkew {
		return errSlackTimestampStale
	}

	mac := hmac.New(sha256.New, []byte(signingSecret))
	// Slack's base string is "v0:<timestamp>:<raw body>". Any deviation
	// (trailing newline, re-encoded body) breaks verification.
	mac.Write([]byte(slackSignatureVersion + ":" + tsHeader + ":" + body))
	expected := mac.Sum(nil)

	if !hmac.Equal(expected, providedSig) {
		return errSlackSignatureMismatch
	}
	return nil
}
