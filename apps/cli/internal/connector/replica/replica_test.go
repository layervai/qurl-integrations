package replica

import (
	"strconv"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	t.Parallel()
	// Cases that fit within MaxDiscriminatorLen pass through cleanly.
	shortCases := []struct {
		in, want string
	}{
		{"REPLICA-7", "replica-7"},
		{"  trim  ", "trim"},
		{"a/b/c", "abc"},
		{"a_b_c", "abc"},
		{"!!!", ""},
		{"", ""},
		// Hyphen-collapse plus leading/trailing strip: a raw "-foo--bar-"
		// must not survive as "-foo--bar-".
		{"-foo--bar-", "foo-bar"},
		{"---hello---world---", "hello-world"},
		// Mixed glyphs: the filter may leave a trailing hyphen post-filter;
		// the post-collapse trim must scrub it.
		{"abc-!!!", "abc"},
		// Only ASCII digits are allowed. unicode.IsDigit admits non-DNS-safe
		// glyphs such as Arabic-Indic digits, which would violate the
		// documented [0-9a-z-] wire contract.
		{"replica-٥", "replica"},
	}
	for _, tc := range shortCases {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeIdempotent(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"REPLICA_1!",
		"abcdef-ghijklmnop",
		"fileviewer-v2-66b6c48dd5-abcde",
		"---a---b---c---",
	} {
		once := Normalize(in)
		twice := Normalize(once)
		if twice != once {
			t.Errorf("Normalize(Normalize(%q)) = %q, want %q", in, twice, once)
		}
	}
}

// TestNormalizeLongInputCollisionSafe: Kubernetes pod hostnames (and
// compose-scaled containers) share a long prefix and differ only in their
// suffix. Plain prefix truncation would collapse them all to the same value
// and re-introduce the duplicate-proxy-name collision the salt exists to fix.
// The hash suffix folds the ORIGINAL raw input into the truncated output so
// distinct long inputs stay distinct.
func TestNormalizeLongInputCollisionSafe(t *testing.T) {
	t.Parallel()
	// Two Kubernetes pod names from the same ReplicaSet — the suffix is the
	// differentiating bit.
	a := "fileviewer-v2-66b6c48dd5-abcde"
	b := "fileviewer-v2-66b6c48dd5-fghij"

	gotA := Normalize(a)
	gotB := Normalize(b)

	if gotA == gotB {
		t.Errorf("Normalize collided on long-shared-prefix inputs: %q and %q both became %q — prefix truncation would re-introduce the duplicate-proxy-name collision the salt exists to fix",
			a, b, gotA)
	}
	if len(gotA) > MaxDiscriminatorLen {
		t.Errorf("Normalize(%q) = %q (len %d), exceeds MaxDiscriminatorLen %d",
			a, gotA, len(gotA), MaxDiscriminatorLen)
	}
	if len(gotB) > MaxDiscriminatorLen {
		t.Errorf("Normalize(%q) = %q (len %d), exceeds MaxDiscriminatorLen %d",
			b, gotB, len(gotB), MaxDiscriminatorLen)
	}

	// Deterministic: the same input always produces the same salt.
	if Normalize(a) != gotA {
		t.Errorf("Normalize not deterministic on %q", a)
	}

	// docker compose --scale=N shape: project_svc_1, project_svc_2.
	c1 := "myproject_connector_1"
	c2 := "myproject_connector_2"
	gotC1 := Normalize(c1)
	gotC2 := Normalize(c2)
	if gotC1 == gotC2 {
		t.Errorf("Normalize collided on compose-shaped names: %q and %q both became %q",
			c1, c2, gotC1)
	}

	seen := map[string]string{}
	for i := range 300 {
		name := "fileviewer-v2-66b6c48dd5-replica-" + strconv.Itoa(i)
		got := Normalize(name)
		if prev, ok := seen[got]; ok {
			t.Fatalf("Normalize collided in 300-replica same-prefix sample: %q and %q both became %q", prev, name, got)
		}
		seen[got] = name
	}
}

// TestNormalizeShortInputStablePrefix: inputs that fit within
// MaxDiscriminatorLen go through unchanged (no hash suffix). A normal
// hostname like "pod-abc-7" stays human-readable in the proxy name.
func TestNormalizeShortInputStablePrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"pod-abc-7", "pod-abc-7"},
		{"host123", "host123"},
		{"a-b-c-d-e", "a-b-c-d-e"},
	}
	for _, tc := range cases {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q (short input must pass through)", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeNoDoubleHyphenAfterTruncation: the prefix slice of a
// hyphen-collapsed value can legitimately end in a hyphen (for example
// "abcdef-ghijklmn" → prefix "abcdef-"). Without trimming the prefix before
// joining with the "-" separator, the result would emit "abcdef--<hash>" — a
// double hyphen that violates the hyphen-collapse contract Normalize
// documents.
func TestNormalizeNoDoubleHyphenAfterTruncation(t *testing.T) {
	t.Parallel()
	// The 7-char prefix of "abcdef-ghijklmnop" is "abcdef-" (ends in hyphen).
	// The result must not contain "--".
	got := Normalize("abcdef-ghijklmnop")
	if strings.Contains(got, "--") {
		t.Errorf("Normalize(%q) = %q contains double hyphen — the prefix slice was not trimmed", "abcdef-ghijklmnop", got)
	}
	// Also exercise a case where two hyphens fall at and just past the cut
	// point — the collapse handles consecutive hyphens before truncation, but
	// the prefix slice itself could still land on a hyphen boundary.
	got2 := Normalize("abc-def-ghi-jkl-mno")
	if strings.Contains(got2, "--") {
		t.Errorf("Normalize(%q) = %q contains double hyphen", "abc-def-ghi-jkl-mno", got2)
	}
}

// TestNormalizeTruncationCapExact pins the truncated shape: at most
// MaxDiscriminatorLen bytes, hash suffix present, and stable across calls.
func TestNormalizeTruncationCapExact(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("a", 40)
	got := Normalize(in)
	if len(got) > MaxDiscriminatorLen {
		t.Fatalf("Normalize(%q) = %q (len %d), exceeds cap %d", in, got, len(got), MaxDiscriminatorLen)
	}
	if !strings.Contains(got, "-") {
		t.Fatalf("Normalize(%q) = %q, want a hash-suffixed truncation", in, got)
	}
	if got != Normalize(in) {
		t.Fatalf("Normalize(%q) is not deterministic", in)
	}
}
