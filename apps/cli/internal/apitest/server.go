package apitest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/layervai/qurl-go/crid"
)

// RecordedRequest captures one request for header/shape assertions.
type RecordedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	// Body is the request body exactly as sent, empty when there was none.
	// The handler that answers the request still reads the same bytes.
	Body []byte
}

// Server is the scriptable mock qURL API.
//
// The share route is authenticated. A share request that carries no bearer
// credential fails the owning test and is answered 401, and no scripted
// handler runs for it: a client that sends one has a defect no scenario
// should be able to hide. The guard is unconditional, so a future path that
// may share without a credential has to change the guard here, deliberately.
type Server struct {
	*httptest.Server
	t *testing.T

	// Key backs the default happy-path responses; DER, resource id, and
	// CRID are mutually consistent, so default share answers verify cleanly.
	Key *ResourceKey

	mu                   sync.Mutex
	requests             []RecordedRequest
	scripts              map[string][]http.HandlerFunc
	shareCRID            string
	shareQURL            string
	downloadPayload      []byte
	publishFoundExisting *bool
	publishOmitCRID      bool
	publisherName        string
	omitPublisher        bool
	// Who can open the mock's one resource. It starts as what a create
	// request that states no privacy makes on the service the mock plays.
	private           bool
	allowedDeviceKeys []string
	// publicByDefault makes the mock a service from before private became
	// the default; see PlayPublicByDefault.
	publicByDefault bool
	// ignoreGrantEdits makes the mock a service from before single device
	// grants could be added or removed; see PlayNoSingleGrantEdits.
	ignoreGrantEdits bool
	// Access requests of the mock's one resource: whether people can ask,
	// who asked, and who was approved. noAccessRequests makes the mock a
	// service from before all of it; see PlayNoAccessRequests.
	accessRequests   bool
	pendingRequests  []accessRequestFixture
	approvedPeople   []approvedPersonFixture
	noAccessRequests bool
	// omitApprovedPeople leaves allowed_passkeys out of every resource row.
	omitApprovedPeople bool
	// failf reports a contract violation to the owning test. It is t.Errorf,
	// which is safe to call from a handler goroutine; this package's own
	// tests replace it to observe the report without failing themselves.
	failf func(format string, args ...any)
}

// DownloadPath is the mock's link-host route: SetShareQURL(srv.URL +
// DownloadPath) makes share answers point at the mock itself, so download
// tests never leave the process.
const DownloadPath = "/file"

// DefaultDownloadPayload is what the download route serves unless
// SetDownloadPayload overrides it. Fixed so goldens can pin byte counts.
const DefaultDownloadPayload = "qURL mock file payload\n"

// PortalPath is the mock's in-browser page route. A share answer of the
// form srv.URL + PortalPath + "#qv2t1.…" mimics a real fragment-credential
// link: the fragment stays client-side, so any plain HTTP GET of the link
// lands here and receives the page — never the content bytes. Tests use it
// to prove the CLI fetches granted content instead of this page.
const PortalPath = "/portal"

// InterstitialTitle is the page title the mock's PortalPath route serves,
// shared with the live sandbox journey so both tiers reject the same page.
//
// TODO(upstream-contract): this is the <title> of the real in-browser
// verification page qurl-service serves for fragment-credential links. If
// the platform retitles that page, update this marker in lockstep.
const InterstitialTitle = "qURL - Private Links That Expire"

