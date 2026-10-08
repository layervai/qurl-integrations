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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	AllowedDeviceKeys []string     `json:"allowed_device_keys"`
	Private           *bool        `json:"private"`
	ResourceID        string       `json:"resource_id"`
	CRID              string       `json:"crid"`
	TargetURL         string       `json:"target_url"`
	Type              string       `json:"type"`
	Status            string       `json:"status"`
	DesiredState      DesiredState `json:"desired_state"`
	ServingEpoch      uint64       `json:"serving_epoch"`
	Description       string       `json:"description"`
	Tags              []string     `json:"tags"`
	CreatedAt         *time.Time   `json:"created_at"`
	ExpiresAt         *time.Time   `json:"expires_at"`
	// Publisher is read by UnmarshalJSON, not by the struct decoder, so a
	// repeated member can be counted; see publisherMember.
	Publisher publisherWire `json:"-"`
}

// UnmarshalJSON decodes the row's fields by the usual lax rules, then reads
// the publisher from the same object on its own terms: a row that repeats the
// publisher member reads as unnamed and unverified instead of letting the
// last occurrence win, and no shape of that member fails the row. Each call
// replaces the whole row from one JSON object, so decoding into a row that
// was used before leaves nothing of the earlier object behind.
func (row *resourceRow) UnmarshalJSON(data []byte) error {
	// resourceFields has the row's fields without this method.
	type resourceFields resourceRow
	var fields resourceFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	fields.Publisher = publisherMember(data)
	*row = resourceRow(fields)
	return nil
}

type sharingRow struct {
	ResourceID      string          `json:"resource_id"`
	CRID            string          `json:"crid"`
	DesiredState    DesiredState    `json:"desired_state"`
	ServingEpoch    uint64          `json:"serving_epoch"`
	ConnectionState ConnectionState `json:"connection_state"`
	// TODO(upstream-contract): qurl-service adds created_at and publisher to
	// the Connector sharing-state response. Both are optional here, so an
	// older service leaves "no date, no name, unverified".
	CreatedAt *time.Time `json:"created_at"`
	// Publisher is read by UnmarshalJSON (decodeSharingField), never by the
	// struct decoder: publisherWire has no decoder of its own, so a tag here
	// would only look like one. isSharingField names the wire member.
	Publisher publisherWire `json:"-"`
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
	seen := make(map[string]bool, 7)
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
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
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
	case "resource_id", "crid", "desired_state", "serving_epoch", "connection_state", "created_at", fieldPublisher:
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
	case "created_at":
		// Descriptive metadata must never fail a lifecycle read: a date this
		// CLI cannot parse is treated as absent.
		var createdAt *time.Time
		if json.Unmarshal(raw, &createdAt) == nil {
			row.CreatedAt = createdAt
		}
		return nil
	case fieldPublisher:
		// Never an error: a publisher this CLI cannot read is unnamed and
		// unverified.
		row.Publisher = parsePublisherWire(raw)
		return nil
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
	FoundExisting *bool  `json:"found_existing"`
}

// TODO(upstream-contract): privacy and tunnel find-or-create fields mirror
// the service's create-resource request; privacy is immutable after creation.
type publishRequest struct {
	// Private is stated in every create request but one. What an absent
	// field means for a new resource is the service's choice and has changed:
	// a newer service reads it as private, an older one as public. The one
	// request without it is the second create of keepExistingPublic.
	Private           *bool    `json:"private,omitempty"`
	AllowedDeviceKeys []string `json:"allowed_device_keys,omitempty"`
	Slug              string   `json:"slug,omitempty"`
	FindOrCreate      bool     `json:"find_or_create,omitempty"`
	Type              string   `json:"type"`
	TargetURL         string   `json:"target_url,omitempty"`
	Description       string   `json:"description,omitempty"`
	Tags              []string `json:"tags,omitempty"`
	Alias             string   `json:"alias,omitempty"`
}

