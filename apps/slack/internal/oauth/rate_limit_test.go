package oauth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captureSlog redirects the package-level slog default to an in-memory text
// handler for the duration of the test and restores it on cleanup. The OAuth
// handlers (start.go, callback.go, rate_limit.go) all log via the slog
// package functions rather than an injected logger, so the only seam to
// observe their output is the default logger. Restore-on-cleanup keeps this
// from leaking into other tests; these tests must not run t.Parallel().
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// sentinelHandler records whether next.ServeHTTP was reached and writes a
// 200 so we can distinguish "passed the limiter" from "rejected with 429".
func sentinelHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

// fixedClock returns a now func over a pointer the test can advance,
// mirroring the Config.Now injection used in start_test.go.
func fixedClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

// albRemoteAddr stands in for the ALB's own socket address — the value
// RemoteAddr carries when traffic arrives via the proxy. clientIP must
// ignore it whenever X-Forwarded-For is present.
const albRemoteAddr = "10.0.0.1:9999"

func reqFromIP(remoteAddr string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start", http.NoBody)
	r.RemoteAddr = remoteAddr
	return r
}

// TestRateLimiterUnderLimitPasses fences acceptance criterion (a): every
// request up to the budget reaches the wrapped handler.
func TestRateLimiterUnderLimitPasses(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	rl := newRateLimiter(fixedClock(&clock))

	for i := range rateLimitMaxRequests {
		reached := false
		h := rl.middleware(sentinelHandler(&reached))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, reqFromIP("203.0.113.7:5000"))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d want 200 (body=%s)", i+1, rec.Code, rec.Body.String())
		}
		if !reached {
			t.Fatalf("request %d: handler not reached", i+1)
		}
	}
}

