package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/layervai/qurl-go/qurl"
)

// OriginHeader is a header the qURL gateway adds to every request it forwards
// to the target URL of a URL resource, so that the origin admits only traffic
// that came through qURL: a deployment protection bypass secret, an access
// service token, a basic-auth credential.
//
// Value is a secret. It leaves this process in one place only, the request
// body built by originHeaderWire, and no answer of the service carries it
// back. The type keeps it out of every generic form it can take: its fmt
// forms and its JSON form carry the name alone, so a stray %v or a debug dump
// of a request option cannot print it.
type OriginHeader struct {
	Name  string
	Value string
}

// String keeps the value out of the fmt forms of the type.
func (h OriginHeader) String() string { return "OriginHeader{Name:" + h.Name + ", Value:[redacted]}" }

// GoString is String, for %#v.
func (h OriginHeader) GoString() string { return h.String() }

// MarshalJSON writes the name only. The request body that must carry the
// value builds originHeaderWire instead.
func (h OriginHeader) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name string `json:"name"`
	}{Name: h.Name})
}

// originHeaderWire is the request member that carries the value: the
// create's origin_header and the PATCH that sets or rotates it.
type originHeaderWire struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (h *OriginHeader) wire() *originHeaderWire {
	if h == nil {
		return nil
	}
	return &originHeaderWire{Name: h.Name, Value: h.Value}
}

// TODO(upstream-contract): the limits, the reserved names and prefixes, and
// the value rules below mirror the qURL API's origin header validation. The
// service is the authority; these checks give a clear error before anything
// is sent. Authorization is not reserved: a basic-auth or bearer origin is a
// use of the feature, and the header replaces the visitor's own.
const (
	MaxOriginHeaderNameBytes  = 256
	MaxOriginHeaderValueBytes = 4096
)

var reservedOriginHeaderNames = map[string]bool{
	"host": true, "connection": true, "keep-alive": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true, "content-length": true, "content-type": true,
	"content-encoding": true, "cookie": true, "forwarded": true, "via": true, "expect": true,
	"x-real-ip": true, "x-request-id": true,
}

var reservedOriginHeaderPrefixes = []string{"x-forwarded-", "x-qurl-", "proxy-"}

// The texts of a refused origin header. None of them repeats the name or the
// value: the value is a secret, and the name is on the command line already.
const (
	msgOriginHeaderNameLength   = "the origin header name must be 1 to 256 characters"
	msgOriginHeaderNameToken    = "the origin header name may contain only letters, digits and !#$%&'*+-.^_`|~"
	msgOriginHeaderNameReserved = "qURL sets that header itself, so it cannot be the origin header; choose another name"
	msgOriginHeaderValueLength  = "the origin header value must be 1 to 4096 bytes"
	msgOriginHeaderValueUTF8    = "the origin header value must be valid UTF-8 text"
	msgOriginHeaderValueSpace   = "the origin header value must not start or end with a space or tab"
	msgOriginHeaderValueControl = "the origin header value must be one line, with no control characters other than tab"
)

// ValidateOriginHeaderName refuses a name the qURL API would refuse: an RFC
// 9110 token of 1 to 256 bytes that is not reserved in any case.
func ValidateOriginHeaderName(name string) error {
	switch {
	case name == "" || len(name) > MaxOriginHeaderNameBytes:
		return errors.New(msgOriginHeaderNameLength)
	case !isHTTPToken(name):
		return errors.New(msgOriginHeaderNameToken)
	case reservedOriginHeaderName(name):
		return errors.New(msgOriginHeaderNameReserved)
	}
	return nil
}

// ValidateOriginHeaderValue refuses a value the qURL API would refuse, or one
// that JSON encoding would change: 1 to 4096 bytes of valid UTF-8, no
// leading or trailing space or tab, and no control character but tab.
func ValidateOriginHeaderValue(value string) error {
	switch {
	case value == "" || len(value) > MaxOriginHeaderValueBytes:
		return errors.New(msgOriginHeaderValueLength)
	case !utf8.ValidString(value):
		return errors.New(msgOriginHeaderValueUTF8)
	case strings.Trim(value, " \t") != value:
		return errors.New(msgOriginHeaderValueSpace)
	case strings.ContainsFunc(value, func(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }):
		return errors.New(msgOriginHeaderValueControl)
	}
	return nil
}

// Validate applies both checks.
func (h *OriginHeader) Validate() error {
	if h == nil {
		return fmt.Errorf("%w: origin header is nil", qurl.ErrInvalidResourceRequest)
	}
	if err := ValidateOriginHeaderName(h.Name); err != nil {
		return err
	}
	return ValidateOriginHeaderValue(h.Value)
}

func reservedOriginHeaderName(name string) bool {
	lower := strings.ToLower(name)
	if reservedOriginHeaderNames[lower] {
		return true
	}
	for _, prefix := range reservedOriginHeaderPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// isHTTPToken reports whether s is one or more RFC 9110 tchar.
func isHTTPToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0 {
			continue
		}
		return false
	}
	return true
}

// TODO(upstream-contract): the qURL API refuses a create that would reuse a
// resource whose origin header differs, or that has none, with HTTP 400 and
// this code, and changes nothing.
const codeOriginHeaderMismatch = "origin_header_mismatch"

