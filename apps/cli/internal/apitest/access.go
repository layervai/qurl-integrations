package apitest

import (
	"net/http"
	"slices"
	"strings"
)

// This file is the mock's publisher API for access requests: the setting on
// the resource, the pending requests, an approval, a denial, and the people
// who were approved.
//
// TODO(upstream-contract): mirrors the service's publisher API. A resource
// row carries access_requests, and allowed_passkeys when there are any. Only
// a private resource may turn access requests on. A request is addressed by
// its six-digit code and an approved person by device id. An approval is one
// change: the person is on the list and the request is gone, or neither. The
// mock answers an unknown code or device id with 404 not_found, a successful
// approval with 200 and the person, and a denial or removal with 204.

// Field names and fixture values of the access-request payloads.
const (
	fieldAccessRequests  = "access_requests"
	fieldAllowedPasskeys = "allowed_passkeys"
	fieldDeviceID        = "device_id"
	fieldName            = "name"
	codeInvalidInput     = "invalid_input"
	titleInvalidInput    = "Invalid Input"

	// The kinds of access-request route, and the path segments that name
	// them.
	accessList             = "list"
	accessApprove          = "approve"
	accessDeny             = "deny"
	accessRemove           = "remove"
	segmentAccessRequests  = "access-requests"
	segmentAllowedPasskeys = "allowed-passkeys"

	// FixtureRequestedAt and FixtureRequestExpiresAt are the times of every
	// pending request the mock holds. The CLI harness clock is two minutes
	// after the first, so a request reads as asked "2m ago".
	FixtureRequestedAt      = "2026-03-01T23:58:00Z"
	FixtureRequestExpiresAt = "2026-03-02T00:58:00Z"
	// FixtureApprovedAt is when the mock approves a request: the CLI harness
	// clock itself. FixtureEarlierApprovedAt is the approval time of a person
	// added with AddApprovedPerson.
	FixtureApprovedAt        = "2026-03-02T00:00:00Z"
	FixtureEarlierApprovedAt = "2026-03-01T12:00:00Z"
)

// accessRequestFixture is one pending request of the mock's resource.
type accessRequestFixture struct {
	code, name, deviceID string
}

// approvedPersonFixture is one person approved for the mock's resource.
type approvedPersonFixture struct {
	deviceID, name, approvedAt string
}

// PlayNoAccessRequests makes the mock answer like a service from before
// access requests: their routes do not exist, a create request and a change
// that carry the setting are answered as if it were not there, and no
// resource row has the setting or the approved people.
func (s *Server) PlayNoAccessRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noAccessRequests = true
}

// PlayStrictWithoutAccessRequests makes the mock answer like the other kind
// of service from before access requests: one that validates request bodies
// strictly. Their routes do not exist and no row has the setting, as after
// PlayNoAccessRequests, and a create request or a change that carries the
// access_requests member is refused with the service's generic validation
// problem, HTTP 400, before anything else about the request is looked at.
// Nothing is created or changed by such a request.
//
// TODO(upstream-contract): mirrors what a deployment without access requests
// answers to a body with a member it does not know. The problem's code and
// wording are that service's generic ones; a client must not read them.
func (s *Server) PlayStrictWithoutAccessRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noAccessRequests = true
	s.refuseAccessRequestsMember = true
}

// refusesAccessRequestsMember answers a request that carries the
// access_requests member the way PlayStrictWithoutAccessRequests describes,
// and reports whether it did.
func (s *Server) refusesAccessRequestsMember(w http.ResponseWriter, stated *bool) bool {
	s.mu.Lock()
	refuse := s.refuseAccessRequestsMember && stated != nil
	s.mu.Unlock()
	if refuse {
		WriteProblem(s.t, w, http.StatusBadRequest, "validation_error", "Validation Error", "Request validation failed")
	}
	return refuse
}

// ListRequestCodes makes the mock put each request's code in its listing
// rows, as the request_code member, the way a build of the service from
// before the codes left the listings does. A client must read that member
// nowhere and show it nowhere, and this is how a test shows that.
func (s *Server) ListRequestCodes() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listRequestCodes = true
}

