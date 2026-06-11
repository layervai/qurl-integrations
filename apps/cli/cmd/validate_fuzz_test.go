package main

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func FuzzValidateURL(f *testing.F) {
	f.Add("https://example.com")
	f.Add("http://example.com/path?q=1")
	f.Add("")
	f.Add("example.com")
	f.Add("ftp://example.com")
	f.Add("https://")
	f.Add("https://example.com\nHost:evil.test")
	f.Add("https://exa mple.com")

	f.Fuzz(func(t *testing.T, raw string) {
		if err := validateURL(raw); err != nil {
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("accepted unparsable URL %q: %v", raw, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			t.Fatalf("accepted URL with scheme %q: %q", u.Scheme, raw)
		}
		if u.Host == "" {
			t.Fatalf("accepted URL without host: %q", raw)
		}
	})
}

func FuzzValidateDuration(f *testing.F) {
	f.Add("")
	f.Add("1s")
	f.Add("30m")
	f.Add("24h")
	f.Add("7d")
	f.Add("0h")
	f.Add("-1h")
	f.Add("1.5h")
	f.Add("999999999999999999999999999999d")

	f.Fuzz(func(t *testing.T, d string) {
		if err := validateDuration(d); err != nil {
			return
		}
		if d == "" {
			return
		}
		m := durationPattern.FindStringSubmatch(d)
		if m == nil {
			t.Fatalf("accepted duration outside pattern: %q", d)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			t.Fatalf("accepted non-positive duration %q parsed as %d (err=%v)", d, n, err)
		}
	})
}

func FuzzValidateResourceID(f *testing.F) {
	f.Add("r_abc123")
	f.Add("r_")
	f.Add("")
	f.Add("R_abc123")
	f.Add("r_\x00")
	f.Add("r_../../etc/passwd")

	f.Fuzz(func(t *testing.T, id string) {
		if err := validateResourceID(id); err != nil {
			return
		}
		assertAcceptedPrefixedMinimum(t, "resource ID", id, "r_", 4)
	})
}

func FuzzValidateAccessToken(f *testing.F) {
	f.Add("at_abc123")
	f.Add("at_")
	f.Add("")
	f.Add("AT_abc123")
	f.Add("at_\x00\x00")
	f.Add("at_abc123\n")

	f.Fuzz(func(t *testing.T, token string) {
		if err := validateAccessToken(token); err != nil {
			return
		}
		assertAcceptedPrefixedMinimum(t, "access token", token, "at_", 6)
	})
}

func assertAcceptedPrefixedMinimum(t *testing.T, kind, value, prefix string, minLen int) {
	t.Helper()
	if !strings.HasPrefix(value, prefix) {
		t.Fatalf("accepted %s without %q prefix: %q", kind, prefix, value)
	}
	if len(value) < minLen {
		t.Fatalf("accepted %s shorter than %d bytes: %q", kind, minLen, value)
	}
}
