// Package connectorimage validates the qURL Connector image reference used in
// customer-facing install snippets.
package connectorimage

import (
	// Load-bearing: without this registration, dockerref.Parse rejects every
	// sha256 digest and valid digest-pinned inputs become MalformedDigest.
	_ "crypto/sha256"
	"strings"

	dockerref "github.com/distribution/reference"
)

// PinStatus describes whether an operator-provided image ref is accepted by
// the production startup policy and, if not, which startup error should explain
// it. Accepted means "specific enough for qURL startup policy": non-latest tags
// are trusted release labels by operator convention, while digest refs are the
// immutable byte-for-byte pins.
type PinStatus int

// PinStatus values returned by ClassifyPin.
const (
	Accepted PinStatus = iota
	Floating
	LatestDigest
	UppercaseDigest
	MalformedReference
	AmbiguousReference
	MalformedDigest
)

// ClassifyPin delegates Docker reference grammar to reference.Parse, then
// layers qURL policy on top. We do not use ParseNormalizedNamed because it
// rewrites slashless refs such as gcr.io:v1 or localhost:5000 into Docker Hub
// library images. Preserving the operator's input lets startup reject those
// ambiguous forms instead of silently normalizing them. The raw guards below
// preserve qURL-specific error precedence for inputs that reference.Parse
// cannot type. Parsed-path adapters defer to those raw helpers so qURL policy
// stays in one place even when slashless registry refs and bare sha256 names
// land on either side of the parser boundary. Re-check the ClassifyPin table
// and fuzz targets when upgrading github.com/distribution/reference.
func ClassifyPin(image string) PinStatus {
	name, digest, hasDigest := strings.Cut(image, "@")
	if hasDigest {
		if status, decided := classifyDigestPolicyBeforeParse(name, digest); decided {
			return status
		}
	}

	parsed, parseErr := dockerref.Parse(image)
	named, ok := parsed.(dockerref.Named)
	if parseErr != nil || !ok {
		return classifyParseFailure(name, hasDigest)
	}

	if hasDigest {
		// Uppercase image-name rejection for digest refs runs in
		// classifyDigestPolicyBeforeParse to preserve legacy error precedence.
		// Bare sha256 names keep their remediation even when the digest is
		// syntactically valid and the parser would type the ref as Digested.
		if isBareSHA256ImageName(named) {
			return MalformedDigest
		}
		// Valid @sha256 refs should parse as Digested after the digest pre-check;
		// keep this fail-closed guard in case a future parser changes that contract.
		if _, ok := parsed.(dockerref.Digested); !ok {
			return MalformedDigest
		}
		if parsedSlashlessRegistryReference(named) {
			// Slashless registry-looking digest refs preserve the legacy
			// malformed-digest remediation; tagged refs use AmbiguousReference.
			return MalformedDigest
		}
		return Accepted
	}

	tagged, ok := parsed.(dockerref.Tagged)
	if !ok {
		return Floating
	}
	// Slashless registry refs and bare sha256 names are mutually exclusive
	// because sha256 is not a registry-looking host; keep this order tied to
	// the legacy slashless-registry precedence.
	if parsedSlashlessRegistryReference(named) {
		return AmbiguousReference
	}
	if isBareSHA256ImageName(named) {
		return MalformedDigest
	}
	if strings.EqualFold(tagged.Tag(), "latest") {
		return Floating
	}
	// Uppercase path components fail reference.Parse; only an uppercase domain
	// can reach this post-parse tagged path today. If a future parser rejects
	// uppercase domains too, classifyParseFailure returns the same status.
	if hasUppercaseASCII(dockerref.Domain(named)) {
		return MalformedReference
	}
	// Non-latest tags are trusted release labels by operator convention; use
	// image@sha256:<digest> when byte-for-byte image immutability is required.
	return Accepted
}

// classifyDigestPolicyBeforeParse returns decided=false when digest validation
// passed and ClassifyPin should continue to reference.Parse for grammar typing.
// The ignored status is fail-closed so a caller bug cannot accidentally accept.
// The only decided=false case is a syntactically valid lowercase sha256 digest.
func classifyDigestPolicyBeforeParse(name, digest string) (PinStatus, bool) {
	if name == "" {
		return MalformedDigest, true
	}
	if rawNameTagStartsImmediatelyAfterSlash(name) {
		return MalformedReference, true
	}
	// Digest refs preserve the legacy digest-path precedence: uppercase image
	// names are reported before visible :latest tags, unlike the tagged path.
	if hasUppercaseASCII(rawRepositoryNameForPolicy(name)) {
		return MalformedReference, true
	}
	// The digest controls image resolution, but reject
	// repo:latest@sha256:<hex> anyway so customer-facing snippets never
	// visibly advertise the floating latest tag. This intentionally reports
	// LatestDigest before validating the digest bytes; if both are wrong, the
	// operator fixes the visible latest tag first.
	if rawNameHasLatestTag(name) {
		return LatestDigest, true
	}
	if rawNameHasMalformedTag(name) {
		return MalformedReference, true
	}
	digestStatus := classifySHA256ImageDigest(digest)
	switch digestStatus {
	case sha256DigestUppercaseHex:
		return UppercaseDigest, true
	case sha256DigestInvalid:
		return MalformedDigest, true
	case sha256DigestValid:
		return MalformedDigest, false
	default:
		// Future sha256DigestStatus values must fail closed.
		return MalformedDigest, true
	}
}