// Field names and fixture values repeated across the mock's JSON payloads.
// Lifted to constants so the builders and the route handlers cannot drift.
const (
	// fieldStatus and fieldCRID are keys of the *resource* payloads this file
	// serves. builders.go's RFC7807 problem document has its own "status" (an
	// int echo of the HTTP status) and deliberately does not share this one.
	fieldStatus = "status"
	fieldCRID   = "crid"
	// fieldType is the `type` key of the resource and share payloads. The
	// two objects give the key different value spaces (url|tunnel for a
	// resource, the link type for a share), but it is one wire field name
	// and must not drift between them. builders.go's RFC7807 problem
	// document has its own "type" (a URI derived from code) and deliberately
	// does not share this one, same as "status" above.
	fieldType = "type"
	// fixtureCreatedAt is the mock's fixed resource created_at. The apps/cli
	// goldens pin it (mutating it reddens them), so it must not drift. It is
	// not shared with builders.go's tombstone closed_at, which no golden pins.
	fixtureCreatedAt = "2026-03-01T00:00:00Z"
	// authTypeAPIKey is the auth_type/kind *value* — distinct from the
	// "api_key" JSON field name that carries the key object.
	authTypeAPIKey = "api_key"
	// fieldCreatedAt and fieldPublisher are the resource metadata keys that
	// ride share answers and resource rows.
	fieldCreatedAt = "created_at"
	// fieldResourceCreatedAt is the share answer's key for the same date:
	// every other share field describes the minted link, so the resource's
	// creation date is named as such there.
	fieldResourceCreatedAt = "resource_created_at"
	fieldPublisher         = "publisher"
	// fieldPrivate and fieldAllowedDeviceKeys say who can open a resource, in
	// create requests and in resource rows.
	fieldPrivate           = "private"
	fieldAllowedDeviceKeys = "allowed_device_keys"
	// publisherPath is the owner's publisher profile route.
	publisherPath = "/v1/me/publisher"
)

// CodePrivacyMismatch is the problem code for a publish whose target is
// already published with the other privacy, and CodeDeviceKeysMismatch the
// one for a publish whose device list differs from the stored list.
//
// TODO(upstream-contract): mirrors the service's codes for those refusals,
// both HTTP 400. LegacyAccessSettingsDetail is what a service from before the
// codes answers instead, in the detail of its generic invalid-input problem,
// for a privacy difference and for a different device list alike.
const (
	CodePrivacyMismatch        = "privacy_mismatch"
	CodeDeviceKeysMismatch     = "device_keys_mismatch"
	LegacyAccessSettingsDetail = "existing resource access settings differ; update allowed_device_keys with PATCH or create a new resource for different privacy"
)

// DefaultPublisherName is the self-declared publisher name the mock owner
// starts with. The apps/cli goldens pin it.
const DefaultPublisherName = "Acme Docs"

// NewServer starts a mock with consistent happy-path handlers for publish,
// share, list, and delete. Close it via t.Cleanup automatically.
func NewServer(t *testing.T) *Server {
	t.Helper()
	return NewServerWithKey(t, GenerateResourceKey(t))
}

// NewServerWithKey starts the mock backed by a caller-supplied resource key;
// golden tests pass FixedResourceKey for deterministic identifiers.
func NewServerWithKey(t *testing.T, key *ResourceKey) *Server {
	t.Helper()
	foundExisting := false
	s := &Server{
		t:                    t,
		Key:                  key,
		scripts:              map[string][]http.HandlerFunc{},
		publishFoundExisting: &foundExisting,
		publisherName:        DefaultPublisherName,
		private:              true,
		failf:                t.Errorf,
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// Script queues handlers for one "METHOD /path" route. Each request to the
// route consumes one queued handler before default behavior resumes. A share
// request with no bearer credential consumes none: see Server.
func (s *Server) Script(method, path string, handlers ...http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := method + " " + path
	s.scripts[key] = append(s.scripts[key], handlers...)
}

// ScriptRepeat queues the same handler n times — e.g. a run of 429s that
// outlasts the client's retry budget.
func (s *Server) ScriptRepeat(method, path string, n int, handler http.HandlerFunc) {
	for range n {
		s.Script(method, path, handler)
	}
}

// SetShareCRID overrides the crid field of share responses — the
// wrong-key mode used to exercise fail-closed verification.
func (s *Server) SetShareCRID(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shareCRID = value
}

// SetPublishFoundExisting makes publish responses report the
// already-published case via meta.found_existing.
func (s *Server) SetPublishFoundExisting(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishFoundExisting = &v
}

// OmitPublishFoundExisting makes publish responses omit the optional
// meta.found_existing field, which means the creation provenance is unknown.
func (s *Server) OmitPublishFoundExisting() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishFoundExisting = nil
}

// SetPublishOmitCRID makes publish return a malformed success response so
// callers can prove the CLI rejects a result without its required CRID.
func (s *Server) SetPublishOmitCRID(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishOmitCRID = v
}

// PlayPublicByDefault makes the mock answer like a service from before
// private became the default: a create request that states no privacy makes a
// public resource, the resource the mock starts with is public, and a publish
// that conflicts with it is refused with the older invalid-input answer
// instead of the privacy-mismatch code. A request that states its privacy
// gets what it asked for on either service, which is why the CLI always
// states it.
func (s *Server) PlayPublicByDefault() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publicByDefault = true
	s.private = false
}

