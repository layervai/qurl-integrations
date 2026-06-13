// Package connectorimage validates the qURL Connector image reference used in
// customer-facing install snippets.
package connectorimage

import (
	_ "crypto/sha256" // register sha256 for github.com/distribution/reference digest parsing
	"strings"

	dockerref "github.com/distribution/reference"
)

// PinStatus describes whether an operator-provided image ref is pinned enough
// for production startup and, if not, which startup error should explain it.
type PinStatus int

// PinStatus values returned by ClassifyPin.
const (
	Pinned PinStatus = iota
	Floating
	LatestDigest
	UppercaseDigest
	MalformedReference
	AmbiguousReference
	MalformedDigest
)

// ClassifyPin delegates Docker reference grammar to
// github.com/distribution/reference, then layers qURL policy on top. We do not
// use ParseNormalizedNamed as the entry point because it rewrites slashless refs
// such as gcr.io:v1 or localhost:5000 into Docker Hub library images. Preserving
// the operator's input lets startup reject those ambiguous forms instead of
// silently normalizing them.
func ClassifyPin(image string) PinStatus {
	name, digest, hasDigest := strings.Cut(image, "@")
	if hasDigest {
		return classifyDigestImagePin(name, digest)
	}
	return classifyTaggedImagePin(name)
}

func classifyDigestImagePin(name, digest string) PinStatus {
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	nameSuffix := name[lastSlash+1:]
	repositoryName := imageRepositoryName(name, lastSlash, lastColon)

	if name == "" {
		return MalformedDigest
	}
	if lastColon == lastSlash+1 {
		return MalformedReference
	}
	if imageNameHasUppercase(repositoryName) {
		return MalformedReference
	}
	// The digest controls image resolution, but reject
	// repo:latest@sha256:<hex> anyway so customer-facing snippets never
	// visibly advertise the floating latest tag.
	if imageNameHasLatestTag(name) {
		return LatestDigest
	}
	if strings.HasSuffix(nameSuffix, ":") || strings.Count(nameSuffix, ":") > 1 {
		return MalformedReference
	}
	digestStatus := classifySHA256ImageDigest(digest)
	switch digestStatus {
	case sha256DigestUppercaseHex:
		return UppercaseDigest
	case sha256DigestInvalid:
		return MalformedDigest
	case sha256DigestValid:
		// Continue into the Docker reference parser below.
	default:
		// Future sha256DigestStatus values must fail closed.
		return MalformedDigest
	}
	if strings.EqualFold(repositoryName, "sha256") || slashlessRegistryReference(name, lastSlash, lastColon) {
		return MalformedDigest
	}
	if !isParsedDockerReferencePinned(name + "@" + digest) {
		return MalformedDigest
	}
	return Pinned
}

func classifyTaggedImagePin(name string) PinStatus {
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")

	if lastColon <= lastSlash {
		return Floating
	}
	// Non-digest refs surface malformed tag syntax before latest-specific
	// guidance. Digest refs check latest first so repo:latest@sha256:... never
	// renders a customer snippet that visibly advertises :latest.
	nameSuffix := name[lastSlash+1:]
	if strings.Count(nameSuffix, ":") > 1 {
		return MalformedReference
	}
	if lastSlash < 0 && looksLikeRegistryHost(name[:lastColon]) {
		return AmbiguousReference
	}
	tag := name[lastColon+1:]
	if tag == "" {
		return MalformedReference
	}
	if imageNameHasLatestTag(name) {
		return Floating
	}
	repositoryName := imageRepositoryName(name, lastSlash, lastColon)
	// Avoid treating bare digest-looking sha256:<hex> input as an image named
	// "sha256" with a tag.
	if strings.EqualFold(repositoryName, "sha256") {
		return MalformedDigest
	}
	if lastColon == lastSlash+1 || imageNameHasUppercase(repositoryName) || !isParsedDockerReferencePinned(name) {
		return MalformedReference
	}
	// Non-latest tags are trusted release labels by operator convention; use
	// image@sha256:<digest> when byte-for-byte image immutability is required.
	return Pinned
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

func imageNameHasLatestTag(name string) bool {
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

func imageRepositoryName(name string, lastSlash, lastColon int) string {
	if lastColon > lastSlash {
		taglessName := name[:lastColon]
		if lastSlash < 0 && looksLikeRegistryHost(taglessName) {
			return name
		}
		return taglessName
	}
	return name
}

func slashlessRegistryReference(name string, lastSlash, lastColon int) bool {
	if lastSlash >= 0 {
		return false
	}
	if lastColon > lastSlash {
		return looksLikeRegistryHost(name[:lastColon])
	}
	return looksLikeRegistryHost(name)
}

func isParsedDockerReferencePinned(image string) bool {
	parsed, err := dockerref.ParseAnyReference(image)
	if err != nil {
		return false
	}
	if _, ok := parsed.(dockerref.Named); !ok {
		return false
	}
	if _, ok := parsed.(dockerref.Tagged); ok {
		return true
	}
	_, ok := parsed.(dockerref.Digested)
	return ok
}

func imageNameHasUppercase(name string) bool {
	for _, r := range name {
		if r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

func firstPathComponentHasValidPort(component string) bool {
	lastColon := strings.LastIndex(component, ":")
	if lastColon < 0 {
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
