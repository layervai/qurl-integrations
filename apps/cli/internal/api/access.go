package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/resourceidentity"
)

// This file is the publisher's side of access requests: turning them on for a
// private resource, reading the pending requests, approving or denying one by
// its code, and taking an approved person's access away.
//
// TODO(upstream-contract): the routes and members here mirror the service's
// publisher API for access requests. A request is addressed by the six-digit
// code the service made for it; an approved person by the device id of the
// device they asked from. The service applies an approval as one change: the
// person is on the list and the request is gone, or neither.

// passkeyRow is one approved person in a resource row.
type passkeyRow struct {
	DeviceID   string     `json:"device_id"`
	Name       string     `json:"name"`
	ApprovedAt *time.Time `json:"approved_at"`
}

// accessRequestRow is one pending request. The listing for all of an owner's
// resources also names the resource; the listing for one resource does not.
//
// The row has no member for a request's code. The service does not send
// one, and a build of it that still does is not read: the code belongs to
// the person who asked, and a member this type does not have can reach no
// output.
type accessRequestRow struct {
	Name        string     `json:"name"`
	DeviceID    string     `json:"device_id"`
	RequestedAt *time.Time `json:"requested_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	ResourceID  string     `json:"resource_id"`
	CRID        string     `json:"crid"`
}

// ValidRequestCode reports whether code is exactly six ASCII digits, the only
// form a request code has.
func ValidRequestCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, digit := range code {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// ValidDeviceID reports whether id has the form xxxx-xxxx-xxxx-xxxx in the
// lowercase base32 alphabet, the only form a device id has.
func ValidDeviceID(id string) bool {
	if len(id) != 19 {
		return false
	}
	for index, character := range id {
		if index%5 == 4 {
			if character != '-' {
				return false
			}
			continue
		}
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			return false
		}
	}
	return true
}

// allowedPasskeys projects the approved people of a resource row. A row
// whose device id is not a device id is an answer outside the contract: the
// id is what a publisher passes to take access away, so it is never shown in
// a form that could not be passed back.
//
// The result is nil only when the row has no such member, which means the
// service did not say. A row that says nobody was approved gives an empty
// list, never nil: "nobody" and "not said" are different answers, and a
// caller that reports who still has access must not read the second as the
// first.
//
// TODO(upstream-contract): the service writes allowed_passkeys on every
// resource row, as an empty array when nobody was approved, on the answer to
// a change as on a read.
func allowedPasskeys(rows []passkeyRow, source string) ([]AllowedPasskey, error) {
	if rows == nil {
		return nil, nil
	}
	people := make([]AllowedPasskey, 0, len(rows))
	for index := range rows {
		row := &rows[index]
		if !ValidDeviceID(row.DeviceID) {
			return nil, fmt.Errorf("%w: %s lists an approved person with an invalid device_id", qurl.ErrInvalidAPIResponse, source)
		}
		people = append(people, AllowedPasskey{DeviceID: row.DeviceID, Name: row.Name, ApprovedAt: knownTime(row.ApprovedAt)})
	}
	return people, nil
}

// resourcePath returns the trimmed identifier of a resource and its escaped
// path, for every request here that names one. It is the idiom of a grant
// change (deviceGrantsPath): the identifier is trimmed, and an empty one, or
// one that could never name a resource, is refused before any request. The
// routes of this file are that path or lie below it, and what they append, a
// request code or a device id, is checked by its own rule before it is added.
//
// What is checked here is the identifier, against the route of the resource
// itself. The routes below it are not looked up in the list of routes for
// supervised requests, and are not on it: see requestRoutes.
func resourcePath(id string) (trimmed, path string, err error) {
	return deviceGrantsPath(id)
}

// accessRequestsSegment is the part of a resource's path that holds its
// pending requests.
const accessRequestsSegment = "/access-requests"

// refusedForNoAccessRequests reports whether an HTTP 400 answer to a request
// that carried the access_requests member is the answer of a service that
// has no access requests. When it is not, the refusal is a real one, and the
// caller shows the service's problem as it is.
//
// A service from before access requests answers such a request in one of two
// ways. It ignores the member, which the checks on the answer catch. Or it
// validates the body strictly and refuses a member it does not know, before
// it looks at anything else: that is its generic validation problem, which
// tells a publisher nothing they can act on.
//
// The wording of that problem is not read. The client asks instead whether
// the service has access requests at all, with the listing that exists only
// on a service that has them. A missing listing means it does not. Any other
// answer to that question, a failure to ask it included, leaves the refusal
// standing: a service that has access requests refused the request for a
// reason of its own, or the client did not learn which it is and claims
// nothing.
func (c *client) refusedForNoAccessRequests(ctx context.Context, reply *restReply) bool {
	if reply.status != http.StatusBadRequest {
		return false
	}
	listing, err := c.doREST(ctx, http.MethodGet, "/v1/access-requests", nil)
	return err == nil && listing.status == http.StatusNotFound
}

// SetAccessRequests turns access requests on or off with one authenticated
// PATCH. It never retries.
//
// The answer must carry the setting that was asked for. A service from before
// access requests ignores the member and answers with a row that does not
// have it; that answer fails here instead of being reported as a change. One
// that refuses the member it does not know is told apart from a service that
// refused the change itself; see refusedForNoAccessRequests.
//
// An answer that turns access requests on must also say that the resource is
// private. What a publisher is told next is that the resource's address is
// safe to send to anyone, because a private resource opens only for the
// people they allow. That is true of a private resource and of no other, so
// it is said only when the service's own answer says the resource is
// private. The service refuses the setting for a public resource; an answer
// that accepts it for one, or does not say, fails here.
func (c *client) SetAccessRequests(ctx context.Context, id string, on bool) (*ResourceSummary, error) {
	// The setting is changed on the route a grant change uses, and the path
	// is built the same way: the identifier trimmed and escaped.
	id, path, err := deviceGrantsPath(id)
	if err != nil {
		return nil, err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPatch, path, struct {
		AccessRequests bool `json:"access_requests"`
	}{AccessRequests: on})
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		if c.refusedForNoAccessRequests(ctx, reply) {
			return nil, &accessRequestsUnsupportedError{}
		}
		return nil, reply.problem()
	}
	var env struct {
		Data resourceRow `json:"data"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode access-request setting: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if err := validateSharingIdentity(id, &sharingRow{CRID: env.Data.CRID, ResourceID: env.Data.ResourceID}); err != nil {
		return nil, err
	}
	switch {
	case env.Data.AccessRequests == nil:
		return nil, &accessRequestsUnsupportedError{detail: msgAccessRequestsSettingIgnored}
	case *env.Data.AccessRequests != on:
		return nil, &answerError{message: msgAccessRequestsSettingUnconfirmed}
	case on && env.Data.Private == nil:
		return nil, &accessRequestsNotPrivateError{}
	case on && !*env.Data.Private:
		return nil, &accessRequestsNotPrivateError{public: true}
	}
	return summarizeResourceRow(&env.Data, "access-request setting")
}

