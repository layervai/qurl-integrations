package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeHostnamePreservesUTF8Boundaries(t *testing.T) {
	t.Parallel()
	if got := normalizeHostname("builder-😀"); got != "builder-😀" {
		t.Fatalf("short hostname = %q, want unchanged UTF-8", got)
	}

	tooLong := strings.Repeat("a", 252) + "😀"
	got := normalizeHostname(tooLong)
	if got != strings.Repeat("a", 252) || len(got) > 255 || !utf8.ValidString(got) {
		t.Fatalf("truncated hostname = %q (%d bytes, valid=%v)", got, len(got), utf8.ValidString(got))
	}

	if got := normalizeHostname("bad\xffhost"); got != metadataHostnameFallback {
		t.Fatalf("invalid UTF-8 hostname = %q, want fallback", got)
	}
	if got := normalizeHostname(" \t "); got != metadataHostnameFallback {
		t.Fatalf("blank hostname = %q, want fallback", got)
	}
	if Hostname() == "" {
		t.Fatal("Hostname() returned empty; the fallback must cover failures")
	}
}

func TestClientVersionMeta(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"v0.5.1":              "v0.5.1",
		"0.5.1":               "0.5.1",
		"0.5.1.260617120102":  "0.5.1",
		"v0.5.1.260617120102": "v0.5.1",
		"dev":                 "dev",
		"0.5.1.local":         "0.5.1.local",
		"":                    "dev",
		"  ":                  "dev",
	} {
		if got := ClientVersionMeta(input); got != want {
			t.Errorf("ClientVersionMeta(%q) = %q, want %q", input, got, want)
		}
	}
}
