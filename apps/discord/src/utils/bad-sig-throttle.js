// Per-IP bad-signature throttle for HMAC-authenticated public routes.
// Without it, an attacker can spam invalid signatures and burn unbounded
// HMAC compute on a public ALB endpoint. Legitimate traffic (valid HMAC)
// is never throttled — the route only calls record() on a verify failure.
//
// Each route gets its OWN counter via a fresh factory instance — a
// webhooks abuser shouldn't ratelimit canary callers, and vice versa.
// SCALING: single-instance only. Move to Redis if the bot ever runs
// horizontally; same caveat as the OAuth rate limiter and the original
// webhooks.js inline throttle this helper subsumes.
//
// Migration note (qurl-integrations#211 follow-up): apps/discord/src/
// routes/webhooks.js still has its own inline copy of this pattern.
// That file's BAD_SIG_WINDOW_MS / BAD_SIG_MAX / recordBadSig + sweep
// interval should migrate to this factory in a separate PR — touching
// it in #139 would expand the canary-route PR's blast radius into a
// security-sensitive route. The shape here is intentionally a superset
// (configurable windowMs / maxPerWindow / perIpCap) so the migration
// is mechanical.

// 5-min sweep cadence — 2× the default 60s window keeps recently-
// expired entries around long enough that a bursty attacker pattern
// surfaces in logs even if it falls below the per-window threshold.
const SWEEP_INTERVAL_MS = 5 * 60_000;

// Hard global cap on the IP-keyed Map — protects against a distributed
// flood of unique IPs that would otherwise grow the Map unboundedly
// between sweeps. Same shape as oauth-rate-limit.js's MAX_STORE_SIZE.
const MAX_DISTINCT_IPS = 10_000;

/**
 * Build a fresh throttle bound to `(windowMs, maxPerWindow)`. Each
 * factory call returns its own private Map + sweep timer — callers
 * MUST share the returned object across requests for a single route
 * (typically by storing it at module scope).
 *
 * @param {object} [opts]
 * @param {number} [opts.windowMs=60000] — rolling window for the per-IP counter.
 * @param {number} [opts.maxPerWindow=30] — bad-sig attempts allowed per IP per window before check() returns true.
 * @returns {{ check: (ip: string) => boolean, record: (ip: string) => number, reset: () => void }}
 *   - check(ip): returns true if the IP is over the limit (caller short-circuits the request).
 *   - record(ip): registers a bad-sig attempt. Returns the post-record count for log context.
 *   - reset(): clears the entire Map. Test-only — DO NOT call from the request path.
 */
function createBadSigThrottle({ windowMs = 60_000, maxPerWindow = 30 } = {}) {
  // Per-IP cap on the timestamp array — prevents a single abusive IP
  // from growing its array unboundedly between sweeps. 4× the
  // per-window threshold keeps the array bounded while preserving
  // enough history for the rolling-window check.
  const perIpCap = maxPerWindow * 4;

  // ip -> number[] (timestamps of bad-sig attempts within the window).
  const attempts = new Map();

  // Sweep stale entries periodically. .unref() so the timer doesn't
  // hold the event loop open at shutdown.
  const sweep = setInterval(() => {
    const cutoff = Date.now() - windowMs * 2;
    for (const [ip, times] of attempts) {
      const recent = times.filter(t => t > cutoff);
      if (recent.length === 0) attempts.delete(ip);
      else attempts.set(ip, recent);
    }
  }, SWEEP_INTERVAL_MS);
  if (typeof sweep.unref === 'function') sweep.unref();

  function check(ip) {
    const now = Date.now();
    const recent = (attempts.get(ip) || []).filter(t => t > now - windowMs);
    return recent.length >= maxPerWindow;
  }

  function record(ip) {
    const now = Date.now();
    let list = (attempts.get(ip) || []).filter(t => t > now - windowMs);
    list.push(now);
    if (list.length > perIpCap) {
      list = list.slice(-perIpCap);
    }
    if (attempts.size > MAX_DISTINCT_IPS) {
      // 10%-drop eviction — single-entry can't keep up with a
      // distributed flood of unique IPs. Same strategy as
      // oauth-rate-limit.js's rateLimitStore.
      const dropCount = Math.max(1, Math.floor(attempts.size / 10));
      const it = attempts.keys();
      for (let i = 0; i < dropCount; i++) {
        const k = it.next().value;
        if (k === undefined) break;
        attempts.delete(k);
      }
    }
    attempts.set(ip, list);
    return list.length;
  }

  function reset() {
    attempts.clear();
  }

  return { check, record, reset };
}

module.exports = { createBadSigThrottle };
