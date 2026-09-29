package qurlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/layervai/qurl-go/qurl"
)

// MaxRequestBody caps a supervisor request body at the response cap. The
// README's request contract documents both caps.
const MaxRequestBody = maxResponseBody

// RequestResponse preserves HTTP failures for supervising apps without exposing
// request metadata or credential-bearing response headers.
type RequestResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

// Request uses only the registered SDK transport's existing route allowlist.
// It performs one attempt; the supervisor owns any retry decision.
// TODO(upstream-contract): requestRouteAllowed mirrors the pinned qurl-go
// registered-device route allowlist, and ValidateRequestTarget also enforces
// the method set and query policy locally, so a looser SDK cannot widen this
// command's authority. Widening it is an explicit change in both repos.
func Request(ctx context.Context, api Client, method, relativePath string, body json.RawMessage, idempotencyKey string) (*RequestResponse, error) {
	registered, ok := api.(*registeredClient)
	if !ok {
		return nil, fmt.Errorf("%w: request requires a registered device", qurl.ErrInvalidClientConfig)
	}
	c, ok := registered.Client.(*client)
	if !ok || c.registeredDoer == nil {
		return nil, fmt.Errorf("%w: registered transport is unavailable", qurl.ErrInvalidClientConfig)
	}
	if err := ValidateRequestTarget(method, relativePath); err != nil {
		return nil, err
	}
	if err := ValidateRequestIdempotencyKey(idempotencyKey); err != nil {
		return nil, err
	}
	if idempotencyKey != "" && method == http.MethodGet {
		return nil, fmt.Errorf("%w: idempotency keys apply only to mutations", qurl.ErrInvalidResourceRequest)
	}
	if len(body) > MaxRequestBody || (len(body) > 0 && !json.Valid(body)) {
		return nil, fmt.Errorf("%w: request body must be JSON of at most %d MiB", qurl.ErrInvalidResourceRequest, MaxRequestBody>>20)
	}
	if (method == http.MethodGet || method == http.MethodDelete) && len(body) > 0 {
		return nil, fmt.Errorf("%w: GET and DELETE requests must not include a body", qurl.ErrInvalidResourceRequest)
	}
	headers := make(http.Header)
	// TODO(upstream-contract): the header protects a retried mutation only on
	// routes where qurl-service implements idempotency; the help says so.
	if idempotencyKey != "" {
		headers.Set("Idempotency-Key", idempotencyKey)
	}
	reply, err := c.doRESTRequest(ctx, method, relativePath, body, headers, false)
	if err != nil {
		return nil, err
	}
	result := &RequestResponse{Status: reply.status, Headers: map[string]string{}, Body: json.RawMessage("null")}
	for _, name := range []string{"content-type", "retry-after", "x-request-id"} {
		if value := reply.header.Get(name); value != "" {
			result.Headers[name] = value
		}
	}
	if len(reply.body) > 0 {
		result.Body = requestResponseBody(reply.body)
	}
	return result, nil
}

// requestResponseBody passes JSON through and encodes anything else as a
// string. json.Valid accepts invalid UTF-8 inside strings; emitting it raw
// would make the whole envelope unparseable, so it takes the string path.
func requestResponseBody(body []byte) json.RawMessage {
	if json.Valid(body) && utf8.Valid(body) {
		return body
	}
	return jsonStringNoHTMLEscape(string(body))
}

// ValidateRequestTarget rejects unsupported methods, URL authority, ambiguous
// encoding, dot segments, and queries outside GET /v1/resources and
// GET /v1/resources/{id}/qurls before a supervisor request can open or enroll
// a device.
func ValidateRequestTarget(method, value string) error {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return fmt.Errorf("%w: request method must be GET, POST, PUT, PATCH, or DELETE", qurl.ErrRegisteredAgentResourceRequestDenied)
	}
	parsed, ok := canonicalRequestPath(value)
	if !ok {
		return fmt.Errorf("%w: request path must be a canonical /v1/ path", qurl.ErrRegisteredAgentResourceRequestDenied)
	}
	pattern, known, validIDs := requestRoutePattern(parsed.Path)
	if !known || !slices.Contains(requestRoutes[pattern], method) {
		return fmt.Errorf("%w: %s %s is not a registered-device route", qurl.ErrRegisteredAgentResourceRequestDenied, method, parsed.Path)
	}
	if !validIDs {
		return fmt.Errorf("%w: request path identifiers must be letters, digits, hyphens or underscores", qurl.ErrRegisteredAgentResourceRequestDenied)
	}
	// Query presence is gated by route; its characters must already be
	// URL-safe, and the service validates the parameters themselves.
	if (parsed.RawQuery != "" || parsed.ForceQuery) && (method != http.MethodGet || (pattern != requestResourcesPath && pattern != requestQurlsPattern)) {
		return fmt.Errorf("%w: queries are allowed only for GET /v1/resources and GET /v1/resources/{id}/qurls", qurl.ErrRegisteredAgentResourceRequestDenied)
	}
	if strings.ContainsFunc(parsed.RawQuery, func(r rune) bool { return r <= ' ' || r >= 0x7f || strings.ContainsRune(`"<>\^`+"`"+`{|}`, r) }) {
		return fmt.Errorf("%w: request query must be URL-encoded", qurl.ErrRegisteredAgentResourceRequestDenied)
	}
	return nil
}

