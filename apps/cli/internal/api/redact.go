package qurlapi

import "regexp"

// Secret-shaped patterns that must never reach a diagnostic surface: qURL
// API keys and bearer credentials. Applied to every verbose transport line
// here and to every stderr rendering in the output package, so a server
// error that echoes a credential back cannot leak it into a terminal
// scrollback or a pasted bug report.
var (
	apiKeyPattern = regexp.MustCompile(`lv_(?:live|test)_[A-Za-z0-9]+`)
	bearerPattern = regexp.MustCompile(`(?i)bearer\s+[^\s"']+`)
)

// Redact masks credential-shaped substrings in s. Data outputs (the minted
// link on stdout, JSON documents) are intentionally not run through this —
// they are the command's product and must stay byte-faithful; redaction
// covers the diagnostic surfaces.
func Redact(s string) string {
	s = apiKeyPattern.ReplaceAllString(s, "lv_***")
	return bearerPattern.ReplaceAllString(s, "Bearer ***")
}