// PlayNoSingleGrantEdits makes the mock answer like a service from before
// single device grants could be added or removed: it ignores those two
// request members and answers a grant change that carries only them with a
// success status and the list as it was. Replacing the complete list still
// works.
func (s *Server) PlayNoSingleGrantEdits() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ignoreGrantEdits = true
}

// SetResourceAccess sets who can open the mock's one resource without a
// request: its privacy and its allowed device keys.
func (s *Server) SetResourceAccess(private bool, allowedDeviceKeys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.private = private
	s.allowedDeviceKeys = append([]string(nil), allowedDeviceKeys...)
}

// addResourceAccess writes the access fields every resource row carries:
// privacy always, and the allowed device keys when there are any.
//
// TODO(upstream-contract): the service writes private on every row it
// returns and omits an empty allowed_device_keys.
func (s *Server) addResourceAccess(row map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row[fieldPrivate] = s.private
	if len(s.allowedDeviceKeys) > 0 {
		row[fieldAllowedDeviceKeys] = append([]string(nil), s.allowedDeviceKeys...)
	}
	s.addAccessRequestFields(row)
}

// SetPublisherName sets the mock owner's publisher name without a request;
// empty means the owner set none, so answers omit the name.
func (s *Server) SetPublisherName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publisherName = name
}

// OmitPublisherMetadata makes the mock answer like a service that predates
// publisher metadata: share answers carry neither publisher nor
// resource_created_at,
// and resource rows carry no publisher.
func (s *Server) OmitPublisherMetadata() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.omitPublisher = true
}

// publisherObject returns the wire publisher object and whether this mock
// sends one at all. verified is always false: no publisher is verified.
//
// TODO(upstream-contract): mirrors the qurl-service Publisher object,
// {"name"?: string, "verified": boolean}, with name omitted when unset.
func (s *Server) publisherObject() (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.omitPublisher {
		return nil, false
	}
	publisher := map[string]any{"verified": false}
	if s.publisherName != "" {
		publisher["name"] = s.publisherName
	}
	return publisher, true
}

// SetShareQURL overrides the qurl field of share responses; download
// tests point it at the mock's own DownloadPath.
func (s *Server) SetShareQURL(u string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shareQURL = u
}

// SetDownloadPayload overrides the bytes the DownloadPath route serves —
// binary-cleanliness tests pass payloads full of NUL and CR bytes.
func (s *Server) SetDownloadPayload(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.downloadPayload = b
}

// Requests returns everything the server has seen, in order.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RecordedRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	// A share request with no credential is recorded like any other, so a
	// test can still see exactly what arrived, but it never reaches a script.
	// It falls through to handleShare, which writes its 401.
	unauthenticatedShare := isShareRequest(r) && bearerCredential(r) == ""
	body, readErr := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))

	s.mu.Lock()
	s.requests = append(s.requests, RecordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Header: r.Header.Clone(),
		Body:   body,
	})
	var scripted http.HandlerFunc
	key := r.Method + " " + r.URL.Path
	if queue := s.scripts[key]; len(queue) > 0 && !unauthenticatedShare {
		scripted = queue[0]
		s.scripts[key] = queue[1:]
	}
	failf := s.failf
	s.mu.Unlock()

	if readErr != nil {
		failf("apitest: read the %s %s request body: %v", r.Method, r.URL.Path, readErr)
	}
	if unauthenticatedShare {
		failf("apitest: %s %s arrived with no bearer credential; share and get must always send the device credential",
			r.Method, r.URL.Path)
	}
	if scripted != nil {
		scripted(w, r)
		return
	}
	s.defaultHandler(w, r)
}

