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

// trust proxy 1: assumes single ALB hop. If exposed directly, attackers can spoof X-Forwarded-For.
app.set('trust proxy', 1);

// Parse JSON for webhooks with raw body for signature verification
app.use('/webhook', express.json({
  verify: (req, res, buf) => {
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
  const healthy = true; // DB is synchronous (better-sqlite3), always available if process is up
  res.status(healthy ? 200 : 503).json({ status: healthy ? 'ok' : 'unhealthy' });
});

// Warn on startup if METRICS_TOKEN is not set in production
if (process.env.NODE_ENV === 'production' && !process.env.METRICS_TOKEN) {
  logger.warn('METRICS_TOKEN is not set — /metrics endpoint is unauthenticated in production');
}

// Metrics endpoint
app.get('/metrics', (req, res) => {
  // Require bearer token if METRICS_TOKEN is configured (production)
  if (process.env.METRICS_TOKEN) {
    const auth = req.headers.authorization || '';
    const expected = `Bearer ${process.env.METRICS_TOKEN}`;
    const authBuf = Buffer.from(auth);
    const expectedBuf = Buffer.from(expected);
    if (authBuf.length !== expectedBuf.length || !crypto.timingSafeEqual(authBuf, expectedBuf)) {
      return res.status(401).json({ error: 'Unauthorized' });
    }
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

// Error handler
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