// canonicalRequestPath parses a relative /v1/ path, refusing authority,
// userinfo, fragments, ambiguous percent-encoding, and dot segments.
func canonicalRequestPath(value string) (*url.URL, bool) {
	parsed, err := url.Parse(value)
	if err != nil || !strings.HasPrefix(value, "/v1/") || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" || parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(value, "#") || parsed.RawPath != "" || path.Clean(parsed.Path) != parsed.Path {
		return nil, false
	}
	return parsed, true
}

const (
	requestResourcesPath = "/v1/resources"
	requestQurlsPattern  = "/v1/resources/{id}/qurls"
)

// requestRoutes mirrors qurl-go's registeredAgentResourceRouteAllowed, minus
// POST /v1/api-keys: a supervisor must never mint a portable credential that
// outlives the sealed namespace. Keys are route patterns, with {id} for a
// resource, qURL, or session identifier. The CLI README lists these routes for
// supervisor authors; update it with any change here.
var requestRoutes = map[string][]string{
	"/v1/account/link":                   {http.MethodPost},
	"/v1/qurls":                          {http.MethodPost},
	"/v1/me":                             {http.MethodGet},
	requestResourcesPath:                 {http.MethodGet, http.MethodPost},
	"/v1/resources/{id}":                 {http.MethodGet, http.MethodPatch, http.MethodDelete},
	"/v1/resources/{id}/sharing":         {http.MethodGet, http.MethodPut},
	"/v1/resources/{id}/sharing/restart": {http.MethodPost},
	"/v1/resources/{id}/share":           {http.MethodPost},
	requestQurlsPattern:                  {http.MethodGet, http.MethodPost},
	"/v1/resources/{id}/qurls/{id}":      {http.MethodPatch, http.MethodDelete},
	"/v1/resources/{id}/sessions":        {http.MethodGet, http.MethodDelete},
	"/v1/resources/{id}/sessions/{id}":   {http.MethodDelete},
}

// requestRoutePattern maps a canonical path to its requestRoutes pattern.
// Resource IDs (unpadded base64url) and CRIDs (lowercase alphanumerics) are
// both ASCII tokens; validIDs is false when an identifier segment is not, so
// a literal {id} can match a pattern only to be refused.
func requestRoutePattern(requestPath string) (pattern string, ok, validIDs bool) {
	parts := strings.Split(requestPath, "/")
	validIDs = true
	if len(parts) >= 4 && parts[2] == "resources" {
		validIDs = isASCIIToken(parts[3])
		parts[3] = "{id}"
		if len(parts) == 6 && (parts[4] == "qurls" || parts[4] == "sessions") {
			validIDs = validIDs && isASCIIToken(parts[5])
			parts[5] = "{id}"
		}
	}
	pattern = strings.Join(parts, "/")
	_, ok = requestRoutes[pattern]
	return pattern, ok, validIDs
}

// isASCIIToken accepts nonempty letters, digits, hyphens and underscores. For
// route identifiers it also keeps a literal {id} from matching a pattern.
func isASCIIToken(value string) bool {
	return value != "" && strings.IndexFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
	}) < 0
}

// The request idempotency-key bounds are a published supervisor contract
// (flag help, README, man page), independent of the enrollment bounds.
const (
	minRequestIdempotencyKey = 32
	maxRequestIdempotencyKey = 256
)

// ValidateRequestIdempotencyKey accepts only bounded, nonsecret ASCII tokens.
// It is deliberately stricter than validateEnrollmentIdempotencyKey because a
// supervisor, not this CLI, chooses the value; do not unify the two.
func ValidateRequestIdempotencyKey(value string) error {
	if value == "" {
		return nil
	}
	if len(value) < minRequestIdempotencyKey || len(value) > maxRequestIdempotencyKey || !isASCIIToken(value) {
		return fmt.Errorf("%w: idempotency key must be %d-%d letters, digits, hyphens or underscores", qurl.ErrInvalidResourceRequest, minRequestIdempotencyKey, maxRequestIdempotencyKey)
	}
	return nil
}

// jsonStringNoHTMLEscape encodes s as a JSON string without HTML escaping, to
// match how the envelope printer emits JSON bodies. Encoding a string cannot
// fail; invalid UTF-8 becomes U+FFFD.
func jsonStringNoHTMLEscape(s string) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
