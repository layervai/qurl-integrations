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
		msgPublishAccessConflict, msgPublishExistingPublic, msgPublishExistingPrivate, msgPublishAccessDiffers,
		msgPrivateUnconfirmed, msgPublicUnconfirmed,
		msgGrantEditUnconfirmed,
		msgAccessRequestsUnsupported, msgAccessRequestsCreateIgnored, msgAccessRequestsSettingIgnored,
		msgAccessRequestsCreateUnconfirmed, msgAccessRequestsSettingUnconfirmed, msgAccessRequestsNotTurnedOn, msgApprovalUnconfirmed,
		msgRequestCodeNotFound, msgDeviceIDNotFound, msgAccessRouteRefused, msgRemovalUnconfirmed,
	}
}

// Access requests: a service that does not offer them, an answer that does
// not confirm what was asked, a code or a device id that is not there, and a
// request this release cannot send.
const (
	// msgAccessRequestsUnsupported is the text of
	// ErrAccessRequestsUnsupported, and the start of every message for it.
	// The two details after it say what the service did do, where the command
	// can know.
	msgAccessRequestsUnsupported    = "this service does not offer access requests yet"
	msgAccessRequestsCreateIgnored  = ". The resource was published as a private resource without them; run the command again without --allow-requests to see its CRID"
	msgAccessRequestsSettingIgnored = ": its answer to this change does not show the setting"

	// The service has access requests and its answer does not show the
	// setting that was asked for.
	msgAccessRequestsCreateUnconfirmed  = "the service did not turn on access requests for this resource, so no CRID was printed. The resource is private. Turn them on with `qurl requests <CRID> --on`; `qurl list` shows its CRID"
	msgAccessRequestsSettingUnconfirmed = "the service did not confirm the change to access requests. Run `qurl grants <CRID>` to see the setting as it is now"

	// msgAccessRequestsNotTurnedOn is the headline for a publish that found
	// the target already published as a private resource and could not turn
	// access requests on for it. The rendering adds why, and the command that
	// tries again with the resource's CRID in it.
	msgAccessRequestsNotTurnedOn = "this target is already published as a private resource, but access requests could not be turned on for it"

	// msgApprovalUnconfirmed is shown when the answer to an approval does not
	// name the device that got access. The request may have been approved.
	msgApprovalUnconfirmed = "the service's answer to the approval does not say who got access. Run `qurl grants <CRID>` to see who has access now"

	// %s is the code, written as two groups of three.
	msgRequestCodeNotFound = "no pending request has the code %s for this resource. It may have expired, or been approved or denied already. Run `qurl requests <CRID>` to see the pending requests"
	// %s is the device id. Nothing was removed, and the message says so: a
	// mistyped id must never read as access taken away.
	msgDeviceIDNotFound = "no approved person has the device id %s on this resource, so nothing was removed. Run `qurl grants <CRID>` to see who has access"

	// msgRemovalUnconfirmed is shown when the service answered a removal
	// with success and the resource still lists the person.
	msgRemovalUnconfirmed = "the service still lists a person whose access was removed. Run `qurl grants <CRID>` to see who has access now"

	// msgAccessRouteRefused is shown when this release may not use the
	// device's identity on the route the command needs. Nothing was sent.
	msgAccessRouteRefused = "this release of qurl cannot send this request with this device's identity yet, so nothing was sent. It needs a later release"
)

// ErrAccessRequestsUnsupported marks a service that does not offer access
// requests: its routes are missing, or its answer to a create or a change
// does not have the setting. The command, the operands and the credential are
// all valid; the service is not serving this surface, which is the
// Unavailable exit code.
var ErrAccessRequestsUnsupported = errors.New(msgAccessRequestsUnsupported)

// accessRequestsUnsupportedError is ErrAccessRequestsUnsupported with what
// the command knows about what the service did instead.
type accessRequestsUnsupportedError struct{ detail string }

