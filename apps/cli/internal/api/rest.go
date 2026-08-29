package qurlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/layervai/qurl-go/crid"
	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/resourceidentity"
)

// maxResponseBody mirrors the SDK's 1 MiB response cap for the direct REST
// calls, so an oversized body fails loudly instead of as a confusing decode.
const maxResponseBody = 1 << 20

// resourceRow mirrors the fields of a /v1/resources row this CLI consumes.
// Decoding is deliberately lax about extra fields: the server owns its own
// payloads, and the projection into ResourceSummary is the contract.
type resourceRow struct {
	ResourceID   string       `json:"resource_id"`
	CRID         string       `json:"crid"`
	TargetURL    string       `json:"target_url"`
	Type         string       `json:"type"`
	Status       string       `json:"status"`
	DesiredState DesiredState `json:"desired_state"`
	ServingEpoch uint64       `json:"serving_epoch"`
	Description  string       `json:"description"`
	Tags         []string     `json:"tags"`
	CreatedAt    *time.Time   `json:"created_at"`
	ExpiresAt    *time.Time   `json:"expires_at"`
}

type sharingRow struct {
	ResourceID      string          `json:"resource_id"`
	CRID            string          `json:"crid"`
	DesiredState    DesiredState    `json:"desired_state"`
	ServingEpoch    uint64          `json:"serving_epoch"`
	ConnectionState ConnectionState `json:"connection_state"`
}

// UnmarshalJSON requires the serving-epoch lifecycle fence to be present and
// rejects duplicate known fields while retaining additive response fields.
func (row *sharingRow) UnmarshalJSON(data []byte) error {
	if row == nil {
		return errors.New("sharing row is nil")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := first.(json.Delim); !ok || delim != '{' {
		return errors.New("sharing row must be an object")
	}
	seen := make(map[string]bool, 5)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return errors.New("sharing row field name is invalid")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if !isSharingField(name) {
			continue
		}
		if seen[name] {
			return fmt.Errorf("duplicate sharing field %q", name)
		}
		seen[name] = true
		if err := decodeSharingField(row, name, raw); err != nil {
			return fmt.Errorf("decode sharing field %q: %w", name, err)
		}
	}
	last, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := last.(json.Delim); !ok || delim != '}' {
		return errors.New("sharing row object is incomplete")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("sharing row has trailing JSON")
		}
		return fmt.Errorf("sharing row trailing JSON: %w", err)
	}
	if !seen["serving_epoch"] {
		return errors.New("sharing row is missing serving_epoch")
	}
	return nil
}

func isSharingField(name string) bool {
	switch name {
	case "resource_id", "crid", "desired_state", "serving_epoch", "connection_state":
		return true
	default:
		return false
	}
}

func decodeSharingField(row *sharingRow, name string, raw json.RawMessage) error {
	switch name {
	case "resource_id":
		return json.Unmarshal(raw, &row.ResourceID)
	case "crid":
		return json.Unmarshal(raw, &row.CRID)
	case "desired_state":
		return json.Unmarshal(raw, &row.DesiredState)
	case "connection_state":
		return json.Unmarshal(raw, &row.ConnectionState)
	case "serving_epoch":
		encoded := strings.TrimSpace(string(raw))
		if encoded == "" || strings.IndexFunc(encoded, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return errors.New("serving_epoch must be an unsigned decimal integer")
		}
		value, err := strconv.ParseUint(encoded, 10, 64)
		if err == nil {
			row.ServingEpoch = value
		}
		return err
	default:
		return fmt.Errorf("unsupported sharing field %q", name)
	}
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
	// Publish has no service idempotency key. A rate-limit response is usually
	// pre-application, but the client cannot prove that a replay would not mint
	// a duplicate resource, so it is deliberately single-shot.
	reply, err := c.doRESTOnce(ctx, http.MethodPost, "/v1/resources", body)
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
	foundExisting := env.Meta.FoundExisting
	return &Published{
		CRID:          env.Data.CRID,
		ResourceID:    env.Data.ResourceID,
		TargetURL:     env.Data.TargetURL,
		Status:        env.Data.Status,
		CreatedAt:     env.Data.CreatedAt,
		ExpiresAt:     env.Data.ExpiresAt,
		FoundExisting: &foundExisting,
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
		summary, err := summarizeResourceRow(&env.Data[i], "resource list row")
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, *summary)
	}
	return page, nil
}

