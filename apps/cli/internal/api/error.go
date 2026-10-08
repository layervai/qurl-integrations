package qurlapi

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

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
		msgAccessRequestsUnsupported, msgAccessRequestsCreateIgnored, msgAccessRequestsCreateRefused, msgAccessRequestsSettingIgnored,
		msgAccessRequestsCreateUnconfirmed, msgAccessRequestsSettingUnconfirmed, msgAccessRequestsNotTurnedOn, msgApprovalUnconfirmed,
		msgAccessRequestsOnPublic, msgAccessRequestsPrivacyNotSaid,
		msgRequestCodeNotFound, msgRequestDeviceNotFound, msgDeviceIDNotFound, msgDeviceIDsNotFound, msgRemovedThenNotFound, msgRemovedThenFailed,
		msgRemovedThenKeysFailed, msgRemovedThenUnexplained, msgRemovedThenListNotRead, msgRemovedButStillListed, msgAnsweredButStillListed,
		msgStillHasAccess, msgStillHaveAccess, msgSeeWhoHasAccess, msgApprovedPersonNotFound, msgRemovalUnconfirmed,
	}
}

// Access requests: a service that does not offer them, an answer that does
// not confirm what was asked, a code or a device id that is not there, and a
// request this release cannot send.
const (
	// msgAccessRequestsUnsupported is the text of
	// ErrAccessRequestsUnsupported, and the start of every message for it.
	// The details after it say what the service did do, where the command can
	// know: a create it answered without the setting, a create it refused
	// because it does not know the member, and a change whose answer does not
	// show the setting.
	msgAccessRequestsUnsupported    = "this service does not offer access requests yet"
	msgAccessRequestsCreateIgnored  = ". The resource was published as a private resource without them; run the command again without --allow-requests to see its CRID"
	msgAccessRequestsCreateRefused  = ". Nothing was published; run the command again without --allow-requests to publish the resource as private"
	msgAccessRequestsSettingIgnored = ": its answer to this change does not show the setting"

	// The service has access requests and its answer does not show the
	// setting that was asked for.
	msgAccessRequestsCreateUnconfirmed  = "the service did not turn on access requests for this resource, so no CRID was printed. The resource is private. Turn them on with `qurl requests <CRID> --on`; `qurl list` shows its CRID"
	msgAccessRequestsSettingUnconfirmed = "the service did not confirm the change to access requests. Run `qurl grants <CRID>` to see the setting as it is now"

	// msgAccessRequestsOnPublic is shown when the answer that turns access
	// requests on says the resource is public. The service refuses the
	// setting for a public resource, so this is a service that did not. The
	// publisher must not be told what is true of a private resource only.
	msgAccessRequestsOnPublic = "access requests are for a private resource, and the service's answer says this one is public: anyone who has the CRID can open it, whether you approve them or not. The service turned the setting on all the same. To turn it off again, run `qurl requests <CRID> --off`"
	// msgAccessRequestsPrivacyNotSaid is the same refusal for an answer
	// that does not say whether the resource is private.
	msgAccessRequestsPrivacyNotSaid = "the service turned access requests on, but its answer does not say that this resource is private. Until you know that it is, treat it as a resource that anyone who has the CRID can open. Run `qurl grants <CRID>` to see the resource as it is"

	// msgAccessRequestsNotTurnedOn is the headline for a publish that found
	// the target already published as a private resource and could not turn
	// access requests on for it. The rendering adds why, and the command that
	// tries again with the resource's CRID in it.
	msgAccessRequestsNotTurnedOn = "this target is already published as a private resource, but access requests could not be turned on for it"

	// msgApprovalUnconfirmed is shown when the answer to an approval does not
	// name the device that got access. The request may have been approved.
	msgApprovalUnconfirmed = "the service's answer to the approval does not say who got access. Run `qurl grants <CRID>` to see who has access now"

	// %s is the code the publisher typed, written as two groups of three.
	// The message says nothing about which codes do exist: a wrong code
	// must teach nothing about a right one.
	msgRequestCodeNotFound = "no pending request has the code %s for this resource. It may have expired, or been approved or denied already. Ask the person for the code on their screen; `qurl requests <CRID>` shows who is waiting"
	// %s is the device id the publisher named for a denial.
	msgRequestDeviceNotFound = "no pending request is from the device id %s for this resource. It may have expired, or been approved or denied already. Run `qurl requests <CRID>` to see who is waiting"
	// The messages of a removal of approved people that did not remove every
	// person it named. A mistyped device id must never read as access taken
	// away, and access that was taken away must never read as a typo, so
	// each says exactly what happened to which device id. %s is a device id
	// or a list of them; the last %s of the three that have one is the
	// identifier the command was given.
	//
	// Nothing was removed: an id is not on the list.
	msgDeviceIDNotFound  = "no approved person has the device id %s on this resource, so nothing was removed"
	msgDeviceIDsNotFound = "no approved person has the device ids %s on this resource, so nothing was removed"
	// Access was taken away from some, and then an id was not found, or a
	// removal failed for another reason.
	msgRemovedThenNotFound = "access was taken away from %s. Then no approved person had the device id %s on this resource, and the command stopped"
	msgRemovedThenFailed   = "access was taken away from %s. Then taking it away from %s failed, and the command stopped"
	// Every removal was made, and the change to public keys that the same
	// command named failed. The message does not say whether any key was
	// changed: a change that failed may still have been made.
	msgRemovedThenKeysFailed = "access was taken away from %s. Then the change to the public keys failed"
	// Access was taken away from some, and then the service answered a
	// removal with "not found" and the command could not find out whether
	// the person or the resource was not found. Nothing is said about
	// whether that person has access.
	msgRemovedThenUnexplained = "access was taken away from %s. Then the service found nothing to remove for %s, and the command stopped before it could find out why"
	// Every removal was made, and the list could not be read afterwards.
	msgRemovedThenListNotRead = "access was taken away from %s. Then the list of who has access could not be read"
	// The service answered every removal as made, and the list it sent
	// afterwards still shows some of the people, or all of them. The second
	// and third %s of the first message are the same people.
	msgRemovedButStillListed  = "access was taken away from %s. The service answered the same for %s, but its list still shows %s"
	msgAnsweredButStillListed = "the service answered that access was taken away from %s, but its list still shows %s"
	// The device ids the command named that still have access as far as it
	// knows, and the next step.
	msgStillHasAccess  = "%s still has access"
	msgStillHaveAccess = "%s still have access"
	msgSeeWhoHasAccess = "Run `qurl grants %s` to see who has access now"
	// msgApprovedPersonNotFound is the text of ErrApprovedPersonNotFound.
	msgApprovedPersonNotFound = "no approved person has that device id"

	// msgRemovalUnconfirmed is shown when the service answered a removal
	// with success and the resource still lists the person.
	msgRemovalUnconfirmed = "the service still lists a person whose access was removed. Run `qurl grants <CRID>` to see who has access now"
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

// RequestCodeLimitError is a "too many requests" answer to an approval or to
// a denial by code. The service limits wrong codes for one resource, and
// while the limit holds it refuses every code for that resource, a right one
// included. The answer is the service's: its sentence, and how long it asked
// the caller to wait. What this adds is what a publisher does next, which is
// to ask the person for the code on their screen.
//
// The client does not send such a request again by itself. Another attempt
// could be one more wrong code, and the wait can be an hour.
type RequestCodeLimitError struct {
	// Problem is the service's answer.
	Problem *Error
}

func (e *RequestCodeLimitError) Error() string { return e.Problem.Error() }

// Unwrap exposes the service's answer, which decides the exit code.
func (e *RequestCodeLimitError) Unwrap() error { return e.Problem }

// accessRequestsNotPrivateError is an answer that turned access requests on
// for a resource it does not say is private: public says that it says the
// resource is public. Nothing about the resource may then be presented as
// private.
type accessRequestsNotPrivateError struct{ public bool }

func (e *accessRequestsNotPrivateError) Error() string { return e.UserMessage() }

// UserMessage is the text the terminal rendering shows.
func (e *accessRequestsNotPrivateError) UserMessage() string {
	if e.public {
		return msgAccessRequestsOnPublic
	}
	return msgAccessRequestsPrivacyNotSaid
}

func (e *accessRequestsNotPrivateError) Unwrap() error { return qurl.ErrInvalidAPIResponse }

// ErrApprovedPersonNotFound marks a removal that named a device id no approved
// person has. The command and the resource are fine; the thing that was named
// is not there, which is the not-found exit code.
var ErrApprovedPersonNotFound = errors.New(msgApprovedPersonNotFound)

// PasskeyRemovalError is a removal of approved people that did not take every
// person it named off the list. It says what happened to each device id, so
// that neither kind of wrong reading is possible: a mistyped id as access
// taken away, or access taken away as a typo.
//
// The three lists hold every device id the command named, each in one of
// them, in the order given.
//
// A removal that has taken access away from someone fails with this error
// and with no other; see finishRemoval.
type PasskeyRemovalError struct {
	// ID is the resource identifier the command was given.
	ID string
	// Removed are the device ids whose access was taken away.
	Removed []string
	// NotFound are the device ids the service did not find: the ones that
	// are not on the list, and one whose removal it answered with "not
	// found" when the command could not find out why.
	NotFound []string
	// NotRemoved are the device ids that still have access as far as the
	// command knows: the ones it did not get to, and one whose removal
	// failed.
	NotRemoved []string
	// KeysNotChanged is set by a caller that was also asked to change the
	// public keys in the same command. That change comes after the removals,
	// so it was not made, and the outcome says so.
	KeysNotChanged bool
	// KeyChange is set by a caller whose removals were all made and whose
	// change to public keys then failed: it is that failure. The people in
	// Removed have lost access all the same, which the failure alone would
	// not say, and running the same command again would find them gone.
	KeyChange error
	// KeyChangeCommand is the command that makes the change to public keys
	// alone. A caller sets it when every person was removed and the change
	// to public keys failed or was not reached: it is what finishes the job.
	KeyChangeCommand string

	// failed is the device id whose removal failed for another reason than
	// not being found, and cause that reason. cause is also why the command
	// stopped in the ways stop names.
	failed string
	cause  error
	// stop says that the removal stopped in one of the ways that are not a
	// device id that was not found and not a removal that failed.
	stop removalStop
	// problem is the service's not-found answer, when a removal got one.
	problem *Error
}

// removalStop is a way a removal can stop after it has taken access away,
// other than a device id that was not found and a removal that failed.
type removalStop int

const (
	// stoppedUnexplained: the service answered a removal with "not found",
	// and the command could not find out what was not found.
	stoppedUnexplained removalStop = iota + 1
	// stoppedListNotRead: every person was removed, and the list could not
	// be read afterwards.
	stoppedListNotRead
	// stoppedStillListed: the service answered every removal as made, and
	// its list still shows some of the people.
	stoppedStillListed
)

// Headline says what happened, in one sentence or two, without the reason of
// a failure and without the next step.
func (e *PasskeyRemovalError) Headline() string {
	var text string
	switch {
	case e.KeyChange != nil:
		text = fmt.Sprintf(msgRemovedThenKeysFailed, wordList(e.Removed))
	case e.stop == stoppedListNotRead:
		text = fmt.Sprintf(msgRemovedThenListNotRead, wordList(e.Removed))
	case e.stop == stoppedStillListed && len(e.Removed) > 0:
		text = fmt.Sprintf(msgRemovedButStillListed, wordList(e.Removed), wordList(e.NotRemoved), wordList(e.NotRemoved))
	case e.stop == stoppedStillListed:
		text = fmt.Sprintf(msgAnsweredButStillListed, wordList(e.NotRemoved), wordList(e.NotRemoved))
	case e.stop == stoppedUnexplained:
		text = fmt.Sprintf(msgRemovedThenUnexplained, wordList(e.Removed), wordList(e.NotFound))
	case e.failed != "":
		text = fmt.Sprintf(msgRemovedThenFailed, wordList(e.Removed), e.failed)
	case len(e.Removed) > 0:
		text = fmt.Sprintf(msgRemovedThenNotFound, wordList(e.Removed), wordList(e.NotFound))
	case len(e.NotFound) == 1:
		text = fmt.Sprintf(msgDeviceIDNotFound, e.NotFound[0])
	default:
		text = fmt.Sprintf(msgDeviceIDsNotFound, wordList(e.NotFound))
	}
	switch len(e.NotRemoved) {
	case 0:
		return text
	case 1:
		return text + ". " + fmt.Sprintf(msgStillHasAccess, e.NotRemoved[0])
	}
	return text + ". " + fmt.Sprintf(msgStillHaveAccess, wordList(e.NotRemoved))
}

// Reason is why a removal, or the change to public keys after the removals,
// failed, when it failed for another reason than a device id that was not
// found; empty otherwise. It is the service's own text for a problem it
// reported, and the failure as it is otherwise.
func (e *PasskeyRemovalError) Reason() string {
	failure := e.cause
	if e.KeyChange != nil {
		failure = e.KeyChange
	}
	// The list that still shows a person is the whole reason, and the
	// headline has it.
	if failure == nil || (e.stop == stoppedStillListed && e.KeyChange == nil) {
		return ""
	}
	var worded interface{ UserMessage() string }
	if errors.As(failure, &worded) {
		return worded.UserMessage()
	}
	var problem *Error
	if errors.As(failure, &problem) {
		if problem.Detail != "" {
			return problem.Detail
		}
		if problem.Title != "" {
			return problem.Title
		}
	}
	return failure.Error()
}

// NextStep is the command that shows who has access now.
func (e *PasskeyRemovalError) NextStep() string {
	id := e.ID
	if id == "" {
		id = "<CRID>"
	}
	return fmt.Sprintf(msgSeeWhoHasAccess, id)
}

func (e *PasskeyRemovalError) Error() string {
	text := e.Headline()
	if reason := e.Reason(); reason != "" {
		text += ": " + reason
	}
	return text + ". " + e.NextStep()
}

// Unwrap exposes what decides the exit code. A device id that was not found
// is the not-found sentinel, with the service's answer when there was one.
// A removal that failed for another reason is that failure.
func (e *PasskeyRemovalError) Unwrap() []error {
	var chain []error
	// An answer the command could not explain is not known to be about the
	// person, so it is not the not-found sentinel: why the command could
	// not explain it decides.
	if len(e.NotFound) > 0 && e.stop != stoppedUnexplained {
		chain = append(chain, ErrApprovedPersonNotFound)
	}
	if e.problem != nil {
		chain = append(chain, e.problem)
	}
	if e.cause != nil {
		chain = append(chain, e.cause)
	}
	if e.KeyChange != nil {
		chain = append(chain, e.KeyChange)
	}
	return chain
}

// wordList writes values as a phrase: "a", "a and b", "a, b and c".
func wordList(values []string) string {
	switch len(values) {
	case 0:
		return ""
	case 1:
		return values[0]
	}
	return strings.Join(values[:len(values)-1], ", ") + " and " + values[len(values)-1]
}

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

// msgGrantEditUnconfirmed is shown when the answer to an add or remove of
// single device grants does not show the change. It says what is known first:
// the answer does not show the change. Why is not known. A service from
// before those requests ignores them and returns the list as it was, so that
// is named as what may be the cause, not as the cause. The message claims
// nothing about the list: the command to read it is the next step.
const msgGrantEditUnconfirmed = "the service's answer does not show the change that was asked for. The service may not support adding or removing single device grants yet. Run `qurl grants <CRID>` to see the list as it is now"

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
	// NamedFlags are the access flags the publisher's command line carried,
	// as written, empty when the caller gave none. The next step names them:
	// they are what has to be left out for the command to work.
	NamedFlags []string

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
