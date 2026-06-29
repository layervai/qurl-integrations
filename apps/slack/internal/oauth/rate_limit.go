package oauth

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// In-memory buckets are per process. Multiple Slack OAuth replicas behind the
// ALB each enforce this budget independently. Within one replica, /start and
// /callback share a source-IP bucket, so shared NAT egresses share budget; ALB
// routing can still send a client's flow to different replicas. IPv6 clients
// are grouped by /64 to avoid per-address rotation within one client network.
const (
	oauthRateLimitRequestsPerMinute = 10
	// Match one minute of steady-state budget so a normal start/callback flow
	// has room for retries without making sustained abuse cheap.
	oauthRateLimitBurst      = 10
	oauthRateLimitPruneEvery = 30 * time.Second
	oauthRateLimitIdleTTL    = 2 * time.Minute
	// Sized well above expected setup concurrency while bounding memory during
	// a wide-source flood; new source IPs shed until idle entries prune.
	oauthRateLimitMaxBuckets        = 20000
	oauthRateLimitHardCapRetryAfter = 60
	oauthRateLimitLogEvery          = time.Minute
	oauthRateLimitRefillPerSecond   = float64(oauthRateLimitRequestsPerMinute) / 60
)

const oauthUnknownClientIP = "unknown"

var defaultOAuthRateLimiter = newOAuthRateLimiter(time.Now)

var defaultOAuthClientIPFallbackLogLimiter = newOAuthClientIPFallbackLogLimiter(time.Now)

type oauthRateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*oauthRateBucket
	now        func() time.Time
	nextPrune  time.Time
	maxBuckets int
	// Limit logs are process-wide to cap flood log volume; suppressed counters
	// preserve approximate scale between emitted warnings.
	lastLimitedLog time.Time
	limitedDropped int
	lastHardCapLog time.Time
	hardCapDropped int
}

type oauthRateBucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
}

type oauthRateLimitDecision struct {
	allowed                bool
	retryAfterSeconds      int
	logLimited             bool
	suppressedSinceLastLog int
}

func newOAuthRateLimiter(now func() time.Time) *oauthRateLimiter {
	if now == nil {
		now = time.Now
	}
	return &oauthRateLimiter{
		buckets:    make(map[string]*oauthRateBucket),
		now:        now,
		nextPrune:  now().Add(oauthRateLimitPruneEvery),
		maxBuckets: oauthRateLimitMaxBuckets,
	}
}

func (l *oauthRateLimiter) allow(key string) oauthRateLimitDecision {
	if l == nil {
		return oauthRateLimitDecision{allowed: true}
	}
	if key == "" {
		key = oauthUnknownClientIP
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if !now.Before(l.nextPrune) {
		l.pruneLocked(now)
	}

	bucket, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxBuckets {
			// Prefer shedding new source IPs over unbounded memory growth during
			// a wide-source flood; the periodic prune above amortizes scan cost.
			logLimited, dropped := l.shouldLogHardCapLocked(now)
			return oauthRateLimitDecision{
				// Use a longer backoff while the bucket table is full to reduce retry churn.
				retryAfterSeconds:      oauthRateLimitHardCapRetryAfter,
				logLimited:             logLimited,
				suppressedSinceLastLog: dropped,
			}
		}
		bucket = &oauthRateBucket{
			tokens:     oauthRateLimitBurst,
			lastRefill: now,
			lastSeen:   now,
		}
		l.buckets[key] = bucket
	}

	refillOAuthBucket(bucket, now)
	bucket.lastSeen = now
	if bucket.tokens >= 1 {
		bucket.tokens--
		return oauthRateLimitDecision{allowed: true}
	}
	logLimited, dropped := l.shouldLogLimitLocked(now)
	return oauthRateLimitDecision{
		retryAfterSeconds:      oauthRetryAfterSeconds(bucket.tokens),
		logLimited:             logLimited,
		suppressedSinceLastLog: dropped,
	}
}

func (l *oauthRateLimiter) shouldLogLimitLocked(now time.Time) (logLimited bool, suppressed int) {
	return shouldLogOAuthRateLimit(now, &l.lastLimitedLog, &l.limitedDropped)
}

func (l *oauthRateLimiter) shouldLogHardCapLocked(now time.Time) (logLimited bool, suppressed int) {
	return shouldLogOAuthRateLimit(now, &l.lastHardCapLog, &l.hardCapDropped)
}

func (l *oauthRateLimiter) pruneLocked(now time.Time) {
	for key, bucket := range l.buckets {
		if now.Sub(bucket.lastSeen) > oauthRateLimitIdleTTL {
			delete(l.buckets, key)
		}
	}
	l.nextPrune = now.Add(oauthRateLimitPruneEvery)
}

func refillOAuthBucket(bucket *oauthRateBucket, now time.Time) {
	elapsed := now.Sub(bucket.lastRefill)
	if elapsed <= 0 {
		return
	}
	bucket.tokens = min(oauthRateLimitBurst, bucket.tokens+elapsed.Seconds()*oauthRateLimitRefillPerSecond)
	bucket.lastRefill = now
}

func shouldLogOAuthRateLimit(now time.Time, lastLog *time.Time, dropped *int) (logLimited bool, suppressed int) {
	if lastLog.IsZero() || now.Sub(*lastLog) >= oauthRateLimitLogEvery {
		suppressed = *dropped
		*lastLog = now
		*dropped = 0
		return true, suppressed
	}
	*dropped++
	return false, 0
}

