package qurlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/layervai/qurl-go/qurl"
)

// maxResponseBody mirrors the SDK's 1 MiB response cap for the direct REST
// calls, so an oversized body fails loudly instead of as a confusing decode.
const maxResponseBody = 1 << 20

// resourceRow mirrors the fields of a /v1/resources row this CLI consumes.
// Decoding is deliberately lax about extra fields: the server owns its own
// payloads, and the projection into ResourceSummary is the contract.
type resourceRow struct {
	ResourceID  string     `json:"resource_id"`
	CRID        string     `json:"crid"`
	TargetURL   string     `json:"target_url"`
	Type        string     `json:"type"`
	Status      string     `json:"status"`
	Description string     `json:"description"`
	Tags        []string   `json:"tags"`
	CreatedAt   *time.Time `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

// envelopeMeta carries the platform's response metadata this CLI consumes.
// has_more — not a present-or-absent cursor — is the pagination terminator:
// the platform legitimately serves short and even zero-item pages with
// has_more=true (it post-filters rows out of a page after cutting it).
type envelopeMeta struct {
	RequestID     string `json:"request_id"`
	NextCursor    string `json:"next_cursor"`
	HasMore       bool   `json:"has_more"`
	FoundExisting bool   `json:"found_existing"`
}

// publishRequest is the pinned publish wire shape: type is required and
// always "url" for CLI publishes (the tunnel type belongs to the Connector).
type publishRequest struct {
	Type        string   `json:"type"`
	TargetURL   string   `json:"target_url"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Alias       string   `json:"alias,omitempty"`
}

// Publish registers targetURL as a protected URL resource. This is a direct
// call rather than the SDK's ProtectURL because the pinned platform contract
// requires the explicit `type: url` discriminator, which qurl-go v0.5.3 does
// not send.
func (c *client) Publish(ctx context.Context, targetURL string, opts PublishOptions) (*Published, error) {
	if err := validateTargetURL(targetURL); err != nil {
		return nil, err
	}
	body := publishRequest{
		Type:        "url",
		TargetURL:   targetURL,
		Description: opts.Description,
		Tags:        opts.Tags,
		Alias:       opts.Alias,
	}
	reply, err := c.doREST(ctx, http.MethodPost, "/v1/resources", body)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusCreated {
		return nil, reply.problem()
	}
	var env struct {
		Data resourceRow  `json:"data"`
		Meta envelopeMeta `json:"meta"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode publish response: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if strings.TrimSpace(env.Data.ResourceID) == "" {
		return nil, fmt.Errorf("%w: publish response missing resource_id", qurl.ErrInvalidAPIResponse)
	}
	return &Published{
		CRID:          env.Data.CRID,
		ResourceID:    env.Data.ResourceID,
		TargetURL:     env.Data.TargetURL,
		Status:        env.Data.Status,
		CreatedAt:     env.Data.CreatedAt,
		ExpiresAt:     env.Data.ExpiresAt,
		FoundExisting: env.Meta.FoundExisting,
	}, nil
}

// validateTargetURL applies the SDK's local target rules (http/https, a
// host, no embedded credentials) before anything goes on the wire.
func validateTargetURL(target string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("%w: target URL must not be empty", qurl.ErrInvalidResourceRequest)
	}
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("%w: target URL: %w", qurl.ErrInvalidResourceRequest, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: target URL must use http or https", qurl.ErrInvalidResourceRequest)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: target URL must include a host", qurl.ErrInvalidResourceRequest)
	}
	if u.User != nil {
		return fmt.Errorf("%w: target URL must not include credentials", qurl.ErrInvalidResourceRequest)
	}
	return nil
}

// List fetches one page of the caller's resources. qurl-go v0.5.3 has no
// generic list surface (its slug lookup is connector-only), so this is a
// direct call on the same /v1/resources endpoint through the shared
// transport.
func (c *client) List(ctx context.Context, opts ListOptions) (*ResourcePage, error) {
	query := url.Values{}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		query.Set("cursor", opts.Cursor)
	}
	if opts.Status != "" {
		query.Set("status", opts.Status)
	}
	if opts.Type != "" {
		query.Set("type", opts.Type)
	}
	path := "/v1/resources"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	reply, err := c.doREST(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}

	var env struct {
		Data []resourceRow `json:"data"`
		Meta envelopeMeta  `json:"meta"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode resource list: %w", qurl.ErrInvalidAPIResponse, err)
	}
	page := &ResourcePage{NextCursor: env.Meta.NextCursor, HasMore: env.Meta.HasMore}
	for i := range env.Data {
		row := &env.Data[i]
		page.Items = append(page.Items, ResourceSummary{
			CRID:        row.CRID,
			ResourceID:  row.ResourceID,
			TargetURL:   row.TargetURL,
			Type:        row.Type,
			Status:      row.Status,
			Description: row.Description,
			Tags:        row.Tags,
			CreatedAt:   row.CreatedAt,
			ExpiresAt:   row.ExpiresAt,
		})
	}
	return page, nil
}

