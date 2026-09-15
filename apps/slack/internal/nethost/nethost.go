// Package nethost holds host-name predicates shared by the Slack app's request
// handlers, its startup configuration checks, and the operator smoke commands.
//
// It is deliberately a leaf with no Slack concepts in it. IsLoopback decides whether a
// plaintext http URL is allowed in three unrelated places — the connector API base
// rendered into customer install manifests, the qURL entry host the agent inspect path
// will fetch, and the smoke commands' -base-url — so it must not live in a package
// named for any one of them. Widening it widens all three at once.
package nethost

import (
	"net"
	"strings"
)

// IsLoopback reports whether host names the local machine, and so is the one condition
// under which callers permit a plaintext http URL.
//
// It trims and lowercases before deciding, so a caller that has not already normalized
// its input still gets the same answer. Trimming accepts a host with surrounding
// Unicode whitespace, which url.URL.Hostname can return for a URL that parsed cleanly;
// such a host still resolves to nothing, and rejecting it is the caller's job if the
// caller cares about the difference.
func IsLoopback(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
