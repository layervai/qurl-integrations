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

// publisherWire is the publisher object the service attaches to resource rows
// and Connector sharing state.
//
// It has no UnmarshalJSON on purpose. encoding/json lets the last of a
// repeated member win, so the decoder of the object that carries a publisher
// reads the member itself and counts it there: resourceRow blanks a repeated
// publisher and sharingRow rejects one. A struct that only tags a field with
// this type decodes nothing into it and reads as unnamed and unverified.
//
// TODO(upstream-contract): mirrors the qurl-service Publisher object
// {"name"?: string, "verified": boolean}. The service always sends verified
// and omits name when none is set. If the object is renamed or reshaped,
// nothing here fails loudly: every publisher reads as unnamed and unverified.
type publisherWire struct {
	name     string
	verified bool
}

// fieldPublisher is the member that carries a publisher object. It is matched
// exactly: a differently cased key is an unknown field.
const fieldPublisher = "publisher"

// parsePublisherWire reads one publisher object. It never fails and never
// guesses: whatever cannot be read is the unnamed, unverified zero value, so
// publisher metadata can never break the read that carries it. Anything that
// is not an object, an object without a usable member, and a service that
// predates the field all read that way. Only one exact, lowercase "verified"
// member holding the JSON literal true verifies: a string, a number, a
// differently cased key, or a repeated member does not.
func parsePublisherWire(data []byte) publisherWire {
	var parsed publisherWire
	seenVerified := false
	if !objectMembers(data, func(name string, value json.RawMessage) {
		switch name {
		case "name":
			parsed.name = ""
			var text string
			if json.Unmarshal(value, &text) == nil {
				parsed.name = text
			}
		case "verified":
			parsed.verified = !seenVerified && string(bytes.TrimSpace(value)) == "true"
			seenVerified = true
		}
	}) {
		return publisherWire{}
	}
	return parsed
}

// publisherMember reads the publisher of one JSON object: the value of its
// exact, lowercase "publisher" member when the object carries that member
// once. An object that repeats the member is ambiguous, so no occurrence wins
// and the publisher reads as unnamed and unverified, as it does when the
// member is absent or the input is not an object. The count belongs to this
// one object: nothing is remembered between calls.
func publisherMember(object []byte) publisherWire {
	var publisher publisherWire
	occurrences := 0
	if !objectMembers(object, func(name string, value json.RawMessage) {
		if name == fieldPublisher {
			occurrences++
			publisher = parsePublisherWire(value)
		}
	}) || occurrences != 1 {
		return publisherWire{}
	}
	return publisher
}

// objectMembers calls visit with each member of one JSON object, in order and
// including repeated names. It reports whether data was an object it could
// read to the end.
func objectMembers(data []byte, visit func(name string, value json.RawMessage)) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if first, err := decoder.Token(); err != nil || first != json.Delim('{') {
		return false
	}
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return false
		}
		visit(name, value)
	}
	return true
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