// TestRateLimiterOverLimitRejects fences acceptance criterion (b): the
// max+1'th request returns 429 with a non-empty, positive integer
// Retry-After and does NOT reach the wrapped handler.
func TestRateLimiterOverLimitRejects(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	rl := newRateLimiter(fixedClock(&clock))

	for i := range rateLimitMaxRequests {
		rec := httptest.NewRecorder()
		rl.middleware(sentinelHandler(new(bool))).ServeHTTP(rec, reqFromIP("203.0.113.9:5000"))
		if rec.Code != http.StatusOK {
			t.Fatalf("warmup request %d: got %d want 200", i+1, rec.Code)
		}
	}

	reached := false
	rec := httptest.NewRecorder()
	rl.middleware(sentinelHandler(&reached)).ServeHTTP(rec, reqFromIP("203.0.113.9:5000"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit: got %d want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	if reached {
		t.Error("over-limit request must not reach the wrapped handler")
	}
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("over-limit: Retry-After header missing")
	}
	secs, err := strconv.Atoi(ra)
	if err != nil {
		t.Fatalf("over-limit: Retry-After %q is not an integer: %v", ra, err)
	}
	if secs < 1 {
		t.Errorf("over-limit: Retry-After %d must be >= 1", secs)
	}
}

// TestRateLimiterSeparateIPsSeparateBuckets fences acceptance criterion
// (c): IP-A exhausting its budget does not reject IP-B.
func TestRateLimiterSeparateIPsSeparateBuckets(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	rl := newRateLimiter(fixedClock(&clock))

	// Exhaust IP-A.
	for range rateLimitMaxRequests {
		rec := httptest.NewRecorder()
		rl.middleware(sentinelHandler(new(bool))).ServeHTTP(rec, reqFromIP("198.51.100.1:4000"))
		if rec.Code != http.StatusOK {
			t.Fatalf("IP-A warmup: got %d want 200", rec.Code)
		}
	}
	// IP-A is now over budget.
	recA := httptest.NewRecorder()
	rl.middleware(sentinelHandler(new(bool))).ServeHTTP(recA, reqFromIP("198.51.100.1:4000"))
	if recA.Code != http.StatusTooManyRequests {
		t.Fatalf("IP-A over-limit: got %d want 429", recA.Code)
	}
	// IP-B must still be served.
	reachedB := false
	recB := httptest.NewRecorder()
	rl.middleware(sentinelHandler(&reachedB)).ServeHTTP(recB, reqFromIP("198.51.100.2:4000"))
	if recB.Code != http.StatusOK {
		t.Fatalf("IP-B: got %d want 200 — separate IPs must keep separate buckets", recB.Code)
	}
	if !reachedB {
		t.Error("IP-B request should have reached the handler")
	}
}

// TestRateLimiterWindowExpiryReopens fences acceptance criterion (d):
// advancing the clock past the window ages out the recorded timestamps so
// the bucket reopens.
func TestRateLimiterWindowExpiryReopens(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	rl := newRateLimiter(fixedClock(&clock))

	for range rateLimitMaxRequests {
		rec := httptest.NewRecorder()
		rl.middleware(sentinelHandler(new(bool))).ServeHTTP(rec, reqFromIP("192.0.2.50:6000"))
		if rec.Code != http.StatusOK {
			t.Fatalf("warmup: got %d want 200", rec.Code)
		}
	}
	// Confirm we're rejecting before the window rolls.
	recBlocked := httptest.NewRecorder()
	rl.middleware(sentinelHandler(new(bool))).ServeHTTP(recBlocked, reqFromIP("192.0.2.50:6000"))
	if recBlocked.Code != http.StatusTooManyRequests {
		t.Fatalf("pre-expiry: got %d want 429", recBlocked.Code)
	}

	// Advance past the window — every recorded timestamp is now stale.
	clock = clock.Add(rateLimitWindow + time.Second)

	reached := false
	rec := httptest.NewRecorder()
	rl.middleware(sentinelHandler(&reached)).ServeHTTP(rec, reqFromIP("192.0.2.50:6000"))
	if rec.Code != http.StatusOK {
		t.Fatalf("post-expiry: got %d want 200 — window should have reopened (body=%s)", rec.Code, rec.Body.String())
	}
	if !reached {
		t.Error("post-expiry request should have reached the handler")
	}
}

// TestClientIPPrefersLastXForwardedForEntry locks the single-hop ALB trust
// assumption: with an X-Forwarded-For chain, the LAST (ALB-appended) entry
// is the trusted client IP, not the spoofable leading entries.
func TestClientIPPrefersLastXForwardedForEntry(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start", http.NoBody)
	r.RemoteAddr = albRemoteAddr // ALB's own address — must be ignored when XFF is set.
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2 , 203.0.113.77")
	if got := clientIP(r); got != "203.0.113.77" {
		t.Errorf("clientIP = %q want last trimmed XFF entry 203.0.113.77", got)
	}
}

// TestClientIPKeysByXForwardedFor proves the limiter buckets by the
// XFF-derived IP: two requests sharing a RemoteAddr but differing in their
// trusted XFF client IP get independent budgets.
func TestClientIPKeysByXForwardedFor(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	rl := newRateLimiter(fixedClock(&clock))

	exhaust := func(xff string) {
		for range rateLimitMaxRequests {
			r := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start", http.NoBody)
			r.RemoteAddr = albRemoteAddr
			r.Header.Set("X-Forwarded-For", xff)
			rec := httptest.NewRecorder()
			rl.middleware(sentinelHandler(new(bool))).ServeHTTP(rec, r)
			if rec.Code != http.StatusOK {
				t.Fatalf("warmup for %q: got %d want 200", xff, rec.Code)
			}
		}
	}
	exhaust("203.0.113.1")

	// Same RemoteAddr, different trusted client IP → fresh bucket.
	r := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start", http.NoBody)
	r.RemoteAddr = albRemoteAddr
	r.Header.Set("X-Forwarded-For", "203.0.113.2")
	rec := httptest.NewRecorder()
	rl.middleware(sentinelHandler(new(bool))).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("second XFF client IP: got %d want 200 — must key by XFF, not RemoteAddr", rec.Code)
	}
}

// TestClientIPFallsBackToUnknown locks the sentinel path: no XFF and an
// unparseable RemoteAddr collapse to the shared "unknown" bucket rather
// than escaping the limit.
func TestClientIPFallsBackToUnknown(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/oauth/qurl/start", http.NoBody)
	r.RemoteAddr = "garbage-no-port"
	if got := clientIP(r); got != unknownIP {
		t.Errorf("clientIP = %q want %q for unparseable RemoteAddr", got, unknownIP)
	}
}

