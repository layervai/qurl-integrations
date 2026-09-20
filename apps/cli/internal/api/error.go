package qurlapi

import (
	"errors"
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
	RetryAfter uint64
	// RequestID correlates the failure with server logs, when provided.
	RequestID string

	err error

	agentEnrollmentScopeRequired     bool
	connectorEnrollmentScopeRequired bool
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

// AgentEnrollmentScopeRequired reports that an account key could not mint the
// one-time registered-device credential.
func (e *Error) AgentEnrollmentScopeRequired() bool {
	return e != nil && e.agentEnrollmentScopeRequired
}

// ConnectorEnrollmentScopeRequired reports that a registered device could
// not mint the one-time credential for a local Connector.
func (e *Error) ConnectorEnrollmentScopeRequired() bool {
	return e != nil && e.connectorEnrollmentScopeRequired
}

// CustomerMessages returns the fixed customer-facing strings this package
// can emit, for the CLI-wide jargon gate. Server-provided problem text is
// out of scope: the gate covers what this repo authors.
func CustomerMessages() []string {
	return []string{errTemplate, msgAccountCallbackInvalid, msgAccountCallbackComplete, msgAccountLoadFailed, msgAccountUnavailable, msgAccountPortBusy, msgAccountBrowserFailed, msgAccountTimedOut, msgAccountCanceled, msgAccountExchangeFailed, msgAccountHTTPSRequired, msgAccountLinkInvalid, msgAccountOwnersInvalid}
}

const (
	msgAccountLoadFailed     = "cannot load account sign-in settings"
	msgAccountUnavailable    = "account sign-in is temporarily unavailable"
	msgAccountPortBusy       = "account sign-in needs local port 8765; close the other sign-in and retry"
	msgAccountBrowserFailed  = "open account sign-in: %w"
	msgAccountTimedOut       = "account sign-in timed out or was canceled; run the command again"
	msgAccountCanceled       = "account sign-in was canceled"
	msgAccountExchangeFailed = "account sign-in could not complete; run the command again"
	msgAccountHTTPSRequired  = "account sign-in requires a trusted HTTPS endpoint"
	msgAccountLinkInvalid    = "%w: invalid account-link response"
	msgAccountOwnersInvalid  = "%w: invalid account owners response"
)

const msgAccountCallbackInvalid = "Invalid sign-in response."
const msgAccountCallbackComplete = "Return to qURL to finish sign-in. You can close this tab."

// Account sign-in failures retain stable exit classes for scripts.
var (
	ErrAccountLoad        = errors.New(msgAccountLoadFailed)
	ErrAccountUnavailable = errors.New(msgAccountUnavailable)
	ErrAccountPort        = errors.New(msgAccountPortBusy)
	ErrAccountDenied      = errors.New(msgAccountCanceled)
	ErrAccountExchange    = errors.New(msgAccountExchangeFailed)
	ErrAccountEndpoint    = errors.New(msgAccountHTTPSRequired)
)
