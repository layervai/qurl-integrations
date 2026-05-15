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
//       Single-guild plain qURL sharing tool. Only /qurl registers
//       (scoped to the guild for instant propagation); no role or
//       channel creation; /auth and /webhook routes dormant. Needs
//       only the 4 runtime perms (ViewChannel, SendMessages,
//       EmbedLinks, UseApplicationCommands).
//
//   (isMultiTenant, ENABLE_OPENNHP_FEATURES=false)
//       Multi-tenant plain qURL sharing tool. Commands register
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
// true; everywhere else the bot is a plain qURL sharing tool with no
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

// AUTH0_DOMAIN must be a bare hostname — the codebase composes it as
// `https://${AUTH0_DOMAIN}/...` for the JWKS endpoint, /authorize, and
// /oauth/token. Round-9 #2: typo'd values like 'https://layerv.auth0.com'
// or a placeholder string would silently flip isQurlOAuthConfigured on,
// then break under load with a confusing URL parse error. Reject at
// config-load time so the bot fails to enable OAuth (legacy fallback
// path activates) instead of crashing mid-flow.
function isValidAuth0DomainShape(d) {
  if (typeof d !== 'string' || !d) return false;
  // DNS-shape cap: RFC 1035 maxes at 253 chars total. Auth0 domains in
  // practice are short (e.g., layerv.us.auth0.com), but an explicit
  // cap is clearer than relying on a regex bound.
  if (d.length > 253) return false;
  // Reject any scheme prefix or path — domain only.
  if (/[:/?#]/.test(d)) return false;
  // Bare hostname, letters/digits/dots/dashes, must contain at least
  // one dot (rejects "placeholder" / "localhost" while permitting
  // custom Auth0 domains like auth.layerv.ai). Case-insensitive on
  // input — Auth0 domains are canonically lowercase but we don't
  // reject mixed case.
  return /^[a-z0-9][a-z0-9.-]+\.[a-z]{2,}$/i.test(d);
}

// True when all four Auth0 env vars are present AND AUTH0_DOMAIN is a
// well-shaped hostname — `/qurl setup` then uses the OAuth-redirect
// flow. False = degrade to the legacy modal-paste flow (kept until
// Justin registers the Auth0 app + sets prod SSM secrets). Single
// derivation point so commands.js + routes/qurl-oauth.js + server.js
// agree on what "configured" means.
const isQurlOAuthConfigured = Boolean(
  isValidAuth0DomainShape(process.env.AUTH0_DOMAIN)
  && process.env.AUTH0_CLIENT_ID
  && process.env.AUTH0_CLIENT_SECRET
  && process.env.AUTH0_AUDIENCE,
);

// True when the Stage-2 "Add to Discord, select server" install flow can
// run end-to-end: needs the bot's Discord OAuth2 client secret (separate
// from the bot token used for normal operations) on top of qURL OAuth.
// The /oauth/discord/callback route gates on this; it returns 503 with a
// "not configured" page when false, so the install link still completes
// (bot lands in the server) but the chained Auth0 leg won't run until
// Justin sets DISCORD_CLIENT_SECRET in SSM.
const isDiscordInstallConfigured = Boolean(
  isQurlOAuthConfigured && process.env.DISCORD_CLIENT_SECRET,
);

// Shard identifier for the shard-aware composite flow_id
// (`<shard_id>#<guild_id>#<channel_id>#<user_id>`, see src/flow-id.js).
// Single-shard today, so the default is `'0:1'` (shard 0 of 1) per the
// zero-downtime design doc. When sharding lands, the runtime will set
// SHARD_ID per process to `'k:n'` (e.g. `'3:8'` for shard 3 of 8); the
// schema is already shard-aware so no migration is needed. Single
// export point so a future grep-and-replace doesn't miss a callsite.
const SHARD_ID = process.env.SHARD_ID || '0:1';

// Configuration from environment variables
module.exports = {
  // Discord
  DISCORD_TOKEN: process.env.DISCORD_TOKEN,
  DISCORD_CLIENT_ID: process.env.DISCORD_CLIENT_ID,
  // Required for the Stage-2 "Add to Discord, select server" install
  // callback (src/routes/discord-install.js). Not used by normal bot
  // operations — only by the OAuth2 token exchange when an admin
  // installs the bot via the install link. Optional: omit and the
  // /oauth/discord/callback route will return 503 with a documented
  // "not configured" page until Justin sets the secret.
  DISCORD_CLIENT_SECRET: process.env.DISCORD_CLIENT_SECRET,
  GUILD_ID: normalizedGuildId,
  isMultiTenant,
  ENABLE_OPENNHP_FEATURES: enableOpenNHPFeatures,
  isOpenNHPActive,
  isQurlOAuthConfigured,
  isDiscordInstallConfigured,

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

  // qURL OAuth (Auth0) — for /qurl setup admin consent flow.
  // When unset, /qurl setup falls back to the legacy modal-paste path so the
  // bot stays usable until Justin registers the Auth0 application + drops
  // these into prod SSM. See project_qurl_bot_onboarding_model.md memory for
  // the OAuth-app shape (Regular Web Application, callback URL = BASE_URL +
  // /oauth/qurl/callback, scopes qurl:write + qurl:read).
  AUTH0_DOMAIN: process.env.AUTH0_DOMAIN,
  AUTH0_CLIENT_ID: process.env.AUTH0_CLIENT_ID,
  AUTH0_CLIENT_SECRET: process.env.AUTH0_CLIENT_SECRET,
  AUTH0_AUDIENCE: process.env.AUTH0_AUDIENCE,

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

  // /qurl map feature toggle. Default OFF — the bot ships without
  // /qurl map registered as a slash subcommand. Operator opts in per
  // deploy by setting MAP_COMMAND_ENABLED=true on the task definition
  // (terraform var `map_command_enabled` plumbs this through). Must
  // be the literal string "true"; any other value (unset, empty,
  // "TRUE", "1", "yes") keeps the feature off, so an env-var typo
  // can't silently re-enable a command that requires a working
  // GOOGLE_MAPS_API_KEY in SSM. See the slash-command builder IIFE
  // in commands.js for the full set of MAP_COMMAND_ENABLED gates.
  //
  // Snapshot semantics: this value is read ONCE at module load and
  // baked into the slash registration (commands.js IIFE) +
  // SETUP_SUCCESS_MSG. Flipping MAP_COMMAND_ENABLED at runtime is
  // a no-op until the task restarts; the deploy model handles this
  // (ECS rolls fresh tasks on every task-def revision).
  MAP_COMMAND_ENABLED: process.env.MAP_COMMAND_ENABLED === 'true',

  // qURL send limits (/qurl send + /qurl map) — both must be > 0. A
  // cooldown of 0 would silently disable the rate limit; a recipients
  // cap of 0 would reject every send.
  //
  // 20,000 default chosen to accommodate the voice-everyone path against
  // stage channels (Discord's largest gathering surface). Heads-up for
  // operators reading the release notes: the same cap bounds EVERY
  // recipient-resolution path — `<@user>` list expansion, `<@&role>`
  // expansion, `<#voice>` expansion, the UserSelectMenu picker, AND
  // `@everyone` (gated on MENTION_EVERYONE). The 50 → 20,000 jump
  // therefore re-shapes the `@everyone` blast radius by 400× for any
  // member granted MENTION_EVERYONE in a guild that takes the new
  // default. Guilds that intentionally constrained the prior 50-recipient
  // ceiling for non-voice surfaces should set `QURL_SEND_MAX_RECIPIENTS`
  // to the desired bound in their env; the voice-everyone path will then
  // partial-resolve up to that bound rather than refusing.
  // Operational implications a max-size send carries:
  //   - up to ceil(20000/TOKENS_PER_RESOURCE) = 2000 re-uploads to
  //     qurl-service per send (`mintLinksInBatches`).
  //   - DM delivery is bounded by Discord's per-bot DM rate limit
  //     (~5/sec); a 20k send takes >1 hour to finish DM fan-out, and
  //     `monitorLinkStatus`'s interval-based progress tracking must
  //     survive that duration. A regression test pinning that
  //     lifetime is tracked in issue #331 — open against any change
  //     that touches `monitorLinkStatus`'s interval/cleanup logic
  //     until it lands.
  // Per-guild operators can dial this down via the env override if
  // their qurl-service plan or DM-throughput posture demands it.
  QURL_SEND_MAX_RECIPIENTS: intEnv('QURL_SEND_MAX_RECIPIENTS', 20000, { minPositive: true }),
  QURL_SEND_COOLDOWN_MS: intEnv('QURL_SEND_COOLDOWN_MS', 30000, { minPositive: true }),

  SHARD_ID,

  // Event-shipper (zero-downtime design, Pillar 1). When true, the
  // gateway tier forwards every Discord dispatch to SQS instead of
  // running handlers in-process, and the worker tier (PROCESS_ROLE=http
  // or combined) polls SQS and routes events through the same
  // handleCommand / handleFlowInteraction path. When false, the bot
  // runs the legacy in-process shape — gateway role both receives WS
  // dispatches AND runs handlers; worker role is dormant on the queue.
  //
  // Must be the literal string "true" — same shape as
  // ENABLE_OPENNHP_FEATURES so an env-var typo (TRUE/1/yes/etc.) can't
  // silently flip a production deploy into the new dispatch path.
  //
  // Rollback cliff: this flag is valid only through PR 10 (gateway
  // strip-down). The follow-up that removes the in-process fallback
  // also removes this flag; after that, rollback is a `git revert` +
  // emergency redeploy. See `apps/discord/docs/zero-downtime-design.md`
  // → "Rollback cliff: ENABLE_EVENT_SHIPPER".
  ENABLE_EVENT_SHIPPER: process.env.ENABLE_EVENT_SHIPPER === 'true',

  // SQS Standard queue the gateway publishes to and the worker
  // consumes from (provisioned by qurl-integrations-infra PR B).
  // Required when ENABLE_EVENT_SHIPPER=true; validated at boot in
  // index.js so a misconfigured deploy fails closed instead of
  // silently no-op'ing the consumer or dropping every dispatch on
  // the producer side.
  QURL_BOT_EVENTS_QUEUE_URL: process.env.QURL_BOT_EVENTS_QUEUE_URL,
};