// fillToCap seeds the limiter with maxStoreSize distinct in-window IPs by
// poking the map directly under the clock — far cheaper than driving 20k
// requests through the middleware, and it lets the reclamation tests
// control exactly which buckets are stale.
func fillToCap(rl *rateLimiter, now time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for i := range maxStoreSize {
		rl.requests[strconv.Itoa(i)] = []time.Time{now}
	}
}

// TestRateLimiterShedsNewIPAtCapacity fences the memory-safety ceiling:
// once maxStoreSize distinct in-window IPs are tracked, a previously
// unseen IP is shed with 429 + a full-window Retry-After and is NOT added
// to the map (the OOM guard holds). A known IP is still served.
func TestRateLimiterShedsNewIPAtCapacity(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	rl := newRateLimiter(fixedClock(&clock))
	fillToCap(rl, clock)

	ok, retryAfter := rl.allow("fresh-ip")
	if ok {
		t.Fatal("at capacity a previously-unseen IP must be shed, got allowed")
	}
	if want := int(rateLimitWindow.Seconds()); retryAfter != want {
		t.Errorf("shed Retry-After = %d want full window %d", retryAfter, want)
	}
	if got := len(rl.requests); got != maxStoreSize {
		t.Errorf("map grew past cap: len = %d want %d (shed IP must not be stored)", got, maxStoreSize)
	}

	// A key that is already tracked must still be served at cap — serving
	// it doesn't grow the map.
	if ok, _ := rl.allow("0"); !ok {
		t.Error("a known IP must still be served at capacity")
	}
}

