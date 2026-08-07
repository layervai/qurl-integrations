package connectorimage

import "testing"

func TestClassifyPin(t *testing.T) {
	tests := []struct {
		name   string
		image  string
		expect PinStatus
	}{
		// Accepted: valid non-latest tags
		{name: "tagged image", image: "ghcr.io/layervai/qurl-connector:v1.2.3", expect: Accepted},
		{name: "tagged image no registry", image: "myimage:v1.0.0", expect: Accepted},
		{name: "tagged image with path", image: "registry.example.com/org/repo:release", expect: Accepted},
		{name: "tagged localhost", image: "localhost:5000/myimage:v1", expect: Accepted},

		// Accepted: valid digest refs
		{name: "digest pinned", image: "ghcr.io/layervai/qurl-connector@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", expect: Accepted},
		{name: "digest with tag", image: "ghcr.io/layervai/qurl-connector:v1@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", expect: Accepted},

		// Floating: no tag or :latest
		{name: "no tag", image: "ghcr.io/layervai/qurl-connector", expect: Floating},
		{name: "latest tag", image: "ghcr.io/layervai/qurl-connector:latest", expect: Floating},
		{name: "latest tag uppercase", image: "ghcr.io/layervai/qurl-connector:LATEST", expect: Floating},
		{name: "latest tag mixed case", image: "ghcr.io/layervai/qurl-connector:Latest", expect: Floating},
		{name: "simple image no tag", image: "myimage", expect: Floating},

		// LatestDigest: repo:latest@sha256:...
		{name: "latest with digest", image: "ghcr.io/layervai/qurl-connector:latest@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", expect: LatestDigest},
		{name: "LATEST with digest", image: "ghcr.io/layervai/qurl-connector:LATEST@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", expect: LatestDigest},

		// UppercaseDigest: uppercase hex in digest
		{name: "uppercase digest hex", image: "ghcr.io/layervai/qurl-connector@sha256:ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789", expect: UppercaseDigest},
		{name: "mixed case digest hex", image: "ghcr.io/layervai/qurl-connector@sha256:AbCdEf0123456789abcdef0123456789abcdef0123456789abcdef0123456789", expect: UppercaseDigest},

		// MalformedReference: uppercase path, malformed tags
		{name: "uppercase domain", image: "GHCR.IO/layervai/qurl-connector:v1", expect: MalformedReference},
		{name: "uppercase path in digest ref", image: "ghcr.io/LayerVAI/qurl-connector@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", expect: MalformedReference},
		{name: "empty tag", image: "ghcr.io/layervai/qurl-connector:", expect: MalformedReference},
		{name: "double colon tag", image: "ghcr.io/layervai/qurl-connector:v1:v2", expect: MalformedReference},
		{name: "tag after slash", image: "ghcr.io/:v1", expect: MalformedReference},

		// AmbiguousReference: slashless registry refs
		{name: "slashless registry with tag", image: "gcr.io:v1", expect: AmbiguousReference},
		{name: "slashless localhost with tag", image: "localhost:v1", expect: AmbiguousReference},
		{name: "slashless dotted host with tag", image: "registry.example.com:v1", expect: AmbiguousReference},
		{name: "slashless registry empty tag", image: "gcr.io:", expect: AmbiguousReference},

		// MalformedDigest: invalid digest format, bare sha256 names
		{name: "bare sha256 tagged", image: "sha256:v1.0.0", expect: MalformedDigest},
		{name: "bare SHA256 tagged uppercase", image: "SHA256:v1.0.0", expect: MalformedDigest},
		{name: "short digest", image: "ghcr.io/layervai/qurl-connector@sha256:abcdef", expect: MalformedDigest},
		{name: "invalid digest chars", image: "ghcr.io/layervai/qurl-connector@sha256:ghijkl0123456789abcdef0123456789abcdef0123456789abcdef0123456789", expect: MalformedDigest},
		{name: "wrong digest algorithm", image: "ghcr.io/layervai/qurl-connector@md5:abcdef0123456789abcdef0123456789", expect: MalformedDigest},
		{name: "empty name with digest", image: "@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", expect: MalformedDigest},
		{name: "slashless registry with digest", image: "gcr.io@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", expect: MalformedDigest},
		{name: "bare sha256 with digest", image: "sha256@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", expect: MalformedDigest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyPin(tc.image)
			if got != tc.expect {
				t.Errorf("ClassifyPin(%q) = %v, want %v", tc.image, statusName(got), statusName(tc.expect))
			}
		})
	}
}

func statusName(s PinStatus) string {
	switch s {
	case Accepted:
		return "Accepted"
	case Floating:
		return "Floating"
	case LatestDigest:
		return "LatestDigest"
	case UppercaseDigest:
		return "UppercaseDigest"
	case MalformedReference:
		return "MalformedReference"
	case AmbiguousReference:
		return "AmbiguousReference"
	case MalformedDigest:
		return "MalformedDigest"
	default:
		return "Unknown"
	}
}

func FuzzClassifyPin(f *testing.F) {
	// Seed corpus with representative inputs
	seeds := []string{
		"ghcr.io/layervai/qurl-connector:v1.2.3",
		"ghcr.io/layervai/qurl-connector:latest",
		"ghcr.io/layervai/qurl-connector",
		"ghcr.io/layervai/qurl-connector@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"myimage:v1.0.0",
		"myimage",
		"localhost:5000/myimage:v1",
		"gcr.io:v1",
		"sha256:v1.0.0",
		"GHCR.IO/layervai/qurl-connector:v1",
		"",
		"@sha256:abc",
		"image:",
		"image::",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, image string) {
		// ClassifyPin should never panic regardless of input
		status := ClassifyPin(image)
		// Status should be a valid PinStatus value
		if status < Accepted || status > MalformedDigest {
			t.Errorf("ClassifyPin(%q) returned invalid status %d", image, status)
		}
	})
}