func classifyParseFailure(name string, hasDigest bool) PinStatus {
	if hasDigest {
		return MalformedDigest
	}
	if rawNameIsUntagged(name) {
		return Floating
	}
	if rawTaggedSlashlessRegistryReference(name) {
		// Slashless registry-looking empty tags, such as localhost: and gcr.io:,
		// keep the legacy ambiguous-reference message; repository-scoped empty
		// tags fall through to malformed-tag handling below.
		return AmbiguousReference
	}
	if rawNameHasMalformedTag(name) {
		return MalformedReference
	}
	// Most bare sha256:<tag> refs parse successfully and are handled through
	// the parsed adapter; uppercase SHA256 forms still fail the parser today.
	if rawNameIsBareSHA256(name) {
		return MalformedDigest
	}
	if rawNameHasLatestTag(name) {
		return Floating
	}
	// Remaining parse failures, including uppercase path components, fail closed.
	return MalformedReference
}

func rawNameIsUntagged(name string) bool {
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	return lastColon <= lastSlash
}

type sha256DigestStatus int

const (
	sha256DigestValid sha256DigestStatus = iota
	sha256DigestUppercaseHex
	sha256DigestInvalid
)

func classifySHA256ImageDigest(digest string) sha256DigestStatus {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		return sha256DigestInvalid
	}
	hex := strings.TrimPrefix(digest, prefix)
	if len(hex) != 64 {
		return sha256DigestInvalid
	}
	hasUpper := false
	for _, r := range hex {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
			hasUpper = true
		default:
			return sha256DigestInvalid
		}
	}
	// Image digests produced by GHCR/Docker for this connector are sha256.
	// Reject other OCI digest algorithms until we have a concrete need for
	// them, rather than silently widening the accepted operator input.
	// Require canonical lowercase rather than normalizing so operator config
	// stays byte-for-byte identical to the pinned digest shown to customers.
	if hasUpper {
		return sha256DigestUppercaseHex
	}
	return sha256DigestValid
}

// The raw helpers intentionally rescan these small startup-time strings so each
// qURL precedence guard stays local and auditable.
func rawNameHasLatestTag(name string) bool {
	components := strings.Split(name, "/")
	for i, component := range components {
		if i == 0 && len(components) > 1 && !firstPathComponentHasValidPort(component) {
			continue
		}
		for _, tag := range strings.Split(component, ":")[1:] {
			if strings.EqualFold(tag, "latest") {
				return true
			}
		}
	}
	return false
}

func rawRepositoryNameForPolicy(name string) string {
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	if lastColon > lastSlash {
		taglessName := name[:lastColon]
		if lastSlash < 0 && looksLikeRegistryHost(taglessName) {
			return name
		}
		return taglessName
	}
	return name
}

func rawNameHasMalformedTag(name string) bool {
	lastSlash := strings.LastIndex(name, "/")
	nameSuffix := name[lastSlash+1:]
	return strings.HasSuffix(nameSuffix, ":") || strings.Count(nameSuffix, ":") > 1
}

func rawNameTagStartsImmediatelyAfterSlash(name string) bool {
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	return lastColon == lastSlash+1
}

func rawNameIsBareSHA256(name string) bool {
	if strings.Contains(name, "/") || rawNameHasMalformedTag(name) {
		// The classifyParseFailure caller checks malformed tags first; keep this
		// helper defensive because tests and fuzz targets also call it directly.
		return false
	}
	imageName, _, _ := strings.Cut(name, ":")
	return strings.EqualFold(imageName, "sha256")
}

func rawTaggedSlashlessRegistryReference(name string) bool {
	_, _, hasTag := strings.Cut(name, ":")
	return hasTag && rawSlashlessRegistryReference(name)
}

func rawSlashlessRegistryReference(name string) bool {
	if strings.Contains(name, "/") || strings.Count(name, ":") > 1 {
		// Multi-colon slashless refs keep MalformedReference precedence instead
		// of being mistaken for ambiguous registry-looking refs.
		return false
	}
	host, _, _ := strings.Cut(name, ":")
	return looksLikeRegistryHost(host)
}

// Parsed helpers adapt reference.Parse success values back into the raw helper
// shape so qURL slashless-registry and bare-sha256 policy has one source.
func parsedSlashlessRegistryReference(named dockerref.Named) bool {
	return rawSlashlessRegistryReference(parsedNameForRawPolicy(named))
}

func isBareSHA256ImageName(named dockerref.Named) bool {
	return rawNameIsBareSHA256(parsedNameForRawPolicy(named))
}

// Re-serialize parsed refs so parse-success and parse-failure paths share the
// same raw qURL policy helpers instead of growing separate policy logic.
// This depends on reference.Parse preserving the operator's casing; do not
// switch to ParseNormalizedNamed without revisiting the slashless policy tests.
func parsedNameForRawPolicy(named dockerref.Named) string {
	name := named.Name()
	if tagged, ok := named.(dockerref.Tagged); ok {
		return name + ":" + tagged.Tag()
	}
	return name
}

func hasUppercaseASCII(value string) bool {
	for _, r := range value {
		if r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

func firstPathComponentHasValidPort(component string) bool {
	lastColon := strings.LastIndex(component, ":")
	if lastColon < 0 {
		// No port-like suffix to validate; keep scanning this component for
		// qURL policy tags such as :latest.
		return true
	}
	port := component[lastColon+1:]
	if port == "" {
		return false
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func looksLikeRegistryHost(host string) bool {
	// Reserve localhost, dotted names, and host:port forms for registry hosts,
	// while leaving bare words as valid Docker-style single-segment image names.
	return strings.EqualFold(host, "localhost") || strings.ContainsAny(host, ".:")
}
