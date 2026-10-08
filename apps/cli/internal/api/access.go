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
	}
	return summarizeResourceRow(&env.Data, "access-request setting")
}

// AccessRequests lists the pending requests of one resource, or of all of the
// owner's resources when id is empty.
//
// TODO(upstream-contract): the service bounds the listing of all resources
// and sets meta.has_more when it may be incomplete; a listing without the
// member is complete.
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
// position. The pinned SDK lets a device credential send only a code there;
// its refusal of a device id becomes the message that says so, until a
// release of the SDK admits it.
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
		if byDevice && errors.Is(err, qurl.ErrRegisteredAgentResourceRequestDenied) {
			return &denyByDeviceRefusedError{cause: err}
		}
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
	default:
		return reply.problem()
	}
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
// removed, and the list can change between the read and a removal. Such an
// error is a PasskeyRemovalError that says exactly which device ids were
// removed, which was not found, and which were not attempted.
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
	if err := c.removePeopleInOrder(ctx, id, base, deviceIDs); err != nil {
		return nil, err
	}
	resource, err := c.Resource(ctx, id)
	if err != nil {
		return nil, err
	}
	for index := range resource.AllowedPasskeys {
		if slices.Contains(deviceIDs, resource.AllowedPasskeys[index].DeviceID) {
			return nil, &answerError{message: msgRemovalUnconfirmed}
		}
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

// removePeopleInOrder sends one DELETE for each device id, in the order
// given, and stops at the first that does not succeed. base is the escaped
// path of the resource.
//
// A failure of the first removal is returned as it is: nothing was removed,
// and the failure is the whole story. After that, and for a device id that
// is not found at any position, the error is the outcome that says what
// happened to each device id.
func (c *client) removePeopleInOrder(ctx context.Context, id, base string, deviceIDs []string) error {
	for index, deviceID := range deviceIDs {
		outcome := &PasskeyRemovalError{ID: id, Removed: deviceIDs[:index], NotRemoved: deviceIDs[index+1:]}
		reply, err := c.doRESTOnce(ctx, http.MethodDelete, base+"/allowed-passkeys/"+deviceID, nil)
		if err == nil && reply.status != http.StatusOK && reply.status != http.StatusNoContent && reply.status != http.StatusNotFound {
			err = reply.problem()
		}
		switch {
		case err != nil && index == 0:
			return err
		case err != nil:
			outcome.NotRemoved, outcome.failed, outcome.cause = deviceIDs[index:], deviceID, err
			return outcome
		case reply.status != http.StatusNotFound:
			continue
		}
		problem, err := c.classifyAccessNotFound(ctx, id, reply)
		if err != nil {
			return err
		}
		outcome.NotFound, outcome.problem = []string{deviceID}, problem
		return outcome
	}
	return nil
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