// Publish registers a URL or pre-creates a Connector resource, private unless
// opts.Public is set. The direct REST call carries fields absent from the
// pinned SDK.
//
//nolint:gocritic // Keep value options in the existing Client contract; this one-shot network operation is not a hot loop.
func (c *client) Publish(ctx context.Context, targetURL string, opts PublishOptions) (*Published, error) {
	if opts.ConnectorID == "" || targetURL != "" {
		if err := validateTargetURL(targetURL); err != nil {
			return nil, err
		}
	}
	wantPrivate := !opts.Public
	body := publishRequest{
		Private:           &wantPrivate,
		AllowedDeviceKeys: opts.AllowedDeviceKeys,
		Type:              "url",
		TargetURL:         targetURL,
		Description:       opts.Description,
		Tags:              opts.Tags,
		Alias:             opts.Alias,
	}
	if opts.ConnectorID != "" {
		body.Type = "tunnel"
		body.Slug = opts.ConnectorID
		body.FindOrCreate = true
	}
	// Publish has no service idempotency key. A rate-limit response is usually
	// pre-application, but the client cannot prove that a replay would not mint
	// a duplicate resource, so it is deliberately single-shot.
	firstCreateSentAt := c.now()
	reply, err := c.doRESTOnce(ctx, http.MethodPost, "/v1/resources", body)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusCreated {
		refusal := publishProblem(reply, &opts)
		if !mayKeepExistingPublic(&opts, refusal) {
			return nil, refusal
		}
		return c.keepExistingPublic(ctx, &body, refusal, firstCreateSentAt)
	}
	published, allowedDeviceKeys, err := publishedFromReply(reply)
	if err != nil {
		return nil, err
	}
	// The row must say which privacy the resource has, and it must be the one
	// asked for. A row that omits it is not read as either: a service that
	// ignored the field may have made the resource with the other privacy.
	if published.Private == nil || *published.Private != wantPrivate {
		return nil, &publishPrivacyError{wantPublic: opts.Public}
	}
	if len(opts.AllowedDeviceKeys) > 0 && !slices.Equal(slices.Sorted(slices.Values(opts.AllowedDeviceKeys)), slices.Sorted(slices.Values(allowedDeviceKeys))) {
		// A resource that already existed keeps the list it has: publishing
		// again never changes it. That is the same conflict the service
		// reports with its own code, seen here in an answer that accepted
		// the request instead. Only a resource that was just made with
		// another list is an answer outside the contract.
		if published.FoundExisting != nil && *published.FoundExisting {
			return nil, &PublishAccessConflictError{Existing: ExistingAccessOtherDevices}
		}
		return nil, fmt.Errorf("%w: API did not confirm the requested device grants", qurl.ErrInvalidAPIResponse)
	}
	return published, nil
}

