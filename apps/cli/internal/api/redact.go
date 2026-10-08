package qurlapi

import "regexp"

// Secret-shaped patterns that must never reach a diagnostic surface: qURL
// API keys and bearer credentials. Applied to every verbose transport line
// here and to every stderr rendering in the output package, so a server
// error that echoes a credential back cannot leak it into a terminal
// scrollback or a pasted bug report.
var (
	apiKeyPattern = regexp.MustCompile(`lv_(?:live|test)_[A-Za-z0-9_-]+`)
	bearerPattern = regexp.MustCompile(`(?i)bearer\s+[^\s"']+`)
	// requestCodePathPattern is the code of an access request where a
	// request path carries it: the six digits after /access-requests/. The
	// code gives a person access for as long as their request is pending,
	// and the publisher typed it, so it is on their command line already.
	// It must not also be in a diagnostic line, which is what gets pasted
	// into a chat or a ticket. A device id in the same place is not six
	// digits and is left as it is: it gives nobody access.
	requestCodePathPattern = regexp.MustCompile(`(/access-requests/)\d{6}\b`)
)

// Redact masks credential-shaped substrings in s. Data outputs (the minted
// link on stdout, JSON documents) are intentionally not run through this —
// they are the command's product and must stay byte-faithful; redaction
// covers the diagnostic surfaces.
func Redact(s string) string {
	s = apiKeyPattern.ReplaceAllString(s, "lv_***")
	s = requestCodePathPattern.ReplaceAllString(s, "${1}******")
	return bearerPattern.ReplaceAllString(s, "Bearer ***")
}