// isShareRequest reports whether r addresses the CRID share operator. Only
// that route is held to the credential rule: another route whose path happens
// to end in /share is not a share request.
func isShareRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	_, ok := shareResourceID(r.URL.Path)
	return ok
}

// shareResourceID returns the {id} of a share route, which is
// /v1/resources/{id}/share or the same path without the version prefix. The
// id is exactly one non-empty path segment.
func shareResourceID(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/v1/resources/")
	if !ok {
		rest, ok = strings.CutPrefix(path, "/resources/")
	}
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, "/share")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// bearerCredential returns the bearer token r presented. It is empty when r
// has no Authorization header, a non-bearer one, or a bearer with no token.
func bearerCredential(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return token
}

// writeUnauthorized answers the way the platform does when a request carries
// no usable credential.
func (s *Server) writeUnauthorized(w http.ResponseWriter) {
	WriteProblem(s.t, w, http.StatusUnauthorized, "unauthorized", "Unauthorized", "Authentication required")
}

func (s *Server) defaultHandler(w http.ResponseWriter, r *http.Request) {
	if s.handleResourceRoute(w, r) {
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/resources":
		s.handlePublish(w, r)

	case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
		s.handleMe(w, r)

	case (r.Method == http.MethodGet || r.Method == http.MethodPatch) && r.URL.Path == publisherPath:
		s.handlePublisher(w, r)

	case isShareRequest(r):
		s.handleShare(w, r)

	case r.Method == http.MethodPatch && (r.URL.Path == "/v1/resources/"+s.Key.CRID || r.URL.Path == "/v1/resources/"+s.Key.ResourceID):
		s.handleDeviceGrants(w, r)

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/resources/"):
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && r.URL.Path == DownloadPath:
		s.handleDownload(w)

	case r.Method == http.MethodGet && r.URL.Path == PortalPath:
		s.handlePortalPage(w)

	default:
		WriteProblem(s.t, w, http.StatusNotFound, "not_found", "Not Found", "no such route in the mock qURL API")
	}
}

const resourceTypeURL = "url"

// handleResourceRoute serves the routes that are tried before the default
// table: the owner's resource reads and the access-request routes. It reports
// whether r was one of them. They come first because the delete of a resource
// in that table matches any path under /v1/resources/, and a denial or a
// removal must not be answered as one.
func (s *Server) handleResourceRoute(w http.ResponseWriter, r *http.Request) bool {
	return (r.Method == http.MethodGet && s.handleResourceRead(w, r)) || s.handleAccessRoute(w, r)
}

// handleResourceRead serves the owner's resource reads for the mock's one
// resource and reports whether the request was one of them.
func (s *Server) handleResourceRead(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/v1/resources":
		// A fully populated list row: the publish-time metadata (type,
		// description, tags) rides every real list row and the CLI projects
		// it into `-o json`, so the default row carries it or the goldens
		// would pin a shape no deployment serves. Like fixtureCreatedAt,
		// these values are pinned by the apps/cli goldens.
		WriteEnvelope(s.t, w, http.StatusOK, []map[string]any{s.resourceRow()}, map[string]any{"has_more": false})
	case "/v1/resources/" + s.Key.CRID + "/sharing":
		// The default resource is a URL, not a Connector, so it has no sharing
		// state. `qurl status` reads this answer, then the resource detail.
		WriteProblem(s.t, w, http.StatusBadRequest, "invalid_input", "Invalid Input", "Resource is not a qURL Connector")
	case "/v1/resources/" + s.Key.CRID:
		// The owner-facing detail read nests the same row under "resource".
		WriteEnvelope(s.t, w, http.StatusOK, map[string]any{"resource": s.resourceRow()}, nil)
	default:
		return false
	}
	return true
}

