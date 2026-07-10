package oauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

const (
	testRateLimitIP      = "203.0.113.10"
	testRateLimitOtherIP = "203.0.113.11"
	testStartIP          = "203.0.113.50"
	testCallbackIP       = "203.0.113.51"
	testIPv6Prefix       = "2001:db8:abcd:12::"
	testIPv6PrefixKey    = "2001:db8:abcd:12::/64"
)

func TestOAuthRateLimiterUnderLimitAccepts(t *testing.T) {
	now := time.Unix(1700000000, 0)
	limiter := newOAuthRateLimiter(func() time.Time { return now })

	for i := 0; i < oauthRateLimitBurst; i++ {
		if decision := limiter.allow(testRateLimitIP); !decision.allowed {
			t.Fatalf("request %d rejected under burst: retry after %ds", i+1, decision.retryAfterSeconds)
		}
	}
}

func TestOAuthRateLimiterOverLimitRejectsThenRefills(t *testing.T) {
	now := time.Unix(1700000000, 0)
	limiter := newOAuthRateLimiter(func() time.Time { return now })

	for i := 0; i < oauthRateLimitBurst; i++ {
		if decision := limiter.allow(testRateLimitIP); !decision.allowed {
			t.Fatalf("request %d rejected under burst: retry after %ds", i+1, decision.retryAfterSeconds)
		}
	}
	decision := limiter.allow(testRateLimitIP)
	if decision.allowed {
		t.Fatal("request over burst was accepted")
	}
	if !decision.logLimited {
		t.Fatal("first rejection should request an operator log")
	}
	if decision := limiter.allow(testRateLimitIP); decision.logLimited {
		t.Fatal("repeated rejection in same log window should not request another operator log")
	}
	if decision.retryAfterSeconds != expectedOAuthRetryAfterSeconds() {
		t.Fatalf("retryAfterSeconds = %d, want %d", decision.retryAfterSeconds, expectedOAuthRetryAfterSeconds())
	}

	now = now.Add(time.Duration(expectedOAuthRetryAfterSeconds())*time.Second + time.Millisecond)
	if decision := limiter.allow(testRateLimitIP); !decision.allowed {
		t.Fatalf("request after refill rejected: retry after %ds", decision.retryAfterSeconds)
	}
}

func TestOAuthRetryAfterSecondsRoundsPartialRefill(t *testing.T) {
	if got := oauthRetryAfterSeconds(0.5); got != oauthSecondsPerToken()/2 {
		t.Fatalf("oauthRetryAfterSeconds(0.5) = %d, want %d", got, oauthSecondsPerToken()/2)
	}
}

func TestOAuthRateLimiterThrottlesLimitLogsAcrossBuckets(t *testing.T) {
	now := time.Unix(1700000000, 0)
	limiter := newOAuthRateLimiter(func() time.Time { return now })

	for i := 0; i < oauthRateLimitBurst; i++ {
		limiter.allow(testRateLimitIP)
	}
	if decision := limiter.allow(testRateLimitIP); !decision.logLimited {
		t.Fatal("first over-limit bucket should request an operator log")
	}

	for i := 0; i < oauthRateLimitBurst; i++ {
		limiter.allow(testRateLimitOtherIP)
	}
	if decision := limiter.allow(testRateLimitOtherIP); decision.logLimited {
		t.Fatal("second over-limit bucket in same log window should not request another operator log")
	}

	now = now.Add(oauthRateLimitLogEvery)
	thirdIP := "203.0.113.12"
	for i := 0; i < oauthRateLimitBurst; i++ {
		limiter.allow(thirdIP)
	}
	decision := limiter.allow(thirdIP)
	if !decision.logLimited {
		t.Fatal("over-limit bucket after log window should request another operator log")
	}
	if decision.suppressedSinceLastLog != 1 {
		t.Fatalf("suppressedSinceLastLog = %d, want 1", decision.suppressedSinceLastLog)
	}
}

