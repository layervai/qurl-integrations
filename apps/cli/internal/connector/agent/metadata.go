package agent

import (
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// metadataHostnameFallback is the deterministic hostname stamped into
// registration metadata when the real hostname is unavailable or unsafe to
// send.
const metadataHostnameFallback = "qurl-connector"

// timestampVersionRe matches build versions that append a numeric timestamp
// component to a semver core (v1.2.3.20260801). Registration metadata reports
// the semver core; the timestamp is a local build artifact.
var timestampVersionRe = regexp.MustCompile(`^([vV]?)(\d+\.\d+\.\d+)\.\d+$`)

// Hostname returns the local hostname normalized for registration metadata,
// or the deterministic fallback when it is unavailable.
func Hostname() string {
	host, err := os.Hostname()
	if err != nil {
		return metadataHostnameFallback
	}
	return normalizeHostname(host)
}

// normalizeHostname bounds the hostname for the wire. An invalid encoding is
// not safe to reinterpret; the deterministic fallback is used instead. For
// valid input, complete trailing runes are removed until truncation cannot
// split one.
func normalizeHostname(host string) string {
	if !utf8.ValidString(host) {
		return metadataHostnameFallback
	}
	if strings.TrimSpace(host) == "" {
		return metadataHostnameFallback
	}
	for len(host) > 255 {
		_, size := utf8.DecodeLastRuneInString(host)
		host = host[:len(host)-size]
	}
	return host
}

// ClientVersionMeta normalizes the build version reported in registration
// metadata: a timestamped local build collapses to its semver core, an empty
// version reports "dev", and anything else is passed through trimmed.
func ClientVersionMeta(clientVersion string) string {
	trimmed := strings.TrimSpace(clientVersion)
	if match := timestampVersionRe.FindStringSubmatch(trimmed); match != nil {
		return match[1] + match[2]
	}
	if trimmed == "" {
		return "dev"
	}
	return trimmed
}