// resourceRow is the one fully populated resource row the list and detail
// reads share: the publish-time metadata and, unless the mock plays an older
// service, the owner's publisher.
func (s *Server) resourceRow() map[string]any {
	row := map[string]any{
		"resource_id":  s.Key.ResourceID,
		fieldCRID:      s.Key.CRID,
		"target_url":   "https://example.com/data",
		fieldType:      resourceTypeURL,
		fieldStatus:    "active",
		"description":  "example data drop",
		"tags":         []string{"demo", "fixture"},
		fieldCreatedAt: fixtureCreatedAt,
	}
	if publisher, ok := s.publisherObject(); ok {
		row[fieldPublisher] = publisher
	}
	s.addResourceAccess(row)
	return row
}

// handlePublish accepts URL creation and Connector find-or-create.
//
// A fresh create stores what was asked for; one that states no privacy gets
// the default of the service the mock plays: private, or public after
// PlayPublicByDefault. A create that finds the existing resource
// (SetPublishFoundExisting) changes nothing. It is refused when it states a
// privacy or a device list other than the resource's, and returns the
// resource as it is otherwise, as the service does. A different
// access-request setting is not a refusal: the answer carries the setting
// the resource has.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AllowedDeviceKeys []string `json:"allowed_device_keys"`
		Private           *bool    `json:"private"`
		AccessRequests    *bool    `json:"access_requests"`
		Slug              string   `json:"slug"`
		FindOrCreate      bool     `json:"find_or_create"`
		Type              string   `json:"type"`
		TargetURL         string   `json:"target_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteProblem(s.t, w, http.StatusBadRequest, "invalid_request", "Bad Request", "request body must be JSON")
		return
	}
	invalid := map[string]string{}
	if body.Type != resourceTypeURL && body.Type != "tunnel" {
		invalid["type"] = "must be url or tunnel"
	}
	if body.Type == resourceTypeURL && body.TargetURL == "" {
		invalid["target_url"] = "is required"
	}
	if body.Type == "tunnel" && (body.Slug == "" || !body.FindOrCreate) {
		invalid["slug"] = "tunnel creation requires slug and find_or_create"
	}
	if len(invalid) > 0 {
		WriteProblemExtra(s.t, w, http.StatusBadRequest, "invalid_request", "Bad Request",
			"the request had invalid fields", invalid, nil)
		return
	}
	meta := map[string]any{}
	s.mu.Lock()
	refusal := s.applyCreateAccess(body.Private, body.AllowedDeviceKeys)
	requestsRefusal := ""
	if refusal == "" {
		existing := s.publishFoundExisting != nil && *s.publishFoundExisting
		if !existing {
			// A resource that was just made starts with access requests off.
			s.accessRequests = false
		}
		requestsRefusal = s.applyAccessRequestsSetting(body.AccessRequests, existing)
	}
	if s.publishFoundExisting != nil {
		meta["found_existing"] = *s.publishFoundExisting
	}
	omitCRID := s.publishOmitCRID
	s.mu.Unlock()
	if requestsRefusal != "" {
		WriteProblem(s.t, w, http.StatusBadRequest, codeInvalidInput, titleInvalidInput, requestsRefusal)
		return
	}
	if refusal != "" {
		s.writeCreateRefusal(w, refusal)
		return
	}
	data := map[string]any{
		"resource_id":  s.Key.ResourceID,
		fieldCRID:      s.Key.CRID,
		"target_url":   body.TargetURL,
		fieldStatus:    "active",
		fieldCreatedAt: fixtureCreatedAt,
	}
	if publisher, ok := s.publisherObject(); ok {
		data[fieldPublisher] = publisher
	}
	s.addResourceAccess(data)
	if omitCRID {
		delete(data, fieldCRID)
	}
	WriteEnvelope(s.t, w, http.StatusCreated, data, meta)
}

// writeCreateRefusal answers a create that applyCreateAccess refused. Each of
// the service's two codes has its own problem; any other refusal is the
// generic invalid-input problem an older service sends for both.
func (s *Server) writeCreateRefusal(w http.ResponseWriter, refusal string) {
	switch refusal {
	case CodePrivacyMismatch:
		WriteProblem(s.t, w, http.StatusBadRequest, CodePrivacyMismatch, "Privacy Mismatch",
			"this target is already published with the other privacy")
	case CodeDeviceKeysMismatch:
		WriteProblem(s.t, w, http.StatusBadRequest, CodeDeviceKeysMismatch, "Device Keys Mismatch",
			"this target is already published with another list of allowed devices")
	default:
		WriteProblem(s.t, w, http.StatusBadRequest, refusal, titleInvalidInput, LegacyAccessSettingsDetail)
	}
}

// applyCreateAccess decides what a create request does to the access
// settings of the mock's one resource, and returns the problem code of the
// refusal, empty when the request is accepted. The caller holds s.mu.
//
// A fresh create stores the privacy that was stated, or the default of the
// service the mock plays, and the device list. A create that finds the
// existing resource changes nothing.
//
// The service reuses that resource with the privacy it has when the request
// states none, refuses a stated privacy that differs with the
// privacy-mismatch code, and refuses a stated device list that differs with
// the device-list code. A service from before those codes reads an absent
// privacy as its default, public, and refuses another privacy or another
// device list with its one invalid-input answer.
func (s *Server) applyCreateAccess(stated *bool, allowedDeviceKeys []string) string {
	if s.publishFoundExisting == nil || !*s.publishFoundExisting {
		s.private = !s.publicByDefault
		if stated != nil {
			s.private = *stated
		}
		s.allowedDeviceKeys = append([]string(nil), allowedDeviceKeys...)
		return ""
	}
	otherKeys := allowedDeviceKeys != nil && !sameKeys(allowedDeviceKeys, s.allowedDeviceKeys)
	if s.publicByDefault {
		private := stated != nil && *stated
		if private != s.private || otherKeys {
			return codeInvalidInput
		}
		return ""
	}
	if stated != nil && *stated != s.private {
		return CodePrivacyMismatch
	}
	if otherKeys {
		return CodeDeviceKeysMismatch
	}
	return ""
}

// maxAllowedDeviceKeys is the most device keys one resource can list.
const maxAllowedDeviceKeys = 256

// handleDeviceGrants serves the owner's change to the device grant list of
// the mock's one resource: PATCH /v1/resources/{id} with allowed_device_keys
// to replace the complete list, or with allowed_device_keys_add and
// allowed_device_keys_remove to change single keys. It answers with the flat
// resource row, which carries the complete resulting list.
//
// TODO(upstream-contract): mirrors the service's grant update. Both edit
// members are applied as one change; a key that is already present or already
// absent is nothing to do; a key in both members and a result above 256 keys
// are refused with 400 invalid_input; and the answer is the whole row.
func (s *Server) handleDeviceGrants(w http.ResponseWriter, r *http.Request) {
	if bearerCredential(r) == "" {
		s.writeUnauthorized(w)
		return
	}
	var body struct {
		Replace        *[]string `json:"allowed_device_keys"`
		Add            []string  `json:"allowed_device_keys_add"`
		Remove         []string  `json:"allowed_device_keys_remove"`
		AccessRequests *bool     `json:"access_requests"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteProblem(s.t, w, http.StatusBadRequest, "invalid_request", "Bad Request", "request body must be JSON")
		return
	}
	s.mu.Lock()
	refusal := s.applyGrantChange(body.Replace, body.Add, body.Remove)
	if refusal == "" {
		refusal = s.applyAccessRequestsSetting(body.AccessRequests, false)
	}
	s.mu.Unlock()
	if refusal != "" {
		WriteProblem(s.t, w, http.StatusBadRequest, codeInvalidInput, titleInvalidInput, refusal)
		return
	}
	WriteEnvelope(s.t, w, http.StatusOK, s.resourceRow(), nil)
}

