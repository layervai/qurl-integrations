// Express server for OAuth and webhooks
const crypto = require('crypto');
const express = require('express');
const config = require('./config');
const db = require('./database');
const logger = require('./logger');
const { renderPage } = require('./templates/page');
const oauthRouter = require('./routes/oauth');
const webhooksRouter = require('./routes/webhooks');

const app = express();

// Trust proxy headers (ECS behind ALB/CloudFront) for correct req.ip in rate limiting
app.set('trust proxy', 1);

// Global security headers on every response. The OAuth template already sets
// its own inline CSP for the HTML pages; these harden the JSON endpoints
// (health, metrics, webhooks) against common browser-level attacks.
app.use((req, res, next) => {
  res.setHeader('X-Content-Type-Options', 'nosniff');
  res.setHeader('X-Frame-Options', 'DENY');
  res.setHeader('Referrer-Policy', 'no-referrer');
  res.setHeader('Strict-Transport-Security', 'max-age=31536000; includeSubDomains');
  next();
});

// Parse JSON for webhooks with raw body for signature verification. MUST be
// registered BEFORE the general app.use(express.json()) below so /webhook
// requests hit this parser first and get req.rawBody populated. Do not
// reorder without also updating routes/webhooks.js verifySignature().
app.use('/webhook', express.json({
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

// Metrics endpoint
app.get('/metrics', (req, res) => {
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

module.exports = { app, startServer };
