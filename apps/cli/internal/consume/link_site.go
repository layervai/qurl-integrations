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
// TODO(upstream-contract): a resource's page on the link site is the site's
// origin followed by a slash and the CRID.
func ResourceAddress(linkSite, resourceCRID string) string {
	if linkSite == "" || resourceCRID == "" {
		return ""
	}
	return linkSite + "/" + resourceCRID
}
