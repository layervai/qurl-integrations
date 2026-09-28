package qurlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

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
	if len(body) > MaxRequestBody || (len(body) > 0 && !json.Valid(body)) {
		return nil, fmt.Errorf("%w: request body must be JSON of at most 1 MiB", qurl.ErrInvalidResourceRequest)
	}
	if (method == http.MethodGet || method == http.MethodDelete) && len(body) > 0 {
		return nil, fmt.Errorf("%w: GET and DELETE requests must not include a body", qurl.ErrInvalidResourceRequest)
	}
	var requestBody any
	if len(body) > 0 {
		requestBody = body
	}
	if err := ValidateRequestIdempotencyKey(idempotencyKey); err != nil {
		return nil, err
	}
	headers := make(http.Header)
	if idempotencyKey != "" {
		headers.Set("Idempotency-Key", idempotencyKey)
	}
	reply, err := c.doRESTRequest(ctx, method, relativePath, requestBody, headers, false)
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
		if json.Valid(reply.body) {
			result.Body = reply.body
		} else {
			result.Body, _ = json.Marshal(string(reply.body))
		}
	}
	return result, nil
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
	parsed, err := url.Parse(value)
	if err != nil || !strings.HasPrefix(value, "/v1/") || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" || parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(value, "#") || parsed.RawPath != "" || path.Clean(parsed.Path) != parsed.Path {
		return fmt.Errorf("%w: request path must be a canonical /v1/ path", qurl.ErrRegisteredAgentResourceRequestDenied)
	}
	if !requestRouteAllowed(method, parsed.Path) {
		return fmt.Errorf("%w: %s %s is not a registered-device route", qurl.ErrRegisteredAgentResourceRequestDenied, method, parsed.Path)
	}
	if (parsed.RawQuery != "" || parsed.ForceQuery) && !queryAllowed(method, parsed.Path) {
		return fmt.Errorf("%w: queries are allowed only for GET /v1/resources and GET /v1/resources/{id}/qurls", qurl.ErrRegisteredAgentResourceRequestDenied)
	}
	return nil
}

const requestResourcesPath = "/v1/resources"

// requestRoutes mirrors qurl-go's registeredAgentResourceRouteAllowed, minus
// POST /v1/api-keys: a supervisor must never mint a portable credential that
// outlives the sealed namespace. Keys are route patterns, with {id} for a
// resource, qURL, or session identifier.
var requestRoutes = map[string][]string{
	"/v1/account/link":                   {http.MethodPost},
	"/v1/qurls":                          {http.MethodPost},
	"/v1/me":                             {http.MethodGet},
	requestResourcesPath:                 {http.MethodGet, http.MethodPost},
	"/v1/resources/{id}":                 {http.MethodGet, http.MethodPatch, http.MethodDelete},
	"/v1/resources/{id}/sharing":         {http.MethodGet, http.MethodPut},
	"/v1/resources/{id}/sharing/restart": {http.MethodPost},
	"/v1/resources/{id}/share":           {http.MethodPost},
	"/v1/resources/{id}/qurls":           {http.MethodGet, http.MethodPost},
	"/v1/resources/{id}/qurls/{id}":      {http.MethodPatch, http.MethodDelete},
	"/v1/resources/{id}/sessions":        {http.MethodGet, http.MethodDelete},
	"/v1/resources/{id}/sessions/{id}":   {http.MethodDelete},
}

func requestRouteAllowed(method, requestPath string) bool {
	parts := strings.Split(requestPath, "/")
	if len(parts) >= 4 && parts[2] == "resources" {
		if !isASCIIToken(parts[3]) {
			return false
		}
		parts[3] = "{id}"
		if len(parts) == 6 && (parts[4] == "qurls" || parts[4] == "sessions") {
			if !isASCIIToken(parts[5]) {
				return false
			}
			parts[5] = "{id}"
		}
	}
	methods, ok := requestRoutes[strings.Join(parts, "/")]
	return ok && slices.Contains(methods, method)
}

// isASCIIToken accepts nonempty letters, digits, hyphens and underscores. For
// route identifiers it also keeps a literal {id} from matching a pattern.
func isASCIIToken(value string) bool {
	return value != "" && strings.IndexFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
	}) < 0
}

func queryAllowed(method, requestPath string) bool {
	if method != http.MethodGet {
		return false
	}
	if requestPath == requestResourcesPath {
		return true
	}
	// Only query presence is gated here; the service validates its contents.
	id, hasPrefix := strings.CutPrefix(requestPath, "/v1/resources/")
	id, hasSuffix := strings.CutSuffix(id, "/qurls")
	return hasPrefix && hasSuffix && id != "" && !strings.Contains(id, "/")
}

// ValidateRequestIdempotencyKey accepts only bounded, nonsecret ASCII tokens.
// It is deliberately stricter than validateEnrollmentIdempotencyKey because a
// supervisor, not this CLI, chooses the value; do not unify the two.
func ValidateRequestIdempotencyKey(value string) error {
	if value == "" {
		return nil
	}
	if len(value) < minIdempotencyKeyLength || len(value) > maxIdempotencyKeyLength || !isASCIIToken(value) {
		return fmt.Errorf("%w: idempotency key must be %d-%d letters, digits, hyphens or underscores", qurl.ErrInvalidResourceRequest, minIdempotencyKeyLength, maxIdempotencyKeyLength)
	}
	return nil
}