// Resource reads one owner-visible resource. The status command uses this
// generic surface only after the connector-only sharing surface says the CRID
// belongs to another resource type.
func (c *client) Resource(ctx context.Context, id string) (*ResourceSummary, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: resource identifier must not be empty", qurl.ErrInvalidResourceRequest)
	}
	reply, err := c.doREST(ctx, http.MethodGet, "/v1/resources/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		// The v2 CLI requires the owner-facing detail route. A 404 is
		// authoritative; do not hide a missing route with an account-wide list
		// scan or retain compatibility with an unreleased edge contract.
		return nil, reply.problem()
	}
	var env struct {
		Data *struct {
			Resource *resourceRow `json:"resource"`
		} `json:"data"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode resource detail: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if env.Data == nil || env.Data.Resource == nil {
		return nil, fmt.Errorf("%w: resource detail has no resource", qurl.ErrInvalidAPIResponse)
	}
	row := env.Data.Resource
	if id != row.CRID && id != row.ResourceID {
		return nil, fmt.Errorf("%w: resource detail identity does not match the request", qurl.ErrInvalidAPIResponse)
	}
	if row.CRID != "" {
		if err := resourceidentity.ValidatePair(row.CRID, row.ResourceID); err != nil {
			return nil, fmt.Errorf("%w: resource detail identity: %w", qurl.ErrInvalidAPIResponse, err)
		}
	}
	return summarizeResourceRow(row, "resource detail")
}

func summarizeResourceRow(row *resourceRow, source string) (*ResourceSummary, error) {
	if row == nil || strings.TrimSpace(row.Type) == "" || strings.TrimSpace(row.Status) == "" {
		return nil, fmt.Errorf("%w: %s has missing type or status", qurl.ErrInvalidAPIResponse, source)
	}
	if row.Type == "tunnel" {
		if err := resourceidentity.ValidatePair(row.CRID, row.ResourceID); err != nil {
			return nil, fmt.Errorf("%w: tunnel %s identity: %w", qurl.ErrInvalidAPIResponse, source, err)
		}
		if row.DesiredState != DesiredStateOn && row.DesiredState != DesiredStateOff {
			return nil, fmt.Errorf("%w: tunnel %s has invalid desired_state %q", qurl.ErrInvalidAPIResponse, source, row.DesiredState)
		}
		if row.DesiredState == DesiredStateOn && row.ServingEpoch == 0 {
			return nil, fmt.Errorf("%w: desired-on tunnel %s has zero serving_epoch", qurl.ErrInvalidAPIResponse, source)
		}
	}
	return &ResourceSummary{
		CRID: row.CRID, ResourceID: row.ResourceID, TargetURL: row.TargetURL,
		Type: row.Type, Status: row.Status, DesiredState: row.DesiredState,
		ServingEpoch: row.ServingEpoch, Description: row.Description, Tags: row.Tags,
		CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt,
	}, nil
}

// Sharing reads one tunnel resource's durable and observed serving state.
func (c *client) Sharing(ctx context.Context, id string) (*Sharing, error) {
	return c.doSharing(ctx, http.MethodGet, id, nil, true)
}

// SetSharing idempotently applies desired to one tunnel resource. The body is
// deliberately a strict single-field document; target metadata and NHP
// session details do not belong on this management surface.
func (c *client) SetSharing(ctx context.Context, id string, desired DesiredState) (*Sharing, error) {
	if desired != DesiredStateOn && desired != DesiredStateOff {
		return nil, fmt.Errorf("%w: desired_state must be on or off", qurl.ErrInvalidResourceRequest)
	}
	return c.doSharing(ctx, http.MethodPut, id, struct {
		DesiredState DesiredState `json:"desired_state"`
	}{DesiredState: desired}, true)
}

// RestartSharing rotates the serving epoch and leaves the resource desired on.
func (c *client) RestartSharing(ctx context.Context, id string) (*Sharing, error) {
	return c.doSharing(ctx, http.MethodPost, id, nil, false)
}

func (c *client) doSharing(ctx context.Context, method, id string, body any, allowRetry bool) (*Sharing, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: resource identifier must not be empty", qurl.ErrInvalidResourceRequest)
	}
	path := "/v1/resources/" + url.PathEscape(id) + "/sharing"
	if method == http.MethodPost {
		path += "/restart"
	}
	var reply *restReply
	var err error
	if allowRetry {
		reply, err = c.doREST(ctx, method, path, body)
	} else {
		reply, err = c.doRESTOnce(ctx, method, path, body)
	}
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusOK {
		return nil, reply.problem()
	}
	var env struct {
		Data sharingRow `json:"data"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, fmt.Errorf("%w: decode sharing response: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if err := validateSharingRow(env.Data); err != nil {
		return nil, err
	}
	if err := validateSharingIdentity(id, env.Data); err != nil {
		return nil, err
	}
	return &Sharing{
		ResourceID: env.Data.ResourceID, CRID: env.Data.CRID,
		DesiredState: env.Data.DesiredState, ServingEpoch: env.Data.ServingEpoch,
		ConnectionState: env.Data.ConnectionState,
	}, nil
}