// applyGrantChange applies one grant change to the mock's device list and
// returns the detail of the refusal, empty when the change is accepted. A
// refused change leaves the list as it was. The caller holds s.mu.
func (s *Server) applyGrantChange(replace *[]string, add, remove []string) string {
	if s.ignoreGrantEdits {
		add, remove = nil, nil
	}
	next := s.allowedDeviceKeys
	switch {
	case replace != nil && len(add)+len(remove) > 0:
		return "replace the complete device list or change single keys, not both"
	case replace != nil:
		next = *replace
	default:
		for _, key := range add {
			if slices.Contains(remove, key) {
				return "a device key cannot be both added and removed"
			}
		}
		next = slices.DeleteFunc(slices.Clone(next), func(key string) bool { return slices.Contains(remove, key) })
		for _, key := range add {
			if !slices.Contains(next, key) {
				next = append(next, key)
			}
		}
	}
	if len(next) > maxAllowedDeviceKeys {
		return "at most 256 allowed device keys"
	}
	s.allowedDeviceKeys = slices.Clone(next)
	return ""
}

// sameKeys reports whether two device key lists hold the same keys in any
// order, which is how the service compares them.
func sameKeys(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// Fixed identity fixtures for the default GET /v1/me answer, stable so
// goldens can pin whoami/login renderings.
const (
	MeOwnerID = "own_cli_fixture"
	MeKeyID   = "key_fixturecli01"
)

// handleMe answers the identity echo the way the platform does: entirely from
// the presented credential. key_prefix mirrors the first 12 characters of the
// bearer, as the platform still sends it. The CLI deliberately drops it, since
// it is a slice of the secret; keeping it here is what lets the whoami goldens
// prove it never reaches output. Scopes come back alphabetical (a platform
// contract), and expires_at is omitted — the default fixture is a non-expiring
// key.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	bearer := bearerCredential(r)
	if bearer == "" {
		s.writeUnauthorized(w)
		return
	}
	apiKey := map[string]any{
		"key_id": MeKeyID,
		"kind":   authTypeAPIKey,
		"scopes": []string{"qurl:read", "qurl:resolve", "qurl:write"},
	}
	if len(bearer) >= 12 {
		apiKey["key_prefix"] = bearer[:12]
	}
	WriteEnvelope(s.t, w, http.StatusOK, map[string]any{
		"owner_id":  MeOwnerID,
		"auth_type": authTypeAPIKey,
		"api_key":   apiKey,
	}, nil)
}