// SetAccessRequestsHasMore makes both listings of pending requests carry
// meta.has_more with this value. Without it the member is left out, which
// means the listing is complete.
func (s *Server) SetAccessRequestsHasMore(more bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessRequestsHasMore = &more
}

// SetAccessRequests turns access requests on or off for the mock's resource
// without a request.
func (s *Server) SetAccessRequests(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessRequests = on
}

// AddAccessRequest adds one pending request to the mock's resource.
func (s *Server) AddAccessRequest(code, name, deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingRequests = append(s.pendingRequests, accessRequestFixture{code: code, name: name, deviceID: deviceID})
}

// AddApprovedPerson adds one approved person to the mock's resource.
func (s *Server) AddApprovedPerson(deviceID, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approvedPeople = append(s.approvedPeople, approvedPersonFixture{deviceID: deviceID, name: name, approvedAt: FixtureEarlierApprovedAt})
}

// addAccessRequestFields writes the access-request members of a resource row.
// The caller holds s.mu.
func (s *Server) addAccessRequestFields(row map[string]any) {
	if s.noAccessRequests {
		return
	}
	row[fieldAccessRequests] = s.accessRequests
	if s.omitApprovedPeople {
		return
	}
	// The member is on every row, as an empty array when nobody was
	// approved: "nobody" is said, never left out.
	people := make([]map[string]any, 0, len(s.approvedPeople))
	for _, person := range s.approvedPeople {
		people = append(people, person.payload())
	}
	row[fieldAllowedPasskeys] = people
}

// OmitApprovedPeople makes the mock leave the list of approved people out of
// every resource row. The service does not do that. A client must read the
// missing member as "not said", never as "nobody", and this is how a test
// shows that it does.
func (s *Server) OmitApprovedPeople() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.omitApprovedPeople = true
}

func (p approvedPersonFixture) payload() map[string]any {
	return map[string]any{fieldDeviceID: p.deviceID, fieldName: p.name, "approved_at": p.approvedAt}
}

// applyAccessRequestsSetting applies the access_requests member of a create
// request or a change, and returns the detail of the refusal, empty when it
// is accepted. A service from before access requests does not read the
// member. Turning them on for a public resource is refused. existing says
// that a create found the resource instead of making it: the service then
// changes nothing and does not refuse, so its answer carries the setting the
// resource already had, whatever the request asked for. The caller holds
// s.mu.
func (s *Server) applyAccessRequestsSetting(stated *bool, existing bool) string {
	if s.noAccessRequests || stated == nil {
		return ""
	}
	switch {
	case *stated && !s.private:
		return "access requests can be turned on only for a private resource"
	case existing:
		return ""
	}
	s.accessRequests = *stated
	return ""
}

// handleAccessRoute serves the routes that exist only on a service with
// access requests, and reports whether r named one of them. On a service
// from before them every one answers 404, as an unknown route does.
func (s *Server) handleAccessRoute(w http.ResponseWriter, r *http.Request) bool {
	route, ok := parseAccessRoute(r.Method, r.URL.Path)
	if !ok {
		return false
	}
	s.mu.Lock()
	missing := s.noAccessRequests
	s.mu.Unlock()
	switch {
	case missing:
		WriteProblem(s.t, w, http.StatusNotFound, "not_found", "Not Found", "no such route in the mock qURL API")
	case bearerCredential(r) == "":
		s.writeUnauthorized(w)
	case route.resource != "" && route.resource != s.Key.CRID && route.resource != s.Key.ResourceID:
		WriteProblem(s.t, w, http.StatusNotFound, "not_found", "Not Found", "the requested resource does not exist")
	default:
		s.serveAccessRoute(w, route)
	}
	return true
}

// accessRoute is one parsed access-request route.
type accessRoute struct {
	// kind is accessList, accessApprove, accessDeny or accessRemove.
	kind string
	// resource is the {id} of the route, empty for the listing of all of the
	// owner's resources. operand is the request code or the device id.
	resource, operand string
}

