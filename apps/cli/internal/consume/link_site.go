package consume

import (
	"net/url"
	"strings"
)

// LinkSite returns the origin of the site where a person opens a resource in
// a browser with only its CRID, for example an origin of the form
// https://host, or the empty string when this install does not know it.
//
// The only place this install can learn it is the deployment settings file
// QURL_DEPLOYMENT names: the origin its CRID link entry says issued links are
// on. Nothing here invents a host. With no settings file, with a file that
// cannot be read, with a file that names no such entry, and with an origin
// that is not a bare HTTPS origin, the answer is "not known", and the caller
// gives people the CRID instead. A wrong settings file is not an error here:
// the commands that need the file to open a link report that themselves.
//
// TODO(upstream-contract): the SDK reads the link origin of its own shipped
// deployment only inside a link request and has no accessor for it. When a
// release ships a deployment that names one, this needs that accessor, or the
// install will still not know its own link site.
func (o *AccessOpener) LinkSite() string {
	d, configured, err := o.loadDeployment()
	if err != nil || !configured || d.CRIDLink == nil {
		return ""
	}
	origin := strings.TrimSpace(d.CRIDLink.LinkOrigin)
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" ||
		strings.ContainsAny(origin, "?#") {
		return ""
	}
	return origin
}

// ResourceAddress returns the address of the resource a CRID names on
// linkSite, or the empty string when linkSite is empty.
//
// The CRID goes into the address as it is, and the address is what a person
// is told to send to others. So the check is made here and not left to the
// callers: a CRID is letters and digits, which mean nothing in an address. A
// value with any other character gets no address, and the caller gives
// people the value itself instead.
//
// TODO(upstream-contract): a resource's page on the link site is the site's
// origin followed by a slash and the CRID.
func ResourceAddress(linkSite, resourceCRID string) string {
	if linkSite == "" || resourceCRID == "" || !lettersAndDigits(resourceCRID) {
		return ""
	}
	return linkSite + "/" + resourceCRID
}

// lettersAndDigits reports whether value has only ASCII letters and digits.
func lettersAndDigits(value string) bool {
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		default:
			return false
		}
	}
	return true
}