// Delete revokes a resource by CRID or public-key identifier. qurl-go
// v0.5.3's delete validates connector-only identifiers (which reject CRIDs),
// so this is a direct call; the service dual-accepts both forms in the path.
//
// Deletion is idempotent end to end: 204 means revoked now, and a 404 (the
// row was hard-deleted and reaped) means the desired state already holds —
// both are success. AlreadyGone reports the 404 case so the UX can say so.
//
// TODO(upstream-contract): 204-or-404 is the verified platform contract for
// re-delete — DELETE on an already-revoked resource answers 204 (the soft
// revoke is idempotent), so the resolve-side gone family (400 `revoked`,
// 410 `resource_tombstoned`) is not expected here and deliberately not
// treated as success. If the platform ever starts answering 410 for a
// delete on a tombstoned row, widen this switch in lockstep.
func (c *client) Delete(ctx context.Context, id string) (*DeleteResult, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: resource identifier must not be empty", qurl.ErrInvalidResourceRequest)
	}
	reply, err := c.doREST(ctx, http.MethodDelete, "/v1/resources/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	switch reply.status {
	case http.StatusNoContent:
		return &DeleteResult{}, nil
	case http.StatusNotFound:
		// Both platform not-found spellings land here; on a delete they mean
		// "nothing left to delete", which is the requested outcome.
		return &DeleteResult{AlreadyGone: true}, nil
	default:
		return nil, reply.problem()
	}
}

// restReply is one fully consumed direct response: status, headers, capped
// body. No live *http.Response escapes doREST, so the body's lifecycle is
// owned in exactly one place.
type restReply struct {
	status int
	header http.Header
	body   []byte
}

// doREST runs one direct request through the shared transport with the same
// authorization the SDK calls use. The response body is fully consumed and
// closed before returning.
func (c *client) doREST(ctx context.Context, method, path string, body any) (*restReply, error) {
	reqBody := io.Reader(http.NoBody)
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode qURL API request: %w", err)
		}
		reqBody = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("build qURL API request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.authorize(ctx, req); err != nil {
		return nil, err
	}
	resp, err := c.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return nil, fmt.Errorf("read qURL API response: %w", err)
	}
	if len(respBody) > maxResponseBody {
		return nil, fmt.Errorf("%w: response exceeds %d-byte cap", qurl.ErrInvalidAPIResponse, maxResponseBody)
	}
	return &restReply{status: resp.StatusCode, header: resp.Header, body: respBody}, nil
}

// problemDocument decodes the platform's error envelope:
//
//	{"error": {type, title, status, detail, instance, code}, "meta": {request_id}}
//
// The validation variant adds invalid_fields inside error (possibly null).
// Programmatic matching is on error.code only — type is derived from code
// server-side and detail is prose that may be reworded. Flat fields are kept
// as a fallback for intermediaries that answer without the envelope.
type problemDocument struct {
	Error struct {
		Code          string            `json:"code"`
		Title         string            `json:"title"`
		Detail        string            `json:"detail"`
		Message       string            `json:"message"`
		InvalidFields map[string]string `json:"invalid_fields"`
	} `json:"error"`
	Code          string            `json:"code"`
	Title         string            `json:"title"`
	Detail        string            `json:"detail"`
	Message       string            `json:"message"`
	InvalidFields map[string]string `json:"invalid_fields"`
	RequestID     string            `json:"request_id"`
	Meta          struct {
		RequestID string `json:"request_id"`
	} `json:"meta"`
}

// problem builds the typed *Error for a non-2xx direct reply.
func (r *restReply) problem() error {
	var doc problemDocument
	_ = json.Unmarshal(r.body, &doc) // non-JSON bodies fall through to the snippet

	e := &Error{
		StatusCode:    r.status,
		Code:          firstNonEmpty(doc.Error.Code, doc.Code),
		Title:         firstNonEmpty(doc.Error.Title, doc.Title),
		Detail:        firstNonEmpty(doc.Error.Detail, doc.Detail, doc.Error.Message, doc.Message),
		RequestID:     firstNonEmpty(doc.Meta.RequestID, doc.RequestID),
		InvalidFields: doc.Error.InvalidFields,
	}
	if e.InvalidFields == nil {
		e.InvalidFields = doc.InvalidFields
	}
	if e.Code == "" && e.Title == "" && e.Detail == "" {
		e.Detail = bodySnippet(r.body)
	}
	if v := strings.TrimSpace(r.header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			e.RetryAfter = secs
		}
	}
	return e
}

const maxSnippet = 256

func bodySnippet(body []byte) string {
	fields := strings.Fields(string(body))
	snippet := strings.Join(fields, " ")
	if len(snippet) > maxSnippet {
		snippet = snippet[:maxSnippet] + "..."
	}
	return snippet
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func trimBaseURL(base string) string {
	return strings.TrimRight(base, "/")
}
