package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

func FuzzValidateURL(f *testing.F) {
	f.Add("https://example.com")
	f.Add("http://example.com/path?q=1")
	f.Add("")
	f.Add("example.com")
	f.Add("ftp://example.com")
	f.Add("https://")
	f.Add("http://:")
	f.Add("https://example.com\nHost:evil.test")
	f.Add("https://exa mple.com")

	f.Fuzz(func(t *testing.T, raw string) {
		if err := validateURL(raw); err != nil {
			return
		}
		assertAcceptedURLIsHTTPClientSafe(t, raw)
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

func assertAcceptedURLIsHTTPClientSafe(t *testing.T, raw string) {
	t.Helper()
	if strings.IndexFunc(raw, unicode.IsControl) != -1 {
		t.Fatalf("accepted URL with raw control character: %q", raw)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, raw, http.NoBody)
	if err != nil {
		t.Fatalf("accepted URL that net/http cannot request: %q: %v", raw, err)
	}
	if req.URL == nil || !req.URL.IsAbs() {
		t.Fatalf("accepted non-absolute URL: %q", raw)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		t.Fatalf("accepted URL with non-HTTP scheme %q: %q", req.URL.Scheme, raw)
	}
	if req.URL.Hostname() == "" || req.Host == "" {
		t.Fatalf("accepted URL without request host: %q", raw)
	}
	if strings.ContainsAny(req.URL.String(), "\r\n") {
		t.Fatalf("accepted URL normalizes with header-breaking bytes: %q", raw)
	}
}