func TestOAuthRateLimiterKeepsSeparateIPBuckets(t *testing.T) {
	now := time.Unix(1700000000, 0)
	limiter := newOAuthRateLimiter(func() time.Time { return now })

	for i := 0; i < oauthRateLimitBurst; i++ {
		limiter.allow(testRateLimitIP)
	}
	if decision := limiter.allow(testRateLimitIP); decision.allowed {
		t.Fatal("first IP should be over limit")
	}
	if decision := limiter.allow(testRateLimitOtherIP); !decision.allowed {
		t.Fatalf("second IP should have a fresh bucket, got retry after %ds", decision.retryAfterSeconds)
	}
}

func TestOAuthRateLimiterPrunesIdleBuckets(t *testing.T) {
	now := time.Unix(1700000000, 0)
	limiter := newOAuthRateLimiter(func() time.Time { return now })
	limiter.maxBuckets = 1

	if decision := limiter.allow(testRateLimitIP); !decision.allowed {
		t.Fatalf("first IP rejected: retry after %ds", decision.retryAfterSeconds)
	}
	now = now.Add(oauthRateLimitIdleTTL + time.Second)
	if decision := limiter.allow(testRateLimitOtherIP); !decision.allowed {
		t.Fatalf("idle bucket should have pruned before new IP, got retry after %ds", decision.retryAfterSeconds)
	}
	if _, ok := limiter.buckets[testRateLimitIP]; ok {
		t.Fatal("idle first-IP bucket was not pruned")
	}
	if _, ok := limiter.buckets[testRateLimitOtherIP]; !ok {
		t.Fatal("new IP bucket was not retained")
	}
}

func TestOAuthRateLimiterRejectsNewIPsAtHardCap(t *testing.T) {
	now := time.Unix(1700000000, 0)
	limiter := newOAuthRateLimiter(func() time.Time { return now })
	limiter.maxBuckets = 1

	if decision := limiter.allow(testRateLimitIP); !decision.allowed {
		t.Fatalf("first IP rejected: retry after %ds", decision.retryAfterSeconds)
	}
	decision := limiter.allow(testRateLimitOtherIP)
	if decision.allowed {
		t.Fatal("new IP was accepted after bucket hard cap")
	}
	if !decision.logLimited {
		t.Fatal("first hard-cap rejection should request an operator log")
	}
	if decision := limiter.allow("203.0.113.12"); decision.logLimited {
		t.Fatal("repeated hard-cap rejection in same log window should not request another operator log")
	}
	if decision.retryAfterSeconds != 60 {
		t.Fatalf("retryAfterSeconds = %d, want 60", decision.retryAfterSeconds)
	}

	now = now.Add(oauthRateLimitLogEvery)
	decision = limiter.allow("203.0.113.13")
	if !decision.logLimited {
		t.Fatal("hard-cap rejection after log window should request another operator log")
	}
	if decision.suppressedSinceLastLog != 1 {
		t.Fatalf("suppressedSinceLastLog = %d, want 1", decision.suppressedSinceLastLog)
	}
}

func TestOAuthClientIPFallbackLogLimiterThrottles(t *testing.T) {
	now := time.Unix(1700000000, 0)
	limiter := newOAuthClientIPFallbackLogLimiter(func() time.Time { return now })
	key := "missing_x_forwarded_for:remote_addr"

	if !limiter.allow(key) {
		t.Fatal("first fallback warning should be allowed")
	}
	if limiter.allow(key) {
		t.Fatal("repeated fallback warning in same log window should be throttled")
	}

	now = now.Add(oauthRateLimitLogEvery)
	if !limiter.allow(key) {
		t.Fatal("fallback warning should be allowed after log window")
	}
}

func TestOAuthRateLimitKeyGroupsIPv6By64(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want string
	}{
		{name: "ipv4 exact", ip: testRateLimitIP, want: testRateLimitIP},
		{name: "ipv6 first address", ip: testIPv6Prefix + "1", want: testIPv6PrefixKey},
		{name: "ipv6 second address", ip: testIPv6Prefix + "2", want: testIPv6PrefixKey},
		{name: "unknown unchanged", ip: oauthUnknownClientIP, want: oauthUnknownClientIP},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := oauthRateLimitKey(tc.ip); got != tc.want {
				t.Fatalf("oauthRateLimitKey(%q) = %q, want %q", tc.ip, got, tc.want)
			}
		})
	}
}