// publishedFromReply decodes a 201 answer to a create request and checks the
// identity of the resource it names. It returns the row's device list beside
// the result, which does not carry it. Privacy is returned as the row has it
// and is the caller's to check.
func publishedFromReply(reply *restReply) (*Published, []string, error) {
	var env struct {
		Data resourceRow  `json:"data"`
		Meta envelopeMeta `json:"meta"`
	}
	if err := json.Unmarshal(reply.body, &env); err != nil {
		return nil, nil, fmt.Errorf("%w: decode publish response: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if strings.TrimSpace(env.Data.ResourceID) == "" {
		return nil, nil, fmt.Errorf("%w: publish response missing resource_id", qurl.ErrInvalidAPIResponse)
	}
	if strings.TrimSpace(env.Data.CRID) == "" {
		return nil, nil, fmt.Errorf("%w: publish response missing crid", qurl.ErrInvalidAPIResponse)
	}
	if err := resourceidentity.ValidatePair(env.Data.CRID, env.Data.ResourceID); err != nil {
		return nil, nil, fmt.Errorf("%w: publish response identity: %w", qurl.ErrInvalidAPIResponse, err)
	}
	return &Published{
		Private:       env.Data.Private,
		CRID:          env.Data.CRID,
		ResourceID:    env.Data.ResourceID,
		TargetURL:     env.Data.TargetURL,
		Status:        env.Data.Status,
		CreatedAt:     knownTime(env.Data.CreatedAt),
		ExpiresAt:     knownTime(env.Data.ExpiresAt),
		FoundExisting: env.Meta.FoundExisting,
		Publisher:     env.Data.Publisher.publisher(),
	}, env.Data.AllowedDeviceKeys, nil
}

// mayKeepExistingPublic reports whether a refused create is the one a publish
// answers by keeping the resource that exists: the publisher named no
// privacy, and the refusal says the target is already published as public, or
// is the older answer that does not say what differs.
func mayKeepExistingPublic(opts *PublishOptions, refusal error) bool {
	if !opts.KeepExistingPublic || opts.Public || len(opts.AllowedDeviceKeys) > 0 {
		return false
	}
	var conflict *PublishAccessConflictError
	if !errors.As(refusal, &conflict) {
		return false
	}
	return conflict.Existing == ExistingAccessPublic || conflict.Existing == ExistingAccessUnknown
}

// createdAtClockTolerance is how far before this command's first create the
// created_at of a resource may lie for the resource to still count as made by
// this command. created_at is read from the service's clock and the moment
// the first create was sent from this machine's, so the two are compared with
// room for a local clock that runs ahead. The room is small on purpose. A
// wider one would let a resource that existed shortly before the command
// pass as new. A local clock that is further ahead only means that a new
// resource is left in place and the publisher is told to look, which is the
// safe side: a delete cannot be undone.
const createdAtClockTolerance = time.Minute

// madeSince reports whether createdAt says that a resource was made after
// this command sent its first create: it is present and not older than that
// moment, less the clock tolerance.
func madeSince(createdAt *time.Time, firstCreateSentAt time.Time) bool {
	return createdAt != nil && !createdAt.Before(firstCreateSentAt.Add(-createdAtClockTolerance))
}

// keepExistingPublic finishes a publish that named no privacy and was refused
// because the target is already published as public. A person who published a
// target while public was the default, and publishes it again after an
// upgrade, never chose a privacy; refusing them on every run would break a
// command that worked. So the create is sent once more, this time without
// stating privacy, which makes the service return the resource that exists
// with the privacy it has.
//
// That second request is the only create that leaves privacy to the service,
// so its answer is held to what was meant. A public resource is accepted
// only when the answer says it already existed, and the result is marked so
// the caller warns the publisher. A private resource is what a publish with
// no flag asks for and is returned as it is. A public resource that was just
// made is one nobody asked for: an older service makes it when the existing
// resource went away between the two requests. It is deleted, and the
// publish fails without naming a CRID.
//
// A delete is final: the CRID never comes back. So the command deletes only
// what two things in the answer say this request made. The answer must say
// the resource did not exist before, and its creation time must not be older
// than firstCreateSentAt, the moment this command sent its first create. An
// answer that lacks either, or that cannot be read, is not acted on: the
// publish fails with the unconfirmed-privacy message, which sends the
// publisher to look at what exists.
//
// TODO(upstream-contract): the delete rests on two members of the service's
// create answer: meta.found_existing is false only for a resource this
// request made, and data.created_at is the time the resource was made.
//
// refusal is the first answer. It is what the publisher is told when the
// second request is refused for the same kind of reason.
func (c *client) keepExistingPublic(ctx context.Context, body *publishRequest, refusal error, firstCreateSentAt time.Time) (*Published, error) {
	again := *body
	again.Private = nil
	reply, err := c.doRESTOnce(ctx, http.MethodPost, "/v1/resources", again)
	if err != nil {
		return nil, err
	}
	if reply.status != http.StatusCreated {
		if problem := publishProblem(reply, &PublishOptions{}); !errors.Is(problem, ErrPublishAccessConflict) {
			return nil, problem
		}
		return nil, refusal
	}
	published, _, err := publishedFromReply(reply)
	if err != nil {
		// The service accepted a create that stated no privacy, and its
		// answer cannot be read, a creation time that is not a time
		// included. A resource may exist and nothing says what it is like.
		return nil, &publishPrivacyError{}
	}
	switch {
	case published.Private == nil:
		return nil, &publishPrivacyError{}
	case *published.Private:
		return published, nil
	case published.FoundExisting == nil:
		// A public resource, and the answer does not say whether it existed.
		// It is not kept, because that was not confirmed, and not deleted,
		// because it may be the one that was published before.
		return nil, &publishPrivacyError{}
	case *published.FoundExisting:
		published.KeptPublic = true
		return published, nil
	case !madeSince(published.CreatedAt, firstCreateSentAt):
		// The answer says the resource is new, and its creation time does
		// not bear that out. It may be the one that was published before,
		// so it is left alone.
		return nil, &publishPrivacyError{}
	}
	if _, err := c.Delete(ctx, published.CRID); err != nil {
		return nil, &unaskedPublicError{notDeleted: err}
	}
	return nil, &unaskedPublicError{}
}

// TODO(upstream-contract): the service refuses a publish whose target is
// already published with the other privacy with HTTP 400 and the code
// privacy_mismatch, and one whose device list differs from the stored list
// with HTTP 400 and the code device_keys_mismatch. A service from before
// those codes answers both with its generic invalid-input problem and this
// text in the detail. Only that older text is matched; the wording a service
// with the codes uses is never read.
const (
	codePrivacyMismatch        = "privacy_mismatch"
	codeDeviceKeysMismatch     = "device_keys_mismatch"
	legacyAccessSettingsDetail = "existing resource access settings differ"
)

// publishProblem builds the error for a refused publish. A refusal because
// the target is already published with other access settings becomes a
// PublishAccessConflictError; every other refusal is the plain problem.
//
// The conflict says what the resource that exists is like only when the
// service said so. The privacy-mismatch code means it has the privacy the
// request did not state, and the device-list code means its list is another
// one. The older answer covers both without saying which, so it names
// neither, whatever the request carried.
func publishProblem(reply *restReply, opts *PublishOptions) error {
	problem := reply.problem()
	var apiErr *Error
	if !errors.As(problem, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return problem
	}
	if strings.EqualFold(apiErr.Code, codePrivacyMismatch) {
		existing := ExistingAccessPublic
		if opts.Public {
			existing = ExistingAccessPrivate
		}
		return &PublishAccessConflictError{Existing: existing, problem: apiErr}
	}
	if strings.EqualFold(apiErr.Code, codeDeviceKeysMismatch) {
		return &PublishAccessConflictError{Existing: ExistingAccessOtherDevices, problem: apiErr}
	}
	if strings.Contains(strings.ToLower(apiErr.Detail), legacyAccessSettingsDetail) {
		return &PublishAccessConflictError{Existing: ExistingAccessUnknown, problem: apiErr}
	}
	return problem
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

// List fetches one page of the caller's resources. qurl-go has no
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
	return summarizeResourceRow(row, "resource detail")
}

func summarizeResourceRow(row *resourceRow, source string) (*ResourceSummary, error) {
	if row == nil || strings.TrimSpace(row.ResourceID) == "" || strings.TrimSpace(row.Type) == "" || strings.TrimSpace(row.Status) == "" {
		return nil, fmt.Errorf("%w: %s has missing resource_id, type, or status", qurl.ErrInvalidAPIResponse, source)
	}
	if err := resourceidentity.ValidatePair(row.CRID, row.ResourceID); err != nil {
		return nil, fmt.Errorf("%w: %s identity: %w", qurl.ErrInvalidAPIResponse, source, err)
	}
	if row.Type == "tunnel" {
		if row.DesiredState != DesiredStateOn && row.DesiredState != DesiredStateOff {
			return nil, fmt.Errorf("%w: tunnel %s has invalid desired_state %q", qurl.ErrInvalidAPIResponse, source, row.DesiredState)
		}
		// TODO(upstream-contract): keep this desired-state/epoch invariant in
		// lockstep with the qurl-service tunnel resource contract.
		if row.DesiredState == DesiredStateOn && row.ServingEpoch == 0 {
			return nil, fmt.Errorf("%w: desired-on tunnel %s has zero serving_epoch", qurl.ErrInvalidAPIResponse, source)
		}
	}
	return &ResourceSummary{
		AllowedDeviceKeys: row.AllowedDeviceKeys,
		Private:           row.Private,
		CRID:              row.CRID, ResourceID: row.ResourceID, TargetURL: row.TargetURL,
		Type: row.Type, Status: row.Status, DesiredState: row.DesiredState,
		ServingEpoch: row.ServingEpoch, Description: row.Description, Tags: row.Tags,
		CreatedAt: knownTime(row.CreatedAt), ExpiresAt: knownTime(row.ExpiresAt),
		Publisher: row.Publisher.publisher(),
	}, nil
}

// knownTime is a date the service actually gave. The all-zeros timestamp
// (0001-01-01T00:00:00Z) is how an unset date serializes, so it is treated as
// absent: no rendering prints year 1 as a creation date, and a resource with
// no expiry is not listed as expired.
func knownTime(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	return t
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
	// Stopping can retain the current serving epoch because the epoch fences a
	// serving generation, not the desired-off transition. Starting a stopped
	// share uses RestartSharing so it must advance to a new fenced generation.
	// In both cases, require the response to confirm the requested state.
	sharing, err := c.doSharing(ctx, http.MethodPut, id, struct {
		DesiredState DesiredState `json:"desired_state"`
	}{DesiredState: desired}, false)
	if err != nil {
		return nil, err
	}
	if sharing.DesiredState != desired {
		return nil, fmt.Errorf("%w: sharing response desired_state %q does not match requested state %q", qurl.ErrInvalidAPIResponse, sharing.DesiredState, desired)
	}
	return sharing, nil
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
	if err := validateSharingRow(&env.Data); err != nil {
		return nil, err
	}
	if err := validateSharingIdentity(id, &env.Data); err != nil {
		return nil, err
	}
	return &Sharing{
		ResourceID: env.Data.ResourceID, CRID: env.Data.CRID,
		DesiredState: env.Data.DesiredState, ServingEpoch: env.Data.ServingEpoch,
		ConnectionState: env.Data.ConnectionState,
		CreatedAt:       knownTime(env.Data.CreatedAt),
		Publisher:       env.Data.Publisher.publisher(),
	}, nil
}

func validateSharingIdentity(requestID string, row *sharingRow) error {
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

func validateSharingRow(row *sharingRow) error {
	if strings.TrimSpace(row.ResourceID) == "" {
		return fmt.Errorf("%w: sharing response missing resource_id", qurl.ErrInvalidAPIResponse)
	}
	if row.DesiredState != DesiredStateOn && row.DesiredState != DesiredStateOff {
		return fmt.Errorf("%w: sharing response has invalid desired_state %q", qurl.ErrInvalidAPIResponse, row.DesiredState)
	}
	if strings.TrimSpace(row.CRID) == "" {
		return fmt.Errorf("%w: sharing response missing crid", qurl.ErrInvalidAPIResponse)
	}
	// TODO(upstream-contract): keep these durable and observed state
	// combinations in lockstep with qurl-service sharing responses.
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
// revoke is idempotent), so the share-side gone family (400 `revoked`,
// 410 `resource_tombstoned`) is not expected here and deliberately not
// treated as success. If the platform ever starts answering 410 for a
// delete on a tombstoned row, widen this switch in lockstep.
func (c *client) Delete(ctx context.Context, id string) (*DeleteResult, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: resource identifier must not be empty", qurl.ErrInvalidResourceRequest)
	}
	reply, err := c.doRESTOnce(ctx, http.MethodDelete, "/v1/resources/"+url.PathEscape(id), nil)
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
// transport replay for a write with no service idempotency key. This includes
// writes whose desired state is idempotent but whose lost response is still
// ambiguous to the caller.
func (c *client) doRESTOnce(ctx context.Context, method, path string, body any) (*restReply, error) {
	return c.doRESTRequest(ctx, method, path, body, nil, false)
}

// doRESTWithHeaders is doREST plus request-specific headers. Shared headers
// (including authorization and the transport-owned X-Request-Id) still come
// from the same transport seam; this narrow variant exists for contracts such
// as Idempotency-Key that cannot be represented in a JSON body.
func (c *client) doRESTWithHeaders(ctx context.Context, method, path string, body any, headers http.Header) (*restReply, error) {
	return c.doRESTRequest(ctx, method, path, body, headers, true)
}

func (c *client) doRESTRequest(ctx context.Context, method, path string, body any, headers http.Header, allowRetry bool) (*restReply, error) {
	reqBody := io.Reader(http.NoBody)
	if raw, ok := body.(json.RawMessage); ok {
		// Pre-encoded JSON goes out as is: json.Marshal would HTML-escape it
		// and could grow a capped supervisor body past its limit. An empty
		// one means no body and no Content-Type.
		body = nil
		if len(raw) > 0 {
			body, reqBody = raw, bytes.NewReader(raw)
		}
	} else if body != nil {
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
	// TODO(upstream-contract): qURL API responses use Retry-After seconds, not
	// HTTP-date. retryDelay uses the same parser below.
	if secs, ok := parseRetryAfterSeconds(r.header.Get("Retry-After")); ok && secs > 0 {
		e.RetryAfter = secs
	}
	return e
}

const maxSnippet = 256

func bodySnippet(body []byte) string {
	clean := strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			return '\uFFFD'
		}
		return r
	}, strings.ToValidUTF8(string(body), "\uFFFD"))
	fields := strings.Fields(clean)
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

// SetDeviceGrants changes grants with one authenticated PATCH. It never retries.
// TODO(upstream-contract): PATCH returns 200 with a flat data resource row,
// including type, status, privacy and grants; GET nests its row under resource.
func (c *client) SetDeviceGrants(ctx context.Context, id string, keys []string) (*ResourceSummary, error) {
	id, path, err := deviceGrantsPath(id)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		keys = []string{}
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPatch, path, map[string]any{"allowed_device_keys": keys})
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
		return nil, fmt.Errorf("%w: decode device grants: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if err := validateSharingIdentity(id, &sharingRow{CRID: env.Data.CRID, ResourceID: env.Data.ResourceID}); err != nil {
		return nil, err
	}
	if !slices.Equal(slices.Sorted(slices.Values(keys)), slices.Sorted(slices.Values(env.Data.AllowedDeviceKeys))) {
		return nil, fmt.Errorf("%w: API did not confirm the device grants", qurl.ErrInvalidAPIResponse)
	}
	return summarizeResourceRow(&env.Data, "device grants")
}

// deviceGrantsPath returns the trimmed identifier and the escaped path of the
// resource a grant change is sent to, as every method here builds its path.
// The path is also held to the routes a device credential may use, so an
// identifier that could never name a resource is refused before a request.
func deviceGrantsPath(id string) (trimmed, path string, err error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", fmt.Errorf("%w: resource identifier must not be empty", qurl.ErrInvalidResourceRequest)
	}
	path = "/v1/resources/" + url.PathEscape(id)
	if err := ValidateRequestTarget(http.MethodPatch, path); err != nil {
		return "", "", err
	}
	return id, path, nil
}

