const config = require('./config');
const logger = require('./logger');
const { client, shutdown: discordShutdown } = require('./discord');
const { registerCommands, handleCommand } = require('./commands');
const { startServer } = require('./server');
const db = require('./database');

// Validate required config. Fail fast at boot so misconfigurations are caught
// during deploy, not when the first request arrives.
const required = ['DISCORD_TOKEN', 'GITHUB_CLIENT_ID', 'GITHUB_CLIENT_SECRET', 'GITHUB_WEBHOOK_SECRET', 'GUILD_ID', 'BASE_URL'];
const missing = required.filter(key => !config[key]);

if (missing.length > 0) {
  logger.error('Missing required environment variables:');
  missing.forEach(key => logger.error(`  - ${key}`));
  logger.error('See .env.example for required variables.');
  process.exit(1);
}

// Production-only required secrets. In dev these are optional so localhost
// workflows stay convenient.
if (process.env.NODE_ENV === 'production') {
  const prodRequired = ['METRICS_TOKEN', 'QURL_API_KEY', 'KEY_ENCRYPTION_KEY'];
  const prodMissing = prodRequired.filter(k => !process.env[k]);
  if (prodMissing.length > 0) {
    logger.error(`NODE_ENV=production but missing required env vars: ${prodMissing.join(', ')}`);
    logger.error('For KEY_ENCRYPTION_KEY, generate with: node -e "console.log(require(\'crypto\').randomBytes(32).toString(\'base64\'))"');
    process.exit(1);
  }
  // OAuth state flows through BASE_URL; plaintext http:// would expose the
  // state token (and the redirect itself) to any network path observer.
  if (!config.BASE_URL.startsWith('https://')) {
    logger.error(`BASE_URL must use https:// in production (got ${config.BASE_URL})`);
    process.exit(1);
  }
}

// Validate numeric config values
if (isNaN(config.PENDING_LINK_EXPIRY_MINUTES) || config.PENDING_LINK_EXPIRY_MINUTES <= 0) {
  logger.error('PENDING_LINK_EXPIRY_MINUTES must be a positive integer');
  process.exit(1);
}
if (!Number.isFinite(config.RATE_LIMIT_WINDOW_MS) || config.RATE_LIMIT_WINDOW_MS <= 0) {
  logger.error('RATE_LIMIT_WINDOW_MS must be a positive integer (set to 0 would disable rate limiting)');
  process.exit(1);
}
if (!Number.isFinite(config.RATE_LIMIT_MAX_REQUESTS) || config.RATE_LIMIT_MAX_REQUESTS <= 0) {
  logger.error('RATE_LIMIT_MAX_REQUESTS must be a positive integer');
  process.exit(1);
}

if (config.QURL_ENDPOINT === 'https://api.layerv.ai') {
  logger.warn('QURL_ENDPOINT is using production default — set via env var for non-prod');
}
if (config.CONNECTOR_URL === 'https://get.qurl.link:9808') {
  logger.warn('CONNECTOR_URL is using production default — set via env var for non-prod');
}

// Register commands when ready
client.once('ready', async () => {
  await registerCommands(client);
});

// Handle interactions
client.on('interactionCreate', handleCommand);

// Error handling
client.on('error', error => {
  logger.error('Discord client error', { error: error.message });
});

// Log and continue on unhandled rejections. The old behavior killed the
// entire process on any stray rejection (transient Discord timeouts, network
// blips) which made the bot fragile. Truly fatal errors surface via
// uncaughtException below.
process.on('unhandledRejection', (error, _promise) => {
  logger.error('Unhandled promise rejection (logged, not fatal)', {
    error: error?.message || error,
    stack: error?.stack,
  });
});

// Uncaught exceptions indicate corrupted process state — no safe recovery.
process.on('uncaughtException', error => {
  logger.error('Uncaught exception', { error: error.message, stack: error.stack });
  gracefulShutdown(1);
});

// Graceful shutdown
let httpServer = null;
let isShuttingDown = false;

async function gracefulShutdown(code = 0) {
  if (isShuttingDown) return;
  isShuttingDown = true;

  // Force exit after 10s if shutdown hangs
  setTimeout(() => { logger.error('Shutdown timed out, forcing exit'); process.exit(1); }, 10000).unref();

  logger.info('Graceful shutdown initiated...');

  try {
    // Wait for in-flight HTTP requests to drain — server.close() is async,
    // process.exit() called immediately after would truncate OAuth callbacks
    // mid-flight and leave users with a consumed pending_link but no GitHub
    // link created.
    if (httpServer) {
      await new Promise(resolve => {
        httpServer.close(err => {
          if (err) logger.warn('HTTP server close reported error', { error: err.message });
          resolve();
        });
      });
    }
    discordShutdown();
    db.close();
    logger.info('Shutdown complete');
  } catch (error) {
    logger.error('Error during shutdown', { error: error.message });
  }

  process.exit(code);
}

// Handle shutdown signals
process.on('SIGTERM', () => {
  logger.info('Received SIGTERM');
  gracefulShutdown(0);
});

process.on('SIGINT', () => {
  logger.info('Received SIGINT');
  gracefulShutdown(0);
});

// Start everything
async function start() {
  logger.info('Starting OpenNHP Bot...');
  logger.info(`Version: ${require('../package.json').version}`);
  logger.info(`Environment: ${process.env.NODE_ENV || 'development'}`);

  // Start web server
  httpServer = startServer();

  // Login to Discord
  await client.login(config.DISCORD_TOKEN);
}

start().catch(error => {
  logger.error('Failed to start', { error: error.message });
  process.exit(1);
});
