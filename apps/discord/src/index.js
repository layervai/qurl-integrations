const config = require('./config');
const logger = require('./logger');
const { client, shutdown: discordShutdown } = require('./discord');
const { registerCommands, handleCommand } = require('./commands');
const { startServer } = require('./server');
const db = require('./database');

// Validate required config
const required = ['DISCORD_TOKEN', 'GITHUB_CLIENT_ID', 'GITHUB_CLIENT_SECRET', 'GITHUB_WEBHOOK_SECRET', 'GUILD_ID'];
const missing = required.filter(key => !config[key]);

if (missing.length > 0) {
  logger.error('Missing required environment variables:');
  missing.forEach(key => logger.error(`  - ${key}`));
  logger.error('See .env.example for required variables.');
  process.exit(1);
}

// Validate numeric config values
if (isNaN(config.PENDING_LINK_EXPIRY_MINUTES) || config.PENDING_LINK_EXPIRY_MINUTES <= 0) {
  logger.error('PENDING_LINK_EXPIRY_MINUTES must be a positive integer');
  process.exit(1);
}

// Register commands when ready
client.once('ready', async () => {
  try {
    await registerCommands(client);
  } catch (error) {
    logger.error('Failed to register commands', { error: error.message });
  }
});

// Handle interactions
client.on('interactionCreate', handleCommand);

// Error handling
client.on('error', error => {
  logger.error('Discord client error', { error: error.message });
});

process.on('unhandledRejection', error => {
  logger.error('Unhandled promise rejection', { error: error?.message || error });
  gracefulShutdown(1);
});

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
    if (httpServer) httpServer.close();
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
