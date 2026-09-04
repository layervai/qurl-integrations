// Express server for OAuth and webhooks
const crypto = require('crypto');
const express = require('express');
const helmet = require('helmet');
const config = require('./config');
const db = require('./store');
const logger = require('./logger');
const { LOG_EVENTS } = require('./constants');
const { renderPage } = require('./templates/page');
const { isPositiveFinite } = require('./utils/time');
const qurlOAuthRouter = require('./routes/qurl-oauth');
const discordInstallRouter = require('./routes/discord-install');
const qurlWebhookRouter = require('./routes/qurl-webhook');
const webhookSubscriptions = require('./webhook-subscriptions');

const app = express();

function generateCspNonce() {
  return crypto.randomBytes(16).toString('base64url');
}

function cspNonceSource(_req, res) {
  return `'nonce-${res.locals.cspNonce}'`;
}

app.use((req, res, next) => {
  res.locals.cspNonce = generateCspNonce();
  res.renderPage = (options) => renderPage({ ...options, cspNonce: res.locals.cspNonce });
  next();
});

// Trust proxy headers (ECS behind ALB) for correct req.ip in rate limiting.
// Controlled by TRUST_PROXY env var: "1"=trust one hop, "2"=two hops, etc.
// Leaving it unset (dev direct-connect) ignores X-Forwarded-For so a local
// caller can't spoof it to bypass rate limiting. Staging behind an LB
// should set TRUST_PROXY=1 even with NODE_ENV != production.
//
// SECURITY: ALWAYS pass a finite POSITIVE integer to `app.set('trust
// proxy', …)`. Express also accepts `true` (trust ALL hops) and `'true'`
// (string), both of which let an attacker spoof X-Forwarded-For via any
// upstream proxy to rotate their apparent IP and bypass the per-IP rate
// limiter. The parseInt + isPositiveFinite chain below rejects all of
// those: `'true'` / `'yes'` parse to NaN, negative values are dropped,
// and a missing env falls through to the production-only default of `1`
// (one hop = the ALB) rather than `true`.
if (process.env.TRUST_PROXY) {
  const hops = parseInt(process.env.TRUST_PROXY, 10);
  if (isPositiveFinite(hops)) {
    app.set('trust proxy', hops);
  } else {
    logger.warn(`Ignoring invalid TRUST_PROXY=${process.env.TRUST_PROXY} (must be a positive integer hop count, NOT 'true')`);
  }
} else if (process.env.NODE_ENV === 'production') {
  // Default for production if nothing configured. Numeric, NEVER boolean.
  app.set('trust proxy', 1);
}

// helmet covers HSTS, X-Content-Type-Options, X-Frame-Options, Referrer-
// Policy, X-DNS-Prefetch-Control, etc. The HTTP CSP is the single source of
// truth for allowing renderPage's nonce'd inline stylesheet.
app.use(helmet({
  contentSecurityPolicy: {
    useDefaults: false,
    directives: {
      defaultSrc: ["'none'"],
      // No legacy inline-style fallback: old CSP1-only browsers get a
      // readable unstyled admin page instead of reopening 'unsafe-inline'.
      styleSrc: [cspNonceSource],
      imgSrc: ["'self'", 'data:'],
      connectSrc: ["'self'"],
      baseUri: ["'none'"],
      frameAncestors: ["'none'"],
      formAction: ["'none'"],
    },
  },
  crossOriginEmbedderPolicy: false,
}));

// Parse JSON for webhooks with raw body for signature verification. The 1mb
// cap is part of the qURL receiver's threat model: it bounds the raw-body
// owner_id parse that selects the per-owner HMAC secret before the request is
// trusted. MUST be registered BEFORE the general app.use(express.json()) below
// so webhook requests hit this parser first and get req.rawBody populated.
// The receiver intentionally ignores req.body; keeping express.json here still
// enforces the cap and rejects malformed JSON before the router runs.
const rawBodyJson = express.json({
  limit: '1mb',
  verify: (req, _res, buf) => { req.rawBody = buf; },
});

// /webhooks (qURL) is the only raw-body surface — the receiver returns
// 503 when the per-guild subscription registry is still warming up
// (cold-start or sibling-replica lag), so a fresh deploy never accepts
// traffic before it can verify a signature.
app.use('/webhooks', rawBodyJson);

app.use(express.json({ limit: '1mb' }));

// Health check — verifies service is actually functional
app.get('/', (req, res) => {
  res.json({
    status: 'ok',
    service: 'qURL Discord Bot',
  });
});

app.get('/health', async (req, res) => {
  // Cheap data-layer probe — fails the check if the backend is
  // blocked/locked so the orchestrator replaces the container.
  // Uses db.healthCheck() (constant cost) instead of db.getStats()
  // (scan + aggregation). On DDB, getStats() is a full-table Scan
  // — at LB health-check cadence (10–30s) that's real RCU and
  // grows with table size. healthCheck() is O(1) (single GetItem
  // on a sentinel key).
  try {
    await db.healthCheck();
    res.status(200).json({ status: 'ok' });
  } catch (err) {
    // Log full detail internally; omit from the response so backend
    // error messages (paths, schema, AWS internals) don't leak to
    // an unauthenticated probe.
    logger.warn('Health check failed', { error: err.message });
    res.status(503).json({ status: 'unhealthy' });
  }
});

