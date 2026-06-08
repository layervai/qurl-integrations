package oauth

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Per-IP rate limiting for the public OAuth surface
// (/oauth/qurl/start and /oauth/qurl/callback).
//
// Mirrors the LIVE Discord limiter at
// apps/discord/src/utils/oauth-rate-limit.js structurally (not a direct
// port): one sliding-window counter per client IP, shared across BOTH
// OAuth routes so an attacker can't double the budget by alternating
// endpoints — that amplification is exactly what the Discord reference's
// header comment warns about.
//
// This is defense-in-depth, not the only line of defense: the signed-state
// contract makes /start return 400 before any outbound Auth0 /
// qurl-service call, and the bot's ALB / WAF is the production moat. The
// limiter caps the CPU spent on HMAC-SHA256 state verifies and the
// per-request allocation cost that the signed-state moat leaves unbounded.
//
// SCALING: single-instance only. State lives in process memory, so if this
// bot ever runs horizontally (multiple ECS tasks behind a load balancer)
// the effective rate becomes N × the configured limit — move the store to
// Redis at that point. Single-task is sufficient for the per-workspace
// install rate this surface sees.
const (
	// rateLimitWindow is the sliding window each IP's request count is
	// measured over.
	rateLimitWindow = 60 * time.Second
	// rateLimitMaxRequests is the per-IP request budget within
	// rateLimitWindow. The issue's explicit acceptance criterion is
	// 10 requests / minute / IP.
	rateLimitMaxRequests = 10
	// maxRequestsPerIP caps the timestamps retained per IP, mirroring the
	// Discord reference's MAX_REQUESTS_PER_IP (also budget × 4). Under the
	// current mutex-serialized flow this trim is unreachable: allow()
	// rejects at rateLimitMaxRequests WITHOUT appending (see the
	// reject-before-append branch below), so a stored slice never exceeds
	// the budget (10) — far under this cap (40). Retained for structural
	// parity with the reference and as belt-and-suspenders: if a future
	// edit ever appended past the budget (e.g. a separate count path), the
	// slice would still stay bounded. Not load-bearing today.
	maxRequestsPerIP = rateLimitMaxRequests * 4
	// maxStoreSize is the hard ceiling on distinct IPs tracked at once.
	// Under a distributed flood from many unique IPs the map can reach this
	// cap; an arriving previously-unseen IP first triggers a bounded
	// reclamation sweep (reclaimStaleLocked) and is only shed with a 429 if
	// that sweep can't free a slot. Known IPs keep being served because
	// they don't grow the map.
	//
	// Steady state once the cap is first hit: reclamation frees exactly one
	// slot per admitted new arrival and that arrival immediately refills it,
	// so the map stays pinned at maxStoreSize for the life of the process
	// even when most keys later go stale. There is no path that drains the
	// key set BELOW the cap — that's intentional (bounded memory, no
	// background goroutine), not a leak; don't expect the map to shrink.
	maxStoreSize = 20000
	// reclaimScanLimit bounds the keys examined per at-cap reclamation
	// sweep so a single arrival can't pay an O(maxStoreSize) cost. One
	// fully-stale key freed is enough to admit the arriving IP; the cap
	// keeps the worst-case in-lock work flat regardless of map size. There
	// is no resumable cursor: Go randomizes map-iteration order, so each
	// at-cap arrival independently samples up to this many keys. Stale keys
	// are therefore reclaimed probabilistically across arrivals rather than
	// via a stateful sweep — which still drains the map over time without a
	// background goroutine (an arrival that happens to sample only live keys
	// is shed with a 429, and the next arrival re-samples).
	reclaimScanLimit = 1024
	// rejectLogInterval throttles the per-path rejection warnings. The whole
	// point of this limiter is to absorb floods cheaply, so logging every
	// rejection would let an attacker turn the control into a log-amplifier
	// (one line per shed request, at flood volume). Instead each rejection
	// path (over-budget, at-cap shed) emits at most one slog.Warn per
	// interval and coalesces the rest into a suppressed-count on the next
	// line — enough for ops to see "the limiter is firing" and its magnitude
	// without the volume tracking the attack. One window is the natural
	// cadence: it's the period over which the budget itself is measured.
	rejectLogInterval = rateLimitWindow
)

