package qurlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/layervai/qurl-go/qurl"
)

// RequestResponse preserves HTTP failures for supervising apps without exposing
// request metadata or credential-bearing response headers.
type RequestResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

// Request uses only the registered SDK transport's existing route allowlist.
// It performs one attempt; the supervisor owns any retry decision.
// TODO(upstream-contract): the pinned qurl-go owns the exact registered-device
// method/route allowlist, including nested resource qURL/session management.
// Queries are permitted only for GET /v1/resources and GET /v1/resources/{id}/qurls.
// Review this command and its negative-route tests together on SDK changes.
func Request(ctx context.Context, api Client, method, relativePath string, body json.RawMessage, idempotencyKey string) (*RequestResponse, error) {
	registered, ok := api.(*registeredClient)
	if !ok {
		return nil, fmt.Errorf("%w: request requires a registered device", qurl.ErrInvalidClientConfig)
	}
	c, ok := registered.Client.(*client)
	if !ok || c.registeredDoer == nil {
		return nil, fmt.Errorf("%w: registered transport is unavailable", qurl.ErrInvalidClientConfig)
	}
	if err := ValidateRequestPath(relativePath); err != nil {
		return nil, err
	}
	if len(body) > 0 && !json.Valid(body) {
		return nil, fmt.Errorf("%w: request body must be JSON", qurl.ErrInvalidResourceRequest)
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
	for _, name := range []string{"content-type", "retry-after"} {
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

// ValidateRequestPath rejects URL authority, ambiguous encoding, and dot
// segments before a supervisor request can open or enroll a device.
func ValidateRequestPath(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || !strings.HasPrefix(value, "/v1/") || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" || parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(value, "#") || parsed.RawPath != "" || path.Clean(parsed.Path) != parsed.Path {
		return fmt.Errorf("%w: request path must be a canonical /v1/ path", qurl.ErrRegisteredAgentResourceRequestDenied)
	}
	return nil
}

// ValidateRequestIdempotencyKey accepts only bounded, nonsecret header values.
func ValidateRequestIdempotencyKey(value string) error {
	if value == "" {
		return nil
	}
	if len(value) < 32 || len(value) > 256 || strings.IndexFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
	}) >= 0 {
		return fmt.Errorf("%w: idempotency key must be 32-256 letters, digits, hyphens or underscores", qurl.ErrInvalidResourceRequest)
	}
	return nil
}
