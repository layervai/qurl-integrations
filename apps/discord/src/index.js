const config = require('./config');
const logger = require('./logger');
const { client, shutdown: discordShutdown } = require('./discord');
const { registerCommands, handleCommand } = require('./commands');
const { startServer } = require('./server');
const db = require('./database');

// Validate required config
const required = ['DISCORD_TOKEN', 'GITHUB_CLIENT_ID', 'GITHUB_CLIENT_SECRET', 'GITHUB_WEBHOOK_SECRET'];
const missing = required.filter(key => !config[key]);

if (missing.length > 0) {
  logger.error('Missing required environment variables:');
  missing.forEach(key => logger.error(`  - ${key}`));
  logger.error('See .env.example for required variables.');
  process.exit(1);
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

process.on('unhandledRejection', error => {
  logger.error('Unhandled promise rejection', { error: error?.message || error });
});

process.on('uncaughtException', error => {
  logger.error('Uncaught exception', { error: error.message, stack: error.stack });
  gracefulShutdown(1);
});

// Graceful shutdown
let isShuttingDown = false;

async function gracefulShutdown(code = 0) {
  if (isShuttingDown) return;
  isShuttingDown = true;

  logger.info('Graceful shutdown initiated...');

  try {
    // Close Discord client
    discordShutdown();

    // Close database
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
  logger.info(`Version: 1.1.0`);
  logger.info(`Environment: ${process.env.NODE_ENV || 'development'}`);

  // Start web server
  startServer();

  // Login to Discord
  await client.login(config.DISCORD_TOKEN);
}

start().catch(error => {
  logger.error('Failed to start', { error: error.message });
  process.exit(1);
});