// unknownIP is the client-IP sentinel used when neither X-Forwarded-For
// nor RemoteAddr yields a usable host. Constant because it doubles as a
// map key and is referenced from multiple sites.
const unknownIP = "unknown"

// xForwardedForHeader is the proxy header the ALB appends the real client
// IP to. Named once so the literal isn't repeated across clientIP and any
// future call site.
const xForwardedForHeader = "X-Forwarded-For"

// rateLimiter is a sliding-window, per-IP request limiter. The zero value
// is not usable; construct with newRateLimiter so the map and clock are
// initialized.
//
// Concurrency: requests is guarded by mu. -race runs in CI, so the map
// MUST be mutex-guarded — a plain map + sync.Mutex (not sync.Map, whose
// per-key semantics don't fit the read-modify-write-the-slice access
// pattern here).
type rateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
	// now is injected for deterministic tests (mirrors Config.Now). Never
	// call time.Now() inline — the window-expiry test advances this clock.
	now func() time.Time

	// Per-path rejection-log throttles, guarded by mu (mutated only inside
	// allow(), which already holds the lock — no extra synchronization). Each
	// path throttles on its own clock so a flood emits ~one line per
	// rejectLogInterval carrying a coalesced suppressed-count rather than one
	// line per shed request.
	overBudgetLog logThrottle // a single IP spending its window
	atCapLog      logThrottle // the store full, shedding new IPs
}

// logThrottle is the per-rejection-path throttle state: when this path last
// emitted a warning and how many rejections it has swallowed since. Not safe
// for concurrent use — every access is under rateLimiter.mu.
type logThrottle struct {
	lastLogged time.Time
	suppressed int
}

// newRateLimiter returns a ready rateLimiter. now may be nil, in which
// case time.Now is used.
func newRateLimiter(now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{
		requests: make(map[string][]time.Time),
		now:      now,
	}
}

// clientIP extracts the request's client IP for rate-limit keying.
//
// ALB trust assumption: the bot sits behind exactly ONE ALB hop, which
// appends the real client IP as the LAST entry of X-Forwarded-For. Earlier
// entries are client-supplied and spoofable, so we take only the last,
// trimmed entry. With no XFF header (direct / test traffic) we fall back to
// the RemoteAddr host, then to a shared sentinel so every keyless request
// still shares one bucket rather than escaping the limit.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get(xForwardedForHeader); xff != "" {
		parts := strings.Split(xff, ",")
		last := strings.TrimSpace(parts[len(parts)-1])
		if last != "" {
			return last
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	return unknownIP
}

// retryAfterSeconds computes the Retry-After value (whole seconds) for a
// rejected request: how long until the oldest in-window timestamp ages out
// and frees a slot. Floored at 1 so the header is never "0" or negative.
func retryAfterSeconds(oldestInWindow, now time.Time) int {
	secs := int(math.Ceil(oldestInWindow.Add(rateLimitWindow).Sub(now).Seconds()))
	if secs < 1 {
		return 1
	}
	return secs
}

// reclaimStaleLocked deletes up to reclaimScanLimit keys whose entire
// bucket has aged out of the window (newest timestamp at or before
// windowStart), reclaiming map slots without a background goroutine. It
// stops early once one slot is freed — that's all an at-cap arrival needs
// — so the common case stays cheap. Caller must hold mu.
//
// Slices are appended in time order, so the last element is the newest; a
// key whose newest timestamp isn't After(windowStart) holds nothing the
// limiter still counts and is safe to drop.
func (rl *rateLimiter) reclaimStaleLocked(windowStart time.Time) {
	scanned := 0
	for ip, ts := range rl.requests {
		if scanned >= reclaimScanLimit {
			return
		}
		scanned++
		if len(ts) == 0 || !ts[len(ts)-1].After(windowStart) {
			delete(rl.requests, ip)
			if len(rl.requests) < maxStoreSize {
				return
			}
		}
	}
}

