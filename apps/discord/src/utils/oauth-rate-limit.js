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
const config = require('../config');
const logger = require('../logger');

const rateLimitStore = new Map();

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

// Absolute cap on how many timestamps we keep per logical bucket per IP so
// an abusive IP can't grow an array unboundedly between eviction sweeps.
// Total per-IP retention is this cap times the fixed bucket count (currently
// two); adding another bucket must account for that linear memory increase.
const MAX_REQUESTS_PER_BUCKET_PER_IP = Math.max(config.RATE_LIMIT_MAX_REQUESTS * 4, 100);
// Hard ceiling on total Map size. Under a distributed attack from many
// unique IPs, new-IP requests get 429 once the store reaches this size
// until the next sweep reclaims space — better to shed load than OOM.
const MAX_STORE_SIZE = 20000;

function evictInstallOnlyEntry() {
  for (const [ip, buckets] of rateLimitStore) {
    if (!buckets.callback) {
      rateLimitStore.delete(ip);
      return true;
    }
  }
  return false;
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
  // Trim each per-IP bucket so one IP cannot accumulate thousands of
  // timestamps between sweeps.
  if (requests.length > MAX_REQUESTS_PER_BUCKET_PER_IP) {
    requests.splice(0, requests.length - MAX_REQUESTS_PER_BUCKET_PER_IP);
  }
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
