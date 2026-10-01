package qurlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/layervai/qurl-go/qurl"
)

// Publisher is the repo-owned description of who published a resource: the
// name the resource owner chose for itself, and whether the platform has
// verified that owner.
//
// The zero value means "no name, unverified", and every gap decodes to it.
// Name is self-declared text chosen by whoever owns the resource, so it must
// never reach a terminal raw: the output package owns the one rendering, which
// quotes and escapes it and always shows it beside the verification status.
// Verified is true only when the service said so explicitly. No publisher is
// verified today.
type Publisher struct {
	Name     string
	Verified bool
}

// publisherWire is the publisher object the service attaches to share
// answers, resource rows, and Connector sharing state.
//
// TODO(upstream-contract): mirrors the qurl-service Publisher object
// {"name"?: string, "verified": boolean}. The service always sends verified
// and omits name when none is set. If the object is renamed or reshaped,
// nothing here fails loudly: every publisher reads as unnamed and unverified.
type publisherWire struct {
	name     string
	verified bool
}

// UnmarshalJSON never fails and never guesses. Anything that is not an
// object, an object without a usable member, and a service that predates the
// field all leave the unnamed, unverified zero value, so publisher metadata
// can never break the resource read that carries it. Only one exact,
// lowercase "verified" member holding the JSON literal true verifies: a
// string, a number, a differently cased key, or a repeated member does not.
func (w *publisherWire) UnmarshalJSON(data []byte) error {
	*w = parsePublisherWire(data)
	return nil
}

// parsePublisherWire reads one publisher object. It has no error result on
// purpose: whatever cannot be read is the unnamed, unverified zero value.
func parsePublisherWire(data []byte) publisherWire {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if first, err := decoder.Token(); err != nil || first != json.Delim('{') {
		return publisherWire{}
	}
	var parsed publisherWire
	seenVerified := false
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return publisherWire{}
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return publisherWire{}
		}
		switch name {
		case "name":
			parsed.name = ""
			var value string
			if json.Unmarshal(raw, &value) == nil {
				parsed.name = value
			}
		case "verified":
			parsed.verified = !seenVerified && string(bytes.TrimSpace(raw)) == "true"
			seenVerified = true
		}
	}
	return parsed
}

func (w publisherWire) publisher() Publisher {
	return Publisher{Name: w.name, Verified: w.verified}
}

// Publisher returns the publisher profile of the owner behind the configured
// credential: the name and verification status the service shows with that
// owner's resources.
func (c *client) Publisher(ctx context.Context) (*Publisher, error) {
	profile, err := c.sdk.Publisher(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return publisherFromSDK(profile)
}

// SetPublisherName sets the owner's self-declared publisher name; an empty
// name removes it. A name the service refuses matches
// qurl.ErrInvalidPublisherName and keeps the service's reason in the *Error.
// Setting a name never verifies the publisher.
func (c *client) SetPublisherName(ctx context.Context, name string) (*Publisher, error) {
	profile, err := c.sdk.SetPublisherName(ctx, name)
	if err != nil {
		return nil, mapError(err)
	}
	return publisherFromSDK(profile)
}

func publisherFromSDK(profile *qurl.Publisher) (*Publisher, error) {
	if profile == nil {
		return nil, fmt.Errorf("%w: publisher profile is empty", qurl.ErrInvalidAPIResponse)
	}
	return &Publisher{Name: profile.Name, Verified: profile.Verified}, nil
}

// PublisherNameReason returns the service's explanation for a refused
// publisher name, or the local reason when the name was rejected before any
// request. It is empty when err is not a publisher-name refusal.
func PublisherNameReason(err error) string {
	if !errors.Is(err, qurl.ErrInvalidPublisherName) {
		return ""
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return firstNonEmpty(apiErr.Detail, apiErr.Title)
	}
	// The SDK's own refusals read "<sentinel>: <reason>".
	text, prefix := err.Error(), qurl.ErrInvalidPublisherName.Error()+": "
	if at := strings.Index(text, prefix); at >= 0 {
		return text[at+len(prefix):]
	}
	return ""
}