// logRejectionLocked emits a throttled slog.Warn for the given rejection
// path. It logs at most once per rejectLogInterval, folding the count of
// rejections suppressed since the previous line into a "suppressed_since"
// attr so ops can gauge flood magnitude without per-request log volume.
// Caller must hold mu.
//
// The attacker-influenced client IP rides as an attribute VALUE (never a
// key), so slog's value escaping neutralizes control bytes — no G706 sink
// here, unlike the format-string-style sites in callback.go.
func (rl *rateLimiter) logRejectionLocked(t *logThrottle, now time.Time, msg string, attrs ...any) {
	if !t.lastLogged.IsZero() && now.Sub(t.lastLogged) < rejectLogInterval {
		t.suppressed++
		return
	}
	slog.Warn(msg, append(attrs, "suppressed_since", t.suppressed)...)
	t.lastLogged = now
	t.suppressed = 0
}

// allow records an attempt from ip and reports whether it is within the
// per-IP budget. When rejected it also returns the Retry-After seconds the
// caller should advertise. All map mutation happens under mu; stale
// timestamps for ip are filtered on the same access, and at cap a bounded
// reclamation sweep (reclaimStaleLocked) drops fully stale keys before any
// new IP is shed — so the key set, not just per-key timestamps, can shrink.
// No background sweep goroutine — that would need lifecycle wiring into
// main.go's shutdown drain and risks leaking in tests.
func (rl *rateLimiter) allow(ip string) (ok bool, retryAfter int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	windowStart := now.Add(-rateLimitWindow)

	existing, seen := rl.requests[ip]
	// Hard memory ceiling: at cap, first try to reclaim slots from fully
	// stale keys; only shed a previously-unseen IP if the sweep couldn't
	// free room. Without this the map's eviction story covers timestamps
	// but never keys, so a one-time flood of distinct IPs would wedge it
	// at cap and 429 every new install for the life of the process. Known
	// IPs fall through — serving them doesn't add keys.
	if !seen && len(rl.requests) >= maxStoreSize {
		rl.reclaimStaleLocked(windowStart)
		if len(rl.requests) >= maxStoreSize {
			// At-cap shed: the store is full of in-window IPs and reclamation
			// freed nothing. This is the high-signal "under a sustained
			// distributed flood, shedding new installs" event worth alerting
			// on — distinct from a single IP merely exhausting its budget.
			rl.logRejectionLocked(&rl.atCapLog, now,
				"oauth rate limiter: store at hard cap, shedding new IP",
				"ip", ip, "store_size", len(rl.requests))
			return false, int(rateLimitWindow.Seconds())
		}
	}

	recent := existing[:0]
	for _, ts := range existing {
		if ts.After(windowStart) {
			recent = append(recent, ts)
		}
	}

	if len(recent) >= rateLimitMaxRequests {
		// recent is sorted ascending (appended in time order), so the
		// first entry is the oldest still in the window.
		rl.requests[ip] = recent
		// Over-budget: this single IP has spent its window. Throttled so a
		// persistent attacker can't turn each rejected request into a log line.
		rl.logRejectionLocked(&rl.overBudgetLog, now,
			"oauth rate limiter: per-IP budget exceeded",
			"ip", ip)
		return false, retryAfterSeconds(recent[0], now)
	}

	recent = append(recent, now)
	// Bound the slice. Unreachable today: we reach here only when the
	// pre-append count was < rateLimitMaxRequests (10), so post-append len
	// is <= 10 < maxRequestsPerIP (40). Kept for parity with the Discord
	// reference's trim and as a guard if the reject-before-append invariant
	// above is ever relaxed. See the maxRequestsPerIP const comment.
	if len(recent) > maxRequestsPerIP {
		recent = recent[len(recent)-maxRequestsPerIP:]
	}
	rl.requests[ip] = recent
	return true, 0
}

// middleware wraps next with the per-IP sliding-window limiter. Over-budget
// requests are rejected with 429 + a Retry-After header (set before the
// status is written) and a bare http.Error body — matching the plain-text
// rejection style used elsewhere in this package (start.go, callback.go)
// rather than rendering an HTML page.
func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, retryAfter := rl.allow(clientIP(r))
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			http.Error(w, "too many requests — slow down and try again shortly", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
