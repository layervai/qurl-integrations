package connectorimage

import (
	"regexp"
	"strings"
	"testing"

	dockerref "github.com/distribution/reference"
)

const (
	testConnectorImageRepo    = "ghcr.io/layervai/qurl-connector"
	testConnectorVersionImage = testConnectorImageRepo + ":v1.2.3"
	testConnectorLatestImage  = testConnectorImageRepo + ":latest"
	testConnectorReleaseTag   = "release_2026-06-13"
	testBareSHA256Image       = "sha256"
)

var testDockerTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func TestClassifyPin(t *testing.T) {
	t.Parallel()
	validDigest := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name  string
		image string
		want  PinStatus
	}{
		{name: "version tag", image: testConnectorVersionImage, want: Accepted},
		{name: "registry host with port and tag", image: "registry.example.com:5000/layervai/qurl-connector:v1.2.3", want: Accepted},
		{name: "uppercase non-latest tag", image: testConnectorImageRepo + ":V1", want: Accepted},
		{name: "uppercase non-latest tag with digest", image: testConnectorImageRepo + ":V1@" + validDigest, want: Accepted},
		{name: "digest", image: testConnectorImageRepo + "@" + validDigest, want: Accepted},
		{name: "registry host with port and digest", image: "registry.example.com:5000/layervai/qurl-connector@" + validDigest, want: Accepted},
		{name: "single segment image digest", image: "qurl-connector@" + validDigest, want: Accepted},
		{name: "version tag with digest", image: testConnectorVersionImage + "@" + validDigest, want: Accepted},
		{name: "latest tag with digest", image: testConnectorLatestImage + "@" + validDigest, want: LatestDigest},
		{name: "uppercase latest tag with digest", image: testConnectorImageRepo + ":LATEST@" + validDigest, want: LatestDigest},
		{name: "multi-colon latest tag with digest", image: testConnectorImageRepo + ":latest:v1@" + validDigest, want: LatestDigest},
		{name: "path component latest tag with digest", image: "ghcr.io/foo:latest/qurl-connector@" + validDigest, want: LatestDigest},
		{name: "uppercase registry host with latest digest", image: "GHCR.IO/layervai/qurl-connector:latest@" + validDigest, want: MalformedReference},
		{name: "uppercase registry host with version digest", image: "GHCR.IO/layervai/qurl-connector:v1@" + validDigest, want: MalformedReference},
		{name: "blank image", want: Floating},
		{name: "implicit latest", image: testConnectorImageRepo, want: Floating},
		{name: "explicit latest", image: testConnectorLatestImage, want: Floating},
		{name: "uppercase latest", image: testConnectorImageRepo + ":LATEST", want: Floating},
		{name: "uppercase registry host with latest tag", image: "GHCR.IO/layervai/qurl-connector:LATEST", want: Floating},
		{name: "path component latest tag", image: "ghcr.io/foo:latest/qurl-connector:v1", want: Floating},
		{name: "path component uppercase latest tag", image: "ghcr.io/foo:LATEST/qurl-connector:v1", want: Floating},
		{name: "uppercase bare image name", image: "QURL-Connector", want: Floating},
		{name: "tagged uppercase bare image name", image: "QURL-Connector:v1", want: MalformedReference},
		{name: "uppercase untagged repository namespace", image: "ghcr.io/LayerV/qurl-connector", want: Floating},
		{name: "bare sha256 image name", image: testBareSHA256Image, want: Floating},
		{name: "tagged bare sha256 image name", image: testBareSHA256Image + ":v1", want: MalformedDigest},
		{name: "empty tag bare sha256 image name", image: testBareSHA256Image + ":", want: MalformedReference},
		{name: "invalid latest-like registry port fails safe", image: "localhost:latest/qurl-connector:v1", want: MalformedReference},
		{name: "multi-colon latest tag", image: testConnectorImageRepo + ":latest:v1", want: MalformedReference},
		{name: "multi-colon non-latest tag", image: testConnectorImageRepo + ":v1:v2", want: MalformedReference},
		{name: "slashless multi-colon non-latest tag", image: "repo:v1:v2", want: MalformedReference},
		{name: "slashless dotted registry multi-colon tag", image: "gcr.io:v1:v2", want: MalformedReference},
		{name: "empty tag", image: testConnectorImageRepo + ":", want: MalformedReference},
		{name: "non-numeric registry port", image: "host:abc/qurl-connector:v1", want: MalformedReference},
		{name: "empty path component with tag", image: "ghcr.io//qurl-connector:v1", want: MalformedReference},
		{name: "uppercase repository namespace", image: "ghcr.io/LayerV/qurl-connector:v1", want: MalformedReference},
		{name: "uppercase registry host", image: "GHCR.IO/layervai/qurl-connector:v1", want: MalformedReference},
		{name: "uppercase registry host with digest", image: "GHCR.IO/layervai/qurl-connector@" + validDigest, want: MalformedReference},
		{name: "uppercase repository namespace with digest", image: "ghcr.io/LayerV/qurl-connector@" + validDigest, want: MalformedReference},
		{name: "empty name with tag", image: ":v1", want: MalformedReference},
		{name: "empty name with latest digest", image: ":latest@" + validDigest, want: MalformedReference},
		{name: "registry port without tag", image: "localhost:5000/layervai/qurl-connector", want: Floating},
		{name: "bare localhost port", image: "localhost:5000", want: AmbiguousReference},
		{name: "mixed-case localhost port", image: "Localhost:5000", want: AmbiguousReference},
		{name: "uppercase localhost port", image: "LOCALHOST:5000", want: AmbiguousReference},
		{name: "bare dotted registry port", image: "registry.example.com:5000", want: AmbiguousReference},
		{name: "bare localhost empty tag", image: "localhost:", want: AmbiguousReference},
		{name: "bare localhost with latest tag", image: "localhost:latest", want: AmbiguousReference},
		{name: "bare dotted registry empty tag", image: "gcr.io:", want: AmbiguousReference},
		{name: "dotted slashless name with tag", image: "gcr.io:v1", want: AmbiguousReference},
		{name: "slashless dotted registry with uppercase latest tag", image: "gcr.io:LATEST", want: AmbiguousReference},
		{name: "numeric tag without registry host", image: "qurl-connector:5000", want: Accepted},
		{name: "multi-colon non-latest tag with digest", image: testConnectorImageRepo + ":v1:v2@" + validDigest, want: MalformedReference},
		{name: "slashless dotted registry multi-colon tag with digest", image: "gcr.io:v1:v2@" + validDigest, want: MalformedReference},
		{name: "empty tag with digest", image: testConnectorImageRepo + ":@" + validDigest, want: MalformedReference},
		{name: "malformed digest", image: testConnectorImageRepo + "@notadigest", want: MalformedDigest},
		{name: "multiple digest separators", image: testConnectorImageRepo + "@" + validDigest + "@extra", want: MalformedDigest},
		{name: "tagged malformed digest", image: testConnectorVersionImage + "@sha256:abc123", want: MalformedDigest},
		{name: "short sha256 digest", image: testConnectorImageRepo + "@sha256:abc123", want: MalformedDigest},
		{name: "uppercase sha256 digest", image: testConnectorImageRepo + "@sha256:" + strings.Repeat("A", 64), want: UppercaseDigest},
		{name: "nameless digest", image: "@" + validDigest, want: MalformedDigest},
		{name: "digest with sha256 image name", image: "sha256@" + validDigest, want: MalformedDigest},
		{name: "tagged bare sha256 digest", image: testBareSHA256Image + ":v1@" + validDigest, want: MalformedDigest},
		{name: "bare registry digest", image: "localhost:5000@" + validDigest, want: MalformedDigest},
		{name: "tagged slashless registry digest", image: "gcr.io:v1@" + validDigest, want: MalformedDigest},
		{name: "non-numeric registry port with digest", image: "host:abc/qurl-connector@" + validDigest, want: MalformedDigest},
		{name: "invalid latest-like registry port with digest", image: "registry.example.com:latest/qurl-connector@" + validDigest, want: MalformedDigest},
		{name: "trailing slash digest", image: "localhost:5000/@" + validDigest, want: MalformedDigest},
		{name: "leading slash digest", image: "/@" + validDigest, want: MalformedDigest},
		{name: "bare dotted registry digest", image: "registry.example.com@" + validDigest, want: MalformedDigest},
		{name: "bare sha256 digest", image: validDigest, want: MalformedDigest},
		{name: "tagged bare sha256 with latest", image: testBareSHA256Image + ":latest", want: MalformedDigest},
		{name: "uppercase bare sha256 digest", image: "SHA256:" + strings.Repeat("a", 64), want: MalformedDigest},
		{name: "short bare sha256 digest", image: "sha256:abc123", want: MalformedDigest},
		{name: "at sign in non-digest suffix", image: testConnectorImageRepo + ":weird@tag", want: MalformedDigest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyPin(tc.image); got != tc.want {
				t.Fatalf("ClassifyPin(%q) = %v, want %v", tc.image, got, tc.want)
			}
		})
	}
}

