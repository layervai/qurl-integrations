package qurlapi

import (
	"fmt"
	"strconv"
)

// errTemplate is the fixed frame around server-provided problem text. It is
// part of the customer surface and covered by the CLI jargon gate.
const errTemplate = "the qURL service reported a problem"

// Error is the repo-owned typed error for any non-2xx qURL API response,
// whether it arrived through the SDK or the direct REST path. Title/Detail
// carry server-provided problem text; the fixed framing lives in errTemplate.
type Error struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Code is the machine-readable problem code, when provided.
	Code string
	// Title is the short human problem summary, when provided.
	Title string
	// Detail is the longer human problem description, when provided.
	Detail string
	// InvalidFields maps field names to what is wrong with them, when the
	// problem document carried per-field validation errors.
	InvalidFields map[string]string
	// RetryAfter is the server-requested wait in seconds for 429 responses
	// that survived the transport's bounded retry, 0 when absent.
	RetryAfter int
	// RequestID correlates the failure with server logs, when provided.
	RequestID string

	err error
}

// Error renders a single line: the fixed frame, the server's title (or
// detail as fallback), and the status. Rich multi-line rendering with hints
// belongs to the output package.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	text := e.Title
	if text == "" {
		text = e.Detail
	}
	if text == "" {
		return errTemplate + " (HTTP " + strconv.Itoa(e.StatusCode) + ")"
	}
	return fmt.Sprintf("%s: %s (HTTP %d)", errTemplate, text, e.StatusCode)
}

// Unwrap keeps the original wire error chain — SDK sentinels included —
// reachable for errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.err }

// CustomerMessages returns the fixed customer-facing strings this package
// can emit, for the CLI-wide jargon gate. Server-provided problem text is
// out of scope: the gate covers what this repo authors.
func CustomerMessages() []string {
	return []string{errTemplate}
}