func (e *accessRequestsUnsupportedError) Error() string { return e.UserMessage() }

// UserMessage is the text the terminal rendering shows.
func (e *accessRequestsUnsupportedError) UserMessage() string {
	return msgAccessRequestsUnsupported + e.detail
}

func (e *accessRequestsUnsupportedError) Unwrap() error { return ErrAccessRequestsUnsupported }

// AccessRequestsNotTurnedOnError is a publish that asked for access requests,
// found the target already published as a private resource with them off,
// and could not turn them on. The resource is as it was before the command.
// What went wrong stays in the chain, so the exit code is that failure's and
// the request id is shown.
type AccessRequestsNotTurnedOnError struct {
	// CRID names the resource that exists. It is shown to the publisher:
	// the create answer confirmed the resource is private, and the CRID is
	// what they need to try again.
	CRID string

	cause error
}

func (e *AccessRequestsNotTurnedOnError) Error() string {
	return msgAccessRequestsNotTurnedOn + ": " + e.Reason()
}

// Headline says what exists and what did not happen. The rendering adds the
// reason and the next step.
func (e *AccessRequestsNotTurnedOnError) Headline() string { return msgAccessRequestsNotTurnedOn }

// Reason is why the change failed, in the words a customer reads: the
// message of a failure that has one, the service's own text for a problem it
// reported, and the failure as it is otherwise.
func (e *AccessRequestsNotTurnedOnError) Reason() string {
	var worded interface{ UserMessage() string }
	if errors.As(e.cause, &worded) {
		return worded.UserMessage()
	}
	var problem *Error
	if errors.As(e.cause, &problem) {
		if problem.Detail != "" {
			return problem.Detail
		}
		if problem.Title != "" {
			return problem.Title
		}
	}
	return e.cause.Error()
}

func (e *AccessRequestsNotTurnedOnError) Unwrap() error { return e.cause }

// answerError is an answer that does not confirm what a command asked for,
// with the message a customer reads. It is an answer outside the contract, so
// it matches the SDK's invalid-response sentinel and has that exit code.
type answerError struct{ message string }

func (e *answerError) Error() string { return e.message }

// UserMessage is the text the terminal rendering shows in place of the
// generic invalid-response wording.
func (e *answerError) UserMessage() string { return e.message }

func (e *answerError) Unwrap() error { return qurl.ErrInvalidAPIResponse }

// accessNotFoundError is a request code or a device id that the service does
// not have for a resource it does have. The service's not-found problem stays
// in the chain, so the exit code is the not-found row and the request id is
// shown.
type accessNotFoundError struct {
	message string
	problem *Error
}

func (e *accessNotFoundError) Error() string { return e.message }

// UserMessage is the text the terminal rendering shows in place of the
// generic not-found hint, which is about a mistyped CRID.
func (e *accessNotFoundError) UserMessage() string { return e.message }

func (e *accessNotFoundError) Unwrap() error { return e.problem }

// accessRouteRefusedError is a request this release may not send with the
// device's identity. The SDK refused it before anything left the machine.
type accessRouteRefusedError struct{ cause error }

func (e *accessRouteRefusedError) Error() string { return msgAccessRouteRefused }

// UserMessage is the text the terminal rendering shows in place of the SDK's
// own wording, which names a method and a path.
func (e *accessRouteRefusedError) UserMessage() string { return msgAccessRouteRefused }

func (e *accessRouteRefusedError) Unwrap() error { return e.cause }

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
	msgPublishAccessDiffers   = "this target is already published, and its access settings differ from what this command asked for"

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
)

// PublishAccessConflictError is a publish refused because the target is
// already published with other access settings. It matches
// ErrPublishAccessConflict, and the service's problem stays in the chain for
// the request id.
type PublishAccessConflictError struct {
	// Existing is what the refusal says about the resource that exists.
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
	case ExistingAccessUnknown:
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