func TestRawNameIsBareSHA256RejectsMalformedTags(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"sha256:", "sha256:v1:v2"} {
		if rawNameIsBareSHA256(name) {
			t.Fatalf("rawNameIsBareSHA256(%q) = true, want false", name)
		}
	}
}

func FuzzClassifyPinKeepsSlashlessPolicyStatuses(f *testing.F) {
	for _, seed := range []string{"v1", "LATEST", "5000", testConnectorReleaseTag} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, tag string) {
		if !testDockerTagPattern.MatchString(tag) {
			return
		}
		cases := []struct {
			image string
			want  PinStatus
		}{
			{image: "gcr.io:" + tag, want: AmbiguousReference},
			{image: "localhost:" + tag, want: AmbiguousReference},
			{image: "sha256:" + tag, want: MalformedDigest},
		}
		for _, tc := range cases {
			if got := ClassifyPin(tc.image); got != tc.want {
				t.Fatalf("ClassifyPin(%q) = %v, want %v", tc.image, got, tc.want)
			}
		}
	})
}

// Parsed refs can reach slashless-registry and bare-sha256 policy through a
// different parser branch; this pins their raw-name adapter against the raw
// helper source of truth.
func FuzzSlashlessPolicyHelpersStayAligned(f *testing.F) {
	validDigest := "sha256:" + strings.Repeat("a", 64)
	for _, seed := range []struct {
		name string
		tag  string
	}{
		{name: "gcr.io", tag: "v1"},
		{name: "localhost", tag: "LATEST"},
		{name: testBareSHA256Image, tag: "v1"},
		{name: "qurl-connector", tag: "5000"},
		{name: "registry.example.com", tag: testConnectorReleaseTag},
	} {
		f.Add(seed.name, seed.tag)
	}
	f.Fuzz(func(t *testing.T, name, tag string) {
		// Slash-containing names are out of scope: both helper families reject
		// them before reaching parser-specific slashless or bare-sha256 policy.
		if strings.ContainsAny(name, "/:@") || !testDockerTagPattern.MatchString(tag) {
			return
		}

		taggedName := name + ":" + tag
		parsed, parseErr := dockerref.Parse(taggedName)
		named, ok := parsed.(dockerref.Named)
		if parseErr != nil || !ok {
			return
		}

		if got, want := parsedSlashlessRegistryReference(named), rawTaggedSlashlessRegistryReference(taggedName); got != want {
			t.Fatalf("slashless registry helper mismatch for %q: parsed=%v raw=%v", taggedName, got, want)
		}
		emptyTagName := name + ":"
		if got, want := parsedSlashlessRegistryReference(named), rawTaggedSlashlessRegistryReference(emptyTagName); got != want {
			t.Fatalf("empty-tag slashless helper mismatch for %q via %q: parsed=%v raw=%v", emptyTagName, taggedName, got, want)
		}
		if got, want := isBareSHA256ImageName(named), rawNameIsBareSHA256(taggedName); got != want {
			t.Fatalf("bare sha256 helper mismatch for %q: parsed=%v raw=%v", taggedName, got, want)
		}

		digestName := taggedName + "@" + validDigest
		digestParsed, digestParseErr := dockerref.Parse(digestName)
		digestNamed, ok := digestParsed.(dockerref.Named)
		if digestParseErr == nil && ok {
			if got, want := parsedSlashlessRegistryReference(digestNamed), rawTaggedSlashlessRegistryReference(taggedName); got != want {
				t.Fatalf("digest slashless helper mismatch for %q via %q: parsed=%v raw=%v", digestName, taggedName, got, want)
			}
			if got, want := isBareSHA256ImageName(digestNamed), rawNameIsBareSHA256(taggedName); got != want {
				t.Fatalf("digest bare sha256 helper mismatch for %q via %q: parsed=%v raw=%v", digestName, taggedName, got, want)
			}
		}

		bareParsed, bareParseErr := dockerref.Parse(name)
		bareNamed, ok := bareParsed.(dockerref.Named)
		if bareParseErr != nil || !ok {
			return
		}
		if got, want := isBareSHA256ImageName(bareNamed), rawNameIsBareSHA256(name); got != want {
			t.Fatalf("bare sha256 helper mismatch for untagged %q: parsed=%v raw=%v", name, got, want)
		}
	})
}

