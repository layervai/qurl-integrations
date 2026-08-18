package apitest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/layervai/qurl-go/crid"
)

// RecordedRequest captures one request for header/shape assertions.
type RecordedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
}

// Server is the scriptable mock qURL API.
type Server struct {
	*httptest.Server
	t *testing.T

	// Key backs the default happy-path responses; DER, resource id, and
	// CRID are mutually consistent, so default resolves verify cleanly.
	Key *ResourceKey

	mu                   sync.Mutex
	requests             []RecordedRequest
	scripts              map[string][]http.HandlerFunc
	resolveCRID          string
	resolveQURL          string
	downloadPayload      []byte
	publishFoundExisting bool
	publishOmitCRID      bool
}

// DownloadPath is the mock's link-host route: SetResolveQURL(srv.URL +
// DownloadPath) makes resolve answers point at the mock itself, so download
// tests never leave the process.
const DownloadPath = "/file"

// DefaultDownloadPayload is what the download route serves unless
// SetDownloadPayload overrides it. Fixed so goldens can pin byte counts.
const DefaultDownloadPayload = "qURL mock file payload\n"

// Field names and fixture values repeated across the mock's JSON payloads.
// Lifted to constants so the builders and the route handlers cannot drift.
const (
	fieldStatus = "status"
	fieldCRID   = "crid"
	// fixtureCreatedAt is the mock's fixed created_at/closed_at stamp; goldens
	// pin it, so it must not vary between payloads.
	fixtureCreatedAt = "2026-03-01T00:00:00Z"
	// authTypeAPIKey is the auth_type/kind *value* — distinct from the
	// "api_key" JSON field name that carries the key object.
	authTypeAPIKey = "api_key"
)

// NewServer starts a mock with consistent happy-path handlers for publish,
// resolve, list, and delete. Close it via t.Cleanup automatically.
func NewServer(t *testing.T) *Server {
	t.Helper()
	return NewServerWithKey(t, GenerateResourceKey(t))
}

// NewServerWithKey starts the mock backed by a caller-supplied resource key;
// golden tests pass FixedResourceKey for deterministic identifiers.
func NewServerWithKey(t *testing.T, key *ResourceKey) *Server {
	t.Helper()
	s := &Server{
		t:       t,
		Key:     key,
		scripts: map[string][]http.HandlerFunc{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// Script queues handlers for one "METHOD /path" route. Each request to the
// route consumes one queued handler before default behavior resumes.
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

// SetResolveCRID overrides the crid field of resolve responses — the
// wrong-key mode used to exercise fail-closed verification.
func (s *Server) SetResolveCRID(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveCRID = value
}

// SetPublishFoundExisting makes publish responses report the
// already-published case via meta.found_existing.
func (s *Server) SetPublishFoundExisting(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishFoundExisting = v
}

// SetPublishOmitCRID makes publish answer without a crid, the shape an
// older deployment returns before it mints CRIDs. The CLI warns and falls
// back to the resource id in that case.
func (s *Server) SetPublishOmitCRID(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishOmitCRID = v
}

// SetResolveQURL overrides the qurl field of resolve responses; download
// tests point it at the mock's own DownloadPath.
func (s *Server) SetResolveQURL(u string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveQURL = u
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
	s.mu.Lock()
	s.requests = append(s.requests, RecordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Header: r.Header.Clone(),
	})
	var scripted http.HandlerFunc
	key := r.Method + " " + r.URL.Path
	if queue := s.scripts[key]; len(queue) > 0 {
		scripted = queue[0]
		s.scripts[key] = queue[1:]
	}
	s.mu.Unlock()

	if scripted != nil {
		scripted(w, r)
		return
	}
	s.defaultHandler(w, r)
}

func (s *Server) defaultHandler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/resources":
		s.handlePublish(w, r)

	case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
		s.handleMe(w, r)

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resolve"):
		s.handleResolve(w, r)

	case r.Method == http.MethodGet && r.URL.Path == "/v1/resources":
		WriteEnvelope(s.t, w, http.StatusOK, []map[string]any{{
			"resource_id": s.Key.ResourceID,
			fieldCRID:     s.Key.CRID,
			"target_url":  "https://example.com/data",
			fieldStatus:   "active",
			"created_at":  fixtureCreatedAt,
		}}, map[string]any{"has_more": false})

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/resources/"):
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && r.URL.Path == DownloadPath:
		s.handleDownload(w)

	default:
		WriteProblem(s.t, w, http.StatusNotFound, "not_found", "Not Found", "no such route in the mock qURL API")
	}
}