// handleShare serves the CRID share operator (POST /v1/resources/{id}/share).
// It refuses a request with no bearer credential, then enforces the pinned
// bind rule — the body must be JSON (`{}` at minimum); a literal empty body
// is a 400 — and answers a consistent minted share link.
//
// TODO(upstream-contract): qurl-service authenticates the share route before
// it looks at the resource, so a request with no credential is answered 401
// `unauthorized` whatever it names. Any credential is the owner here: tests
// script the 404 a caller who is neither owner nor allowed receives.
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	// This is the 401 writer for the guard in handle: handle reports the
	// request and keeps it from a script, then falls through to here for
	// the answer. The two checks are one rule and must change together.
	if bearerCredential(r) == "" {
		s.writeUnauthorized(w)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil || len(raw) == 0 || !json.Valid(raw) {
		WriteProblem(s.t, w, http.StatusBadRequest, "invalid_request", "Bad Request",
			"request body must be a JSON object")
		return
	}
	// defaultHandler routes here only for a path isShareRequest matched.
	id, _ := shareResourceID(r.URL.Path)
	s.mu.Lock()
	qurlLink := s.shareQURL
	s.mu.Unlock()
	if qurlLink == "" {
		qurlLink = "https://qurl.link/#qv2t1.1.1.1.AQ.AQ.AQ"
	}
	data := map[string]any{
		"qurl":               qurlLink,
		fieldCRID:            s.cridFor(id),
		fieldType:            "qv2",
		"expires_at":         "2026-03-01T00:05:00Z",
		"expires_in_seconds": 300,
		"single_use":         true,
	}
	// The resource's creation date and publisher ride the share answer itself;
	// there is no second, anonymous metadata request to serve.
	if publisher, ok := s.publisherObject(); ok {
		data[fieldResourceCreatedAt] = fixtureCreatedAt
		data[fieldPublisher] = publisher
	}
	WriteEnvelope(s.t, w, http.StatusOK, data, nil)
}

