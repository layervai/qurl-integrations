// Shared rate-limit middleware for OAuth callback routes.
//
// Extracted from src/routes/oauth.js so the GitHub OAuth flow, the qURL
// OAuth flow, and the Discord install flow all share the same per-IP
// budget — otherwise each router carries its own counter and an attacker
// can amplify by hammering the same IP across multiple routes.
//
// SCALING: single-instance only. If this bot ever runs horizontally
// (multiple ECS tasks behind a LB), move this to Redis so limits are
// shared — otherwise each replica carries its own counter and effective
// rate is N × configured.
const config = require('../config');
const logger = require('../logger');

const rateLimitStore = new Map();

// Evict stale entries on a 30-second timer (was 5 minutes). Under a burst
// from many unique IPs, a longer sweep interval lets the Map grow much
// larger than the per-request 10% eviction can keep up with. 30s is a
// sweet spot: short enough to bound steady-state memory, long enough to
// not matter as load.
const sweepHandle = setInterval(() => {
  const cutoff = Date.now() - config.RATE_LIMIT_WINDOW_MS * 2;
  for (const [ip, requests] of rateLimitStore) {
    const recent = requests.filter(t => t > cutoff);
    if (recent.length === 0) rateLimitStore.delete(ip);
    else rateLimitStore.set(ip, recent);
  }
}, 30 * 1000);
sweepHandle.unref();

// Absolute cap on how many timestamps we keep per IP so an abusive IP
// can't grow its array unboundedly between eviction sweeps.
const MAX_REQUESTS_PER_IP = Math.max(config.RATE_LIMIT_MAX_REQUESTS * 4, 100);
// Hard ceiling on total Map size. Under a distributed attack from many
// unique IPs the 10% drop eviction can't keep up if new IPs arrive faster
// than the sweep runs. Once the Map exceeds this, new-IP requests get 429
// until the next sweep reclaims space — better to shed load than OOM.
const MAX_STORE_SIZE = 20000;

function rateLimit(req, res, next) {
  const ip = req.ip || 'unknown'; // req.ip uses x-forwarded-for via 'trust proxy' (server.js)
  const now = Date.now();
  const windowStart = now - config.RATE_LIMIT_WINDOW_MS;

  // Hard memory ceiling: if the store is already at MAX_STORE_SIZE and
  // this is a new IP, shed the request rather than grow the Map further.
  // Known IPs still get served because they're not growing the Map.
  if (rateLimitStore.size >= MAX_STORE_SIZE && !rateLimitStore.has(ip)) {
    logger.warn('Rate limit store at hard cap, rejecting new IP', { ip, size: rateLimitStore.size });
    return res.status(429).send(res.renderPage({
      title: 'Too Many Requests',
      icon: '⏳',
      heading: 'Service Overloaded',
      message: 'The service is under heavy load. Please try again in a moment.',
      type: 'warning',
    }));
  }

  const requests = (rateLimitStore.get(ip) || []).filter(time => time > windowStart);
  if (requests.length >= config.RATE_LIMIT_MAX_REQUESTS) {
    logger.warn('OAuth rate limit exceeded', { ip, path: req.path });
    return res.status(429).send(res.renderPage({
      title: 'Too Many Requests',
      icon: '⏳',
      heading: 'Slow Down!',
      message: 'You\'ve made too many requests. Please wait a moment and try again.',
      type: 'warning',
    }));
  }

  requests.push(now);
  // Trim the per-IP array to MAX_REQUESTS_PER_IP so one IP can't
  // accumulate thousands of timestamps between sweeps.
  if (requests.length > MAX_REQUESTS_PER_IP) {
    requests.splice(0, requests.length - MAX_REQUESTS_PER_IP);
  }
  rateLimitStore.set(ip, requests);
  // Under a distributed attack from many unique IPs, evicting only one
  // entry at a time can't keep up. When we cross 10k, drop the oldest
  // 10% (Map iteration is insertion order) so the store reclaims
  // meaningfully.
  if (rateLimitStore.size >= 10000) {
    const dropCount = Math.max(1, Math.floor(rateLimitStore.size / 10));
    const it = rateLimitStore.keys();
    for (let i = 0; i < dropCount; i++) {
      const k = it.next().value;
      if (k === undefined) break;
      rateLimitStore.delete(k);
    }
  }
  return next();
}

module.exports = { rateLimit, rateLimitStore };