func oauthRetryAfterSeconds(tokens float64) int {
	if tokens <= 0 {
		return oauthSecondsPerToken()
	}
	deficit := 1 - tokens
	return int(math.Ceil(deficit / oauthRateLimitRefillPerSecond))
}

func oauthSecondsPerToken() int {
	return (60 + oauthRateLimitRequestsPerMinute - 1) / oauthRateLimitRequestsPerMinute
}

func rateLimitOAuth(limiter *oauthRateLimiter, next http.Handler) http.Handler {
	if limiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Run before method/state validation so malformed or unsupported requests
		// consume source budget instead of reaching OAuth handlers.
		ip := oauthClientIP(r)
		key := oauthRateLimitKey(ip)
		decision := limiter.allow(key)
		if decision.allowed {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", strconv.Itoa(decision.retryAfterSeconds))
		if decision.logLimited {
			slog.Warn("oauth rate limit exceeded", //nolint:gosec // G706: slog escapes control bytes; values are needed for operator triage.
				"ip", ip,
				"rate_limit_key", key,
				"path", r.URL.Path,
				"retry_after_seconds", decision.retryAfterSeconds,
				"suppressed_since_last_log", decision.suppressedSinceLastLog)
		}
		renderOAuthErrorPage(w, http.StatusTooManyRequests, "Too many setup attempts",
			"Too many qURL™ setup requests came from this IP address.",
			"Wait a moment, then return to Slack and try again.")
	})
}

func oauthRateLimitKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return addr.String()
	}
	return prefix.Masked().String()
}

func oauthClientIP(r *http.Request) string {
	return oauthClientIPWithFallbackLogLimiter(r, defaultOAuthClientIPFallbackLogLimiter)
}

func oauthClientIPWithFallbackLogLimiter(r *http.Request, fallbackLogLimiter *oauthClientIPFallbackLogLimiter) string {
	// Deployment assumption: Slack OAuth is reachable only through one trusted
	// ALB, which appends the client address to X-Forwarded-For. Revisit this if
	// direct access is allowed or another proxy is inserted in front of the ALB.
	forwardedFor := r.Header.Values("X-Forwarded-For")
	if ip, ok := oauthForwardedForIP(forwardedFor); ok {
		return ip
	}
	// Missing or malformed X-Forwarded-For falls back to coarse buckets
	// (RemoteAddr, then "unknown") so proxy misconfigurations fail closed.
	// Behind the ALB those buckets can throttle all clients together; the
	// warning log below is the signal to fix proxy forwarding.
	remote := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	reason := oauthForwardedForFallbackReason(forwardedFor)
	if ip, ok := normalizeOAuthIP(remote); ok {
		warnOAuthClientIPFallback(fallbackLogLimiter, r, reason, "remote_addr")
		return ip
	}
	warnOAuthClientIPFallback(fallbackLogLimiter, r, reason, oauthUnknownClientIP)
	return oauthUnknownClientIP
}

func warnOAuthClientIPFallback(limiter *oauthClientIPFallbackLogLimiter, r *http.Request, reason, fallback string) {
	if !limiter.allow(reason + ":" + fallback) {
		return
	}
	slog.Warn("oauth client ip using fallback", //nolint:gosec // G706: path is request-controlled; slog escapes control bytes and the path is useful for proxy triage.
		"path", r.URL.Path,
		"reason", reason,
		"fallback", fallback)
}

type oauthClientIPFallbackLogLimiter struct {
	mu  sync.Mutex
	now func() time.Time
	// Bounded by the fixed reason/fallback combinations; no prune is needed.
	// Tests that assert fallback logs should install a fresh package limiter.
	last map[string]time.Time
}

func newOAuthClientIPFallbackLogLimiter(now func() time.Time) *oauthClientIPFallbackLogLimiter {
	if now == nil {
		now = time.Now
	}
	return &oauthClientIPFallbackLogLimiter{
		now:  now,
		last: make(map[string]time.Time),
	}
}

func (l *oauthClientIPFallbackLogLimiter) allow(key string) bool {
	if l == nil {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	last := l.last[key]
	if last.IsZero() || now.Sub(last) >= oauthRateLimitLogEvery {
		l.last[key] = now
		return true
	}
	return false
}

func oauthForwardedForFallbackReason(headers []string) string {
	candidate, ok := oauthRightmostForwardedForValue(headers)
	if !ok {
		return "missing_x_forwarded_for"
	}
	if strings.TrimSpace(candidate) == "" {
		return "empty_x_forwarded_for"
	}
	return "invalid_x_forwarded_for"
}

func oauthForwardedForIP(headers []string) (string, bool) {
	candidate, ok := oauthRightmostForwardedForValue(headers)
	if !ok {
		return "", false
	}
	// Trust only the ALB-appended rightmost value. If it is empty or malformed,
	// fall back to RemoteAddr rather than walking left into client input.
	return normalizeOAuthIP(candidate)
}

func oauthRightmostForwardedForValue(headers []string) (string, bool) {
	if len(headers) == 0 {
		return "", false
	}
	parts := strings.Split(headers[len(headers)-1], ",")
	return parts[len(parts)-1], true
}

func normalizeOAuthIP(raw string) (string, bool) {
	candidate := strings.Trim(strings.TrimSpace(raw), `"`)
	if candidate == "" {
		return "", false
	}
	if ip := net.ParseIP(candidate); ip != nil {
		return ip.String(), true
	}
	if host, _, err := net.SplitHostPort(candidate); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String(), true
		}
	}
	candidate = strings.Trim(candidate, "[]")
	if ip := net.ParseIP(candidate); ip != nil {
		return ip.String(), true
	}
	return "", false
}
