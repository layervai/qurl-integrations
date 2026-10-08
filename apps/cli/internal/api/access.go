package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
type accessRequestRow struct {
	Code        string     `json:"request_code"`
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
func allowedPasskeys(rows []passkeyRow, source string) ([]AllowedPasskey, error) {
	if len(rows) == 0 {
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

// accessRequestsPath is the pending-request listing of one resource.
func accessRequestsPath(id string) string {
	return "/v1/resources/" + url.PathEscape(id) + "/access-requests"
}

// doAccess sends one request on a route that exists only on a service with
// access requests. This release's device credential can be used on a fixed
// list of routes, and a route outside it is refused before anything is sent;
// that refusal becomes the message that says so.
func (c *client) doAccess(ctx context.Context, method, path string, body any, allowRetry bool) (*restReply, error) {
	var reply *restReply
	var err error
	if allowRetry {
		reply, err = c.doREST(ctx, method, path, body)
	} else {
		reply, err = c.doRESTOnce(ctx, method, path, body)
	}
	if errors.Is(err, qurl.ErrRegisteredAgentResourceRequestDenied) {
		return nil, &accessRouteRefusedError{cause: err}
	}
	return reply, err
}

// SetAccessRequests turns access requests on or off with one authenticated
// PATCH. It never retries.
//
// The answer must carry the setting that was asked for. A service from before
// access requests ignores the member and answers with a row that does not
// have it; that answer fails here instead of being reported as a change.
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
func (c *client) AccessRequests(ctx context.Context, id string) ([]AccessRequest, error) {
	id = strings.TrimSpace(id)
	path := "/v1/access-requests"
	if id != "" {
		path = accessRequestsPath(id)
	}
	reply, err := c.doAccess(ctx, http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	switch reply.status {
	case http.StatusOK:
	case http.StatusNotFound:
		// Only the listing for one resource can mean that the resource is
		// unknown. The listing for all of them has nothing to be unknown.
		if id == "" {
			return nil, &accessRequestsUnsupportedError{}
		}
		// For one resource it is the resource that is unknown, or the route.
		offered, err := c.accessRequestsOffered(ctx, id, false)
		switch {
		case err != nil:
			return nil, err
		case !offered:
			return nil, &accessRequestsUnsupportedError{}
		default:
			return nil, reply.problem()
		}
	default:
		return nil, reply.problem()
	}
	var env struct {
		Data []accessRequestRow `json:"data"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode access requests: %w", qurl.ErrInvalidAPIResponse, err)
	}
	requests := make([]AccessRequest, 0, len(env.Data))
	for index := range env.Data {
		row := &env.Data[index]
		if !ValidRequestCode(row.Code) || !ValidDeviceID(row.DeviceID) {
			return nil, fmt.Errorf("%w: access request has an invalid request_code or device_id", qurl.ErrInvalidAPIResponse)
		}
		request := AccessRequest{
			Code: row.Code, Name: row.Name, DeviceID: row.DeviceID,
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
		requests = append(requests, request)
	}
	return requests, nil
}

// ApproveAccessRequest approves one pending request with one authenticated
// POST. It never retries: after an approval the request is gone, so a replay
// of a request that succeeded would read as "not found".
func (c *client) ApproveAccessRequest(ctx context.Context, id, code string) (*AllowedPasskey, error) {
	if !ValidRequestCode(code) {
		return nil, fmt.Errorf("%w: a request code is six digits", qurl.ErrInvalidResourceRequest)
	}
	reply, err := c.doAccess(ctx, http.MethodPost, accessRequestsPath(id)+"/"+code+"/approve", struct{}{}, false)
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
func (c *client) DenyAccessRequest(ctx context.Context, id, code string) error {
	if !ValidRequestCode(code) {
		return fmt.Errorf("%w: a request code is six digits", qurl.ErrInvalidResourceRequest)
	}
	reply, err := c.doAccess(ctx, http.MethodDelete, accessRequestsPath(id)+"/"+code, nil, false)
	if err != nil {
		return err
	}
	switch reply.status {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return c.accessNotFound(ctx, id, reply, fmt.Sprintf(msgRequestCodeNotFound, spacedRequestCode(code)))
	default:
		return reply.problem()
	}
}

// RemoveAllowedPasskeys takes approved people off the list, one
// authenticated DELETE for each device id, in the order given. No request is
// retried. A device id that is not on the list is an error, not a removal: a
// mistyped id must never read as access taken away. The requests are separate
// changes, so the first failure stops the rest and the people before it stay
// removed.
//
// Then the resource is read, and no removed device id may be on the list it
// shows. That read is the resource the caller prints.
func (c *client) RemoveAllowedPasskeys(ctx context.Context, id string, deviceIDs []string) (*ResourceSummary, error) {
	if len(deviceIDs) == 0 {
		return nil, fmt.Errorf("%w: no device id to remove", qurl.ErrInvalidResourceRequest)
	}
	for _, deviceID := range deviceIDs {
		if !ValidDeviceID(deviceID) {
			return nil, fmt.Errorf("%w: a device id has the form xxxx-xxxx-xxxx-xxxx", qurl.ErrInvalidResourceRequest)
		}
	}
	for _, deviceID := range deviceIDs {
		reply, err := c.doAccess(ctx, http.MethodDelete, "/v1/resources/"+url.PathEscape(id)+"/allowed-passkeys/"+deviceID, nil, false)
		if err != nil {
			return nil, err
		}
		switch reply.status {
		case http.StatusOK, http.StatusNoContent:
		case http.StatusNotFound:
			return nil, c.accessNotFound(ctx, id, reply, fmt.Sprintf(msgDeviceIDNotFound, deviceID))
		default:
			return nil, reply.problem()
		}
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

// spacedRequestCode writes a valid request code as two groups of three, the
// form it is read aloud and typed in.
func spacedRequestCode(code string) string {
	return code[:3] + " " + code[3:]
}

// accessNotFound explains a 404 from a route that names a request code or a
// device id. The service gives that one answer for three different facts, and
// the remedy differs for each, so the client asks which it is:
//
//   - the resource is unknown: the resource read's own not-found answer;
//   - the service has no access requests at all: the unsupported error;
//   - neither: the code or the device id is the thing that was not found, and
//     the error says so with message.
func (c *client) accessNotFound(ctx context.Context, id string, reply *restReply, message string) error {
	offered, err := c.accessRequestsOffered(ctx, id, true)
	if err != nil {
		return err
	}
	if !offered {
		return &accessRequestsUnsupportedError{}
	}
	var problem *Error
	if !errors.As(reply.problem(), &problem) {
		problem = &Error{StatusCode: http.StatusNotFound}
	}
	return &accessNotFoundError{message: message, problem: problem}
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
	reply, err := c.doREST(ctx, http.MethodGet, "/v1/resources/"+url.PathEscape(id), nil)
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
	reply, err = c.doAccess(ctx, http.MethodGet, accessRequestsPath(id), nil, true)
	if err != nil {
		return false, err
	}
	return reply.status != http.StatusNotFound, nil
}
