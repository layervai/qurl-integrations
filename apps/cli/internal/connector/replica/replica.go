// Package replica normalizes the per-replica discriminator string that makes
// this process unique among co-deployed replicas of the same qURL Connector.
//
// Why this exists: when multiple Connector replicas register against the same
// tunnel server under a shared load-balancer group, each replica's proxy name
// must be unique across the group or the server rejects the second
// registration as a duplicate. The routing key is shared deliberately (that is
// what makes the replicas one balanced pool); the per-replica salt appended to
// the proxy name is what keeps the registrations distinct.
//
// This package carries only the normalization half of that mechanism: the
// glyph filter, the length cap, and the collision-safe truncation the salt
// must survive before it is rendered into a proxy name. The runtime resolver
// chain that discovers a salt (operator env, orchestrator metadata, hostname,
// random fallback) belongs to the supervisor and is not part of this package.
package replica

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// MaxDiscriminatorLen caps the discriminator at 16 characters. The rendered
// proxy name is "<route>-<discriminator>"; with typical route identifiers this
// keeps the combined name well under any reasonable wire limit. 16 characters
// of base16 carry 64 bits of entropy for randomly generated salts — enough to
// make a collision vanishingly unlikely at realistic replica counts.
const MaxDiscriminatorLen = 16

// hashSuffixLen is the length of the hex digest appended when Normalize must
// truncate a long input. 8 hex characters = 32 bits on the differentiating
// suffix; the birthday bound is approximately N^2 / 2^33 for N live replicas
// sharing a long prefix.
const hashSuffixLen = 8

// Normalize lower-cases, filters to [0-9a-z-], collapses consecutive hyphens
// to one, strips leading/trailing hyphens, and caps the result at
// MaxDiscriminatorLen. Normalize is idempotent: callers may store a canonical
// value for logging while the proxy-name renderer normalizes again at the
// wire boundary as defense in depth.
//
// Cap behavior is collision-safe for inputs that share a long prefix
// (Kubernetes pod names, compose-scaled container hostnames): when the
// filtered and collapsed value exceeds MaxDiscriminatorLen, the result keeps
// a short readable prefix of the filtered value and appends "-" plus an
// 8-hex-character SHA-256 digest of the original raw input (before
// normalization). Two names like "fileviewer-v2-66b6c48dd5-abcde" and
// "fileviewer-v2-66b6c48dd5-fghij" — which both shrink to the same 16-char
// prefix — therefore produce distinct salts. Inputs at or under the cap are
// returned as-is so short salts keep their readable spelling.
//
// The character filter is conservative on purpose: proxy names surface in
// server status views and logs, so keeping the glyph set DNS-safe is defense
// in depth for operational surfaces, not a routing requirement.
func Normalize(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	b.Grow(len(lower))
	lastWasHyphen := false
	for _, r := range lower {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			lastWasHyphen = false
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			lastWasHyphen = false
		case r == '-':
			if !lastWasHyphen && b.Len() > 0 {
				b.WriteRune(r)
				lastWasHyphen = true
			}
		}
	}
	out := b.String()
	// Strip trailing hyphens (collapse left a single hyphen in place; trim it
	// if it ended up terminal after the filter dropped the surrounding glyph).
	// Leading-hyphen suppression is handled in the loop by the b.Len() > 0
	// guard.
	out = strings.TrimRight(out, "-")
	if len(out) > MaxDiscriminatorLen {
		// Collision-safe truncation: a short prefix of the filtered value plus
		// an 8-hex digest of the ORIGINAL raw input. The digest covers the
		// differentiating suffix that pure prefix truncation would drop.
		const prefixLen = MaxDiscriminatorLen - hashSuffixLen - 1
		prefix := out[:prefixLen]
		// The prefix slice can legitimately end in a hyphen when the filtered
		// value has one at that position (e.g. "abcdef-ghijklmn" → "abcdef-").
		// Without trimming, the join below would emit a double hyphen,
		// violating the hyphen-collapse contract this function documents. The
		// trimmed prefix may shrink below prefixLen — uniqueness is unaffected
		// because the hash carries the differentiation.
		prefix = strings.TrimRight(prefix, "-")
		out = prefix + "-" + shortHash(raw, hashSuffixLen)
		// Sanity clamp in case the join produced more than the cap (it should
		// not: prefixLen + 1 + hashSuffixLen == MaxDiscriminatorLen).
		if len(out) > MaxDiscriminatorLen {
			out = out[:MaxDiscriminatorLen]
		}
	}
	return out
}

// shortHash returns the first n hex characters of sha256(raw). The hash
// domain is the original raw input, not the filtered value, so two inputs
// whose normalized prefixes are identical still diverge in the suffix (the
// unfiltered suffix bytes feed into the digest).
func shortHash(raw string, n int) string {
	sum := sha256.Sum256([]byte(raw))
	hexed := hex.EncodeToString(sum[:])
	if n > len(hexed) {
		n = len(hexed)
	}
	return hexed[:n]
}
