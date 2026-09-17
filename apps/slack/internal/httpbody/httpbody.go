// Package httpbody provides bounded response reads for the Slack app and its smoke commands.
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

// ReadResponseBody reads up to limit+1 bytes to detect oversized responses.
// It returns the existing method-specific error text and wraps ErrResponseTooLarge
// on overflow. An oversized body gets a bounded drain; callers must close it.
// Negative limits act as zero. Callers use fixed limits well below math.MaxInt64.
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

// DrainResponseBody discards at most limit+1 bytes. Negative limits act as zero.
// This can permit HTTP/1 connection reuse when the remaining body fits the budget.
// Larger bodies are left for the caller to close, limiting work on oversized replies.
// HTTP/2 connection reuse does not require draining the response stream.
func DrainResponseBody(body io.Reader, limit int64) {
	if limit < 0 {
		limit = 0
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, limit+1))
}

// oversizeResponseError preserves the existing message while exposing the sentinel.
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
