const path = require('path');

// Safe int parser: handles NaN and falsy-zero correctly. If minPositive
// is set (the common case for cooldowns + caps), reject non-positive
// values — an env of "0" would otherwise silently disable a cooldown or
// block every send.
function intEnv(key, defaultVal, { minPositive = false } = {}) {
  const v = parseInt(process.env[key], 10);
  if (isNaN(v)) return defaultVal;
  if (minPositive && v <= 0) {
    console.warn(`[config] ${key}=${v} rejected (must be > 0); using default ${defaultVal}`);
    return defaultVal;
  }
  return v;
}

// Normalize GUILD_ID: accept only a valid Discord snowflake (17–20 digits).
// Any other value — including an unset env, the literal string "PLACEHOLDER"
// that SSM-seeded params carry, or a whitespace-only value — normalizes to
// null so every downstream truthy check (`if (config.GUILD_ID)`) correctly
// treats the bot as multi-tenant. Prevents a malformed SSM value from
// silently registering commands to a nonexistent guild.
const rawGuildId = process.env.GUILD_ID;
let normalizedGuildId = null;
if (rawGuildId) {
  const trimmed = rawGuildId.trim();
  if (/^\d{17,20}$/.test(trimmed)) {
    normalizedGuildId = trimmed;
  } else {
    // logger isn't loaded this early in config import — use console directly.
    console.warn(`[config] GUILD_ID=${JSON.stringify(rawGuildId)} is not a valid Discord snowflake (17-20 digits); starting in multi-tenant mode. To run in single-guild mode, set GUILD_ID to a real guild ID.`);
  }
}

// Multi-tenant mode: derived once here, consumed everywhere else. When true,
// the bot treats itself as a public multi-server app (commands global,
// OpenNHP features dormant, /auth + /webhook routes not mounted). When
// false, the bot runs in single-guild mode targeting normalizedGuildId.
// Keeping this derived in config.js (single source of truth) means every
// downstream check is `if (config.isMultiTenant)` — semantic name at
// every callsite.
//
// Together with ENABLE_OPENNHP_FEATURES (below), this selects one of
// three supported modes:
//
//   (!isMultiTenant, ENABLE_OPENNHP_FEATURES=true)
//       Single-guild OpenNHP community server. Full command set
//       registers scoped to the guild; ensureRolesAndChannels creates
//       contributor roles + #contribute / #github-feed; /auth and
//       /webhook routes mount; weekly digest runs. Requires
//       ManageRoles + ManageChannels perms in the guild.
//
//   (!isMultiTenant, ENABLE_OPENNHP_FEATURES=false)
//       Single-guild plain /qurl send tool. Only /qurl registers
//       (scoped to the guild for instant propagation); no role or
//       channel creation; /auth and /webhook routes dormant. Needs
//       only the 4 runtime perms (ViewChannel, SendMessages,
//       EmbedLinks, UseApplicationCommands).
//
//   (isMultiTenant, ENABLE_OPENNHP_FEATURES=false)
//       Multi-tenant plain /qurl send tool. Commands register
//       globally (up to 1 hr Discord cache propagation); per-guild
//       config via /qurl setup; every OpenNHP code path is gated off.
//       Default for the public-bot install.
//
//   (isMultiTenant, ENABLE_OPENNHP_FEATURES=true)
//       Not a supported combination. OpenNHP behaviors need a
//       specific guild cache to target; the ready handler skips its
//       single-guild setup when isMultiTenant, so the flag has no
//       effect in multi-tenant mode.
const isMultiTenant = !normalizedGuildId;

// OpenNHP community features (role auto-creation + auto-assign, channel
// auto-creation, welcome DM, badge announcements). Default OFF so a
// vanilla install of the bot into any guild — single-tenant or
// multi-tenant — only exercises the 4 runtime permissions it was
// invited with (View Channels, Send Messages, Embed Links, Use
// Application Commands). Only the OpenNHP community server sets this
// true; everywhere else the bot is a plain /qurl send tool with no
// elevated expectations. Must be the literal string "true" — any other
// value (including unset, empty, "TRUE", "1", "yes") keeps it disabled,
// so an env-var typo can't silently re-enable role/channel creation
// attempts in a guild that hasn't granted those permissions.
const enableOpenNHPFeatures = process.env.ENABLE_OPENNHP_FEATURES === 'true';

