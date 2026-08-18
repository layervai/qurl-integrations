// Package httpbody holds the bounded response-body read shared by every Slack HTTP
// caller in this app: the production Lambda in apps/slack/cmd and the two
// operator-triggered smoke commands beside it.
//
// It is deliberately a leaf carrying no Slack concepts beyond the method name it puts
// in an error string. The read sits on a per-invocation Lambda path and on a hand-run
// command at once, so it must not live in a package named for either — the same reason
// nethost is separate from slacksmoke, which is where these two functions lived while
// the smoke commands were their only callers.
//
// Callers keep their own response-size ceiling and pass it to ReadResponseBody and
// DrainResponseBody; see the latter for why the limit is a parameter rather than a
// constant here.
package httpbody

import (
	"errors"
	"fmt"
	"io"
)

// ErrResponseTooLarge marks a response body that ran past the caller's ceiling. Its own
// text is never printed: ReadResponseBody's error renders the operator-facing message
// and unwraps to this, so a caller can attach its own bookkeeping — slack-dm-smoke
// records a result code — by matching on the sentinel rather than on the message.
var ErrResponseTooLarge = errors.New("response exceeded caller limit")

// ReadResponseBody reads a Slack response body under the caller's ceiling, returning
// the bytes or an error carrying the text callers print. method names the Slack Web API
// method the read belongs to, which is what that text leads with.
//
// The read is limit+1 bytes — deliberately one past the ceiling — and an oversized body
// is detected by having read that extra byte, rather than by trusting a Content-Length
// the caller has no way to verify. The over-read and the comparison against it are why
// this is one function rather than an idiom copied per call site: reading limit bytes,
// or comparing with >=, silently truncates a body that exactly fills the ceiling into a
// JSON parse error. Nothing in CI would catch that drift between copies — dupl's
// threshold is 150 tokens and the idiom is a third of that, which is how one of the six
// Lambda copies came to be missing its drain entirely.
//
// An oversized body is drained before the error returns, and the error unwraps to
// ErrResponseTooLarge so a caller can record its own code for that case. limit is a
// parameter, and a negative one is clamped, both for the reasons given on
// DrainResponseBody.
func ReadResponseBody(method string, body io.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		limit = 0
	}
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s response read: %w", method, err)
	}
	if int64(len(raw)) > limit {
		DrainResponseBody(body, limit)
		return nil, oversizeResponseError{method: method, limit: limit}
	}
	return raw, nil
}

// DrainResponseBody discards up to limit+1 bytes of body — one past the caller's
// ceiling, matching ReadResponseBody's over-read. It is a best-effort attempt at
// connection reuse; Close tears the response down if bytes still remain.
//
// The bound is what makes this safe to share with the Lambda. Draining is only ever
// worth what a reused connection saves, and it is worth nothing at all against Slack
// today: slack.com serves HTTP/2, where the transport reuses the connection whether the
// body is drained, bounded-drained, or not read at all — a RST_STREAM does not close
// the TCP connection. Only on an HTTP/1.1 fallback does the drain decide reuse, and
// there a bounded and an unbounded drain behave identically for any body below
// 2*limit+2 bytes, which covers every realistic overshoot. Past that the bounded form
// hangs up rather than spending the rest of the client timeout discarding bytes from a
// peer that just broke the ceiling — the right trade for a per-invocation-billed
// Lambda, and the one apps/cli already makes in its own two drains.
//
// limit is a parameter rather than a package constant because the callers' ceilings
// differ by two orders of magnitude — slack-dm-smoke reads small chat.postMessage
// envelopes, while slack-history-upload-smoke reads whole conversations.history pages.
// When this function was duplicated per command, the same body silently drained a 64x
// different budget depending on which file it sat in. A negative limit is clamped to
// zero, so it drains at most one byte rather than having io.LimitReader treat a
// negative count as immediate EOF and skip the drain entirely.
func DrainResponseBody(body io.Reader, limit int64) {
	if limit < 0 {
		limit = 0
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, limit+1))
}

// oversizeResponseError renders the over-limit message operators read while unwrapping
// to ErrResponseTooLarge. The wrapping is done with a type rather than a
// fmt.Errorf("...: %w", ErrResponseTooLarge), because that would append the sentinel's
// own text to a message that has to stay byte-identical to the one every caller printed
// before this was hoisted.
type oversizeResponseError struct {
	method string
	limit  int64
}

// Error renders the operator-facing message; the sentinel's text never appears in it.
func (e oversizeResponseError) Error() string {
	return fmt.Sprintf("%s response exceeded %d bytes", e.method, e.limit)
}

// Unwrap reports ErrResponseTooLarge, which is what callers match on.
func (e oversizeResponseError) Unwrap() error { return ErrResponseTooLarge }
