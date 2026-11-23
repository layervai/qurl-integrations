package apitest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// CodeOriginHeaderMismatch is the code of a create refused because the
// target is already published with another origin header, or with none.
const CodeOriginHeaderMismatch = "origin_header_mismatch"

// originHeaderFixture is the mock resource's origin header. The value is
// stored only so a test can check what arrived; no answer carries it.
type originHeaderFixture struct {
	name  string
	value string
}

type originHeaderBody struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SetOriginHeader sets the mock resource's origin header without a request;
// an empty name removes it.
func (s *Server) SetOriginHeader(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		s.originHeader = nil
		return
	}
	s.originHeader = &originHeaderFixture{name: http.CanonicalHeaderKey(name), value: value}
}

// OriginHeader returns the mock resource's origin header: its canonical
// name and its value, both empty for none.
func (s *Server) OriginHeader() (name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.originHeader == nil {
		return "", ""
	}
	return s.originHeader.name, s.originHeader.value
}

// applyCreateOriginHeader decides what a create does to the origin header and
// returns the refusal code, empty when accepted. The caller holds s.mu.
//
// TODO(upstream-contract): mirrors the qURL API. A fresh create stores the
// header it states. A create that reuses the existing resource changes
// nothing: without origin_header it keeps the header the resource has, and
// with one that differs from it, or with one for a resource that has none,
// it is refused with origin_header_mismatch.
func (s *Server) applyCreateOriginHeader(header *originHeaderBody, existing bool) string {
	if !existing {
		s.originHeader = nil
		if header != nil {
			s.originHeader = &originHeaderFixture{name: http.CanonicalHeaderKey(header.Name), value: header.Value}
		}
		return ""
	}
	if header == nil {
		return ""
	}
	if s.originHeader == nil || !strings.EqualFold(s.originHeader.name, header.Name) || s.originHeader.value != header.Value {
		return CodeOriginHeaderMismatch
	}
	return ""
}

// applyCreateAccessAndOriginHeader applies a create's access settings, then
// its origin header, and returns the first refusal. The caller holds s.mu.
func (s *Server) applyCreateAccessAndOriginHeader(stated *bool, allowedDeviceKeys []string, header *originHeaderBody) string {
	if refusal := s.applyCreateAccess(stated, allowedDeviceKeys); refusal != "" {
		return refusal
	}
	return s.applyCreateOriginHeader(header, s.publishFoundExisting != nil && *s.publishFoundExisting)
}

// addOriginHeaderName writes origin_header_name on a row when the resource
// has one, as the service does. The caller holds s.mu.
func (s *Server) addOriginHeaderName(row map[string]any) {
	if s.originHeader != nil {
		row["origin_header_name"] = s.originHeader.name
	}
}

// isOriginHeaderPatch reports whether r is a PATCH whose body has an
// origin_header member, an object or null, and leaves the body readable.
// Such a PATCH is served as an origin header change only: the CLI never
// sends one together with a grant change, so the mock does not combine them.
func isOriginHeaderPatch(r *http.Request) bool {
	body, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return false
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(body, &members) != nil {
		return false
	}
	_, ok := members["origin_header"]
	return ok
}

// handleOriginHeaderPatch serves PATCH /v1/resources/{id} with origin_header:
// an object sets or rotates the header, null removes it, and the answer is
// the flat resource row, which names the header and never carries its value.
func (s *Server) handleOriginHeaderPatch(w http.ResponseWriter, r *http.Request) {
	if bearerCredential(r) == "" {
		s.writeUnauthorized(w)
		return
	}
	var body struct {
		OriginHeader *originHeaderBody `json:"origin_header"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteProblem(s.t, w, http.StatusBadRequest, "invalid_request", "Bad Request", "request body must be JSON")
		return
	}
	s.mu.Lock()
	s.originHeader = nil
	if body.OriginHeader != nil {
		s.originHeader = &originHeaderFixture{name: http.CanonicalHeaderKey(body.OriginHeader.Name), value: body.OriginHeader.Value}
	}
	s.mu.Unlock()
	WriteEnvelope(s.t, w, http.StatusOK, s.resourceRow(), nil)
}