func TestOAuthClientIPUsesRightmostForwardedFor(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, StartPath, http.NoBody)
	req.Header.Set("X-Forwarded-For", "198.51.100.99, 203.0.113.42")
	req.RemoteAddr = "10.0.0.10:12345"

	if got := oauthClientIPForTest(req); got != "203.0.113.42" {
		t.Fatalf("oauthClientIP = %q, want rightmost ALB-appended address", got)
	}
}

func TestOAuthClientIPUsesRightmostForwardedForHeaderLine(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, StartPath, http.NoBody)
	req.Header.Add("X-Forwarded-For", "198.51.100.99")
	req.Header.Add("X-Forwarded-For", "203.0.113.42")
	req.RemoteAddr = "10.0.0.10:12345"

	if got := oauthClientIPForTest(req); got != "203.0.113.42" {
		t.Fatalf("oauthClientIP = %q, want rightmost ALB-appended header value", got)
	}
}

func TestOAuthClientIPFallsBackToRemoteAddrWhenForwardedForIsInvalid(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, StartPath, http.NoBody)
	req.Header.Set("X-Forwarded-For", "198.51.100.99, not-an-ip")
	req.RemoteAddr = "198.51.100.44:12345"

	if got := oauthClientIPForTest(req); got != "198.51.100.44" {
		t.Fatalf("oauthClientIP = %q, want RemoteAddr host", got)
	}
}

func TestOAuthForwardedForIPDoesNotWalkLeftWhenRightmostIsInvalid(t *testing.T) {
	if got, ok := oauthForwardedForIP([]string{"198.51.100.99, not-an-ip"}); ok || got != "" {
		t.Fatalf("oauthForwardedForIP accepted %q, ok=%v; want invalid rightmost rejected", got, ok)
	}
}

func TestOAuthForwardedForIPDoesNotWalkLeftWhenRightmostIsEmpty(t *testing.T) {
	if got, ok := oauthForwardedForIP([]string{"198.51.100.99, "}); ok || got != "" {
		t.Fatalf("oauthForwardedForIP accepted %q, ok=%v; want empty rightmost rejected", got, ok)
	}
}