// Per-IP rate limit on /metrics. Even a token holder shouldn't be able to
// hammer the endpoint — getStats() runs a paginated full-table Scan per
// counted table, plus memoryUsage() + uptime(), every hit. Simple in-memory window; single-instance only
// (matches the SCALING comments on the OAuth/webhooks rate limiters).
const metricsRateStore = new Map(); // ip -> number[] (request timestamps)
const METRICS_WINDOW_MS = 60_000;
const METRICS_MAX_PER_WINDOW = 30;
// Evict stale entries periodically so the Map can't grow unboundedly
// under scans from many unique IPs. Stored so it can be cleared on
// graceful shutdown — .unref() keeps it from blocking exit, but an
// explicit clear keeps the shutdown path symmetric with other intervals.
const metricsSweepInterval = setInterval(() => {
  const cutoff = Date.now() - METRICS_WINDOW_MS * 2;
  for (const [ip, times] of metricsRateStore) {
    const recent = times.filter(t => t > cutoff);
    if (recent.length === 0) metricsRateStore.delete(ip);
    else metricsRateStore.set(ip, recent);
  }
}, 30_000);
metricsSweepInterval.unref();

function metricsRateLimit(req, res, next) {
  const ip = req.ip || 'unknown';
  const now = Date.now();
  const windowStart = now - METRICS_WINDOW_MS;
  const recent = (metricsRateStore.get(ip) || []).filter(t => t > windowStart);
  if (recent.length >= METRICS_MAX_PER_WINDOW) {
    return res.status(429).json({ error: 'Rate limit exceeded' });
  }
  recent.push(now);
  metricsRateStore.set(ip, recent);
  next();
}

// Metrics endpoint
app.get('/metrics', metricsRateLimit, async (req, res) => {
  // Default-deny: require METRICS_TOKEN in every environment. An accidentally
  // unset NODE_ENV in staging/preview should never expose stats.
  if (!process.env.METRICS_TOKEN) {
    return res.status(503).json({ error: 'Metrics not configured' });
  }
  const auth = req.headers.authorization || '';
  const expected = `Bearer ${process.env.METRICS_TOKEN}`;
  // Hash both to fixed-length buffers before constant-time compare so the
  // length check itself does not leak the expected token's length.
  const authHash = crypto.createHash('sha256').update(auth).digest();
  const expectedHash = crypto.createHash('sha256').update(expected).digest();
  if (!crypto.timingSafeEqual(authHash, expectedHash)) {
    return res.status(401).json({ error: 'Unauthorized' });
  }
  const stats = await db.getStats();
  res.json({
    status: 'ok',
    uptime: process.uptime(),
    memory: process.memoryUsage(),
    stats,
  });
});

// Unconditional mount. The receiver returns 503 while the per-guild
// subscription registry (src/webhook-subscriptions.js) is unprimed
// OR within the sibling-replica lag window — qurl-service retries
// 503, so a fresh deploy never silently drops inbound webhooks.
// Pure-BYOK setups (no QURL_WEBHOOK_SECRET) are supported; the
// receiver matches each inbound event against the per-guild secret
// the linking flow registered.
app.use('/webhooks', qurlWebhookRouter);
if (!config.QURL_WEBHOOK_SECRET) {
  if (config.QURL_WEBHOOK_PURE_BYOK) {
    logger.warn('QURL_WEBHOOK_SECRET unset with QURL_WEBHOOK_PURE_BYOK=true — running without a default-key subscription');
  } else {
    logger.error('QURL_WEBHOOK_SECRET unset without QURL_WEBHOOK_PURE_BYOK=true — guild webhook linking will fail closed');
  }
}

// Cache-Control: no-store on every response from the OAuth surfaces —
// success page surfaces guild + qURL email + key prefix; error pages
// could leak detail in the future; not-configured page is also OAuth-
// adjacent. Applying as a router-level default removes a conditional
// invariant per-handler (round-9 #6) and is zero-cost on these
// low-traffic paths. `no-store` alone is sufficient — intermediates
// MUST NOT cache regardless of any other header.
function noStoreHeaders(req, res, next) {
  res.setHeader('Cache-Control', 'no-store');
  next();
}

