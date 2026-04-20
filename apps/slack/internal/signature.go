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
// Malformed-signature and malformed-timestamp are separate sentinels so a
// dashboard can tell "client sent junk" from "API Gateway is base64-wrapping
// us and we can't parse the body" — those look identical under a single
// "malformed" bucket but are different ops problems.
var (
	errSlackSigningSecretEmpty = errors.New("slack: signing secret is empty")
	errSlackSignatureMissing   = errors.New("slack: missing X-Slack-Signature or X-Slack-Request-Timestamp")
	errSlackSignatureMalformed = errors.New("slack: malformed X-Slack-Signature")
	errSlackTimestampMalformed = errors.New("slack: malformed X-Slack-Request-Timestamp")
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
	providedHex, hasPrefix := strings.CutPrefix(sigHeader, slackSignatureVersion+"=")
	if !hasPrefix {
		return errSlackSignatureMalformed
	}
	if len(providedHex) != sha256.Size*2 {
		return errSlackSignatureMalformed
	}
	providedSig, err := hex.DecodeString(providedHex)
	if err != nil {
		return errSlackSignatureMalformed
	}

	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return errSlackTimestampMalformed
	}
	// Bound ts so a MinInt64 can't wrap the skew math below. The +24h
	// upper bound is strictly belt-and-suspenders — the 5-min skew check
	// already rejects anything meaningfully in the future, and with ts
	// bounded to [0, MaxInt64) time.Unix / now.Sub won't wrap for centuries.
	// Keeping the upper bound documented so a reader doesn't wonder why
	// two overlapping checks exist.
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