// handlePublisher serves the owner's publisher profile: GET reads it and
// PATCH {"name": "..."} sets the name, an empty name removing it. It enforces
// the parts of the contract a client can get wrong: the body is exactly one
// name member (so a request can never carry verified), and a name is refused
// with 400 invalid_input and a reason.
//
// TODO(upstream-contract): mirrors qurl-service GET and PATCH
// /v1/me/publisher. Only the two naming rules the CLI tests exercise are
// modeled; the service owns the full rule set.
func (s *Server) handlePublisher(w http.ResponseWriter, r *http.Request) {
	if bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); bearer == "" || bearer == r.Header.Get("Authorization") {
		WriteProblem(s.t, w, http.StatusUnauthorized, "unauthorized", "Unauthorized", "Authentication required")
		return
	}
	if r.Method == http.MethodPatch {
		var body struct {
			Name *string `json:"name"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || body.Name == nil {
			WriteProblem(s.t, w, http.StatusBadRequest, "invalid_input", "Bad Request",
				"the request body must be exactly one name member")
			return
		}
		name := *body.Name
		switch {
		case utf8.RuneCountInString(name) > 64:
			WriteProblem(s.t, w, http.StatusBadRequest, "invalid_input", "Bad Request",
				"name must be at most 64 characters")
			return
		case strings.Contains(strings.ToLower(name), "verified"):
			WriteProblem(s.t, w, http.StatusBadRequest, "invalid_input", "Bad Request",
				"name must not contain the word verified")
			return
		}
		s.SetPublisherName(name)
	}
	publisher, ok := s.publisherObject()
	if !ok {
		WriteProblem(s.t, w, http.StatusNotFound, "not_found", "Not Found", "no such route in the mock qURL API")
		return
	}
	WriteEnvelope(s.t, w, http.StatusOK, publisher, nil)
}

// handleDownload serves the link-host bytes for DownloadPath.
func (s *Server) handleDownload(w http.ResponseWriter) {
	s.mu.Lock()
	payload := s.downloadPayload
	s.mu.Unlock()
	if payload == nil {
		payload = []byte(DefaultDownloadPayload)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := w.Write(payload); err != nil {
		s.t.Errorf("write download payload: %v", err)
	}
}

// handlePortalPage serves the stand-in for the platform's in-browser
// verification page: an HTML document, never content bytes. Any download
// that lands here fetched the link instead of the granted content — the
// exact defect the PortalPath tests exist to catch.
func (s *Server) handlePortalPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := "<!doctype html><html><head><title>" + InterstitialTitle +
		"</title></head><body>This page needs a browser to open the link.</body></html>"
	if _, err := io.WriteString(w, page); err != nil {
		s.t.Errorf("write portal page: %v", err)
	}
}

// cridFor answers the crid field for a share: the override when scripted,
// the requested CRID echoed back when the caller shared by CRID, or the
// CRID derived from the requested key when the caller shared by resource
// id — i.e. a consistent server by default.
func (s *Server) cridFor(requestedID string) string {
	s.mu.Lock()
	override := s.shareCRID
	s.mu.Unlock()
	if override != "" {
		return override
	}
	if crid.MatchesShape(requestedID) {
		return requestedID
	}
	if requestedID == s.Key.ResourceID {
		return s.Key.CRID
	}
	return s.Key.CRID
}
