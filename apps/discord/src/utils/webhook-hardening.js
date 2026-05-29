// Shared hardening for HTTP webhook receivers (GitHub, qURL).
//
// Both routes need the same anti-abuse machinery (per-IP bad-signature
// counter with sweep + cap) and the same constant-time HMAC compare.
// Keeping the two implementations in lockstep by hand drifted at least
// once (GitHub's sweep had no clearInterval handle so graceful shutdown
// leaked the timer; the qURL copy added one); a shared module makes
// that class of drift impossible.
//
// SCALING: single-instance only. Move to Redis if the bot runs
// horizontally.

const crypto = require('crypto');

// 5min sweep deletes entries older than 2 windows — keeps an entry's
// next-request landing inside the active 60s window from being
// prematurely evicted. Do NOT "fix" the * 2 to * 1; the second window
// is the load-bearing slack against sweep-vs-request race.
function createBadSigLimiter({
  windowMs = 60_000,
  max = 30,
  perIpCap,
  sweepMs = 5 * 60 * 1000,
  globalCap = 10_000,
} = {}) {
  // perIpCap defaults to max * 4 so a caller bumping max=60 doesn't
  // leave the per-IP array sized for max=30.
  if (perIpCap === undefined) perIpCap = max * 4;
  const attempts = new Map();
  const sweep = setInterval(() => {
    const cutoff = Date.now() - windowMs * 2;
    for (const [ip, times] of attempts) {
      const recent = times.filter(t => t > cutoff);
      if (recent.length === 0) attempts.delete(ip);
      else attempts.set(ip, recent);
    }
  }, sweepMs);
  sweep.unref();

  return {
    // Skip the .filter() allocation when there's no prior entry — the
    // happy path of legit traffic never has a bad-sig history.
    shouldThrottle(ip) {
      const list = attempts.get(ip);
      if (!list) return false;
      return list.filter(t => t > Date.now() - windowMs).length >= max;
    },
    recordBadSig(ip) {
      const now = Date.now();
      let list = (attempts.get(ip) || []).filter(t => t > now - windowMs);
      list.push(now);
      if (list.length > perIpCap) list = list.slice(-perIpCap);
      // 10%-drop on global cap: single-entry eviction can't keep up
      // with a distributed flood of unique IPs.
      if (attempts.size > globalCap) {
        const drop = Math.max(1, Math.floor(attempts.size / 10));
        const it = attempts.keys();
        for (let i = 0; i < drop; i++) {
          const k = it.next().value;
          if (k === undefined) break;
          attempts.delete(k);
        }
      }
      attempts.set(ip, list);
      return list.length;
    },
    stopSweep() { clearInterval(sweep); },
  };
}

// `expectedHex` is the bare 64-char hex digest the caller pulled from
// the request header (any vendor-specific prefix like `sha256=` must be
// stripped first). Returns false on any error so callers can branch on
// a single bool.
function verifyHmacSha256(rawBody, secret, expectedHex) {
  if (!rawBody || !secret || !expectedHex) return false;
  const digest = crypto.createHmac('sha256', secret).update(rawBody).digest('hex');
  try {
    return crypto.timingSafeEqual(Buffer.from(expectedHex), Buffer.from(digest));
  } catch {
    return false;
  }
}

module.exports = { createBadSigLimiter, verifyHmacSha256 };