const (
	// msgOriginHeaderMismatch is a publish that stated an origin header for a
	// target that is already published with another one, or with none.
	msgOriginHeaderMismatch = "this target is already published with a different origin header, or with none, and publishing again does not change it. Nothing was changed. Run `qurl list` to find its CRID, then set the header with `qurl origin-header set <CRID> <name>`, or publish again without --origin-header to keep the one it has"
	// msgOriginHeaderUnconfirmed is a publish whose answer does not name the
	// origin header that was asked for. The resource may exist without it.
	msgOriginHeaderUnconfirmed = "the service did not confirm the origin header for %s. The resource exists, and requests to the target may not carry the header. Set it with `qurl origin-header set %s <name>`, or check that this service offers origin headers"
	// msgOriginHeaderChangeUnconfirmed is a set or clear whose answer does
	// not show the change.
	msgOriginHeaderChangeUnconfirmed = "the service did not confirm the change to the origin header. Run `qurl status <CRID>` to see the header it has now"
)

// ErrOriginHeaderMismatch marks a publish refused because the target is
// already published with another origin header, or with none. Nothing in the
// request is invalid: it conflicts with a resource that exists, so it has the
// Conflict exit code, like ErrPublishAccessConflict.
var ErrOriginHeaderMismatch = errors.New(msgOriginHeaderMismatch)

// OriginHeaderMismatchError is that refusal. It matches
// ErrOriginHeaderMismatch, and the service's problem stays in the chain for
// the request id.
type OriginHeaderMismatchError struct{ problem *Error }

func (e *OriginHeaderMismatchError) Error() string { return msgOriginHeaderMismatch }

// UserMessage is the text the terminal rendering shows.
func (e *OriginHeaderMismatchError) UserMessage() string { return msgOriginHeaderMismatch }

func (e *OriginHeaderMismatchError) Unwrap() []error {
	return []error{ErrOriginHeaderMismatch, e.problem}
}

// originHeaderUnconfirmedError is an answer that does not name the origin
// header that was asked for. It is an answer outside the contract, so it
// matches the SDK's invalid-response sentinel and has that exit code.
type originHeaderUnconfirmedError struct{ message string }

func (e *originHeaderUnconfirmedError) Error() string { return e.message }

// UserMessage is the text the terminal rendering shows.
func (e *originHeaderUnconfirmedError) UserMessage() string { return e.message }

func (e *originHeaderUnconfirmedError) Unwrap() error { return qurl.ErrInvalidAPIResponse }

// validatePublishOriginHeader refuses an origin header a publish cannot send:
// one for a Connector resource, or one the service would refuse.
func validatePublishOriginHeader(opts *PublishOptions) error {
	if opts.OriginHeader == nil {
		return nil
	}
	if opts.ConnectorID != "" {
		return fmt.Errorf("%w: an origin header applies only to a URL resource", qurl.ErrInvalidResourceRequest)
	}
	if err := opts.OriginHeader.Validate(); err != nil {
		return fmt.Errorf("%w: %w", qurl.ErrInvalidResourceRequest, err)
	}
	return nil
}

// originHeaderConfirmed wraps a publish result in confirmOriginHeader.
func originHeaderConfirmed(header *OriginHeader) func(*Published, error) (*Published, error) {
	return func(published *Published, err error) (*Published, error) {
		if err != nil {
			return nil, err
		}
		if err := confirmOriginHeader(header, published); err != nil {
			return nil, err
		}
		return published, nil
	}
}

// confirmOriginHeader holds a publish that asked for an origin header to an
// answer that names it, in canonical form.
func confirmOriginHeader(header *OriginHeader, published *Published) error {
	if header == nil || published.OriginHeaderName == http.CanonicalHeaderKey(header.Name) {
		return nil
	}
	return &originHeaderUnconfirmedError{message: fmt.Sprintf(msgOriginHeaderUnconfirmed, published.CRID, published.CRID)}
}

// originHeaderPatch is the PATCH body that sets, rotates or removes the
// origin header. A nil Header is sent as an explicit null, which removes it.
type originHeaderPatch struct {
	Header *originHeaderWire `json:"origin_header"`
}

// SetOriginHeader sets or rotates the origin header of one URL resource, or
// removes it when header is nil, with one authenticated PATCH that is never
// replayed. The answer must name the header that was set, or none after a
// removal.
//
// TODO(upstream-contract): PATCH returns 200 with a flat data resource row
// that names the header as origin_header_name and never carries the value.
func (c *client) SetOriginHeader(ctx context.Context, id string, header *OriginHeader) (*ResourceSummary, error) {
	if header != nil {
		if err := header.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %w", qurl.ErrInvalidResourceRequest, err)
		}
	}
	id, path, err := deviceGrantsPath(id)
	if err != nil {
		return nil, err
	}
	reply, err := c.doRESTOnce(ctx, http.MethodPatch, path, originHeaderPatch{Header: header.wire()})
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
		return nil, fmt.Errorf("%w: decode origin header change: %w", qurl.ErrInvalidAPIResponse, err)
	}
	if err := validateSharingIdentity(id, &sharingRow{CRID: env.Data.CRID, ResourceID: env.Data.ResourceID}); err != nil {
		return nil, err
	}
	want := ""
	if header != nil {
		want = http.CanonicalHeaderKey(header.Name)
	}
	if env.Data.OriginHeaderName != want {
		return nil, &originHeaderUnconfirmedError{message: msgOriginHeaderChangeUnconfirmed}
	}
	return summarizeResourceRow(&env.Data, "origin header change")
}