// parseAccessRoute recognizes the access-request routes by method and path.
func parseAccessRoute(method, path string) (accessRoute, bool) {
	if path == "/v1/"+segmentAccessRequests {
		return accessRoute{kind: accessList}, method == http.MethodGet
	}
	rest, ok := strings.CutPrefix(path, "/v1/resources/")
	if !ok {
		return accessRoute{}, false
	}
	segments := strings.Split(rest, "/")
	switch {
	case len(segments) == 2 && segments[1] == segmentAccessRequests && method == http.MethodGet:
		return accessRoute{kind: accessList, resource: segments[0]}, true
	case len(segments) == 4 && segments[1] == segmentAccessRequests && segments[3] == accessApprove && method == http.MethodPost:
		return accessRoute{kind: accessApprove, resource: segments[0], operand: segments[2]}, true
	case len(segments) == 3 && segments[1] == segmentAccessRequests && method == http.MethodDelete:
		return accessRoute{kind: accessDeny, resource: segments[0], operand: segments[2]}, true
	case len(segments) == 3 && segments[1] == segmentAllowedPasskeys && method == http.MethodDelete:
		return accessRoute{kind: accessRemove, resource: segments[0], operand: segments[2]}, true
	default:
		return accessRoute{}, false
	}
}

func (s *Server) serveAccessRoute(w http.ResponseWriter, route accessRoute) {
	switch route.kind {
	case accessList:
		s.mu.Lock()
		rows := make([]map[string]any, 0, len(s.pendingRequests))
		for _, request := range s.pendingRequests {
			// A row says who asked and when. It does not carry the code:
			// the code is on the screen of the person who asked, and
			// nowhere a publisher or an agent could read it from.
			row := map[string]any{
				fieldName: request.name, fieldDeviceID: request.deviceID,
				"requested_at": FixtureRequestedAt, "expires_at": FixtureRequestExpiresAt,
			}
			if s.listRequestCodes {
				row["request_code"] = request.code
			}
			if route.resource == "" {
				row["resource_id"], row[fieldCRID] = s.Key.ResourceID, s.Key.CRID
			}
			rows = append(rows, row)
		}
		var meta map[string]any
		if s.accessRequestsHasMore != nil {
			meta = map[string]any{"has_more": *s.accessRequestsHasMore}
		}
		s.mu.Unlock()
		WriteEnvelope(s.t, w, http.StatusOK, rows, meta)
	case accessApprove:
		s.mu.Lock()
		index := slices.IndexFunc(s.pendingRequests, func(request accessRequestFixture) bool { return request.code == route.operand })
		var person approvedPersonFixture
		if index >= 0 {
			request := s.pendingRequests[index]
			person = approvedPersonFixture{deviceID: request.deviceID, name: request.name, approvedAt: FixtureApprovedAt}
			s.approvedPeople = append(s.approvedPeople, person)
			s.pendingRequests = slices.Delete(s.pendingRequests, index, index+1)
		}
		s.mu.Unlock()
		if index < 0 {
			s.writeAccessNotFound(w, "no pending access request has that code")
			return
		}
		WriteEnvelope(s.t, w, http.StatusOK, person.payload(), nil)
	case accessDeny:
		// A request is refused by the device id it came from, which a
		// listing shows, or by its code, which only the person who asked
		// could have given.
		s.mu.Lock()
		before := len(s.pendingRequests)
		s.pendingRequests = slices.DeleteFunc(s.pendingRequests, func(request accessRequestFixture) bool {
			return request.code == route.operand || request.deviceID == route.operand
		})
		removed := len(s.pendingRequests) != before
		s.mu.Unlock()
		if !removed {
			s.writeAccessNotFound(w, "no pending access request matches")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case accessRemove:
		s.mu.Lock()
		before := len(s.approvedPeople)
		s.approvedPeople = slices.DeleteFunc(s.approvedPeople, func(person approvedPersonFixture) bool { return person.deviceID == route.operand })
		removed := len(s.approvedPeople) != before
		s.mu.Unlock()
		if !removed {
			s.writeAccessNotFound(w, "no approved passkey has that device id")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) writeAccessNotFound(w http.ResponseWriter, detail string) {
	WriteProblem(s.t, w, http.StatusNotFound, "not_found", "Not Found", detail)
}