// Unsupported combination — catch at config load so an operator doesn't
// spend time wondering why "ENABLE_OPENNHP_FEATURES=true" had no effect
// in multi-tenant mode. logger isn't available this early (config is
// a require() dependency of logger's callers), so use console.warn.
if (!normalizedGuildId && enableOpenNHPFeatures) {
  console.warn('[config] ENABLE_OPENNHP_FEATURES=true is ignored when GUILD_ID is unset (multi-tenant mode): OpenNHP behaviors target a cached single guild that multi-tenant mode never populates. Either set GUILD_ID to the OpenNHP guild snowflake, or clear ENABLE_OPENNHP_FEATURES to silence this warning.');
}

// Single source of truth for "OpenNHP is active". Consumed by
// commands.js (command-set filter), server.js (route-mount gate),
// boot-requirements.js (which env-vars are required), and discord.js
// (every OpenNHP short-circuit). Deriving in one place means a future
// change to the predicate — e.g. adding a third flag, or broadening
// what "multi-tenant" means — only touches this file.
const isOpenNHPActive = !isMultiTenant && enableOpenNHPFeatures;

// Configuration from environment variables
module.exports = {
  // Discord
  DISCORD_TOKEN: process.env.DISCORD_TOKEN,
  DISCORD_CLIENT_ID: process.env.DISCORD_CLIENT_ID,
  GUILD_ID: normalizedGuildId,
  isMultiTenant,
  ENABLE_OPENNHP_FEATURES: enableOpenNHPFeatures,
  isOpenNHPActive,

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

  // Admin Discord user IDs (comma-separated) — can use /forcelink, /bulklink,
  // /unlinked. Each entry is validated to look like a Discord snowflake
  // (17–20 digits) so a typo like "1234, 5678 " (stray space or non-numeric)
  // can't silently create a dead admin ID that never matches an interaction.
  ADMIN_USER_IDS: (process.env.ADMIN_USER_IDS || '')
    .split(',')
    .map(s => s.trim())
    .filter(s => {
      if (!s) return false;
      if (!/^\d{17,20}$/.test(s)) {
        // Using console.warn directly — logger isn't loaded this early in config import.
        console.warn(`[config] Dropping malformed ADMIN_USER_IDS entry (not a Discord snowflake): ${JSON.stringify(s)}`);
        return false;
      }
      return true;
    }),

  // Milestones to announce (star counts) - extended for mature repos
  STAR_MILESTONES: [10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 15000, 20000, 25000, 50000, 75000, 100000],

  // Weekly digest schedule (cron format) - default Sunday 9am UTC
  WEEKLY_DIGEST_CRON: process.env.WEEKLY_DIGEST_CRON || '0 9 * * 0',

  // Welcome message (for new member DM)
  WELCOME_DM_ENABLED: process.env.WELCOME_DM_ENABLED !== 'false',

  // qURL. In production we fall back to the real endpoints; in dev we fall
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

  // /qurl send limits — both must be > 0. A cooldown of 0 would silently
  // disable the rate limit; a recipients cap of 0 would reject every send.
  QURL_SEND_MAX_RECIPIENTS: intEnv('QURL_SEND_MAX_RECIPIENTS', 50, { minPositive: true }),
  QURL_SEND_COOLDOWN_MS: intEnv('QURL_SEND_COOLDOWN_MS', 30000, { minPositive: true }),

  // Store backend selection. Read directly in `src/store/index.js` (not
  // via this object) because it's a boot-time choice that controls how
  // the store module is initialized — shoehorning it into the config
  // object would require store/index.js to import config, which in turn
  // inverts the intended "config is consumed by everyone, depends on
  // nothing app-internal" layering.
  //
  //   sqlite (default) — better-sqlite3 over local file / EFS. Current
  //                      prod + sandbox setup pre-PR-4b.
  //   ddb              — DynamoDB, landing in PR 4b. Required for
  //                      horizontal scale (desired_count > 1) on the
  //                      greenfield prod bot module.
  //
  // Surfaced here as a doc-only reference so the env-var surface is
  // discoverable in one place. Unknown values throw at boot (see
  // `store/index.js`); a typo like `STORE_TYPE=sqlitte` fails loud,
  // never silently falls back to the default.
  // STORE_TYPE: process.env.STORE_TYPE || 'sqlite',
};
