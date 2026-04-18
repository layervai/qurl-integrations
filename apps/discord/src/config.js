const path = require('path');

// Safe int parser: handles NaN and falsy-zero correctly
function intEnv(key, defaultVal) {
  const v = parseInt(process.env[key], 10);
  return isNaN(v) ? defaultVal : v;
}

// Configuration from environment variables
module.exports = {
  // Discord
  DISCORD_TOKEN: process.env.DISCORD_TOKEN,
  DISCORD_CLIENT_ID: process.env.DISCORD_CLIENT_ID,
  GUILD_ID: process.env.GUILD_ID,

  // Role names for progression
  CONTRIBUTOR_ROLE_NAME: process.env.CONTRIBUTOR_ROLE_NAME || 'Contributor',
  ACTIVE_CONTRIBUTOR_ROLE_NAME: process.env.ACTIVE_CONTRIBUTOR_ROLE_NAME || 'Active Contributor',
  CORE_CONTRIBUTOR_ROLE_NAME: process.env.CORE_CONTRIBUTOR_ROLE_NAME || 'Core Contributor',
  CHAMPION_ROLE_NAME: process.env.CHAMPION_ROLE_NAME || 'Champion',

  // Role thresholds (lowered for realistic contribution cadence)
  ACTIVE_CONTRIBUTOR_THRESHOLD: intEnv('ACTIVE_CONTRIBUTOR_THRESHOLD', 3),
  CORE_CONTRIBUTOR_THRESHOLD: intEnv('CORE_CONTRIBUTOR_THRESHOLD', 10),
  CHAMPION_THRESHOLD: intEnv('CHAMPION_THRESHOLD', 25),

  // Channel names
  GENERAL_CHANNEL_NAME: process.env.GENERAL_CHANNEL_NAME || 'general',
  NOTIFICATION_CHANNEL_NAME: process.env.NOTIFICATION_CHANNEL_NAME || 'general',
  ANNOUNCEMENTS_CHANNEL_NAME: process.env.ANNOUNCEMENTS_CHANNEL_NAME || 'announcements',
  CONTRIBUTE_CHANNEL_NAME: process.env.CONTRIBUTE_CHANNEL_NAME || 'contribute',
  GITHUB_FEED_CHANNEL_NAME: process.env.GITHUB_FEED_CHANNEL_NAME || 'github-feed',

  // GitHub OAuth
  GITHUB_CLIENT_ID: process.env.GITHUB_CLIENT_ID,
  GITHUB_CLIENT_SECRET: process.env.GITHUB_CLIENT_SECRET,
  GITHUB_WEBHOOK_SECRET: process.env.GITHUB_WEBHOOK_SECRET,

  // Allowed GitHub organizations (comma-separated)
  ALLOWED_GITHUB_ORGS: (process.env.ALLOWED_GITHUB_ORGS || 'OpenNHP').split(',').map(s => s.trim().toLowerCase()).filter(Boolean),

  // Server
  PORT: intEnv('PORT', 3000),
  BASE_URL: process.env.BASE_URL || 'http://localhost:3000',

  // Rate limiting
  RATE_LIMIT_WINDOW_MS: intEnv('RATE_LIMIT_WINDOW_MS', 60000), // 1 minute
  RATE_LIMIT_MAX_REQUESTS: intEnv('RATE_LIMIT_MAX_REQUESTS', 30),

  // OAuth link expiry (in minutes)
  // Shortened from 30 to 10 minutes: the OAuth state is not bound to the
  // initiating browser session, so a shorter expiry narrows the window for
  // a leaked/shoulder-surfed state token to be replayed by an attacker.
  PENDING_LINK_EXPIRY_MINUTES: intEnv('PENDING_LINK_EXPIRY_MINUTES', 10),

  // Database — absolute path so the DB is anchored to the bot's source tree
  // regardless of the cwd the process was launched from.
  DATABASE_PATH: process.env.DATABASE_PATH
    ? path.resolve(process.env.DATABASE_PATH)
    // Keep the 'opennhp-bot.db' filename: it matches the mounted EFS volume
    // for existing deployments. Migrating requires a rename operation in
    // infra. Set DATABASE_PATH env to override for new deployments.
    : path.resolve(__dirname, '..', 'data', 'opennhp-bot.db'),

  // Admin Discord user IDs (comma-separated) - can use /forcelink, /bulklink, /unlinked
  ADMIN_USER_IDS: (process.env.ADMIN_USER_IDS || '').split(',').map(s => s.trim()).filter(Boolean),

  // Milestones to announce (star counts) - extended for mature repos
  STAR_MILESTONES: [10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 15000, 20000, 25000, 50000, 75000, 100000],

  // Weekly digest schedule (cron format) - default Sunday 9am UTC
  WEEKLY_DIGEST_CRON: process.env.WEEKLY_DIGEST_CRON || '0 9 * * 0',

  // Welcome message (for new member DM)
  WELCOME_DM_ENABLED: process.env.WELCOME_DM_ENABLED !== 'false',

  // QURL. In production we fall back to the real endpoints; in dev we fall
  // back to localhost so a missing .env file doesn't silently hit prod APIs.
  // index.js enforces that both env vars are set when NODE_ENV=production.
  QURL_API_KEY: process.env.QURL_API_KEY,
  QURL_ENDPOINT: process.env.QURL_ENDPOINT
    || (process.env.NODE_ENV === 'production' ? 'https://api.layerv.ai' : 'http://localhost:8080'),

  // qurl-s3-connector
  CONNECTOR_URL: process.env.CONNECTOR_URL
    || (process.env.NODE_ENV === 'production' ? 'https://get.qurl.link:9808' : 'http://localhost:9808'),

  // Google Maps (location autocomplete)
  GOOGLE_MAPS_API_KEY: process.env.GOOGLE_MAPS_API_KEY,

  // /qurl send limits
  QURL_SEND_MAX_RECIPIENTS: intEnv('QURL_SEND_MAX_RECIPIENTS', 50),
  QURL_SEND_COOLDOWN_MS: intEnv('QURL_SEND_COOLDOWN_MS', 30000),
};