// handlePublish enforces the pinned publish contract: type=url and
// target_url are required; violations are the 400 validation shape.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type      string `json:"type"`
		TargetURL string `json:"target_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteProblem(s.t, w, http.StatusBadRequest, "invalid_request", "Bad Request", "request body must be JSON")
		return
	}
	invalid := map[string]string{}
	if body.Type != "url" {
		invalid["type"] = "must be url"
	}
	if body.TargetURL == "" {
		invalid["target_url"] = "is required"
	}
	if len(invalid) > 0 {
		WriteProblemExtra(s.t, w, http.StatusBadRequest, "invalid_request", "Bad Request",
			"the request had invalid fields", invalid, nil)
		return
	}
	meta := map[string]any{}
	s.mu.Lock()
	if s.publishFoundExisting {
		meta["found_existing"] = true
	}
	omitCRID := s.publishOmitCRID
	s.mu.Unlock()
	data := map[string]any{
		"resource_id": s.Key.ResourceID,
		fieldCRID:     s.Key.CRID,
		"target_url":  body.TargetURL,
		fieldStatus:   "active",
		"created_at":  fixtureCreatedAt,
	}
	if omitCRID {
		delete(data, fieldCRID)
	}
	WriteEnvelope(s.t, w, http.StatusCreated, data, meta)
}

// Fixed identity fixtures for the default GET /v1/me answer, stable so
// goldens can pin whoami/login renderings.
const (
	MeOwnerID = "own_cli_fixture"
	MeKeyID   = "key_fixturecli01"
)

// handleMe answers the identity echo the way the platform does: entirely from
// the presented credential. key_prefix mirrors the first 12 characters of the
// bearer the client actually sent, so contract tests see exactly which key
// authenticated. Scopes come back alphabetical (a platform contract), and
// expires_at is omitted — the default fixture is a non-expiring key.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if bearer == "" || bearer == r.Header.Get("Authorization") {
		WriteProblem(s.t, w, http.StatusUnauthorized, "unauthorized", "Unauthorized", "Authentication required")
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

// handleResolve enforces the pinned resolve bind rule — the body must be
// JSON (`{}` at minimum); a literal empty body is a 400 — then answers a
// consistent minted link.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil || len(raw) == 0 || !json.Valid(raw) {
		WriteProblem(s.t, w, http.StatusBadRequest, "invalid_request", "Bad Request",
			"request body must be a JSON object")
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/resources/"), "/resolve")
	s.mu.Lock()
	qurlLink := s.resolveQURL
	s.mu.Unlock()
	if qurlLink == "" {
		qurlLink = "https://qurl.link/#qv2.test.link"
	}
	WriteEnvelope(s.t, w, http.StatusOK, map[string]any{
		"qurl":               qurlLink,
		fieldCRID:            s.cridFor(id),
		"type":               "qv2",
		"expires_at":         "2026-03-01T00:05:00Z",
		"expires_in_seconds": 300,
		"single_use":         true,
	}, nil)
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
	// #nosec G705 -- the mock link host echoes the test's own payload bytes
	// as an octet-stream download; no browser or HTML context exists here.
	if _, err := w.Write(payload); err != nil {
		s.t.Errorf("write download payload: %v", err)
	}
}

// cridFor answers the crid field for a resolve: the override when scripted,
// the requested CRID echoed back when the caller resolved by CRID, or the
// CRID derived from the requested key when the caller resolved by resource
// id — i.e. a consistent server by default.
func (s *Server) cridFor(requestedID string) string {
	s.mu.Lock()
	override := s.resolveCRID
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