func TestOAuthForwardedForFallbackReason(t *testing.T) {
	tests := []struct {
		name    string
		headers []string
		want    string
	}{
		{name: "missing", want: "missing_x_forwarded_for"},
		{name: "empty", headers: []string{" , \t "}, want: "empty_x_forwarded_for"},
		{name: "trailing empty", headers: []string{"198.51.100.99, "}, want: "empty_x_forwarded_for"},
		{name: "invalid", headers: []string{"198.51.100.99, not-an-ip"}, want: "invalid_x_forwarded_for"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := oauthForwardedForFallbackReason(tc.headers); got != tc.want {
				t.Fatalf("oauthForwardedForFallbackReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOAuthClientIPFallsBackToRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, StartPath, http.NoBody)
	req.RemoteAddr = "198.51.100.44:12345"

	if got := oauthClientIPForTest(req); got != "198.51.100.44" {
		t.Fatalf("oauthClientIP = %q, want RemoteAddr host", got)
	}
}

func TestOAuthClientIPFallsBackToUnknownWhenNoAddressParses(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, StartPath, http.NoBody)
	req.RemoteAddr = "not-an-ip"

	if got := oauthClientIPForTest(req); got != oauthUnknownClientIP {
		t.Fatalf("oauthClientIP = %q, want unknown", got)
	}
}

func TestNormalizeOAuthIPEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "quoted ipv4", raw: `"198.51.100.70"`, want: "198.51.100.70", ok: true},
		{name: "ipv6 host port", raw: "[2001:db8::1]:443", want: "2001:db8::1", ok: true},
		{name: "bracketed ipv6", raw: " [2001:db8::2] ", want: "2001:db8::2", ok: true},
		{name: "invalid", raw: "not-an-ip", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeOAuthIP(tc.raw)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("normalizeOAuthIP(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestRegisterRoutesRateLimitsStartAndCallback(t *testing.T) {
	now := time.Unix(1700000000, 0)
	limiter := newOAuthRateLimiter(func() time.Time { return now })
	cfg := newStartCfg()
	mux := http.NewServeMux()
	registerRoutes(mux, cfg, limiter)

	state, err := MintState(cfg.OAuthStateSecret, testStateTeamID, testStateUserID, cfg.Now())
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	startURL := StartPath + "?state=" + url.QueryEscape(state)
	for i := 0; i < oauthRateLimitBurst; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, startURL, http.NoBody)
		req.Header.Set("X-Forwarded-For", testStartIP)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("start request %d status = %d, want 302 (body=%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, startURL, http.NoBody)
	req.Header.Set("X-Forwarded-For", testStartIP)
	mux.ServeHTTP(rec, req)
	assertRateLimited(t, rec)

	for i := 0; i < oauthRateLimitBurst; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, callbackPath, http.NoBody)
		req.Header.Set("X-Forwarded-For", testCallbackIP)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("callback request %d status = %d, want handler-owned 400 (body=%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, callbackPath, http.NoBody)
	req.Header.Set("X-Forwarded-For", testCallbackIP)
	mux.ServeHTTP(rec, req)
	assertRateLimited(t, rec)
}

func TestRegisterRoutesGroupsIPv6SourcesBy64(t *testing.T) {
	now := time.Unix(1700000000, 0)
	limiter := newOAuthRateLimiter(func() time.Time { return now })
	cfg := newStartCfg()
	mux := http.NewServeMux()
	registerRoutes(mux, cfg, limiter)

	state, err := MintState(cfg.OAuthStateSecret, testStateTeamID, testStateUserID, cfg.Now())
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	startURL := StartPath + "?state=" + url.QueryEscape(state)
	for i := 0; i < oauthRateLimitBurst; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, startURL, http.NoBody)
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("%s%x", testIPv6Prefix, i+1))
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("start request %d status = %d, want 302 (body=%s)", i+1, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, startURL, http.NoBody)
	req.Header.Set("X-Forwarded-For", testIPv6Prefix+"ffff")
	mux.ServeHTTP(rec, req)
	assertRateLimited(t, rec)
}

func TestRegisterRoutesSharesRateLimitBucketAcrossStartAndCallback(t *testing.T) {
	now := time.Unix(1700000000, 0)
	limiter := newOAuthRateLimiter(func() time.Time { return now })
	cfg := newStartCfg()
	mux := http.NewServeMux()
	registerRoutes(mux, cfg, limiter)

	state, err := MintState(cfg.OAuthStateSecret, testStateTeamID, testStateUserID, cfg.Now())
	if err != nil {
		t.Fatalf("MintState: %v", err)
	}
	startURL := StartPath + "?state=" + url.QueryEscape(state)
	sharedIP := "203.0.113.52"
	for i := 0; i < oauthRateLimitBurst-1; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, startURL, http.NoBody)
		req.Header.Set("X-Forwarded-For", sharedIP)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("start request %d status = %d, want 302 (body=%s)", i+1, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, callbackPath, http.NoBody)
	req.Header.Set("X-Forwarded-For", sharedIP)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("callback status = %d, want handler-owned 400 (body=%s)", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, startURL, http.NoBody)
	req.Header.Set("X-Forwarded-For", sharedIP)
	mux.ServeHTTP(rec, req)
	assertRateLimited(t, rec)
}

func assertRateLimited(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Retry-After"), expectedOAuthRetryAfterHeader(); got != want {
		t.Fatalf("Retry-After = %q, want %s", got, want)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	assertOAuthErrorPage(t, rec, "Too many setup attempts")
}

func expectedOAuthRetryAfterHeader() string {
	return strconv.Itoa(expectedOAuthRetryAfterSeconds())
}

func expectedOAuthRetryAfterSeconds() int {
	return oauthSecondsPerToken()
}

func oauthClientIPForTest(req *http.Request) string {
	return oauthClientIPWithFallbackLogLimiter(req, newOAuthClientIPFallbackLogLimiter(func() time.Time {
		return time.Unix(1700000000, 0)
	}))
}