func validateSharingIdentity(requestID string, row sharingRow) error {
	der, err := resourceidentity.ValidateResourceID(row.ResourceID)
	if err != nil {
		return fmt.Errorf("%w: sharing response resource identity: %w", qurl.ErrInvalidAPIResponse, err)
	}
	matched, err := crid.KeyMatches(row.CRID, der)
	if err != nil || !matched {
		return fmt.Errorf("%w: sharing response CRID does not match resource identity", qurl.ErrInvalidAPIResponse)
	}
	if requestID == row.ResourceID {
		return nil
	}
	matched, err = crid.KeyMatches(requestID, der)
	if err != nil || !matched {
		return fmt.Errorf("%w: sharing response identity does not match requested resource", qurl.ErrInvalidAPIResponse)
	}
	return nil
}

func validateSharingRow(row sharingRow) error {
	if strings.TrimSpace(row.ResourceID) == "" {
		return fmt.Errorf("%w: sharing response missing resource_id", qurl.ErrInvalidAPIResponse)
	}
	if row.DesiredState != DesiredStateOn && row.DesiredState != DesiredStateOff {
		return fmt.Errorf("%w: sharing response has invalid desired_state %q", qurl.ErrInvalidAPIResponse, row.DesiredState)
	}
	if strings.TrimSpace(row.CRID) == "" {
		return fmt.Errorf("%w: sharing response missing crid", qurl.ErrInvalidAPIResponse)
	}
	switch row.ConnectionState {
	case ConnectionStopped:
		if row.DesiredState != DesiredStateOff {
			return fmt.Errorf("%w: stopped sharing response must be desired off", qurl.ErrInvalidAPIResponse)
		}
	case ConnectionConnecting, ConnectionServing:
		if row.DesiredState != DesiredStateOn || row.ServingEpoch == 0 {
			return fmt.Errorf("%w: active sharing response must be desired on with a nonzero serving_epoch", qurl.ErrInvalidAPIResponse)
		}
	default:
		return fmt.Errorf("%w: sharing response has invalid connection_state %q", qurl.ErrInvalidAPIResponse, row.ConnectionState)
	}
	return nil
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
	return c.doRESTWithHeaders(ctx, method, path, body, nil)
}

// doRESTOnce preserves the normal authorization/identity headers but disables
// transport replay for a non-idempotent request such as sharing restart.
func (c *client) doRESTOnce(ctx context.Context, method, path string, body any) (*restReply, error) {
	return c.doRESTRequest(ctx, method, path, body, nil, false)
}

// doRESTWithHeaders is doREST plus request-specific headers. Shared headers
// (including authorization) still come from the same transport seam; this
// narrow variant exists for contracts such as Idempotency-Key that cannot be
// represented in a JSON body.
func (c *client) doRESTWithHeaders(ctx context.Context, method, path string, body any, headers http.Header) (*restReply, error) {
	return c.doRESTRequest(ctx, method, path, body, headers, true)
}

func (c *client) doRESTRequest(ctx context.Context, method, path string, body any, headers http.Header, allowRetry bool) (*restReply, error) {
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
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	req = withRequestRetryIntent(req, allowRetry)
	var resp *http.Response
	if c.registeredDoer != nil {
		// The registered doer owns request authorization and delegates to the
		// shared transport. The request context carries this caller's retry
		// intent through qurl-go's Do-only registered transport seam.
		resp, err = c.registeredDoer.Do(req)
	} else {
		if c.authorize == nil {
			return nil, fmt.Errorf("%w: API client has no request authority", qurl.ErrInvalidClientConfig)
		}
		if err := c.authorize(ctx, req); err != nil {
			return nil, err
		}
		if allowRetry {
			resp, err = c.transport.Do(req)
		} else {
			resp, err = c.transport.DoOnce(req)
		}
	}
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
	fields := strings.Fields(strings.ToValidUTF8(string(body), "\uFFFD"))
	snippet := strings.Join(fields, " ")
	if len(snippet) > maxSnippet {
		end := maxSnippet
		for end > 0 && !utf8.RuneStart(snippet[end]) {
			end--
		}
		snippet = snippet[:end] + "..."
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