// deviceGrantEdit is the PATCH body that adds and removes single device keys.
// It never carries allowed_device_keys: that member replaces the whole list.
//
// TODO(upstream-contract): the service applies both members as one change,
// treats a key that is already present or already absent as nothing to do,
// refuses a key that is in both and a result above 256 keys, and answers
// with the complete resulting list.
type deviceGrantEdit struct {
	Add    []string `json:"allowed_device_keys_add,omitempty"`
	Remove []string `json:"allowed_device_keys_remove,omitempty"`
}

// EditDeviceGrants adds and removes single device keys with one authenticated
// PATCH. It never retries.
//
// The answer is checked against the request: every added key must be on the
// returned list and no removed key may be. A service from before these
// members ignores them and returns the list as it was, with a success status;
// that answer fails here instead of being reported as a change that was made.
func (c *client) EditDeviceGrants(ctx context.Context, id string, add, remove []string) (*ResourceSummary, error) {
	if len(add) == 0 && len(remove) == 0 {
		return nil, fmt.Errorf("%w: no device key to add or remove", qurl.ErrInvalidResourceRequest)
	}
	for _, key := range add {
		if slices.Contains(remove, key) {
			return nil, fmt.Errorf("%w: a device key cannot be both added and removed", qurl.ErrInvalidResourceRequest)
		}
	}
	id, path, err := deviceGrantsPath(id)
	if err != nil {
		return nil, err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPatch, path, deviceGrantEdit{Add: add, Remove: remove})
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
		return nil, fmt.Errorf("%w: decode device grants: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if err := validateSharingIdentity(id, &sharingRow{CRID: env.Data.CRID, ResourceID: env.Data.ResourceID}); err != nil {
		return nil, err
	}
	for _, key := range add {
		if !slices.Contains(env.Data.AllowedDeviceKeys, key) {
			return nil, &grantEditError{}
		}
	}
	for _, key := range remove {
		if slices.Contains(env.Data.AllowedDeviceKeys, key) {
			return nil, &grantEditError{}
		}
	}
	return summarizeResourceRow(&env.Data, "device grants")
}