func FuzzClassifyPinRejectsLatest(f *testing.F) {
	for _, seed := range []string{
		"",
		"qurl-connector",
		testConnectorImageRepo,
		"registry.example.com:5000/layervai/qurl-connector",
		"sha256",
	} {
		f.Add(seed)
	}
	validDigest := "sha256:" + strings.Repeat("a", 64)
	f.Fuzz(func(t *testing.T, fragment string) {
		cases := []string{
			fragment + ":latest",
			fragment + ":LATEST",
			fragment + ":latest@" + validDigest,
			fragment + ":latest/qurl-connector:v1",
			fragment + ":LATEST/qurl-connector@" + validDigest,
			"ghcr.io/foo:latest/" + fragment + ":v1",
			"ghcr.io/foo:LATEST/" + fragment + "@" + validDigest,
		}
		for _, image := range cases {
			if got := ClassifyPin(image); got == Accepted {
				t.Fatalf("ClassifyPin(%q) = Accepted, want latest-tagged refs rejected", image)
			}
		}
	})
}

func FuzzClassifyPinKeepsKnownGoodPins(f *testing.F) {
	for _, seed := range []string{"v1.2.3", "V1", testConnectorReleaseTag} {
		f.Add(seed)
	}
	validDigest := "sha256:" + strings.Repeat("a", 64)
	f.Fuzz(func(t *testing.T, tag string) {
		if !testDockerTagPattern.MatchString(tag) || strings.EqualFold(tag, "latest") {
			return
		}
		cases := []string{
			testConnectorImageRepo + ":" + tag,
			testConnectorImageRepo + ":" + tag + "@" + validDigest,
			testConnectorImageRepo + "@" + validDigest,
		}
		for _, image := range cases {
			if got := ClassifyPin(image); got != Accepted {
				t.Fatalf("ClassifyPin(%q) = %v, want Accepted", image, got)
			}
		}
	})
}
