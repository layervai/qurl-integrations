package qurlapi

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/layervai/qurl-go/qurl"
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
	shareNotFound                    bool
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

// ShareNotFound reports that the share operator answered not-found for a
// CRID.
//
// TODO(upstream-contract): qurl-service gives share that one answer for a
// CRID that does not exist, a resource that was removed, and a device that is
// neither the owner's nor allowed. The answer therefore identifies none of
// them, and nothing here may treat it as any one of them.
func (e *Error) ShareNotFound() bool {
	return e != nil && e.shareNotFound
}

// CustomerMessages returns the fixed customer-facing strings this package
// can emit, for the CLI-wide jargon gate. Server-provided problem text is
// out of scope: the gate covers what this repo authors.
func CustomerMessages() []string {
	return []string{
		errTemplate, msgAccountCallbackInvalid, msgAccountCallbackComplete, msgAccountLoadFailed, msgAccountUnavailable, msgAccountPortBusy, msgAccountBrowserFailed, msgAccountTimedOut, msgAccountCanceled, msgAccountExchangeFailed, msgAccountHTTPSRequired, msgAccountLinkInvalid, msgAccountOwnersInvalid,
		msgPublishAccessConflict, msgPublishExistingPublic, msgPublishExistingPrivate, msgPublishOtherDevices, msgPublishAccessDiffers,
		msgPrivateUnconfirmed, msgPublicUnconfirmed, msgUnaskedPublicDeleted, msgUnaskedPublicNotDeleted,
		msgGrantEditUnconfirmed,
	}
}

// msgGrantEditUnconfirmed is shown when the answer to an add or remove of
// single device grants does not show the change. A service from before those
// requests ignores them and returns the list as it was, so that is the cause
// the message names. It claims nothing about the list: the command to read it
// is the next step.
const msgGrantEditUnconfirmed = "this service cannot add or remove single device grants yet: its answer does not show the change that was asked for. Run `qurl grants <CRID>` to see the list as it is now"

// grantEditError is an answer to an add or remove of device grants that does
// not show the change. It is an answer outside the contract, so it matches
// the SDK's invalid-response sentinel and has that exit code.
type grantEditError struct{}

func (e *grantEditError) Error() string { return msgGrantEditUnconfirmed }

// UserMessage is the text the terminal rendering shows in place of the
// generic invalid-response wording.
func (e *grantEditError) UserMessage() string { return msgGrantEditUnconfirmed }

func (e *grantEditError) Unwrap() error { return qurl.ErrInvalidAPIResponse }

// Publish refusals and answers that do not confirm the privacy asked for.
const (
	// msgPublishAccessConflict is the text of ErrPublishAccessConflict. The
	// three messages after it are what a customer reads, one for each thing
	// the refusal can say about the resource that exists.
	msgPublishAccessConflict  = "this target is already published with other access settings"
	msgPublishExistingPublic  = "this target is already published as public, and privacy is fixed when a resource is first published"
	msgPublishExistingPrivate = "this target is already published as private, and privacy is fixed when a resource is first published"
	msgPublishOtherDevices    = "this target is already published, and its allowed devices differ from the ones this command named"
	msgPublishAccessDiffers   = "this target is already published, and its privacy or its allowed devices differ from what this command asked for"

	// A publish that named no privacy went to keep using the public resource
	// that exists, and the service made a new public resource instead. Nobody
	// asked for one, so it is not kept and its CRID is not named. The first
	// message is for a resource the command deleted, the second for one it
	// could not delete.
	msgUnaskedPublicDeleted    = "this target was no longer published when the command went to keep using its public resource, and the service made a new public resource for it. Nobody asked for a public one, so the command deleted it and printed no CRID. Run the command again to publish the target as private"
	msgUnaskedPublicNotDeleted = "this target was no longer published when the command went to keep using its public resource, and the service made a new public resource for it. Nobody asked for a public one, and the command could not delete it, so no CRID was printed. Run `qurl list` to find it, delete it with `qurl delete <CRID>`, and publish again"

	// The answer to a create request did not say that the resource has the
	// privacy the request stated. Nothing is printed on stdout, and the
	// message says what may now exist, because the service may have made the
	// resource before it answered.
	msgPrivateUnconfirmed = "the service did not confirm that this resource is private, so no CRID was printed. A resource may now exist for this target and may be public: run `qurl list` to check, and delete it if you did not mean to publish it"
	msgPublicUnconfirmed  = "the service did not confirm that this resource is public, so no CRID was printed. A resource may now exist for this target and may be private: run `qurl list` to check"
)