// qURL OAuth routes (/oauth/qurl/start + /oauth/qurl/callback). These
// always mount because /qurl setup is the canonical path for any guild
// (multi-tenant or single-guild) to configure a qURL API key, and the
// route gates internally on
// config.isQurlSetupAvailable (returns 503 with a "not configured yet"
// page when AUTH0_* env vars are unset, rather than a hard 404). That way
// setting valid AUTH0_* secrets and an accepted optional connection policy in
// SSM is all that is needed to turn OAuth on — no code change or feature-flag
// re-flip required.
app.use('/oauth/qurl', noStoreHeaders, qurlOAuthRouter);
const auth0Connection = config.AUTH0_EMAIL_CONNECTION || null;
const auth0ConnectionPolicyMetadata = {
  event: LOG_EVENTS.QURL_OAUTH_AUTH0_CONNECTION_POLICY,
  connection: auth0Connection,
  state: config.auth0EmailConnectionState,
  oauth_configured: config.isQurlOAuthConfigured,
};
if (!config.isQurlSetupAvailable) {
  logger.info(config.isAuth0EmailConnectionRejected
    ? 'qURL OAuth routes mounted in not-configured mode because AUTH0_EMAIL_CONNECTION was rejected. /qurl setup is blocked until the deployment value is corrected; other bot operations remain available.'
    : 'qURL OAuth routes mounted in not-configured mode because AUTH0_* settings are incomplete/invalid. /qurl setup will fall back to the legacy modal-paste path.');
  let inactiveConnectionMessage;
  if (config.isAuth0EmailConnectionRejected) {
    inactiveConnectionMessage = 'AUTH0_EMAIL_CONNECTION was rejected and is inactive because qURL OAuth setup is disabled until the deployment value is corrected.';
  } else if (auth0Connection) {
    inactiveConnectionMessage = `AUTH0_EMAIL_CONNECTION="${auth0Connection}" is set but inactive because qURL OAuth AUTH0_* settings are incomplete.`;
  } else {
    inactiveConnectionMessage = 'AUTH0_EMAIL_CONNECTION is unset and inactive because qURL OAuth AUTH0_* settings are incomplete.';
  }
  if (config.isAuth0EmailConnectionRejected) {
    logger.error(inactiveConnectionMessage, auth0ConnectionPolicyMetadata);
  } else {
    logger.info(inactiveConnectionMessage, auth0ConnectionPolicyMetadata);
  }
} else {
  let auth0ConnectionMessage;
  if (auth0Connection) {
    auth0ConnectionMessage = `qURL OAuth authorize redirects pin Auth0 connection "${auth0Connection}"; the Auth0 application must enable it.`;
  } else {
    auth0ConnectionMessage = 'qURL OAuth authorize redirects send no connection pin (AUTH0_EMAIL_CONNECTION unset); upstream identity-provider sessions may still select an account until #1365.';
  }
  // Stable event metadata supports exact text/term filtering while the
  // human-readable message remains self-sufficient if metadata is flattened.
  // This is an operational logger line with a timestamp/level prefix, not a
  // bare logger.audit JSON record, so CloudWatch JSON field filters do not
  // apply to it.
  // OAuth-live + unpinned warns once per process because the risk is reachable;
  // an inactive pin stays info-level because incomplete OAuth disables the
  // affected flow entirely.
  if (auth0Connection) {
    logger.info(auth0ConnectionMessage, auth0ConnectionPolicyMetadata);
  } else {
    logger.warn(auth0ConnectionMessage, auth0ConnectionPolicyMetadata);
  }
}

// Stage-2 Discord install flow. /oauth/discord/install creates the
// session-bound Discord authorization URL; Discord then returns to
// /oauth/discord/callback after an admin selects a server. Always mount
// so both public URLs are stable regardless of config; the route gates internally on
// config.isDiscordInstallConfigured (returns 503 when credentials are
// incomplete/invalid or BASE_URL cannot retain the Secure session cookie).
app.use('/oauth/discord', noStoreHeaders, discordInstallRouter);
if (!config.isDiscordInstallConfigured) {
  logger.info('Discord install flow mounted in not-configured mode (credentials incomplete/invalid or BASE_URL unsafe for Secure cookies).');
}

// Error handler (Express requires the 4-arg signature; `next` unused)
// eslint-disable-next-line no-unused-vars
app.use((err, req, res, next) => {
  logger.error('Express error', { error: err.message, stack: err.stack });
  res.status(500).send(res.renderPage({
    title: 'Server Error',
    icon: '💥',
    heading: 'Internal Server Error',
    message: 'Something went wrong on our end. Please try again later.',
    type: 'error',
  }));
});

// Start server
function startServer() {
  const server = app.listen(config.PORT, () => {
    logger.info(`Web server listening on port ${config.PORT}`);
    logger.info(`Metrics URL: ${config.BASE_URL}/metrics`);
  });
  return server;
}

function stopIntervals() {
  clearInterval(metricsSweepInterval);
  // The qURL webhook router owns a per-IP bad-sig sweep; stop it on
  // graceful shutdown so the interval doesn't outlive the server.
  if (typeof qurlWebhookRouter.stopIntervals === 'function') qurlWebhookRouter.stopIntervals();
  // 30s subscription-registry refresh ticker (per-guild webhook
  // secrets cache). No-op on the gateway tier where the registry was
  // never started; required on the HTTP tier so the ticker doesn't
  // outlive the server during graceful shutdown.
  webhookSubscriptions.stop();
}

module.exports = { app, startServer, stopIntervals };