// TestRateLimiterReclaimsStaleKeysAtCapacity fences the fix for the cr's
// significant finding: at cap, a fresh IP triggers a bounded sweep that
// drops a fully stale key so the key set can actually shrink. A one-time
// flood of distinct IPs must not wedge the limiter into 429ing every new
// install forever. After the window rolls, the arriving IP is admitted
// (not shed) and a previously-seeded stale key is gone — the new IP took
// the freed slot, so total size stays at cap by design (one stale out, one
// fresh in).
func TestRateLimiterReclaimsStaleKeysAtCapacity(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	rl := newRateLimiter(fixedClock(&clock))
	fillToCap(rl, clock)

	// Roll the clock past the window so every seeded bucket is now stale.
	clock = clock.Add(rateLimitWindow + time.Second)

	ok, _ := rl.allow("fresh-ip")
	if !ok {
		t.Fatal("after the window rolled, the arriving IP must be admitted via reclamation, got shed")
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()
	if _, present := rl.requests["fresh-ip"]; !present {
		t.Error("admitted IP must be stored")
	}
	// Exactly one stale seed key should have been reclaimed to make room
	// (the sweep stops after freeing the first slot the arrival needs).
	remaining := 0
	for i := range maxStoreSize {
		if _, present := rl.requests[strconv.Itoa(i)]; present {
			remaining++
		}
	}
	if remaining != maxStoreSize-1 {
		t.Errorf("stale seed keys remaining = %d want %d (a stale key must be reclaimed)", remaining, maxStoreSize-1)
	}
}

// TestRateLimiterReclamationUnwedgesAfterFlood is the end-to-end proof of
// the cr's scenario: a one-time flood fills the map with now-stale keys,
// and afterward a stream of fresh installs all succeed instead of being
// permanently 429'd. Each admit reclaims one stale key, so the wedge
// clears progressively rather than never.
func TestRateLimiterReclamationUnwedgesAfterFlood(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	rl := newRateLimiter(fixedClock(&clock))
	fillToCap(rl, clock)

	// The flood is over; the window rolls so every flood key is stale.
	clock = clock.Add(rateLimitWindow + time.Second)

	for i := range 50 {
		ok, _ := rl.allow("post-flood-" + strconv.Itoa(i))
		if !ok {
			t.Fatalf("post-flood install %d was 429'd — the map stayed wedged at cap", i)
		}
	}
}

// TestRateLimiterConcurrentSameIPNeverExceedsBudget fences the mutex
// contract the whole limiter rests on: with the clock frozen (the window
// never rolls), many goroutines hammering ONE IP must see EXACTLY
// rateLimitMaxRequests admits in total, regardless of scheduling. The
// evict-count-append in allow() is a read-modify-write on the per-IP
// slice; if any of it escaped the lock, two goroutines could both observe
// a sub-budget count and overspend. The serial tests assert correctness by
// construction — this is the only one that exercises real contention (run
// with -race, as CI does, to also surface the data race directly).
func TestRateLimiterConcurrentSameIPNeverExceedsBudget(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	rl := newRateLimiter(fixedClock(&clock))

	const goroutines = 64
	const perGoroutine = 5 // 320 attempts, far over the budget of 10.
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				if ok, _ := rl.allow("203.0.113.42"); ok {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != rateLimitMaxRequests {
		t.Errorf("concurrent same-IP admits = %d, want exactly %d — budget breached under contention", got, rateLimitMaxRequests)
	}
}

// TestRateLimiterRetryAfterReflectsWindowRemainder pins the Retry-After
// arithmetic at a non-trivial clock offset. TestRateLimiterOverLimitRejects
// fills the budget at a single frozen instant, so every timestamp coincides
// and the advertised value is always the full window — that can't catch a
// ceil/offset regression. Here the budget is filled at t0, the clock
// advances partway into the window, and the rejected request must advertise
// the whole-seconds remainder until the oldest timestamp ages out.
func TestRateLimiterRetryAfterReflectsWindowRemainder(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	rl := newRateLimiter(fixedClock(&clock))

	// Fill the budget at t0 — all rateLimitMaxRequests timestamps are t0.
	for range rateLimitMaxRequests {
		if ok, _ := rl.allow("198.51.100.7"); !ok {
			t.Fatal("warmup request unexpectedly rejected")
		}
	}
	// Advance 15s into the 60s window; the oldest timestamp (t0) ages out
	// in the remaining 45s.
	clock = clock.Add(15 * time.Second)
	ok, retryAfter := rl.allow("198.51.100.7")
	if ok {
		t.Fatal("request over budget should have been rejected")
	}
	if want := int((rateLimitWindow - 15*time.Second).Seconds()); retryAfter != want {
		t.Errorf("Retry-After = %d, want %d (whole seconds until the oldest timestamp ages out)", retryAfter, want)
	}
}

// minimalRouterConfig returns a Config that passes Validate() with the
// smallest viable surface — enough to call RegisterRoutes. AdminStore is
// left nil (so BindClassifyError isn't required) and the clock is pinned so
// the wiring test is deterministic. The handlers it builds are never driven
// to completion here; the test only proves the limiter wrap is present.
func minimalRouterConfig(clock *time.Time) Config {
	return Config{
		OAuthStateSecret: bytes.Repeat([]byte("k"), 32),
		SlackBaseURL:     "https://slack-bot.example.test",
		Now:              fixedClock(clock),
	}
}

// TestRegisterRoutesWrapsBothRoutesWithLimiter fences the wiring cr flagged:
// the limiter is only useful if RegisterRoutes actually wraps BOTH routes. A
// regression that dropped an rl.middleware(...) wrap would leave the route
// reachable past the budget. Driving each registered path 11× (budget+1)
// from one IP must surface a 429 — proving the wrap is in place per route —
// and the SHARED limiter means the second route is already over budget from
// the first route's traffic (same IP), which the assertion tolerates by
// checking "429 seen within budget+1 requests" rather than an exact count.
func TestRegisterRoutesWrapsBothRoutesWithLimiter(t *testing.T) {
	clock := time.Unix(1700000000, 0)
	mux := http.NewServeMux()
	RegisterRoutes(mux, minimalRouterConfig(&clock))

	for _, path := range []string{StartPath, callbackPath} {
		saw429 := false
		for range rateLimitMaxRequests + 1 {
			r := httptest.NewRequest(http.MethodGet, path, http.NoBody)
			r.RemoteAddr = "203.0.113.200:5000" // one IP across both routes (shared budget)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)
			if rec.Code == http.StatusTooManyRequests {
				saw429 = true
				break
			}
		}
		if !saw429 {
			t.Errorf("route %s: no 429 within %d requests — limiter wrap appears missing", path, rateLimitMaxRequests+1)
		}
	}
}

// TestRateLimiterOverBudgetLogIsThrottled fences the observability +
// throttle contract cr pushed on: an IP that keeps hammering after exhausting
// its budget must emit exactly ONE warning per rejectLogInterval (not one per
// rejected request — that would make the limiter a log-amplifier), and the
// suppressed rejections must be coalesced into the next line's count. After
// the interval elapses the path logs again, this time reporting how many it
// swallowed in between.
func TestRateLimiterOverBudgetLogIsThrottled(t *testing.T) {
	buf := captureSlog(t)
	clock := time.Unix(1700000000, 0)
	rl := newRateLimiter(fixedClock(&clock))

	// Spend the budget (these admits don't log).
	for range rateLimitMaxRequests {
		if ok, _ := rl.allow("203.0.113.51"); !ok {
			t.Fatal("warmup admit unexpectedly rejected")
		}
	}
	// 20 rejections at the SAME frozen instant: the first logs, the other 19
	// are suppressed (clock hasn't advanced past the interval).
	for range 20 {
		if ok, _ := rl.allow("203.0.113.51"); ok {
			t.Fatal("over-budget request unexpectedly admitted")
		}
	}
	if got := strings.Count(buf.String(), "per-IP budget exceeded"); got != 1 {
		t.Fatalf("over-budget warnings before interval elapsed = %d, want exactly 1 (rest must be throttled)", got)
	}

	// Advance past the throttle interval. rejectLogInterval == rateLimitWindow,
	// so the window has also rolled and the original timestamps have aged out:
	// a sustained attacker re-saturates the budget, then the next rejection
	// logs again — this is the real "still under attack a window later" path,
	// not an artificial one. The 19 swallowed in phase 1 are still pending
	// (admits don't touch the suppressed counter), so they surface now.
	clock = clock.Add(rejectLogInterval + time.Second)
	buf.Reset()
	for range rateLimitMaxRequests {
		if ok, _ := rl.allow("203.0.113.51"); !ok {
			t.Fatal("re-saturation admit unexpectedly rejected after window rolled")
		}
	}
	if ok, _ := rl.allow("203.0.113.51"); ok {
		t.Fatal("re-saturated over-budget request unexpectedly admitted")
	}
	out := buf.String()
	if got := strings.Count(out, "per-IP budget exceeded"); got != 1 {
		t.Fatalf("over-budget warnings after interval = %d, want 1", got)
	}
	if !strings.Contains(out, "suppressed_since=19") {
		t.Errorf("second warning must coalesce the 19 suppressed rejections; got: %s", out)
	}
}

// TestRateLimiterAtCapShedLogsOnce fences the high-signal at-cap shed
// warning — the event cr most wanted alertable. The shed path is throttled on
// its OWN clock, independent of the over-budget path, so a distributed flood
// of distinct IPs all shed at one frozen instant produces a single line (with
// the store_size attr) rather than one per shed IP.
func TestRateLimiterAtCapShedLogsOnce(t *testing.T) {
	buf := captureSlog(t)
	clock := time.Unix(1700000000, 0)
	rl := newRateLimiter(fixedClock(&clock))
	fillToCap(rl, clock) // every seeded key is in-window, so reclamation frees nothing

	for i := range 5 {
		if ok, _ := rl.allow("fresh-" + strconv.Itoa(i)); ok {
			t.Fatalf("shed %d: a fresh IP must be shed at cap", i)
		}
	}
	out := buf.String()
	if got := strings.Count(out, "store at hard cap"); got != 1 {
		t.Fatalf("at-cap shed warnings = %d, want exactly 1 (throttled)", got)
	}
	if !strings.Contains(out, "store_size=") {
		t.Errorf("shed warning must carry store_size for ops; got: %s", out)
	}
	// The over-budget path must NOT have logged — the two throttles are
	// independent, and nothing here exercised an over-budget IP.
	if strings.Contains(out, "per-IP budget exceeded") {
		t.Error("at-cap shed must not emit the over-budget message")
	}
}
