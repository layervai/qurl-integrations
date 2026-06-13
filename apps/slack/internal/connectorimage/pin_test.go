package connectorimage

import (
	"strings"
	"testing"
)

const (
	testConnectorImageRepo    = "ghcr.io/layervai/qurl-connector"
	testConnectorVersionImage = testConnectorImageRepo + ":v1.2.3"
	testConnectorLatestImage  = testConnectorImageRepo + ":latest"
)

func TestClassifyPin(t *testing.T) {
	t.Parallel()
	validDigest := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name  string
		image string
		want  PinStatus
	}{
		{name: "version tag", image: testConnectorVersionImage, want: Pinned},
		{name: "registry host with port and tag", image: "registry.example.com:5000/layervai/qurl-connector:v1.2.3", want: Pinned},
		{name: "uppercase non-latest tag", image: testConnectorImageRepo + ":V1", want: Pinned},
		{name: "uppercase non-latest tag with digest", image: testConnectorImageRepo + ":V1@" + validDigest, want: Pinned},
		{name: "digest", image: testConnectorImageRepo + "@" + validDigest, want: Pinned},
		{name: "registry host with port and digest", image: "registry.example.com:5000/layervai/qurl-connector@" + validDigest, want: Pinned},
		{name: "single segment image digest", image: "qurl-connector@" + validDigest, want: Pinned},
		{name: "version tag with digest", image: testConnectorVersionImage + "@" + validDigest, want: Pinned},
		{name: "latest tag with digest", image: testConnectorLatestImage + "@" + validDigest, want: LatestDigest},
		{name: "uppercase latest tag with digest", image: testConnectorImageRepo + ":LATEST@" + validDigest, want: LatestDigest},
		{name: "multi-colon latest tag with digest", image: testConnectorImageRepo + ":latest:v1@" + validDigest, want: LatestDigest},
		{name: "path component latest tag with digest", image: "ghcr.io/foo:latest/qurl-connector@" + validDigest, want: LatestDigest},
		{name: "blank image", want: Floating},
		{name: "implicit latest", image: testConnectorImageRepo, want: Floating},
		{name: "explicit latest", image: testConnectorLatestImage, want: Floating},
		{name: "uppercase latest", image: testConnectorImageRepo + ":LATEST", want: Floating},
		{name: "path component latest tag", image: "ghcr.io/foo:latest/qurl-connector:v1", want: Floating},
		{name: "path component uppercase latest tag", image: "ghcr.io/foo:LATEST/qurl-connector:v1", want: Floating},
		{name: "invalid latest-like registry port fails safe", image: "localhost:latest/qurl-connector:v1", want: MalformedReference},
		{name: "multi-colon latest tag", image: testConnectorImageRepo + ":latest:v1", want: MalformedReference},
		{name: "multi-colon non-latest tag", image: testConnectorImageRepo + ":v1:v2", want: MalformedReference},
		{name: "slashless multi-colon non-latest tag", image: "repo:v1:v2", want: MalformedReference},
		{name: "empty tag", image: testConnectorImageRepo + ":", want: MalformedReference},
		{name: "non-numeric registry port", image: "host:abc/qurl-connector:v1", want: MalformedReference},
		{name: "empty path component with tag", image: "ghcr.io//qurl-connector:v1", want: MalformedReference},
		{name: "uppercase repository namespace", image: "ghcr.io/LayerV/qurl-connector:v1", want: MalformedReference},
		{name: "uppercase registry host", image: "GHCR.IO/layervai/qurl-connector:v1", want: MalformedReference},
		{name: "uppercase registry host with digest", image: "GHCR.IO/layervai/qurl-connector@" + validDigest, want: MalformedReference},
		{name: "empty name with tag", image: ":v1", want: MalformedReference},
		{name: "empty name with latest digest", image: ":latest@" + validDigest, want: MalformedReference},
		{name: "registry port without tag", image: "localhost:5000/layervai/qurl-connector", want: Floating},
		{name: "bare localhost port", image: "localhost:5000", want: AmbiguousReference},
		{name: "mixed-case localhost port", image: "Localhost:5000", want: AmbiguousReference},
		{name: "uppercase localhost port", image: "LOCALHOST:5000", want: AmbiguousReference},
		{name: "bare dotted registry port", image: "registry.example.com:5000", want: AmbiguousReference},
		{name: "dotted slashless name with tag", image: "gcr.io:v1", want: AmbiguousReference},
		{name: "numeric tag without registry host", image: "qurl-connector:5000", want: Pinned},
		{name: "multi-colon non-latest tag with digest", image: testConnectorImageRepo + ":v1:v2@" + validDigest, want: MalformedReference},
		{name: "empty tag with digest", image: testConnectorImageRepo + ":@" + validDigest, want: MalformedReference},
		{name: "malformed digest", image: testConnectorImageRepo + "@notadigest", want: MalformedDigest},
		{name: "tagged malformed digest", image: testConnectorVersionImage + "@sha256:abc123", want: MalformedDigest},
		{name: "short sha256 digest", image: testConnectorImageRepo + "@sha256:abc123", want: MalformedDigest},
		{name: "uppercase sha256 digest", image: testConnectorImageRepo + "@sha256:" + strings.Repeat("A", 64), want: UppercaseDigest},
		{name: "nameless digest", image: "@" + validDigest, want: MalformedDigest},
		{name: "digest with sha256 image name", image: "sha256@" + validDigest, want: MalformedDigest},
		{name: "bare registry digest", image: "localhost:5000@" + validDigest, want: MalformedDigest},
		{name: "non-numeric registry port with digest", image: "host:abc/qurl-connector@" + validDigest, want: MalformedDigest},
		{name: "invalid latest-like registry port with digest", image: "registry.example.com:latest/qurl-connector@" + validDigest, want: MalformedDigest},
		{name: "trailing slash digest", image: "localhost:5000/@" + validDigest, want: MalformedDigest},
		{name: "leading slash digest", image: "/@" + validDigest, want: MalformedDigest},
		{name: "bare dotted registry digest", image: "registry.example.com@" + validDigest, want: MalformedDigest},
		{name: "bare sha256 digest", image: validDigest, want: MalformedDigest},
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
			"ghcr.io/foo:latest/" + fragment + ":v1",
			"ghcr.io/foo:LATEST/" + fragment + "@" + validDigest,
		}
		for _, image := range cases {
			if got := ClassifyPin(image); got == Pinned {
				t.Fatalf("ClassifyPin(%q) = Pinned, want latest-tagged refs rejected", image)
			}
		}
	})
}