// AccessRequests lists the pending requests of one resource, or of all of the
// owner's resources when id is empty.
//
// TODO(upstream-contract): the service bounds the listing of all resources
// and sets meta.has_more when it may be incomplete; a listing without the
// member is complete. That listing takes no cursor, so there is no next page
// to ask for, and none is asked for here. The listing of one resource holds
// at most 20 requests.
func (c *client) AccessRequests(ctx context.Context, id string) (*AccessRequestList, error) {
	id = strings.TrimSpace(id)
	path := "/v1/access-requests"
	if id != "" {
		var base string
		var err error
		if id, base, err = resourcePath(id); err != nil {
			return nil, err
		}
		path = base + accessRequestsSegment
	}
	reply, err := c.doREST(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		return nil, c.accessListingProblem(ctx, id, reply)
	}
	var env struct {
		Data []accessRequestRow `json:"data"`
		Meta envelopeMeta       `json:"meta"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode access requests: %w", qurl.ErrInvalidAPIResponse, err)
	}
	list := &AccessRequestList{Requests: make([]AccessRequest, 0, len(env.Data)), HasMore: env.Meta.HasMore}
	for index := range env.Data {
		row := &env.Data[index]
		if !ValidDeviceID(row.DeviceID) {
			return nil, fmt.Errorf("%w: access request has an invalid device_id", qurl.ErrInvalidAPIResponse)
		}
		request := AccessRequest{
			Name: row.Name, DeviceID: row.DeviceID,
			RequestedAt: knownTime(row.RequestedAt), ExpiresAt: knownTime(row.ExpiresAt),
			CRID: row.CRID, ResourceID: row.ResourceID,
		}
		if id == "" {
			// The listing for all resources is the only source of the
			// resource a request is for, so the pair must be a real one.
			if err := resourceidentity.ValidatePair(row.CRID, row.ResourceID); err != nil {
				return nil, fmt.Errorf("%w: access request resource identity: %w", qurl.ErrInvalidAPIResponse, err)
			}
		} else {
			// One resource was asked about. Its requests are that resource's,
			// whatever a row says.
			request.CRID, request.ResourceID = id, ""
		}
		list.Requests = append(list.Requests, request)
	}
	return list, nil
}

// accessListingProblem is the error for a listing that was not answered with
// 200. Only the listing for one resource can mean that the resource is
// unknown; the listing for all of them has nothing to be unknown, so its
// not-found answer is a service without access requests.
func (c *client) accessListingProblem(ctx context.Context, id string, reply *restReply) error {
	if reply.status != http.StatusNotFound {
		return reply.problem()
	}
	if id == "" {
		return &accessRequestsUnsupportedError{}
	}
	// For one resource it is the resource that is unknown, or the route.
	offered, err := c.accessRequestsOffered(ctx, id, false)
	switch {
	case err != nil:
		return err
	case !offered:
		return &accessRequestsUnsupportedError{}
	default:
		return reply.problem()
	}
}

// ApproveAccessRequest approves one pending request with one authenticated
// POST. It never retries: after an approval the request is gone, so a replay
// of a request that succeeded would read as "not found".
//
// TODO(upstream-contract): the service limits wrong codes. After 5 wrong
// codes for one resource within an hour it answers an approval, and a denial
// by code, with 429 and a Retry-After, whatever the code, a right one
// included. That answer is never sent again by this client either: another
// attempt could be one more wrong code, and the wait can be an hour.
func (c *client) ApproveAccessRequest(ctx context.Context, id, code string) (*AllowedPasskey, error) {
	if !ValidRequestCode(code) {
		return nil, fmt.Errorf("%w: a request code is six digits", qurl.ErrInvalidResourceRequest)
	}
	id, base, err := resourcePath(id)
	if err != nil {
		return nil, err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPost, base+accessRequestsSegment+"/"+code+"/approve", struct{}{})
	if err != nil {
		return nil, err
	}
	switch reply.status {
	case http.StatusOK, http.StatusCreated:
	case http.StatusNotFound:
		return nil, c.accessNotFound(ctx, id, reply, fmt.Sprintf(msgRequestCodeNotFound, spacedRequestCode(code)))
	case http.StatusTooManyRequests:
		return nil, codeLimit(reply)
	default:
		return nil, reply.problem()
	}
	var env struct {
		Data passkeyRow `json:"data"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode approval: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if !ValidDeviceID(env.Data.DeviceID) {
		// The request may have been approved. Without the device id the
		// command cannot say who got access, so it does not claim anything.
		return nil, &answerError{message: msgApprovalUnconfirmed}
	}
	return &AllowedPasskey{DeviceID: env.Data.DeviceID, Name: env.Data.Name, ApprovedAt: knownTime(env.Data.ApprovedAt)}, nil
}

// DenyAccessRequest removes one pending request with one authenticated
// DELETE. It never retries.
//
// request names the request in one of two ways: by the device id it came
// from, which is what a listing shows a publisher, or by its six-digit code,
// for a publisher who was given a code and wants to refuse it. Each is sent
// as it is, in the same place of the path. A value that is neither is refused
// before any request.
//
// TODO(upstream-contract): the service takes a device id or a code in that
// position, and so does the SDK for a device credential.
func (c *client) DenyAccessRequest(ctx context.Context, id, request string) error {
	byDevice := ValidDeviceID(request)
	if !byDevice && !ValidRequestCode(request) {
		return fmt.Errorf("%w: a request is named by a device id of the form xxxx-xxxx-xxxx-xxxx or by a six-digit code", qurl.ErrInvalidResourceRequest)
	}
	id, base, err := resourcePath(id)
	if err != nil {
		return err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodDelete, base+accessRequestsSegment+"/"+request, nil)
	if err != nil {
		return err
	}
	switch reply.status {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		if byDevice {
			return c.accessNotFound(ctx, id, reply, fmt.Sprintf(msgRequestDeviceNotFound, request))
		}
		return c.accessNotFound(ctx, id, reply, fmt.Sprintf(msgRequestCodeNotFound, spacedRequestCode(request)))
	case http.StatusTooManyRequests:
		if !byDevice {
			return codeLimit(reply)
		}
		return reply.problem()
	default:
		return reply.problem()
	}
}

// codeLimit is the error for a "too many requests" answer to a request that
// carried a code.
func codeLimit(reply *restReply) error {
	limit := &RequestCodeLimitError{}
	if !errors.As(reply.problem(), &limit.Problem) {
		return reply.problem()
	}
	return limit
}

// RemoveAllowedPasskeys takes approved people off the list, and returns the
// resource as it reads afterwards.
//
// The list is read first, and every device id is checked against it before
// any access is taken away. A device id that is not on the list stops the
// command there: nothing was removed, and the error says so. A mistyped id
// must never read as access taken away, and with several ids in one command
// the reverse must not happen either, access taken away from one person
// while the output says that nothing changed.
//
// Then the people are removed, one authenticated DELETE for each device id,
// in the order given. No request is retried. The requests are separate
// changes, so one that fails after others succeeded leaves those people
// removed, and the list can change between the read and a removal.
//
// Until the first removal is made, nothing was removed, and a failure is
// returned as it is: it is the whole story. From the first removal on, access
// was taken away from someone, and everything that can still fail is in
// finishRemoval, whose failure is a PasskeyRemovalError and nothing else. It
// says who lost access, what failed, and who still has access as far as the
// command knows.
//
// Last, the resource is read again, and no removed device id may be on the
// list it shows. That read is the resource the caller prints.
func (c *client) RemoveAllowedPasskeys(ctx context.Context, id string, deviceIDs []string) (*ResourceSummary, error) {
	if err := validateDeviceIDs(deviceIDs); err != nil {
		return nil, err
	}
	id, base, err := resourcePath(id)
	if err != nil {
		return nil, err
	}
	before, err := c.Resource(ctx, id)
	if err != nil {
		return nil, err
	}
	if outcome := peopleNotOnList(id, before.AllowedPasskeys, deviceIDs); outcome != nil {
		return nil, outcome
	}
	answer, err := c.removePerson(ctx, base, deviceIDs[0])
	if err != nil {
		return nil, err
	}
	if answer.status == http.StatusNotFound {
		problem, err := c.classifyAccessNotFound(ctx, id, answer)
		if err != nil {
			return nil, err
		}
		return nil, &PasskeyRemovalError{ID: id, NotFound: deviceIDs[:1], NotRemoved: deviceIDs[1:], problem: problem}
	}
	resource, outcome := c.finishRemoval(ctx, base, &removalProgress{id: id, named: deviceIDs, removed: 1})
	if outcome != nil {
		return nil, outcome
	}
	return resource, nil
}

// removePerson sends the one DELETE that takes a person off the list, and
// returns the service's answer. The answer has one of two meanings: the
// person was removed, or, with the status "not found", something was not
// found, and the caller finds out what. Every other answer is an error.
func (c *client) removePerson(ctx context.Context, base, deviceID string) (*restReply, error) {
	reply, err := c.doRESTOnce(ctx, http.MethodDelete, base+"/allowed-passkeys/"+deviceID, nil)
	if err != nil {
		return nil, err
	}
	switch reply.status {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return reply, nil
	default:
		return nil, reply.problem()
	}
}

// removalProgress is what a removal knows once its first person is off the
// list: the device ids the command named, in order, and how many of them,
// from the front, were removed.
//
// Its methods are the only places that build the failure of a removal that
// has taken access away. Each starts from the same outcome, which has the
// people who lost access in it, so no failure can leave them out.
type removalProgress struct {
	id      string
	named   []string
	removed int
}

// outcome is the start of every failure: who lost access, and who the
// command did not get to.
func (p *removalProgress) outcome() *PasskeyRemovalError {
	return &PasskeyRemovalError{ID: p.id, Removed: p.named[:p.removed], NotRemoved: p.named[p.removed:]}
}

// failed is a removal that failed for another reason than "not found". The
// person still has access as far as the command knows.
func (p *removalProgress) failed(cause error) *PasskeyRemovalError {
	outcome := p.outcome()
	outcome.failed, outcome.cause = p.named[p.removed], cause
	return outcome
}

// notFound is a removal of a device id that no approved person has any more.
func (p *removalProgress) notFound(problem *Error) *PasskeyRemovalError {
	outcome := p.outcome()
	outcome.NotFound, outcome.NotRemoved, outcome.problem = p.named[p.removed:p.removed+1], p.named[p.removed+1:], problem
	return outcome
}

// unexplained is a removal that the service answered with "not found", when
// the command could not find out whether the person or the resource was not
// found. cause is why it could not. The command does not say that this
// person still has access: it does not know.
func (p *removalProgress) unexplained(cause error) *PasskeyRemovalError {
	outcome := p.notFound(nil)
	outcome.stop, outcome.cause = stoppedUnexplained, cause
	return outcome
}

// listNotRead is a removal of every person, after which the list could not
// be read again. Everyone the command named lost access.
func (p *removalProgress) listNotRead(cause error) *PasskeyRemovalError {
	outcome := p.outcome()
	outcome.stop, outcome.cause = stoppedListNotRead, cause
	return outcome
}

// stillListed is a removal of every person that the service answered as
// made, with a list read afterwards that still shows some of them. The list
// is what the service says now, so the people on it still have access, and
// the others lost it.
func (p *removalProgress) stillListed(listed []string) *PasskeyRemovalError {
	outcome := p.outcome()
	outcome.Removed = slices.DeleteFunc(slices.Clone(p.named), func(deviceID string) bool { return slices.Contains(listed, deviceID) })
	outcome.NotRemoved, outcome.stop, outcome.cause = listed, stoppedStillListed, &answerError{message: msgRemovalUnconfirmed}
	return outcome
}

// finishRemoval removes the people after the first, reads the resource again
// and checks it. base is the escaped path of the resource.
//
// Its failure type is the outcome, not error, on purpose. Access has been
// taken away from at least one person when it is called, and a failure that
// did not say so would read as "nothing happened": the same command run
// again would then find that person gone and say that nothing was removed.
// So a path that returns a plain error here does not compile, and every
// outcome is built by removalProgress, which puts the people who lost access
// into it.
func (c *client) finishRemoval(ctx context.Context, base string, progress *removalProgress) (*ResourceSummary, *PasskeyRemovalError) {
	for progress.removed < len(progress.named) {
		answer, err := c.removePerson(ctx, base, progress.named[progress.removed])
		if err != nil {
			return nil, progress.failed(err)
		}
		if answer.status == http.StatusNotFound {
			problem, err := c.classifyAccessNotFound(ctx, progress.id, answer)
			if err != nil {
				return nil, progress.unexplained(err)
			}
			return nil, progress.notFound(problem)
		}
		progress.removed++
	}
	resource, err := c.Resource(ctx, progress.id)
	if err != nil {
		return nil, progress.listNotRead(err)
	}
	var listed []string
	for _, deviceID := range progress.named {
		if slices.ContainsFunc(resource.AllowedPasskeys, func(person AllowedPasskey) bool { return person.DeviceID == deviceID }) {
			listed = append(listed, deviceID)
		}
	}
	if len(listed) > 0 {
		return nil, progress.stillListed(listed)
	}
	return resource, nil
}

// validateDeviceIDs refuses a removal that names nobody, a value that is not
// a device id, and a device id given twice, before any request.
func validateDeviceIDs(deviceIDs []string) error {
	if len(deviceIDs) == 0 {
		return fmt.Errorf("%w: no device id to remove", qurl.ErrInvalidResourceRequest)
	}
	for index, deviceID := range deviceIDs {
		if !ValidDeviceID(deviceID) {
			return fmt.Errorf("%w: a device id has the form xxxx-xxxx-xxxx-xxxx", qurl.ErrInvalidResourceRequest)
		}
		if slices.Contains(deviceIDs[:index], deviceID) {
			return fmt.Errorf("%w: a device id was given twice", qurl.ErrInvalidResourceRequest)
		}
	}
	return nil
}

// peopleNotOnList checks every device id of a removal against the list of
// approved people as it was just read, and returns the outcome when one is
// not on it: nothing was removed, and nothing will be. It returns nil when
// every device id is on the list.
//
// It also returns nil for a list the service did not send, which cannot be
// checked against. The removals then find out one at a time, and report as
// exactly.
func peopleNotOnList(id string, approved []AllowedPasskey, deviceIDs []string) *PasskeyRemovalError {
	if approved == nil {
		return nil
	}
	outcome := &PasskeyRemovalError{ID: id}
	for _, deviceID := range deviceIDs {
		if slices.ContainsFunc(approved, func(person AllowedPasskey) bool { return person.DeviceID == deviceID }) {
			outcome.NotRemoved = append(outcome.NotRemoved, deviceID)
		} else {
			outcome.NotFound = append(outcome.NotFound, deviceID)
		}
	}
	if len(outcome.NotFound) == 0 {
		return nil
	}
	return outcome
}

// spacedRequestCode writes a valid request code as two groups of three, the
// form it is read aloud and typed in. The text output of a listing writes
// codes the same way, with its own function for a value it did not check; a
// change to one form belongs in both.
func spacedRequestCode(code string) string {
	return code[:3] + " " + code[3:]
}

// accessNotFound explains a 404 from a route that names a request code. The
// thing that was not found is told apart by classifyAccessNotFound; when it
// is the code, the error says so with message.
func (c *client) accessNotFound(ctx context.Context, id string, reply *restReply, message string) error {
	problem, err := c.classifyAccessNotFound(ctx, id, reply)
	if err != nil {
		return err
	}
	return &accessNotFoundError{message: message, problem: problem}
}

// classifyAccessNotFound tells apart the facts behind a 404 from a route that
// names a request code or a device id. The service gives that one answer for
// three different facts, and the remedy differs for each, so the client asks
// which it is:
//
//   - the resource is unknown: the resource read's own not-found answer;
//   - the service has no access requests at all: the unsupported error;
//   - neither: the code or the device id is the thing that was not found.
//     Then there is no error, and the service's problem is returned for the
//     caller's own message.
func (c *client) classifyAccessNotFound(ctx context.Context, id string, reply *restReply) (*Error, error) {
	offered, err := c.accessRequestsOffered(ctx, id, true)
	if err != nil {
		return nil, err
	}
	if !offered {
		return nil, &accessRequestsUnsupportedError{}
	}
	var problem *Error
	if !errors.As(reply.problem(), &problem) {
		problem = &Error{StatusCode: http.StatusNotFound}
	}
	return problem, nil
}

// accessRequestsOffered reports whether the service offers access requests,
// for a resource that exists. It returns the resource read's problem when the
// resource does not.
//
// A service with access requests writes the access_requests member on a
// resource row. A row without it is not proof of an older service, because a
// service may leave a false value out, so the listing of pending requests is
// asked too when probeList is set: that route exists only on a service that
// has them.
func (c *client) accessRequestsOffered(ctx context.Context, id string, probeList bool) (bool, error) {
	_, base, err := resourcePath(id)
	if err != nil {
		return false, err
	}
	reply, err := c.doREST(ctx, http.MethodGet, base, nil)
	if err != nil {
		return false, err
	}
	if reply.status != http.StatusOK {
		return false, reply.problem()
	}
	var env struct {
		Data *struct {
			Resource *resourceRow `json:"resource"`
		} `json:"data"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return false, fmt.Errorf("%w: decode resource detail: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if env.Data != nil && env.Data.Resource != nil && env.Data.Resource.AccessRequests != nil {
		return true, nil
	}
	if !probeList {
		return false, nil
	}
	reply, err = c.doREST(ctx, http.MethodGet, base+accessRequestsSegment, nil)
	if err != nil {
		return false, err
	}
	return reply.status != http.StatusNotFound, nil
}