// ErrPublishAccessConflict marks a publish the service refused because the
// target is already published with other access settings. Command, operand
// and credential are all valid; the request conflicts with a resource that
// exists, so it has the Conflict exit code.
var ErrPublishAccessConflict = errors.New(msgPublishAccessConflict)

// ExistingAccess is what a refused publish says about the resource that is
// already published for the target.
type ExistingAccess int

const (
	// ExistingAccessUnknown means the refusal does not say whether privacy or
	// the device list is what differs.
	ExistingAccessUnknown ExistingAccess = iota
	// ExistingAccessPublic means the target is already published as public.
	ExistingAccessPublic
	// ExistingAccessPrivate means the target is already published as private.
	ExistingAccessPrivate
	// ExistingAccessOtherDevices means the target is already published with
	// another list of allowed devices than the request named.
	ExistingAccessOtherDevices
)

// PublishAccessConflictError is a publish that cannot be given because the
// target is already published with other access settings. It matches
// ErrPublishAccessConflict. When the service refused the request, its problem
// stays in the chain for the request id; a conflict read from an answer that
// accepted the request has none.
type PublishAccessConflictError struct {
	// Existing is what is known about the resource that exists.
	Existing ExistingAccess

	problem *Error
}

// Error states what exists, in the customer's words. The rendering adds the
// next step.
func (e *PublishAccessConflictError) Error() string {
	switch e.Existing {
	case ExistingAccessPublic:
		return msgPublishExistingPublic
	case ExistingAccessPrivate:
		return msgPublishExistingPrivate
	case ExistingAccessOtherDevices:
		return msgPublishOtherDevices
	case ExistingAccessUnknown:
		return msgPublishAccessDiffers
	}
	return msgPublishAccessDiffers
}

// Unwrap exposes the sentinel and the service's problem.
func (e *PublishAccessConflictError) Unwrap() []error {
	if e.problem == nil {
		return []error{ErrPublishAccessConflict}
	}
	return []error{ErrPublishAccessConflict, e.problem}
}

// publishPrivacyError is a create answer that did not confirm the privacy the
// request stated. It is an answer outside the contract, so it matches the
// SDK's invalid-response sentinel and has that exit code.
type publishPrivacyError struct{ wantPublic bool }

func (e *publishPrivacyError) Error() string { return e.UserMessage() }

// UserMessage is the text the terminal rendering shows in place of the
// generic invalid-response wording.
func (e *publishPrivacyError) UserMessage() string {
	if e.wantPublic {
		return msgPublicUnconfirmed
	}
	return msgPrivateUnconfirmed
}

func (e *publishPrivacyError) Unwrap() error { return qurl.ErrInvalidAPIResponse }

// unaskedPublicError is a publish that named no privacy and was answered with
// a new public resource. That is an answer outside what the request meant, so
// it matches the SDK's invalid-response sentinel and has that exit code.
// notDeleted is why the command could not delete the resource, nil when it
// did.
type unaskedPublicError struct{ notDeleted error }

func (e *unaskedPublicError) Error() string {
	if e.notDeleted != nil {
		return msgUnaskedPublicNotDeleted + ": " + e.notDeleted.Error()
	}
	return msgUnaskedPublicDeleted
}

// UserMessage is the text the terminal rendering shows in place of the
// generic invalid-response wording.
func (e *unaskedPublicError) UserMessage() string {
	if e.notDeleted != nil {
		return msgUnaskedPublicNotDeleted
	}
	return msgUnaskedPublicDeleted
}

func (e *unaskedPublicError) Unwrap() error { return qurl.ErrInvalidAPIResponse }

const (
	msgAccountLoadFailed     = "cannot load account sign-in settings"
	msgAccountUnavailable    = "account sign-in is temporarily unavailable"
	msgAccountPortBusy       = "account sign-in needs local port 8765; close the other sign-in and retry"
	msgAccountBrowserFailed  = "open account sign-in: %w"
	msgAccountTimedOut       = "account sign-in timed out after 15 minutes; run the command again"
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
