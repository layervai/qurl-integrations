// Shared rate-limit middleware for OAuth callback routes.
//
// Extracted from src/routes/oauth.js so the GitHub OAuth flow, the qURL
// OAuth flow, and the Discord install callback share the same per-IP budget.
// The public Discord install entrypoint gets a separate per-IP bucket in the
// same bounded store, so entry-page traffic from one IP cannot consume that
// IP's callback budget. At the global hard cap, callbacks can evict an
// install-only entry; public entry-page traffic therefore cannot starve a
// short-lived Discord authorization code arriving from a new IP.
//
// SCALING: single-instance only. If this bot ever runs horizontally
// (multiple ECS tasks behind a LB), move this to Redis so limits are
// shared — otherwise each replica carries its own counter and effective
// rate is N × configured.
//
// OVERLOAD: the store deliberately grows to the 20k hard cap and then sheds
// every unseen IP until the periodic sweep can reclaim entries (up to two
// rate-limit windows). This preserves accumulated per-IP counters instead of
// weakening the limiter with bulk eviction. Callback traffic may displace an
// install-only entry in O(1), but it cannot displace another callback entry.
const config = require('../config');
const logger = require('../logger');

const installOnlyIps = new Set();
class IndexedRateLimitStore extends Map {
  set(ip, buckets) {
    super.set(ip, buckets);
    if (buckets['discord-install-entry'] && !buckets.callback) installOnlyIps.add(ip);
    else installOnlyIps.delete(ip);
    return this;
  }

  delete(ip) {
    installOnlyIps.delete(ip);
    return super.delete(ip);
  }

  clear() {
    installOnlyIps.clear();
    super.clear();
  }
}
const rateLimitStore = new IndexedRateLimitStore();

// Evict stale entries on a 30-second timer (was 5 minutes). Under a burst
// from many unique IPs, a longer sweep interval lets the Map grow much
// larger between sweeps. The hard ceiling below bounds bursts, while 30s is
// short enough to reclaim expired entries without making sweeps hot-path work.
function sweepRateLimitStore() {
  const cutoff = Date.now() - config.RATE_LIMIT_WINDOW_MS * 2;
  for (const [ip, buckets] of rateLimitStore) {
    const recentBuckets = {};
    for (const [bucket, requests] of Object.entries(buckets)) {
      const recent = requests.filter(t => t > cutoff);
      if (recent.length > 0) recentBuckets[bucket] = recent;
    }
    if (Object.keys(recentBuckets).length === 0) rateLimitStore.delete(ip);
    else rateLimitStore.set(ip, recentBuckets);
  }
}

const sweepHandle = setInterval(sweepRateLimitStore, 30 * 1000);
sweepHandle.unref();

// Hard ceiling on total Map size. Under a distributed attack from many
// unique IPs, new-IP requests get 429 once the store reaches this size
// until the next sweep reclaims space — better to shed load than OOM.
const MAX_STORE_SIZE = 20000;

function evictInstallOnlyEntry() {
  const ip = installOnlyIps.values().next().value;
  return ip === undefined ? false : rateLimitStore.delete(ip);
}

function rateLimitForBucket(bucket, req, res, next) {
  const ip = req.ip || 'unknown'; // req.ip uses x-forwarded-for via 'trust proxy' (server.js)
  const now = Date.now();
  const windowStart = now - config.RATE_LIMIT_WINDOW_MS;

  // Hard memory ceiling: callbacks take priority over install-only entries.
  // This preserves the bound while ensuring traffic to the public /install
  // page cannot globally starve an in-flight callback from a new IP. If the
  // store contains only callback traffic, the normal overload shed remains.
  if (rateLimitStore.size >= MAX_STORE_SIZE && !rateLimitStore.has(ip)) {
    if (bucket === 'callback') evictInstallOnlyEntry();
  }
  if (rateLimitStore.size >= MAX_STORE_SIZE && !rateLimitStore.has(ip)) {
    logger.warn('Rate limit store at hard cap, rejecting new IP', {
      ip, bucket, size: rateLimitStore.size,
    });
    return res.status(429).send(res.renderPage({
      title: 'Too Many Requests',
      icon: '⏳',
      heading: 'Service Overloaded',
      message: 'The service is under heavy load. Please try again in a moment.',
      type: 'warning',
    }));
  }

  const buckets = rateLimitStore.get(ip) || {};
  const requests = (buckets[bucket] || []).filter(time => time > windowStart);
  if (requests.length >= config.RATE_LIMIT_MAX_REQUESTS) {
    logger.warn('OAuth rate limit exceeded', { ip, path: req.path, bucket });
    return res.status(429).send(res.renderPage({
      title: 'Too Many Requests',
      icon: '⏳',
      heading: 'Slow Down!',
      message: 'You\'ve made too many requests. Please wait a moment and try again.',
      type: 'warning',
    }));
  }

  requests.push(now);
  // The rejection above bounds each bucket at RATE_LIMIT_MAX_REQUESTS.
  rateLimitStore.set(ip, { ...buckets, [bucket]: requests });
  return next();
}

function rateLimit(req, res, next) {
  return rateLimitForBucket('callback', req, res, next);
}

function installRateLimit(req, res, next) {
  return rateLimitForBucket('discord-install-entry', req, res, next);
}

module.exports = {
  MAX_RATE_LIMIT_STORE_SIZE: MAX_STORE_SIZE,
  installRateLimit,
  rateLimit,
  rateLimitStore,
  sweepRateLimitStore,
};
