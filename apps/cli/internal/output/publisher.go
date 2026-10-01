package output

import (
	"bytes"
	"fmt"
	"strconv"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// This file is the one rendering authority for publisher metadata. Every
// command that shows a publisher goes through it, so three rules hold
// everywhere at once:
//
//   - A publisher name is self-declared text chosen by whoever owns the
//     resource. It never reaches a terminal raw: it is quoted, and control,
//     format (bidirectional overrides, zero-width), and other non-printing
//     characters are escaped.
//   - A name is never rendered without its verification status beside it.
//   - "verified" is rendered only for a publisher whose Verified field is
//     true. Every gap upstream decodes to false, so a missing or garbled
//     field can only ever read as UNVERIFIED.

// Fixed customer-facing publisher strings, registered in CustomerMessages.
const (
	labelPublisher = "Publisher:"
	labelCreated   = "Created:"

	// msgPublisherNoName stands in for a name the publisher never set.
	msgPublisherNoName = "no name provided"
	// msgPublisherUnverified is capitalized so it carries without color.
	msgPublisherUnverified = "UNVERIFIED"
	msgPublisherVerified   = "verified by LayerV"
	// msgPublisherSelfDeclared explains UNVERIFIED beside a name;
	// msgPublisherUnconfirmed explains it when there is no name to qualify.
	msgPublisherSelfDeclared = "self-declared name, not confirmed by LayerV"
	msgPublisherUnconfirmed  = "not confirmed by LayerV"

	// The stderr notice, for modes whose stdout must stay bare. Operands are
	// the status word, the quoted name (or msgPublisherNoName), and the
	// explanation.
	msgPublisherNoticeNamed           = "%s publisher %s (%s)."
	msgPublisherNoticeUnnamed         = "%s publisher, %s (%s)."
	msgPublisherNoticeVerifiedNamed   = "Publisher %s (%s)."
	msgPublisherNoticeVerifiedUnnamed = "Publisher %s, %s."
	// msgPublisherNoticeCreated follows the notice when the service gave the
	// resource's creation date.
	msgPublisherNoticeCreated = " Created %s."

	// Status notes for `qurl publisher`. The saved note names the status
	// recipients see, so it has a form for each status.
	msgPublisherNameSaved         = "Publisher name saved. Anyone who requests a link for your CRIDs sees it, marked UNVERIFIED."
	msgPublisherNameSavedVerified = "Publisher name saved. Anyone who requests a link for your CRIDs sees it."
	msgPublisherNameRemoved       = "Publisher name removed. Your CRIDs now show no publisher name."
	msgPublisherNameUnset         = "No publisher name is set. Run `qurl publisher set <name>` to add one."
)

// maxPublisherNameRunes bounds a displayed name so a service answering
// outside its contract cannot flood a terminal.
//
// TODO(upstream-contract): qurl-service accepts publisher names of 1-64
// characters. The bound here is deliberately looser, so it never cuts a name
// the service accepted.
const maxPublisherNameRunes = 256

// PublisherChange says what a `qurl publisher` invocation did, so the text
// rendering can confirm it.
type PublisherChange int

// The publisher profile outcomes.
const (
	// PublisherShown is a plain read of the profile.
	PublisherShown PublisherChange = iota
	// PublisherNameSet follows a successful `qurl publisher set`.
	PublisherNameSet
	// PublisherNameCleared follows a successful `qurl publisher clear`.
	PublisherNameCleared
)

// publisherJSON is the `publisher` object of every JSON document. verified is
// always present, so a script never has to treat a missing key as a status;
// name is omitted when the publisher set none.
type publisherJSON struct {
	Name     escapedJSONString `json:"name,omitempty"`
	Verified bool              `json:"verified"`
}

type publisherProfileJSON struct {
	Publisher publisherJSON `json:"publisher"`
}

func publisherDocument(publisher qurlapi.Publisher) publisherJSON {
	return publisherJSON{Name: escapedJSONString(publisher.Name), Verified: publisher.Verified}
}

// escapedJSONString marshals as an ordinary JSON string whose non-printing
// characters are written as \uXXXX escapes. A JSON parser reads back exactly
// the same text, but the bytes are safe to print: encoding/json escapes
// control characters on its own and leaves format characters such as
// bidirectional overrides raw, and `-o json` is often read on a terminal.
type escapedJSONString string

// MarshalJSON implements json.Marshaler.
func (s escapedJSONString) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('"')
	for _, r := range string(s) {
		switch {
		case r == '"' || r == '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case r == utf8.RuneError || !strconv.IsPrint(r):
			if r1, r2 := utf16.EncodeRune(r); r1 != utf8.RuneError {
				fmt.Fprintf(&out, `\u%04x\u%04x`, r1, r2)
			} else {
				fmt.Fprintf(&out, `\u%04x`, r)
			}
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
	return out.Bytes(), nil
}

// quotedPublisherName quotes a name and escapes every non-printing character
// in it. cut reports that the name was longer than any the service accepts
// and was shortened.
func (p *Printer) quotedPublisherName(name string) (quoted string, cut bool) {
	if runes := []rune(name); len(runes) > maxPublisherNameRunes {
		name, cut = string(runes[:maxPublisherNameRunes]), true
	}
	if p.ascii {
		return strconv.QuoteToASCII(name), cut
	}
	return strconv.Quote(name), cut
}

// publisherName is the only form in which a name sits beside other text:
// quoted, with every non-printing character escaped.
func (p *Printer) publisherName(name string) string {
	quoted, cut := p.quotedPublisherName(name)
	if cut {
		quoted += p.ellipsis()
	}
	return quoted
}

// escapedPublisherName is the same escaping without the surrounding quotes.
// It is the bare-value form for `qurl publisher --quiet`, where no label
// stands beside the name: a name the service accepts prints exactly as it
// is, and anything else prints escaped.
func (p *Printer) escapedPublisherName(name string) string {
	quoted, cut := p.quotedPublisherName(name)
	escaped := quoted[1 : len(quoted)-1]
	if cut {
		escaped += p.ellipsis()
	}
	return escaped
}

// dash separates a name from its status, degraded for non-UTF-8 locales.
func (p *Printer) dash() string {
	if p.ascii {
		return "-"
	}
	return "—"
}

// unverifiedLabel renders the status word. Bold yellow when color is on; the
// capitals carry it when color is off.
func (p *Printer) unverifiedLabel() string {
	return p.style(ansiBold+ansiYellow, msgPublisherUnverified)
}

// publisherParts returns the name as displayed and the explanation of an
// unverified status that fits it.
func (p *Printer) publisherParts(publisher qurlapi.Publisher) (name, explanation string, named bool) {
	if publisher.Name == "" {
		return msgPublisherNoName, msgPublisherUnconfirmed, false
	}
	return p.publisherName(publisher.Name), msgPublisherSelfDeclared, true
}

// publisherStatus is the value of every `Publisher:` row:
//
//	"Acme Docs" — UNVERIFIED (self-declared name, not confirmed by LayerV)
//	no name provided — UNVERIFIED (not confirmed by LayerV)
//
// It carries ANSI styling when color is on. tabwriter counts those bytes as
// cell width, so in a table this value must stay the last cell of its line
// (a trailing cell is never padded).
func (p *Printer) publisherStatus(publisher qurlapi.Publisher) string {
	name, explanation, _ := p.publisherParts(publisher)
	if publisher.Verified {
		return fmt.Sprintf("%s %s %s", name, p.dash(), p.green(msgPublisherVerified))
	}
	return fmt.Sprintf("%s %s %s (%s)", name, p.dash(), p.unverifiedLabel(), explanation)
}

// createdDate is the absolute UTC date of a creation time, or empty when the
// service gave none.
func createdDate(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.DateOnly)
}

// createdText is the value of a `Created:` row: the absolute UTC date plus
// the relative form, or empty when the service gave no date.
func (p *Printer) createdText(t *time.Time) string {
	date := createdDate(t)
	if date == "" {
		return ""
	}
	return fmt.Sprintf("%s (%s)", date, p.relativeTime(*t))
}

// publisherNotice builds the one-line form for stderr and reports whether it
// is a warning (an unverified publisher) or a plain note.
func (p *Printer) publisherNotice(publisher qurlapi.Publisher, createdAt *time.Time) (line string, warning bool) {
	name, explanation, named := p.publisherParts(publisher)
	switch {
	case publisher.Verified && named:
		line = fmt.Sprintf(msgPublisherNoticeVerifiedNamed, name, msgPublisherVerified)
	case publisher.Verified:
		line = fmt.Sprintf(msgPublisherNoticeVerifiedUnnamed, msgPublisherVerified, name)
	case named:
		line = fmt.Sprintf(msgPublisherNoticeNamed, p.unverifiedLabel(), name, explanation)
	default:
		line = fmt.Sprintf(msgPublisherNoticeUnnamed, p.unverifiedLabel(), name, explanation)
	}
	if date := createdDate(createdAt); date != "" {
		line += fmt.Sprintf(msgPublisherNoticeCreated, date)
	}
	return line, !publisher.Verified
}

// PublisherNotice writes the publisher and creation date of a shared resource
// to stderr as one line, for the modes whose stdout cannot carry them: a bare
// link in a pipe, or the raw bytes of a download. --quiet means "the primary
// value only" and JSON carries the same facts in its document, so both
// suppress it.
func (p *Printer) PublisherNotice(link *qurlapi.ShareLink) {
	if p.quiet || p.format == FormatJSON {
		return
	}
	line, warning := p.publisherNotice(link.Publisher, link.ResourceCreatedAt)
	if warning {
		p.Warnf("%s", line)
		return
	}
	p.Notef("%s", line)
}

// PublisherProfile renders the owner's own publisher profile for
// `qurl publisher`. The profile is data, so the text row goes to stdout like
// whoami; the confirmation of a change is a status note on stderr. --quiet
// prints the bare escaped name, or nothing when none is set.
func (p *Printer) PublisherProfile(publisher *qurlapi.Publisher, change PublisherChange) error {
	switch {
	case p.format == FormatJSON:
		return p.writeJSON(publisherProfileJSON{Publisher: publisherDocument(*publisher)})
	case p.quiet:
		if publisher.Name == "" {
			return nil
		}
		_, err := fmt.Fprintln(p.out, p.escapedPublisherName(publisher.Name))
		return err
	}
	if _, err := fmt.Fprintf(p.out, "%s %s\n", p.bold(labelPublisher), p.publisherStatus(*publisher)); err != nil {
		return err
	}
	switch {
	case change == PublisherNameSet && publisher.Name != "" && publisher.Verified:
		p.Notef("%s", msgPublisherNameSavedVerified)
	case change == PublisherNameSet && publisher.Name != "":
		p.Notef("%s", msgPublisherNameSaved)
	case change == PublisherNameCleared && publisher.Name == "":
		p.Notef("%s", msgPublisherNameRemoved)
	case change == PublisherShown && publisher.Name == "":
		p.Notef("%s", msgPublisherNameUnset)
	}
	return nil
}
