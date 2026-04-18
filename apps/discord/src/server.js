// Express server for OAuth and webhooks
const crypto = require('crypto');
const express = require('express');
const helmet = require('helmet');
const config = require('./config');
const db = require('./database');
const logger = require('./logger');
const { renderPage } = require('./templates/page');
const oauthRouter = require('./routes/oauth');
const webhooksRouter = require('./routes/webhooks');

const app = express();

// Trust proxy headers (ECS behind ALB) for correct req.ip in rate limiting.
// Controlled by TRUST_PROXY env var: "1"=trust one hop, "2"=two hops, etc.
// Leaving it unset (dev direct-connect) ignores X-Forwarded-For so a local
// caller can't spoof it to bypass rate limiting. Staging behind an LB
// should set TRUST_PROXY=1 even with NODE_ENV != production.
if (process.env.TRUST_PROXY) {
  const hops = parseInt(process.env.TRUST_PROXY, 10);
  if (Number.isFinite(hops) && hops > 0) {
    app.set('trust proxy', hops);
  } else {
    logger.warn(`Ignoring invalid TRUST_PROXY=${process.env.TRUST_PROXY}`);
  }
} else if (process.env.NODE_ENV === 'production') {
  // Default for production if nothing configured.
  app.set('trust proxy', 1);
}

// helmet covers HSTS, X-Content-Type-Options, X-Frame-Options, Referrer-
// Policy, X-DNS-Prefetch-Control, etc. We set a restrictive DEFAULT CSP
// here so any future HTML route that forgets its own <meta http-equiv> CSP
// still gets strong defaults. Templates that need specific policies (e.g.
// a page that renders an inline style) can override with their own
// <meta> tag, which takes precedence over the HTTP header.
app.use(helmet({
  contentSecurityPolicy: {
    useDefaults: false,
    directives: {
      defaultSrc: ["'none'"],
      styleSrc: ["'self'", "'unsafe-inline'"],
      imgSrc: ["'self'", 'data:'],
      connectSrc: ["'self'"],
      baseUri: ["'none'"],
      frameAncestors: ["'none'"],
      formAction: ["'none'"],
    },
  },
  crossOriginEmbedderPolicy: false,
}));

// Parse JSON for webhooks with raw body for signature verification. MUST be
// registered BEFORE the general app.use(express.json()) below so /webhook
// requests hit this parser first and get req.rawBody populated. Do not
// reorder without also updating routes/webhooks.js verifySignature().
//
// Startup contract: routes/webhooks.js verifySignature() asserts req.rawBody
// exists at request time and refuses the request with an error log if the
// middleware chain drops it. See that file's guard comment for details.
app.use('/webhook', express.json({
  // GitHub push-event payloads can exceed Express's 100KB default. Cap at
  // 1MB so we accept legitimate payloads but still bound request memory.
  limit: '1mb',
  verify: (req, _res, buf) => {
    req.rawBody = buf;
  }
}));

app.use(express.json({ limit: '1mb' }));

// Health check — verifies service is actually functional
app.get('/', (req, res) => {
  res.json({
    status: 'ok',
    service: 'QURL Discord Bot',
  });
});

app.get('/health', (req, res) => {
  // Actually probe the DB — if better-sqlite3 is blocked/locked we want the
  // health check to fail so the orchestrator replaces the container.
  try {
    db.getStats();
    res.status(200).json({ status: 'ok' });
  } catch (err) {
    // Log full detail internally; omit from the response so better-sqlite3
    // error messages (paths, schema) don't leak to an unauthenticated probe.
    logger.warn('Health check failed', { error: err.message });
    res.status(503).json({ status: 'unhealthy' });
  }
});

// Per-IP rate limit on /metrics. Even a token holder shouldn't be able to
// hammer the endpoint — getStats() does several SQL reads + memoryUsage() +
// uptime() every hit. Simple in-memory window; single-instance only
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
app.get('/metrics', metricsRateLimit, (req, res) => {
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
  const stats = db.getStats();
  res.json({
    status: 'ok',
    uptime: process.uptime(),
    memory: process.memoryUsage(),
    stats,
  });
});

// Mount routers
app.use('/auth', oauthRouter);
app.use('/webhook', webhooksRouter);

// Error handler (Express requires the 4-arg signature; `next` unused)
// eslint-disable-next-line no-unused-vars
app.use((err, req, res, next) => {
  logger.error('Express error', { error: err.message, stack: err.stack });
  res.status(500).send(renderPage({
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
    logger.info(`OAuth URL: ${config.BASE_URL}/auth/github`);
    logger.info(`Webhook URL: ${config.BASE_URL}/webhook/github`);
    logger.info(`Metrics URL: ${config.BASE_URL}/metrics`);
  });
  return server;
}

function stopIntervals() {
  clearInterval(metricsSweepInterval);
}

module.exports = { app, startServer, stopIntervals };
